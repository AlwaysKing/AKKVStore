package store

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"
)

// PutKey 以流式方式写入 value，完整覆盖同名 key。
func (s *Store) PutKey(p Project, table, key, contentType string, r io.Reader) (KeyMeta, error) {
	return s.putKey(p, table, key, contentType, r, time.Now().UnixMilli(), WriteCondition{}, false)
}

// PutKeyCreate 只在 key 不存在或已删除时创建，不会覆盖当前有效记录。
func (s *Store) PutKeyCreate(p Project, table, key, contentType string, r io.Reader) (KeyMeta, error) {
	return s.putKey(p, table, key, contentType, r, time.Now().UnixMilli(), WriteCondition{}, true)
}

// PutKeyConditional 在可选条件满足时写入 value。
// table 不存在时，首次 PUT 会在同一事务中自动创建。
func (s *Store) PutKeyConditional(
	p Project,
	tableName, key, contentType string,
	r io.Reader,
	updatedAt int64,
	condition WriteCondition,
) (KeyMeta, error) {
	return s.putKey(p, tableName, key, contentType, r, updatedAt, condition, false)
}

func (s *Store) putKey(
	p Project,
	tableName, key, contentType string,
	r io.Reader,
	updatedAt int64,
	condition WriteCondition,
	createOnly bool,
) (KeyMeta, error) {
	if !ValidateTableName(tableName) {
		return KeyMeta{}, fmt.Errorf("%w: table 名称不合法", ErrInvalid)
	}
	if err := ValidateKey(key); err != nil {
		return KeyMeta{}, err
	}
	ct, err := NormalizeContentType(contentType)
	if err != nil {
		return KeyMeta{}, err
	}
	if updatedAt <= 0 {
		return KeyMeta{}, fmt.Errorf("%w: updatedAt 必须是正整数 Unix 毫秒时间戳", ErrInvalid)
	}
	if err := ValidateWriteCondition(condition); err != nil {
		return KeyMeta{}, err
	}

	tmp, err := os.CreateTemp(filepath.Join(s.dataDir, "tmp"), "put-*")
	if err != nil {
		return KeyMeta{}, err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	h := sha256.New()
	var src io.Reader = r
	if s.maxValueBytes > 0 {
		src = io.LimitReader(r, s.maxValueBytes+1)
	}
	buf := make([]byte, 128<<10)
	size, err := io.CopyBuffer(tmp, io.TeeReader(src, h), buf)
	if err != nil {
		tmp.Close()
		return KeyMeta{}, err
	}
	if s.maxValueBytes > 0 && size > s.maxValueBytes {
		tmp.Close()
		return KeyMeta{}, ErrTooLarge
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return KeyMeta{}, err
	}
	if err := tmp.Close(); err != nil {
		return KeyMeta{}, err
	}

	hashHex := hex.EncodeToString(h.Sum(nil))
	now := time.Now().UnixMilli()

	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	if err := s.ensureProjectExists(p.ID); err != nil {
		return KeyMeta{}, err
	}
	db, err := s.openProjectDB(p)
	if err != nil {
		return KeyMeta{}, err
	}
	defer db.Close()
	tx, err := db.Begin()
	if err != nil {
		return KeyMeta{}, err
	}
	defer tx.Rollback()

	table, tableCreated, err := ensureTableTx(tx, tableName, now, true)
	if err != nil {
		return KeyMeta{}, err
	}

	state, err := loadRecordState(tx, table, key)
	if err != nil {
		return KeyMeta{}, err
	}
	if createOnly && state.Exists && !state.Deleted {
		return KeyMeta{}, ErrKeyExists
	}
	matches, err := s.matchesCondition(p, table, state, condition)
	if err != nil {
		return KeyMeta{}, err
	}
	if !matches {
		return KeyMeta{}, preconditionFailed(key, state)
	}

	var (
		value       []byte
		blobHash    sql.NullString
		createdBlob bool
	)
	if size <= s.inlineThreshold {
		value, err = os.ReadFile(tmpName)
		if err != nil {
			return KeyMeta{}, err
		}
	} else {
		blobDir, err := s.ensureTableBlobDir(p, table)
		if err != nil {
			return KeyMeta{}, err
		}
		finalPath := filepath.Join(blobDir, hashHex)
		if _, statErr := os.Stat(finalPath); errors.Is(statErr, os.ErrNotExist) {
			if err := os.Rename(tmpName, finalPath); err != nil {
				return KeyMeta{}, err
			}
			createdBlob = true
			// 目录项落盘后才能提交数据库；否则断电后可能出现 DB 已提交但文件不可见。
			if err := syncDir(blobDir); err != nil {
				_ = os.Remove(finalPath)
				return KeyMeta{}, err
			}
		} else if statErr != nil {
			return KeyMeta{}, statErr
		}
		blobHash = sql.NullString{String: hashHex, Valid: true}
	}

	meta, removeOld, err := s.upsertLocked(tx, p, table, key, ct, hashHex, value, blobHash, size, now, updatedAt, state)
	if err != nil {
		if createdBlob {
			_ = os.Remove(filepath.Join(s.tableBlobDir(p, table), hashHex))
		}
		if tableCreated {
			_ = os.RemoveAll(s.tableBlobDir(p, table))
		}
		return KeyMeta{}, err
	}
	if err := tx.Commit(); err != nil {
		if createdBlob {
			_ = os.Remove(filepath.Join(s.tableBlobDir(p, table), hashHex))
		}
		if tableCreated {
			_ = os.RemoveAll(s.tableBlobDir(p, table))
		}
		return KeyMeta{}, err
	}
	if removeOld != "" {
		_ = os.Remove(filepath.Join(s.tableBlobDir(p, table), removeOld))
	}
	return meta, nil
}

func (s *Store) ensureTableBlobDir(p Project, table Table) (string, error) {
	projectDir := s.projectDir(p.ID)
	blobsDir := filepath.Join(projectDir, "blobs")
	createdBlobs, err := ensureDir(blobsDir)
	if err != nil {
		return "", err
	}
	if createdBlobs {
		if err := syncDir(projectDir); err != nil {
			return "", err
		}
	}
	tableDir := s.tableBlobDir(p, table)
	createdTable, err := ensureDir(tableDir)
	if err != nil {
		return "", err
	}
	if createdTable {
		if err := syncDir(blobsDir); err != nil {
			return "", err
		}
	}
	return tableDir, nil
}

func ensureDir(path string) (bool, error) {
	info, err := os.Stat(path)
	if err == nil {
		if !info.IsDir() {
			return false, fmt.Errorf("path is not a directory: %s", path)
		}
		return false, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		if errors.Is(err, os.ErrExist) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func (s *Store) upsertLocked(
	tx *sql.Tx,
	p Project,
	table Table,
	key, contentType, contentHash string,
	value []byte,
	blobHash sql.NullString,
	size, serverNow, updatedAt int64,
	state recordState,
) (KeyMeta, string, error) {
	createdAt := serverNow
	if state.Exists && state.CreatedAt > 0 {
		createdAt = state.CreatedAt
	}
	keyDelta := int64(0)
	if !state.Exists || state.Deleted {
		keyDelta = 1
	}
	if err := touchTableTx(tx, table.ID, keyDelta, serverNow); err != nil {
		return KeyMeta{}, "", err
	}

	var inline any
	if value != nil {
		inline = value
	}
	var external any
	if blobHash.Valid {
		external = blobHash.String
	}

	_, err := tx.Exec(fmt.Sprintf(`INSERT INTO "%s"
		(key, value, blob_hash, content_hash, content_type, size, created_at, updated_at, deleted)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, 0)
		ON CONFLICT(key) DO UPDATE SET
			value = excluded.value,
			blob_hash = excluded.blob_hash,
			content_hash = excluded.content_hash,
			content_type = excluded.content_type,
			size = excluded.size,
			updated_at = excluded.updated_at,
			deleted = 0`, table.TableName),
		key, inline, external, contentHash, contentType, size, createdAt, updatedAt)
	if err != nil {
		return KeyMeta{}, "", err
	}

	removeOld := ""
	if state.BlobHash != "" && state.BlobHash != blobHash.String {
		var refs int64
		if err := tx.QueryRow(fmt.Sprintf(`SELECT COUNT(*) FROM "%s" WHERE blob_hash = ?`, table.TableName), state.BlobHash).Scan(&refs); err != nil {
			return KeyMeta{}, "", err
		}
		if refs == 0 {
			removeOld = state.BlobHash
		}
	}
	return KeyMeta{
		Key:         key,
		ContentType: contentType,
		Size:        size,
		CreatedAt:   createdAt,
		UpdatedAt:   updatedAt,
		ContentHash: contentHash,
		External:    blobHash.Valid,
	}, removeOld, nil
}

func loadRecordState(tx *sql.Tx, table Table, key string) (recordState, error) {
	var (
		state    recordState
		blobHash sql.NullString
	)
	state.Key = key
	err := tx.QueryRow(fmt.Sprintf(`SELECT value, blob_hash, content_type, size, created_at, updated_at, deleted, content_hash
		FROM "%s" WHERE key = ?`, table.TableName), key).Scan(
		&state.Value,
		&blobHash,
		&state.ContentType,
		&state.Size,
		&state.CreatedAt,
		&state.UpdatedAt,
		&state.Deleted,
		&state.ContentHash,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return recordState{Key: key}, nil
	}
	if err != nil {
		return recordState{}, err
	}
	state.Exists = true
	if blobHash.Valid {
		state.BlobHash = blobHash.String
	}
	return state, nil
}

func preconditionFailed(key string, state recordState) error {
	return &PreconditionFailed{
		Key:           key,
		Exists:        state.Exists,
		Deleted:       state.Deleted,
		UpdatedAt:     state.UpdatedAt,
		CreatedAt:     state.CreatedAt,
		ContentType:   state.ContentType,
		ContentHash:   state.ContentHash,
		ContentLength: state.Size,
	}
}

func touchTableTx(tx *sql.Tx, tableID, keyDelta, now int64) error {
	res, err := tx.Exec(`UPDATE kv_tables
		SET key_count = key_count + ?, updated_at = ?
		WHERE id = ?`, keyDelta, now, tableID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrTableNotFound
	}
	return nil
}

// GetKey 返回元数据与可 Seek 的内容读取器，调用方负责 Close。
func (s *Store) GetKey(p Project, tableName, key string) (KeyMeta, ReadSeekCloser, error) {
	if !ValidateTableName(tableName) {
		return KeyMeta{}, nil, fmt.Errorf("%w: table 名称不合法", ErrInvalid)
	}
	if err := ValidateKey(key); err != nil {
		return KeyMeta{}, nil, err
	}
	s.writeMu.RLock()
	defer s.writeMu.RUnlock()
	if err := s.ensureProjectExists(p.ID); err != nil {
		return KeyMeta{}, nil, err
	}
	db, err := s.openProjectDB(p)
	if err != nil {
		return KeyMeta{}, nil, err
	}
	defer db.Close()
	table, err := getTable(db, tableName)
	if err != nil {
		return KeyMeta{}, nil, err
	}

	var (
		value       []byte
		blobHash    sql.NullString
		contentType string
		size        int64
		createdAt   int64
		updatedAt   int64
		deleted     bool
		contentHash string
	)
	q := fmt.Sprintf(`SELECT value, blob_hash, content_type, size, created_at, updated_at, deleted, content_hash
		FROM "%s" WHERE key = ?`, table.TableName)
	err = db.QueryRow(q, key).Scan(
		&value,
		&blobHash,
		&contentType,
		&size,
		&createdAt,
		&updatedAt,
		&deleted,
		&contentHash,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return KeyMeta{}, nil, ErrNotFound
	}
	if err != nil {
		return KeyMeta{}, nil, err
	}
	if deleted {
		return KeyMeta{}, nil, ErrNotFound
	}
	meta := KeyMeta{
		Key:         key,
		ContentType: contentType,
		Size:        size,
		CreatedAt:   createdAt,
		UpdatedAt:   updatedAt,
		ContentHash: contentHash,
		External:    blobHash.Valid,
	}
	if blobHash.Valid {
		if !blobFileRe.MatchString(blobHash.String) {
			return KeyMeta{}, nil, fmt.Errorf("%w: invalid blob hash", ErrInvalid)
		}
		f, err := os.Open(filepath.Join(s.tableBlobDir(p, table), blobHash.String))
		if err != nil {
			return KeyMeta{}, nil, fmt.Errorf("open blob: %w", err)
		}
		st, err := f.Stat()
		if err != nil {
			f.Close()
			return KeyMeta{}, nil, err
		}
		meta.Size = st.Size()
		return meta, f, nil
	}
	return meta, nopReadSeekCloser{bytes.NewReader(value)}, nil
}

// RenameKey 在 table 内原子重命名 key。
func (s *Store) RenameKey(p Project, tableName, oldKey, newKey string) (KeyMeta, error) {
	if !ValidateTableName(tableName) {
		return KeyMeta{}, fmt.Errorf("%w: table 名称不合法", ErrInvalid)
	}
	if err := ValidateKey(oldKey); err != nil {
		return KeyMeta{}, err
	}
	if err := ValidateKey(newKey); err != nil {
		return KeyMeta{}, err
	}
	if oldKey == newKey {
		meta, rc, err := s.GetKey(p, tableName, oldKey)
		if err != nil {
			return KeyMeta{}, err
		}
		_ = rc.Close()
		return meta, nil
	}

	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if err := s.ensureProjectExists(p.ID); err != nil {
		return KeyMeta{}, err
	}
	db, err := s.openProjectDB(p)
	if err != nil {
		return KeyMeta{}, err
	}
	defer db.Close()
	tx, err := db.Begin()
	if err != nil {
		return KeyMeta{}, err
	}
	defer tx.Rollback()
	table, err := getTableTx(tx, tableName)
	if err != nil {
		return KeyMeta{}, err
	}

	var (
		blobHash    sql.NullString
		contentType string
		size        int64
		createdAt   int64
		contentHash string
		value       []byte
		deleted     bool
	)
	err = tx.QueryRow(fmt.Sprintf(`SELECT value, blob_hash, content_type, size, created_at,
		content_hash, deleted FROM "%s" WHERE key = ?`, table.TableName), oldKey).Scan(
		&value,
		&blobHash,
		&contentType,
		&size,
		&createdAt,
		&contentHash,
		&deleted,
	)
	if errors.Is(err, sql.ErrNoRows) || deleted {
		return KeyMeta{}, ErrNotFound
	}
	if err != nil {
		return KeyMeta{}, err
	}

	var targetDeleted bool
	err = tx.QueryRow(fmt.Sprintf(`SELECT deleted FROM "%s" WHERE key = ?`, table.TableName), newKey).Scan(&targetDeleted)
	targetExists := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return KeyMeta{}, err
	}
	if targetExists && !targetDeleted {
		return KeyMeta{}, ErrKeyExists
	}

	serverNow := time.Now().UnixMilli()
	updatedAt := serverNow
	if _, err := tx.Exec(fmt.Sprintf(`UPDATE "%s" SET
		value = ?, blob_hash = NULL, content_hash = '', content_type = ?, size = 0,
		updated_at = ?, deleted = 1
		WHERE key = ?`, table.TableName),
		[]byte{}, "application/octet-stream", updatedAt, oldKey); err != nil {
		return KeyMeta{}, err
	}

	var inline any
	if !blobHash.Valid {
		inline = value
	}
	var external any
	if blobHash.Valid {
		external = blobHash.String
	}
	if targetExists {
		if _, err := tx.Exec(fmt.Sprintf(`UPDATE "%s" SET
			value = ?, blob_hash = ?, content_hash = ?, content_type = ?, size = ?,
			created_at = ?, updated_at = ?, deleted = 0
			WHERE key = ?`, table.TableName),
			inline, external, contentHash, contentType, size, createdAt, updatedAt, newKey); err != nil {
			return KeyMeta{}, err
		}
	} else {
		if _, err := tx.Exec(fmt.Sprintf(`INSERT INTO "%s"
			(key, value, blob_hash, content_hash, content_type, size, created_at, updated_at, deleted)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, 0)`, table.TableName),
			newKey, inline, external, contentHash, contentType, size, createdAt, updatedAt); err != nil {
			if strings.Contains(err.Error(), "UNIQUE") {
				return KeyMeta{}, ErrKeyExists
			}
			return KeyMeta{}, err
		}
	}
	if err := touchTableTx(tx, table.ID, 0, serverNow); err != nil {
		return KeyMeta{}, err
	}
	if err := tx.Commit(); err != nil {
		return KeyMeta{}, err
	}
	return KeyMeta{
		Key:         newKey,
		ContentType: contentType,
		Size:        size,
		CreatedAt:   createdAt,
		UpdatedAt:   updatedAt,
		ContentHash: contentHash,
		External:    blobHash.Valid,
	}, nil
}

// UpdateKeyContentType 只更新 key 的 Content-Type，不改变 value、存储位置或 content hash。
func (s *Store) UpdateKeyContentType(
	p Project,
	tableName, key, contentType string,
	updatedAt int64,
) (KeyMeta, error) {
	if !ValidateTableName(tableName) {
		return KeyMeta{}, fmt.Errorf("%w: table 名称不合法", ErrInvalid)
	}
	if err := ValidateKey(key); err != nil {
		return KeyMeta{}, err
	}
	contentType, err := NormalizeContentType(contentType)
	if err != nil {
		return KeyMeta{}, err
	}
	if updatedAt <= 0 {
		return KeyMeta{}, fmt.Errorf("%w: updatedAt 必须是正整数 Unix 毫秒时间戳", ErrInvalid)
	}

	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if err := s.ensureProjectExists(p.ID); err != nil {
		return KeyMeta{}, err
	}
	db, err := s.openProjectDB(p)
	if err != nil {
		return KeyMeta{}, err
	}
	defer db.Close()
	tx, err := db.Begin()
	if err != nil {
		return KeyMeta{}, err
	}
	defer tx.Rollback()
	table, err := getTableTx(tx, tableName)
	if err != nil {
		return KeyMeta{}, err
	}
	state, err := loadRecordState(tx, table, key)
	if err != nil {
		return KeyMeta{}, err
	}
	if !state.Exists || state.Deleted {
		return KeyMeta{}, ErrNotFound
	}
	serverNow := time.Now().UnixMilli()
	if _, err := tx.Exec(fmt.Sprintf(`UPDATE "%s"
		SET content_type = ?, updated_at = ?
		WHERE key = ?`, table.TableName), contentType, updatedAt, key); err != nil {
		return KeyMeta{}, err
	}
	if err := touchTableTx(tx, table.ID, 0, serverNow); err != nil {
		return KeyMeta{}, err
	}
	if err := tx.Commit(); err != nil {
		return KeyMeta{}, err
	}
	return KeyMeta{
		Key:         key,
		ContentType: contentType,
		Size:        state.Size,
		CreatedAt:   state.CreatedAt,
		UpdatedAt:   updatedAt,
		ContentHash: state.ContentHash,
		External:    state.BlobHash != "",
	}, nil
}

// DeleteKey 删除 key；key 不存在时也返回成功，保证幂等。
func (s *Store) DeleteKey(p Project, tableName, key string) error {
	_, err := s.DeleteKeyConditional(p, tableName, key, time.Now().UnixMilli(), WriteCondition{})
	return err
}

// DeleteKeyConditional 在条件满足时写入删除墓碑。
func (s *Store) DeleteKeyConditional(
	p Project,
	tableName, key string,
	updatedAt int64,
	condition WriteCondition,
) (KeyMeta, error) {
	if !ValidateTableName(tableName) {
		return KeyMeta{}, fmt.Errorf("%w: table 名称不合法", ErrInvalid)
	}
	if err := ValidateKey(key); err != nil {
		return KeyMeta{}, err
	}
	if updatedAt <= 0 {
		return KeyMeta{}, fmt.Errorf("%w: updatedAt 必须是正整数 Unix 毫秒时间戳", ErrInvalid)
	}
	if err := ValidateWriteCondition(condition); err != nil {
		return KeyMeta{}, err
	}

	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if err := s.ensureProjectExists(p.ID); err != nil {
		return KeyMeta{}, err
	}
	db, err := s.openProjectDB(p)
	if err != nil {
		return KeyMeta{}, err
	}
	defer db.Close()
	tx, err := db.Begin()
	if err != nil {
		return KeyMeta{}, err
	}
	defer tx.Rollback()
	table, err := getTableTx(tx, tableName)
	if err != nil {
		return KeyMeta{}, err
	}

	state, err := loadRecordState(tx, table, key)
	if err != nil {
		return KeyMeta{}, err
	}
	matches, err := s.matchesCondition(p, table, state, condition)
	if err != nil {
		return KeyMeta{}, err
	}
	if !matches {
		return KeyMeta{}, preconditionFailed(key, state)
	}

	serverNow := time.Now().UnixMilli()
	createdAt := serverNow
	if state.Exists && state.CreatedAt > 0 {
		createdAt = state.CreatedAt
	}
	keyDelta := int64(0)
	if state.Exists && !state.Deleted {
		keyDelta = -1
	}
	if err := touchTableTx(tx, table.ID, keyDelta, serverNow); err != nil {
		return KeyMeta{}, err
	}
	if _, err := tx.Exec(fmt.Sprintf(`INSERT INTO "%s"
		(key, value, blob_hash, content_hash, content_type, size, created_at, updated_at, deleted)
		VALUES (?, ?, NULL, '', ?, 0, ?, ?, 1)
		ON CONFLICT(key) DO UPDATE SET
			value = excluded.value,
			blob_hash = NULL,
			content_hash = '',
			content_type = excluded.content_type,
			size = 0,
			updated_at = excluded.updated_at,
			deleted = 1`, table.TableName),
		key, []byte{}, "application/octet-stream", createdAt, updatedAt); err != nil {
		return KeyMeta{}, err
	}

	removeBlob := ""
	if state.BlobHash != "" {
		var refs int64
		if err := tx.QueryRow(fmt.Sprintf(`SELECT COUNT(*) FROM "%s" WHERE blob_hash = ?`, table.TableName), state.BlobHash).Scan(&refs); err != nil {
			return KeyMeta{}, err
		}
		if refs == 0 {
			removeBlob = state.BlobHash
		}
	}
	if err := tx.Commit(); err != nil {
		return KeyMeta{}, err
	}
	if removeBlob != "" {
		_ = os.Remove(filepath.Join(s.tableBlobDir(p, table), removeBlob))
	}
	return KeyMeta{
		Key:         key,
		ContentType: "application/octet-stream",
		CreatedAt:   createdAt,
		UpdatedAt:   updatedAt,
		Deleted:     true,
	}, nil
}

// ListKeys 按 key 前缀分页查询当前记录，不含删除墓碑。
func (s *Store) ListKeys(p Project, tableName, prefix string, limit, offset int) ([]KeyMeta, int64, error) {
	result, err := s.QueryRecords(p, tableName, RecordQuery{
		Prefix: prefix,
		Limit:  limit,
		Offset: offset,
	})
	if err != nil {
		return nil, 0, err
	}
	keys := make([]KeyMeta, 0, len(result.Records))
	for _, record := range result.Records {
		keys = append(keys, record.KeyMeta)
	}
	return keys, result.Total, nil
}

// QueryRecords 按前缀、GLOB 和客户端修改时间查询 table 内记录。
func (s *Store) QueryRecords(p Project, tableName string, query RecordQuery) (RecordQueryResult, error) {
	if !ValidateTableName(tableName) {
		return RecordQueryResult{}, fmt.Errorf("%w: table 名称不合法", ErrInvalid)
	}
	if err := validateRecordQuery(query); err != nil {
		return RecordQueryResult{}, err
	}
	if query.Limit == 0 {
		query.Limit = defaultQueryLimit
	}
	if query.Limit < 0 {
		query.Limit = defaultQueryLimit
	}
	if query.Limit > maxQueryLimit {
		query.Limit = maxQueryLimit
	}
	if query.Offset < 0 {
		query.Offset = 0
	}

	s.writeMu.RLock()
	defer s.writeMu.RUnlock()
	if err := s.ensureProjectExists(p.ID); err != nil {
		return RecordQueryResult{}, err
	}
	db, err := s.openProjectDB(p)
	if err != nil {
		return RecordQueryResult{}, err
	}
	defer db.Close()
	table, err := getTable(db, tableName)
	if err != nil {
		return RecordQueryResult{}, err
	}

	whereClause, args := recordWhere(query)
	var total int64
	countQuery := fmt.Sprintf(`SELECT COUNT(*) FROM "%s"%s`, table.TableName, whereClause)
	if err := db.QueryRow(countQuery, args...).Scan(&total); err != nil {
		return RecordQueryResult{}, err
	}

	q := fmt.Sprintf(`SELECT key, value, blob_hash, content_type, size, created_at, updated_at,
		deleted, content_hash FROM "%s"%s ORDER BY key ASC LIMIT ? OFFSET ?`,
		table.TableName, whereClause)
	queryArgs := append(append([]any{}, args...), query.Limit, query.Offset)
	rows, err := db.Query(q, queryArgs...)
	if err != nil {
		return RecordQueryResult{}, err
	}
	defer rows.Close()

	result := RecordQueryResult{Records: make([]Record, 0, query.Limit), Total: total}
	var valueBytes int64
	for rows.Next() {
		var (
			record   Record
			value    []byte
			blobHash sql.NullString
		)
		if err := rows.Scan(
			&record.Key,
			&value,
			&blobHash,
			&record.ContentType,
			&record.Size,
			&record.CreatedAt,
			&record.UpdatedAt,
			&record.Deleted,
			&record.ContentHash,
		); err != nil {
			return RecordQueryResult{}, err
		}
		record.External = blobHash.Valid
		if query.IncludeValues && !record.Deleted {
			if record.Size > maxQueryValueBytes-valueBytes {
				return RecordQueryResult{}, fmt.Errorf(
					"%w: includeValues 单页总内容不能超过 %d 字节",
					ErrQueryValueLimit,
					maxQueryValueBytes,
				)
			}
			valueBytes += record.Size
			if blobHash.Valid {
				if !blobFileRe.MatchString(blobHash.String) {
					return RecordQueryResult{}, fmt.Errorf("%w: invalid blob hash", ErrInvalid)
				}
				data, err := os.ReadFile(filepath.Join(s.tableBlobDir(p, table), blobHash.String))
				if err != nil {
					return RecordQueryResult{}, fmt.Errorf("read record value %q: %w", record.Key, err)
				}
				record.Value = data
			} else {
				record.Value = append([]byte(nil), value...)
			}
		}
		result.Records = append(result.Records, record)
	}
	if err := rows.Err(); err != nil {
		return RecordQueryResult{}, err
	}
	return result, nil
}

func validateRecordQuery(query RecordQuery) error {
	if len(query.Prefix) > maxKeyBytes {
		return fmt.Errorf("%w: prefix 不能超过 %d 字节", ErrInvalid, maxKeyBytes)
	}
	if len(query.Pattern) > maxPatternBytes {
		return fmt.Errorf("%w: pattern 不能超过 %d 字节", ErrInvalid, maxPatternBytes)
	}
	if !utf8.ValidString(query.Prefix) || !utf8.ValidString(query.Pattern) {
		return fmt.Errorf("%w: prefix 和 pattern 必须是合法 UTF-8", ErrInvalid)
	}
	if query.UpdatedAfter < 0 || query.UpdatedBefore < 0 {
		return fmt.Errorf("%w: 时间范围不能为负数", ErrInvalid)
	}
	if query.UpdatedAfter > 0 && query.UpdatedBefore > 0 && query.UpdatedAfter > query.UpdatedBefore {
		return fmt.Errorf("%w: updatedAfter 不能大于 updatedBefore", ErrInvalid)
	}
	return nil
}

func recordWhere(query RecordQuery) (string, []any) {
	clauses := make([]string, 0, 6)
	args := make([]any, 0, 6)
	if !query.IncludeDeleted {
		clauses = append(clauses, `deleted = 0`)
	}
	if query.Prefix != "" {
		lower, upper, hasUpper := prefixRange(query.Prefix)
		if hasUpper {
			clauses = append(clauses, `key >= ? AND key < ?`)
			args = append(args, lower, upper)
		} else {
			clauses = append(clauses, `key >= ?`)
			args = append(args, lower)
		}
	}
	if query.Pattern != "" {
		clauses = append(clauses, `key GLOB ?`)
		args = append(args, query.Pattern)
	}
	if query.UpdatedAfter > 0 {
		clauses = append(clauses, `updated_at >= ?`)
		args = append(args, query.UpdatedAfter)
	}
	if query.UpdatedBefore > 0 {
		clauses = append(clauses, `updated_at <= ?`)
		args = append(args, query.UpdatedBefore)
	}
	if len(clauses) == 0 {
		return "", args
	}
	return " WHERE " + strings.Join(clauses, " AND "), args
}

func prefixRange(prefix string) (string, string, bool) {
	runes := []rune(prefix)
	for i := len(runes) - 1; i >= 0; i-- {
		r := runes[i]
		if r == utf8.MaxRune {
			continue
		}
		if r == 0xD7FF {
			runes[i] = 0xE000
		} else {
			runes[i]++
		}
		return prefix, string(runes[:i+1]), true
	}
	return prefix, "", false
}
