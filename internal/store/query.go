package store

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// QueryCondition 是查询请求中的一条字段条件。
type QueryCondition struct {
	Field string `json:"field"`
	Op    string `json:"op"`
	Value any    `json:"value"`
}

// QueryConditionSet 描述查询条件，所有条件按 AND 计算。
type QueryConditionSet struct {
	All []QueryCondition `json:"all,omitempty"`
}

// AdvancedRecordQuery 描述跨 table 或单 table 的条件查询。
type AdvancedRecordQuery struct {
	Tables         []string
	Condition      QueryConditionSet
	IncludeDeleted bool
	IncludeValues  bool
	Limit          int
	Offset         int
}

// ProjectRecord 是带 table 名称的查询结果。
type ProjectRecord struct {
	Table  string
	Record Record
}

// ProjectRecordQueryResult 是 project 条件查询结果。
type ProjectRecordQueryResult struct {
	Records []ProjectRecord
	Total   int64
}

type normalizedQueryCondition struct {
	column string
	op     string
	value  any
}

// QueryTableRecords 对一个 table 执行条件查询。
func (s *Store) QueryTableRecords(
	p Project,
	tableName string,
	query AdvancedRecordQuery,
) (ProjectRecordQueryResult, error) {
	if !ValidateTableName(tableName) {
		return ProjectRecordQueryResult{}, fmt.Errorf("%w: table 名称不合法", ErrInvalid)
	}
	query.Tables = []string{tableName}
	return s.queryProjectRecords(p, query)
}

// QueryProjectRecords 对一个 project 内的 table 执行条件查询。
func (s *Store) QueryProjectRecords(
	p Project,
	query AdvancedRecordQuery,
) (ProjectRecordQueryResult, error) {
	return s.queryProjectRecords(p, query)
}

func (s *Store) queryProjectRecords(
	p Project,
	query AdvancedRecordQuery,
) (ProjectRecordQueryResult, error) {
	limit := query.Limit
	if limit == 0 {
		limit = defaultQueryLimit
	}
	if limit < 0 {
		return ProjectRecordQueryResult{}, fmt.Errorf("%w: limit 不能为负数", ErrInvalid)
	}
	if limit > maxQueryLimit {
		return ProjectRecordQueryResult{}, fmt.Errorf("%w: limit 不能超过 %d", ErrInvalid, maxQueryLimit)
	}
	if query.Offset < 0 {
		return ProjectRecordQueryResult{}, fmt.Errorf("%w: offset 不能为负数", ErrInvalid)
	}

	whereClause, args, err := buildQueryWhere(query)
	if err != nil {
		return ProjectRecordQueryResult{}, err
	}

	s.writeMu.RLock()
	defer s.writeMu.RUnlock()
	if err := s.ensureProjectExists(p.ID); err != nil {
		return ProjectRecordQueryResult{}, err
	}
	db, err := s.openProjectDB(p)
	if err != nil {
		return ProjectRecordQueryResult{}, err
	}
	defer db.Close()

	tables, err := s.selectQueryTables(db, query.Tables)
	if err != nil {
		return ProjectRecordQueryResult{}, err
	}

	counts := make([]int64, len(tables))
	var total int64
	for i, table := range tables {
		q := fmt.Sprintf(`SELECT COUNT(*) FROM "%s"%s`, table.TableName, whereClause)
		if err := db.QueryRow(q, args...).Scan(&counts[i]); err != nil {
			return ProjectRecordQueryResult{}, err
		}
		total += counts[i]
	}

	records := make([]ProjectRecord, 0, limit)
	remainingOffset := query.Offset
	remainingLimit := limit
	var valueBytes int64
	for i, table := range tables {
		if remainingLimit == 0 {
			break
		}
		if remainingOffset >= int(counts[i]) {
			remainingOffset -= int(counts[i])
			continue
		}
		take := int(counts[i]) - remainingOffset
		if take > remainingLimit {
			take = remainingLimit
		}
		selectColumns := `key, content_type, size, created_at, updated_at, deleted, content_hash, NULL, NULL`
		if query.IncludeValues {
			selectColumns = `key, content_type, size, created_at, updated_at, deleted, content_hash, value, blob_hash`
		}
		q := fmt.Sprintf(`SELECT %s FROM "%s"%s ORDER BY key ASC LIMIT ? OFFSET ?`,
			selectColumns, table.TableName, whereClause)
		queryArgs := append(append([]any{}, args...), take, remainingOffset)
		rows, err := db.Query(q, queryArgs...)
		if err != nil {
			return ProjectRecordQueryResult{}, err
		}
		for rows.Next() {
			record, err := s.scanAdvancedRecord(rows, p, table, query.IncludeValues, &valueBytes)
			if err != nil {
				rows.Close()
				return ProjectRecordQueryResult{}, err
			}
			records = append(records, ProjectRecord{Table: table.Name, Record: record})
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return ProjectRecordQueryResult{}, err
		}
		rows.Close()
		remainingOffset = 0
		remainingLimit -= take
	}
	return ProjectRecordQueryResult{Records: records, Total: total}, nil
}

