package store

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// BatchWriteOperation 描述一次批量写入中的 put 或 delete。
type BatchWriteOperation struct {
	TableName   string
	Key         string
	Delete      bool
	ContentType ValueType
	Value       []byte
	UpdatedAt   int64
	Condition   WriteCondition
}

// BatchOperationFailure 标识批量操作中的失败项。
type BatchOperationFailure struct {
	Index int
	Table string
	Key   string
	Err   error
}

func (e *BatchOperationFailure) Error() string {
	return fmt.Sprintf("operation %d %s/%s: %v", e.Index, e.Table, e.Key, e.Err)
}

func (e *BatchOperationFailure) Unwrap() error {
	return e.Err
}

type oldBlobCandidate struct {
	table Table
	hash  string
}

// BatchWriteAtomic 在同一个 project.db 事务中执行多个 table 的 put/delete。
func (s *Store) BatchWriteAtomic(
	p Project,
	operations []BatchWriteOperation,
) ([]KeyMeta, error) {
	if len(operations) == 0 {
		return nil, fmt.Errorf("%w: operations 不能为空", ErrInvalid)
	}
	if err := ValidateBatchOperations(operations); err != nil {
		return nil, err
	}

	db, release, err := s.acquireProjectDB(p)
	if err != nil {
		return nil, err
	}
	defer release()
	blobLocked := true
	s.blobLifecycleMu.RLock()
	defer func() {
		if blobLocked {
			s.blobLifecycleMu.RUnlock()
		}
	}()
	tx, err := db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	results := make([]KeyMeta, 0, len(operations))
	var createdBlobPaths []string
	var createdTableDirs []string
	var oldBlobs []oldBlobCandidate
	tableLookup := make(map[string]Table)
	committed := false
	defer func() {
		if committed {
			return
		}
		for _, path := range createdBlobPaths {
			_ = os.Remove(path)
		}
		for _, dir := range createdTableDirs {
			_ = os.RemoveAll(dir)
		}
	}()

	for i, operation := range operations {
		now := time.Now().UnixMilli()
		var (
			meta      KeyMeta
			removeOld string
		)
		if operation.Delete {
			table, ok := tableLookup[operation.TableName]
			if !ok {
				table, err = getTableTx(tx, operation.TableName)
				if err != nil {
					return nil, batchFailure(i, operation, err)
				}
				tableLookup[operation.TableName] = table
			}
			meta, removeOld, err = s.deleteKeyTx(
				tx, p, table, operation.Key, operation.UpdatedAt, operation.Condition,
			)
			if err != nil {
				return nil, batchFailure(i, operation, err)
			}
			if removeOld != "" {
				oldBlobs = append(oldBlobs, oldBlobCandidate{table: table, hash: removeOld})
			}
		} else {
			table, ok := tableLookup[operation.TableName]
			tableCreated := false
			if !ok {
				var lookupErr error
				table, tableCreated, lookupErr = ensureTableTx(tx, operation.TableName, now, true)
				if lookupErr != nil {
					return nil, batchFailure(i, operation, lookupErr)
				}
				tableLookup[operation.TableName] = table
			}
			state, err := loadRecordState(tx, table, operation.Key)
			if err != nil {
				return nil, batchFailure(i, operation, err)
			}
			matches, err := s.matchesCondition(p, table, state, operation.Condition)
			if err != nil {
				return nil, batchFailure(i, operation, err)
			}
			if !matches {
				return nil, batchFailure(i, operation, preconditionFailed(operation.Key, state))
			}

			if err := ValidateValueType(operation.ContentType); err != nil {
				return nil, batchFailure(i, operation, err)
			}
			size := int64(len(operation.Value))
			if s.maxValueBytes > 0 && size > s.maxValueBytes {
				return nil, batchFailure(i, operation, ErrTooLarge)
			}
			hash := sha256.Sum256(operation.Value)
			hashHex := hex.EncodeToString(hash[:])
			var (
				inline   []byte
				blobHash sql.NullString
			)
			if size <= s.inlineThreshold {
				inline = operation.Value
			} else {
				blobDir, err := s.ensureTableBlobDir(p, table)
				if err != nil {
					return nil, batchFailure(i, operation, err)
				}
				if tableCreated {
					createdTableDirs = append(createdTableDirs, blobDir)
				}
				blobPath := filepath.Join(blobDir, hashHex)
				created, err := writeBlobFile(blobPath, operation.Value)
				if err != nil {
					return nil, batchFailure(i, operation, err)
				}
				if created {
					createdBlobPaths = append(createdBlobPaths, blobPath)
				}
				blobHash = sql.NullString{String: hashHex, Valid: true}
			}
			meta, removeOld, err = s.upsertLocked(
				tx, p, table, operation.Key, operation.ContentType, hashHex,
				inline, blobHash, size, now, operation.UpdatedAt, state,
			)
			if err != nil {
				return nil, batchFailure(i, operation, err)
			}
			if removeOld != "" {
				oldBlobs = append(oldBlobs, oldBlobCandidate{table: table, hash: removeOld})
			}
		}
		results = append(results, meta)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	committed = true
	if blobLocked {
		s.blobLifecycleMu.RUnlock()
		blobLocked = false
	}
	s.cleanupOldBatchBlobs(db, p, oldBlobs)
	return results, nil
}

func ValidateBatchOperations(operations []BatchWriteOperation) error {
	seen := make(map[string]struct{}, len(operations))
	for i, operation := range operations {
		if !ValidateTableName(operation.TableName) {
			return batchFailure(i, operation, fmt.Errorf("%w: table 名称不合法", ErrInvalid))
		}
		if err := ValidateKey(operation.Key); err != nil {
			return batchFailure(i, operation, err)
		}
		if operation.UpdatedAt <= 0 {
			return batchFailure(i, operation, fmt.Errorf("%w: updatedAt 必须是正整数 Unix 毫秒时间戳", ErrInvalid))
		}
		if err := ValidateWriteCondition(operation.Condition); err != nil {
			return batchFailure(i, operation, err)
		}
		if !operation.Delete {
			if err := ValidateValueType(operation.ContentType); err != nil {
				return batchFailure(i, operation, err)
			}
		}
		identity := operation.TableName + "\x00" + operation.Key
		if _, ok := seen[identity]; ok {
			return batchFailure(i, operation, fmt.Errorf("%w: 同一批次不能重复操作相同 table/key", ErrInvalid))
		}
		seen[identity] = struct{}{}
	}
	return nil
}

func batchFailure(index int, operation BatchWriteOperation, err error) error {
	return &BatchOperationFailure{
		Index: index,
		Table: operation.TableName,
		Key:   operation.Key,
		Err:   err,
	}
}

func writeBlobFile(path string, value []byte) (bool, error) {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, os.ErrExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	ok := false
	defer func() {
		_ = file.Close()
		if !ok {
			_ = os.Remove(path)
		}
	}()
	if _, err := file.Write(value); err != nil {
		return false, err
	}
	if err := file.Close(); err != nil {
		return false, err
	}
	ok = true
	return true, nil
}

func (s *Store) cleanupOldBatchBlobs(db *sql.DB, p Project, candidates []oldBlobCandidate) {
	s.blobLifecycleMu.Lock()
	defer s.blobLifecycleMu.Unlock()
	seen := make(map[string]struct{}, len(candidates))
	for _, candidate := range candidates {
		if candidate.hash == "" || !blobFileRe.MatchString(candidate.hash) {
			continue
		}
		identity := candidate.table.TableName + "\x00" + candidate.hash
		if _, ok := seen[identity]; ok {
			continue
		}
		seen[identity] = struct{}{}
		var refs int64
		q := fmt.Sprintf(`SELECT COUNT(*) FROM "%s" WHERE blob_hash = ?`, candidate.table.TableName)
		if err := db.QueryRow(q, candidate.hash).Scan(&refs); err != nil || refs > 0 {
			continue
		}
		_ = os.Remove(filepath.Join(s.tableBlobDir(p, candidate.table), candidate.hash))
	}
}

// PutBatchOperation 为事务关闭模式执行单个 put。
func (s *Store) PutBatchOperation(p Project, operation BatchWriteOperation) (KeyMeta, error) {
	return s.PutKeyConditional(
		p,
		operation.TableName,
		operation.Key,
		operation.ContentType,
		bytes.NewReader(operation.Value),
		operation.UpdatedAt,
		operation.Condition,
	)
}

// DeleteBatchOperation 为事务关闭模式执行单个 delete。
func (s *Store) DeleteBatchOperation(p Project, operation BatchWriteOperation) (KeyMeta, error) {
	return s.DeleteKeyConditional(
		p,
		operation.TableName,
		operation.Key,
		operation.UpdatedAt,
		operation.Condition,
	)
}

// BatchOperationDescription 生成错误消息中的操作描述。
func BatchOperationDescription(operation BatchWriteOperation) string {
	kind := "put"
	if operation.Delete {
		kind = "delete"
	}
	return strings.TrimSpace(kind + " " + operation.TableName + "/" + operation.Key)
}
