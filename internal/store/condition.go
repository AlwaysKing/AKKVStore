package store

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const maxConditionValueBytes = 8 << 10

// Condition 是客户端提交的一条原子写入前置条件。
type Condition struct {
	Field string `json:"field"`
	Op    string `json:"op"`
	Value any    `json:"value"`
}

type recordState struct {
	Key         string
	Exists      bool
	Deleted     bool
	Value       []byte
	BlobHash    string
	ContentType string
	ContentHash string
	Size        int64
	CreatedAt   int64
	UpdatedAt   int64
}

// ValidateWriteCondition 校验条件字段、操作符和值类型。
func ValidateWriteCondition(condition WriteCondition) error {
	for i, term := range condition.All {
		if _, err := normalizeCondition(term); err != nil {
			return fmt.Errorf("%w: 条件 %d: %v", ErrInvalid, i, err)
		}
	}
	return nil
}

type normalizedCondition struct {
	field string
	op    string
	value any
}

func normalizeCondition(term Condition) (normalizedCondition, error) {
	field := strings.TrimSpace(term.Field)
	op := strings.ToLower(strings.TrimSpace(term.Op))
	switch field {
	case "exists":
		if op != "eq" && op != "ne" {
			return normalizedCondition{}, fmt.Errorf("字段 %s 只支持 eq、ne", field)
		}
		if _, ok := term.Value.(bool); !ok {
			return normalizedCondition{}, fmt.Errorf("字段 %s 的值必须是布尔值", field)
		}
	case "deleted":
		if op != "eq" && op != "ne" {
			return normalizedCondition{}, fmt.Errorf("字段 %s 只支持 eq、ne", field)
		}
		if _, ok := term.Value.(bool); !ok {
			return normalizedCondition{}, fmt.Errorf("字段 %s 的值必须是布尔值", field)
		}
	case "updatedAt", "createdAt", "size":
		if !isOrderedConditionOp(op) {
			return normalizedCondition{}, fmt.Errorf("字段 %s 不支持操作符 %s", field, term.Op)
		}
		value, err := conditionInt64(term.Value)
		if err != nil {
			return normalizedCondition{}, fmt.Errorf("字段 %s 的值必须是整数", field)
		}
		term.Value = value
	case "contentType", "contentHash":
		if !isStringConditionOp(op) {
			return normalizedCondition{}, fmt.Errorf("字段 %s 不支持操作符 %s", field, term.Op)
		}
		if _, ok := term.Value.(string); !ok {
			return normalizedCondition{}, fmt.Errorf("字段 %s 的值必须是字符串", field)
		}
	case "value":
		if !isValueConditionOp(op) {
			return normalizedCondition{}, fmt.Errorf("字段 %s 不支持操作符 %s", field, term.Op)
		}
		value, ok := term.Value.(string)
		if !ok {
			return normalizedCondition{}, fmt.Errorf("字段 %s 的值必须是字符串", field)
		}
		if len(value) > maxConditionValueBytes {
			return normalizedCondition{}, fmt.Errorf("value 条件不能超过 %d 字节", maxConditionValueBytes)
		}
	default:
		return normalizedCondition{}, fmt.Errorf("不支持的字段 %q", term.Field)
	}
	return normalizedCondition{field: field, op: op, value: term.Value}, nil
}

func isOrderedConditionOp(op string) bool {
	switch op {
	case "eq", "ne", "lt", "lte", "gt", "gte":
		return true
	default:
		return false
	}
}

func isStringConditionOp(op string) bool {
	return isOrderedConditionOp(op) || op == "startswith" || op == "endswith"
}

func isValueConditionOp(op string) bool {
	return isStringConditionOp(op)
}

func conditionInt64(value any) (int64, error) {
	switch v := value.(type) {
	case json.Number:
		return v.Int64()
	case int:
		return int64(v), nil
	case int8:
		return int64(v), nil
	case int16:
		return int64(v), nil
	case int32:
		return int64(v), nil
	case int64:
		return v, nil
	case uint:
		if uint64(v) > uint64(^uint64(0)>>1) {
			return 0, strconv.ErrRange
		}
		return int64(v), nil
	case uint8:
		return int64(v), nil
	case uint16:
		return int64(v), nil
	case uint32:
		return int64(v), nil
	case uint64:
		if v > uint64(^uint64(0)>>1) {
			return 0, strconv.ErrRange
		}
		return int64(v), nil
	default:
		return 0, errors.New("not an integer")
	}
}

func (s *Store) matchesCondition(
	p Project,
	table Table,
	state recordState,
	condition WriteCondition,
) (bool, error) {
	for _, term := range condition.All {
		normalized, err := normalizeCondition(term)
		if err != nil {
			return false, fmt.Errorf("%w: %v", ErrInvalid, err)
		}
		ok, err := s.matchConditionTerm(p, table, state, normalized)
		if err != nil {
			return false, err
		}
		if !ok {
			return false, nil
		}
	}
	return true, nil
}