func (s *Store) selectQueryTables(db *sql.DB, requested []string) ([]Table, error) {
	all, err := s.listTablesLocked(db)
	if err != nil {
		return nil, err
	}
	byName := make(map[string]Table, len(all))
	for _, table := range all {
		byName[table.Name] = table
	}
	if len(requested) == 0 {
		return all, nil
	}
	seen := make(map[string]struct{}, len(requested))
	out := make([]Table, 0, len(requested))
	for _, name := range requested {
		name = strings.TrimSpace(name)
		if !ValidateTableName(name) {
			return nil, fmt.Errorf("%w: table 名称不合法", ErrInvalid)
		}
		if _, ok := seen[name]; ok {
			continue
		}
		table, ok := byName[name]
		if !ok {
			return nil, ErrTableNotFound
		}
		seen[name] = struct{}{}
		out = append(out, table)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func buildQueryWhere(query AdvancedRecordQuery) (string, []any, error) {
	clauses := make([]string, 0, len(query.Condition.All)+1)
	args := make([]any, 0, len(query.Condition.All)*2+1)
	if !query.IncludeDeleted {
		clauses = append(clauses, "deleted = 0")
	}
	for i, term := range query.Condition.All {
		condition, err := normalizeQueryTerm(term)
		if err != nil {
			return "", nil, fmt.Errorf("%w: 条件 %d: %v", ErrInvalid, i, err)
		}
		clause, values, err := queryConditionSQL(condition.column, condition.op, condition.value)
		if err != nil {
			return "", nil, fmt.Errorf("%w: 条件 %d: %v", ErrInvalid, i, err)
		}
		clauses = append(clauses, clause)
		args = append(args, values...)
	}
	if len(clauses) == 0 {
		return "", args, nil
	}
	return " WHERE " + strings.Join(clauses, " AND "), args, nil
}

func normalizeQueryTerm(term QueryCondition) (normalizedQueryCondition, error) {
	field := strings.TrimSpace(term.Field)
	op := strings.ToLower(strings.TrimSpace(term.Op))
	switch field {
	case "key":
		if op != "glob" && !isStringConditionOp(op) {
			return normalizedQueryCondition{}, fmt.Errorf("字段 key 不支持操作符 %s", term.Op)
		}
		value, ok := term.Value.(string)
		if !ok {
			return normalizedQueryCondition{}, errors.New("字段 key 的值必须是字符串")
		}
		if len(value) > maxKeyBytes {
			return normalizedQueryCondition{}, fmt.Errorf("key 条件不能超过 %d 字节", maxKeyBytes)
		}
		return normalizedQueryCondition{column: "key", op: op, value: value}, nil
	case "createdAt":
		return normalizeQueryIntCondition("created_at", term, op)
	case "updatedAt":
		return normalizeQueryIntCondition("updated_at", term, op)
	case "size":
		return normalizeQueryIntCondition("size", term, op)
	case "deleted":
		if op != "eq" && op != "ne" {
			return normalizedQueryCondition{}, fmt.Errorf("字段 deleted 只支持 eq、ne")
		}
		value, ok := term.Value.(bool)
		if !ok {
			return normalizedQueryCondition{}, errors.New("字段 deleted 的值必须是布尔值")
		}
		if value {
			return normalizedQueryCondition{column: "deleted", op: op, value: int64(1)}, nil
		}
		return normalizedQueryCondition{column: "deleted", op: op, value: int64(0)}, nil
	case "contentType":
		return normalizeQueryStringCondition("content_type", term, op)
	case "contentHash":
		return normalizeQueryStringCondition("content_hash", term, op)
	default:
		return normalizedQueryCondition{}, fmt.Errorf("不支持的查询字段 %q", term.Field)
	}
}

func normalizeQueryIntCondition(
	column string,
	term QueryCondition,
	op string,
) (normalizedQueryCondition, error) {
	if !isOrderedConditionOp(op) {
		return normalizedQueryCondition{}, fmt.Errorf("字段 %s 不支持操作符 %s", term.Field, term.Op)
	}
	value, err := conditionInt64(term.Value)
	if err != nil {
		return normalizedQueryCondition{}, fmt.Errorf("字段 %s 的值必须是整数", term.Field)
	}
	return normalizedQueryCondition{column: column, op: op, value: value}, nil
}

func normalizeQueryStringCondition(
	column string,
	term QueryCondition,
	op string,
) (normalizedQueryCondition, error) {
	if !isStringConditionOp(op) {
		return normalizedQueryCondition{}, fmt.Errorf("字段 %s 不支持操作符 %s", term.Field, term.Op)
	}
	value, ok := term.Value.(string)
	if !ok {
		return normalizedQueryCondition{}, fmt.Errorf("字段 %s 的值必须是字符串", term.Field)
	}
	return normalizedQueryCondition{column: column, op: op, value: value}, nil
}

func queryConditionSQL(column, op string, value any) (string, []any, error) {
	switch op {
	case "eq", "ne", "lt", "lte", "gt", "gte":
		sqlOp := map[string]string{
			"eq": "=", "ne": "<>", "lt": "<", "lte": "<=", "gt": ">", "gte": ">=",
		}[op]
		return column + " " + sqlOp + " ?", []any{value}, nil
	case "startswith":
		return "substr(" + column + ", 1, length(?)) = ?", []any{value, value}, nil
	case "endswith":
		if stringValue, ok := value.(string); ok && stringValue == "" {
			return "1 = 1", nil, nil
		}
		return "substr(" + column + ", -length(?)) = ?", []any{value, value}, nil
	case "glob":
		if column != "key" {
			return "", nil, errors.New("glob 只支持 key 字段")
		}
		return "key GLOB ?", []any{value}, nil
	default:
		return "", nil, fmt.Errorf("不支持的操作符 %q", op)
	}
}

func (s *Store) scanAdvancedRecord(
	rows *sql.Rows,
	p Project,
	table Table,
	includeValues bool,
	valueBytes *int64,
) (Record, error) {
	var (
		record   Record
		value    []byte
		blobHash sql.NullString
	)
	if err := rows.Scan(
		&record.Key,
		&record.ContentType,
		&record.Size,
		&record.CreatedAt,
		&record.UpdatedAt,
		&record.Deleted,
		&record.ContentHash,
		&value,
		&blobHash,
	); err != nil {
		return Record{}, err
	}
	record.External = blobHash.Valid
	if !includeValues || record.Deleted {
		return record, nil
	}
	if record.Size > maxQueryValueBytes-*valueBytes {
		return Record{}, fmt.Errorf(
			"%w: includeValues 查询总内容不能超过 %d 字节",
			ErrQueryValueLimit,
			maxQueryValueBytes,
		)
	}
	*valueBytes += record.Size
	if blobHash.Valid {
		if !blobFileRe.MatchString(blobHash.String) {
			return Record{}, fmt.Errorf("%w: invalid blob hash", ErrInvalid)
		}
		data, err := os.ReadFile(filepath.Join(s.tableBlobDir(p, table), blobHash.String))
		if err != nil {
			return Record{}, fmt.Errorf("read record value %q: %w", record.Key, err)
		}
		record.Value = data
	} else {
		record.Value = append([]byte(nil), value...)
	}
	return record, nil
}