func (s *Store) matchConditionTerm(
	p Project,
	table Table,
	state recordState,
	condition normalizedCondition,
) (bool, error) {
	switch condition.field {
	case "exists":
		return compareBool(state.Exists, condition.value.(bool), condition.op), nil
	case "deleted":
		return compareBool(state.Deleted, condition.value.(bool), condition.op), nil
	case "updatedAt":
		return compareInt64(state.UpdatedAt, condition.value.(int64), condition.op), nil
	case "createdAt":
		return compareInt64(state.CreatedAt, condition.value.(int64), condition.op), nil
	case "size":
		return compareInt64(state.Size, condition.value.(int64), condition.op), nil
	case "contentType":
		return compareString(state.ContentType, condition.value.(string), condition.op), nil
	case "contentHash":
		return compareString(state.ContentHash, condition.value.(string), condition.op), nil
	case "value":
		if !state.Exists || state.Deleted {
			return compareBytes(nil, []byte(condition.value.(string)), condition.op)
		}
		return s.compareRecordValue(p, table, state, []byte(condition.value.(string)), condition.op)
	default:
		return false, fmt.Errorf("%w: 不支持的条件字段 %q", ErrInvalid, condition.field)
	}
}

func compareBool(current, expected bool, op string) bool {
	switch op {
	case "eq":
		return current == expected
	case "ne":
		return current != expected
	default:
		return false
	}
}

func compareInt64(current, expected int64, op string) bool {
	switch op {
	case "eq":
		return current == expected
	case "ne":
		return current != expected
	case "lt":
		return current < expected
	case "lte":
		return current <= expected
	case "gt":
		return current > expected
	case "gte":
		return current >= expected
	default:
		return false
	}
}

func compareString(current, expected, op string) bool {
	switch op {
	case "eq":
		return current == expected
	case "ne":
		return current != expected
	case "lt":
		return current < expected
	case "lte":
		return current <= expected
	case "gt":
		return current > expected
	case "gte":
		return current >= expected
	case "startswith":
		return strings.HasPrefix(current, expected)
	case "endswith":
		return strings.HasSuffix(current, expected)
	default:
		return false
	}
}

func compareBytes(current, expected []byte, op string) (bool, error) {
	switch op {
	case "eq":
		return bytes.Equal(current, expected), nil
	case "ne":
		return !bytes.Equal(current, expected), nil
	case "lt":
		return bytes.Compare(current, expected) < 0, nil
	case "lte":
		return bytes.Compare(current, expected) <= 0, nil
	case "gt":
		return bytes.Compare(current, expected) > 0, nil
	case "gte":
		return bytes.Compare(current, expected) >= 0, nil
	case "startswith":
		return bytes.HasPrefix(current, expected), nil
	case "endswith":
		return bytes.HasSuffix(current, expected), nil
	default:
		return false, fmt.Errorf("%w: value 条件不支持操作符 %q", ErrInvalid, op)
	}
}

func (s *Store) compareRecordValue(
	p Project,
	table Table,
	state recordState,
	expected []byte,
	op string,
) (bool, error) {
	switch op {
	case "eq", "ne":
		if state.Size != int64(len(expected)) {
			return op == "ne", nil
		}
		current, err := s.readRecordBytes(p, table, state, 0, state.Size)
		if err != nil {
			return false, err
		}
		return compareBytes(current, expected, op)
	case "startswith":
		if int64(len(expected)) > state.Size {
			return false, nil
		}
		current, err := s.readRecordBytes(p, table, state, 0, int64(len(expected)))
		if err != nil {
			return false, err
		}
		return bytes.HasPrefix(current, expected), nil
	case "endswith":
		if int64(len(expected)) > state.Size {
			return false, nil
		}
		offset := state.Size - int64(len(expected))
		current, err := s.readRecordBytes(p, table, state, offset, int64(len(expected)))
		if err != nil {
			return false, err
		}
		return bytes.HasSuffix(current, expected), nil
	case "lt", "lte", "gt", "gte":
		readSize := state.Size
		if int64(len(expected))+1 < readSize {
			readSize = int64(len(expected)) + 1
		}
		current, err := s.readRecordBytes(p, table, state, 0, readSize)
		if err != nil {
			return false, err
		}
		return compareBytes(current, expected, op)
	default:
		return false, fmt.Errorf("%w: value 条件不支持操作符 %q", ErrInvalid, op)
	}
}

func (s *Store) readRecordBytes(
	p Project,
	table Table,
	state recordState,
	offset, length int64,
) ([]byte, error) {
	if length == 0 {
		return []byte{}, nil
	}
	if offset < 0 || length < 0 || offset+length > state.Size {
		return nil, fmt.Errorf("%w: 读取记录条件内容越界", ErrInvalid)
	}
	if state.BlobHash == "" {
		if offset > int64(len(state.Value)) {
			return nil, fmt.Errorf("%w: 读取记录条件内容越界", ErrInvalid)
		}
		end := offset + length
		if end > int64(len(state.Value)) {
			return nil, fmt.Errorf("%w: 读取记录条件内容越界", ErrInvalid)
		}
		return append([]byte(nil), state.Value[offset:end]...), nil
	}
	if !blobFileRe.MatchString(state.BlobHash) {
		return nil, fmt.Errorf("%w: invalid blob hash", ErrInvalid)
	}
	file, err := os.Open(filepath.Join(s.tableBlobDir(p, table), state.BlobHash))
	if err != nil {
		return nil, fmt.Errorf("open blob for condition: %w", err)
	}
	defer file.Close()
	buf := make([]byte, length)
	if _, err := file.ReadAt(buf, offset); err != nil {
		return nil, fmt.Errorf("read blob for condition: %w", err)
	}
	return buf, nil
}
