package httpapi

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"akkvstore/internal/store"
)

// API 提供 /v1 下的公开精确记录和分页查询接口。
type API struct {
	store         *store.Store
	maxValueBytes int64
	allowAll      bool
	allowed       map[string]struct{}
	log           *slog.Logger
}

// New 构造 API。origins 支持 "*" 或精确的 Origin 列表。
func New(st *store.Store, maxValueBytes int64, origins []string, log *slog.Logger) *API {
	a := &API{
		store:         st,
		maxValueBytes: maxValueBytes,
		allowed:       make(map[string]struct{}),
		log:           log,
	}
	for _, o := range origins {
		if o == "*" {
			a.allowAll = true
			continue
		}
		a.allowed[o] = struct{}{}
	}
	return a
}

// Handler 返回挂载在 /v1 前缀下的处理器（由调用方 StripPrefix）。
func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()
	// 不按方法注册，自己分发，保证 405/404 也走统一的 JSON 错误格式。
	mux.HandleFunc("/{project}/tables", a.authed(a.handleTables))
	mux.HandleFunc("/{project}/tables/{table}", a.authed(a.handleTable))
	mux.HandleFunc("/{project}/tables/{table}/kv/{key...}", a.handleKV)
	mux.HandleFunc("/{project}/tables/{table}/records", a.authed(a.handleRecords))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "not_found", "接口不存在")
	})
	return a.cors(mux)
}

func (a *API) handleKV(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		a.authed(a.handleGet)(w, r)
	case http.MethodPut:
		a.authed(a.handlePut)(w, r)
	case http.MethodDelete:
		a.authed(a.handleDelete)(w, r)
	default:
		w.Header().Set("Allow", "GET, HEAD, PUT, DELETE, OPTIONS")
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "不支持的 HTTP 方法")
	}
}

type authedHandler func(http.ResponseWriter, *http.Request, store.Project, store.ProjectToken)

type tableItem struct {
	Name      string `json:"name"`
	KeyCount  int64  `json:"keyCount"`
	CreatedAt int64  `json:"createdAt"`
	UpdatedAt int64  `json:"updatedAt"`
}

type tablesResponse struct {
	Items []tableItem `json:"items"`
}

type createTableRequest struct {
	Name string `json:"name"`
}

type recordItem struct {
	Key         string `json:"key"`
	CreatedAt   int64  `json:"createdAt"`
	UpdatedAt   int64  `json:"updatedAt"`
	Deleted     bool   `json:"deleted"`
	ContentType string `json:"contentType,omitempty"`
	Size        int64  `json:"size"`
	ContentHash string `json:"contentHash,omitempty"`
	ValueBase64 string `json:"valueBase64,omitempty"`
	ValueText   string `json:"valueText,omitempty"`
}

type recordsResponse struct {
	Items      []recordItem `json:"items"`
	Total      int64        `json:"total"`
	NextOffset int          `json:"nextOffset"`
	HasMore    bool         `json:"hasMore"`
}

func (a *API) authed(next authedHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token, ok := bearerToken(r)
		if !ok {
			w.Header().Set("WWW-Authenticate", `Bearer realm="akkv"`)
			writeError(w, http.StatusUnauthorized, "unauthorized", "缺少 Bearer token")
			return
		}
		p, projectToken, err := a.store.VerifyProjectToken(r.PathValue("project"), token)
		if err != nil {
			if errors.Is(err, store.ErrUnauthorized) {
				w.Header().Set("WWW-Authenticate", `Bearer realm="akkv"`)
				writeError(w, http.StatusUnauthorized, "unauthorized", "project 或 token 不正确")
				return
			}
			a.log.Error("verify token", "err", err)
			writeError(w, http.StatusInternalServerError, "internal", "服务内部错误")
			return
		}
		if err := a.store.RecordTokenAccess(
			projectToken.ProjectID,
			projectToken.ID,
			r.Method == http.MethodGet || r.Method == http.MethodHead,
		); err != nil {
			a.log.Error("record token access", "token_id", projectToken.ID, "err", err)
		}
		next(w, r, p, projectToken)
	}
}

func bearerToken(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	scheme, token, ok := strings.Cut(h, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	token = strings.TrimSpace(token)
	return token, token != ""
}

func (a *API) handleTables(w http.ResponseWriter, r *http.Request, p store.Project, _ store.ProjectToken) {
	switch r.Method {
	case http.MethodGet:
		tables, err := a.store.ListTables(p)
		if err != nil {
			a.writeStoreError(w, "list tables", p, err)
			return
		}
		items := make([]tableItem, 0, len(tables))
		for _, table := range tables {
			items = append(items, tableItem{
				Name:      table.Name,
				KeyCount:  table.KeyCount,
				CreatedAt: table.CreatedAt,
				UpdatedAt: table.UpdatedAt,
			})
		}
		writeJSON(w, http.StatusOK, tablesResponse{Items: items})
	case http.MethodPost:
		r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
		var req createTableRequest
		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", "请求体必须是 {\"name\":\"table-name\"}")
			return
		}
		table, err := a.store.CreateTable(p, req.Name)
		if err != nil {
			switch {
			case errors.Is(err, store.ErrTableExists):
				writeError(w, http.StatusConflict, "table_exists", "table 已存在")
			case errors.Is(err, store.ErrInvalid):
				writeError(w, http.StatusBadRequest, "invalid_table", err.Error())
			case errors.Is(err, store.ErrNotFound):
				writeError(w, http.StatusNotFound, "project_not_found", "project 不存在")
			default:
				a.log.Error("create table", "project", p.Name, "err", err)
				writeError(w, http.StatusInternalServerError, "internal", "服务内部错误")
			}
			return
		}
		w.Header().Set("Location", "/v1/"+p.Name+"/tables/"+table.Name)
		writeJSON(w, http.StatusCreated, tableItem{
			Name:      table.Name,
			CreatedAt: table.CreatedAt,
			UpdatedAt: table.UpdatedAt,
		})
	default:
		w.Header().Set("Allow", "GET, POST, OPTIONS")
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "不支持的 HTTP 方法")
	}
}

func (a *API) handleTable(w http.ResponseWriter, r *http.Request, p store.Project, _ store.ProjectToken) {
	switch r.Method {
	case http.MethodGet:
		table, err := a.store.GetTableSummary(p, r.PathValue("table"))
		if err != nil {
			a.writeTableError(w, "get table", p, err)
			return
		}
		writeJSON(w, http.StatusOK, tableItem{
			Name:      table.Name,
			KeyCount:  table.KeyCount,
			CreatedAt: table.CreatedAt,
			UpdatedAt: table.UpdatedAt,
		})
	case http.MethodDelete:
		if err := a.store.DeleteTable(p, r.PathValue("table")); err != nil {
			a.writeTableError(w, "delete table", p, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		w.Header().Set("Allow", "GET, DELETE, OPTIONS")
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "不支持的 HTTP 方法")
	}
}

func validateKVPath(w http.ResponseWriter, r *http.Request) (string, string, bool) {
	tableName := r.PathValue("table")
	if !store.ValidateTableName(tableName) {
		writeError(w, http.StatusBadRequest, "invalid_table", "table 名称不合法")
		return "", "", false
	}
	key := r.PathValue("key")
	if err := store.ValidateKey(key); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_key", err.Error())
		return "", "", false
	}
	return tableName, key, true
}

func (a *API) handleGet(w http.ResponseWriter, r *http.Request, p store.Project, _ store.ProjectToken) {
	tableName, key, ok := validateKVPath(w, r)
	if !ok {
		return
	}
	meta, rc, err := a.store.GetKey(p, tableName, key)
	if err != nil {
		if errors.Is(err, store.ErrTableNotFound) {
			writeError(w, http.StatusNotFound, "table_not_found", "table 不存在")
			return
		}
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "key 不存在")
			return
		}
		if errors.Is(err, store.ErrInvalid) {
			writeError(w, http.StatusBadRequest, "invalid_key", err.Error())
			return
		}
		a.log.Error("get key", "project", p.Name, "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "服务内部错误")
		return
	}
	defer rc.Close()

	w.Header().Set("Content-Type", meta.ContentType)
	w.Header().Set("ETag", etag(meta.ContentHash))
	w.Header().Set("X-Created-At", strconv.FormatInt(meta.CreatedAt, 10))
	w.Header().Set("X-Updated-At", strconv.FormatInt(meta.UpdatedAt, 10))
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "sandbox; default-src 'none'")
	http.ServeContent(w, r, "", time.UnixMilli(meta.UpdatedAt), rc)
}

func (a *API) handlePut(w http.ResponseWriter, r *http.Request, p store.Project, projectToken store.ProjectToken) {
	tableName, key, ok := validateKVPath(w, r)
	if !ok {
		return
	}
	condition, err := parseWriteCondition(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_condition", err.Error())
		return
	}
	updatedAt, err := parseUpdatedAt(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_updated_at", err.Error())
		return
	}
	if a.maxValueBytes > 0 {
		if r.ContentLength > a.maxValueBytes {
			writeError(w, http.StatusRequestEntityTooLarge, "too_large",
				fmt.Sprintf("value 不能超过 %d 字节", a.maxValueBytes))
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, a.maxValueBytes)
	}

	meta, err := a.store.PutKeyConditional(
		p,
		tableName,
		key,
		r.Header.Get("Content-Type"),
		r.Body,
		updatedAt,
		condition,
	)
	if err != nil {
		var mbe *http.MaxBytesError
		switch {
		case errors.As(err, &mbe), errors.Is(err, store.ErrTooLarge):
			writeError(w, http.StatusRequestEntityTooLarge, "too_large",
				fmt.Sprintf("value 不能超过 %d 字节", a.maxValueBytes))
		case errors.Is(err, store.ErrInvalid):
			writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		case errors.Is(err, store.ErrNotFound):
			writeError(w, http.StatusNotFound, "project_not_found", "project 不存在")
		case errors.Is(err, store.ErrPreconditionFailed):
			writePreconditionFailed(w, err)
		case errors.Is(err, io.ErrUnexpectedEOF):
			writeError(w, http.StatusBadRequest, "bad_body", "请求体不完整")
		default:
			a.log.Error("put key", "project", p.Name, "err", err)
			writeError(w, http.StatusInternalServerError, "internal", "服务内部错误")
		}
		return
	}

	w.Header().Set("ETag", etag(meta.ContentHash))
	w.Header().Set("X-Value-Size", fmt.Sprintf("%d", meta.Size))
	w.Header().Set("X-Created-At", strconv.FormatInt(meta.CreatedAt, 10))
	w.Header().Set("X-Updated-At", strconv.FormatInt(meta.UpdatedAt, 10))
	if err := a.store.RecordTokenWrite(projectToken.ID); err != nil {
		a.log.Error("record token write", "token_id", projectToken.ID, "err", err)
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) handleDelete(w http.ResponseWriter, r *http.Request, p store.Project, projectToken store.ProjectToken) {
	tableName, key, ok := validateKVPath(w, r)
	if !ok {
		return
	}
	condition, err := parseWriteCondition(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_condition", err.Error())
		return
	}
	updatedAt, err := parseUpdatedAt(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_updated_at", err.Error())
		return
	}
	meta, err := a.store.DeleteKeyConditional(p, tableName, key, updatedAt, condition)
	if err != nil {
		if errors.Is(err, store.ErrTableNotFound) {
			writeError(w, http.StatusNotFound, "table_not_found", "table 不存在")
			return
		}
		if errors.Is(err, store.ErrInvalid) {
			writeError(w, http.StatusBadRequest, "invalid_key", err.Error())
			return
		}
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "project_not_found", "project 不存在")
			return
		}
		if errors.Is(err, store.ErrPreconditionFailed) {
			writePreconditionFailed(w, err)
			return
		}
		a.log.Error("delete key", "project", p.Name, "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "服务内部错误")
		return
	}
	w.Header().Set("X-Created-At", strconv.FormatInt(meta.CreatedAt, 10))
	w.Header().Set("X-Updated-At", strconv.FormatInt(meta.UpdatedAt, 10))
	if err := a.store.RecordTokenWrite(projectToken.ID); err != nil {
		a.log.Error("record token write", "token_id", projectToken.ID, "err", err)
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) handleRecords(w http.ResponseWriter, r *http.Request, p store.Project, _ store.ProjectToken) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET, OPTIONS")
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "不支持的 HTTP 方法")
		return
	}
	tableName := r.PathValue("table")
	if !store.ValidateTableName(tableName) {
		writeError(w, http.StatusBadRequest, "invalid_table", "table 名称不合法")
		return
	}
	query, err := parseRecordQuery(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_query", err.Error())
		return
	}
	result, err := a.store.QueryRecords(p, tableName, query)
	if err != nil {
		if errors.Is(err, store.ErrTableNotFound) {
			writeError(w, http.StatusNotFound, "table_not_found", "table 不存在")
			return
		}
		if errors.Is(err, store.ErrQueryValueLimit) {
			writeError(w, http.StatusRequestEntityTooLarge, "query_value_limit", err.Error())
			return
		}
		if errors.Is(err, store.ErrInvalid) {
			writeError(w, http.StatusBadRequest, "invalid_query", err.Error())
			return
		}
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "project_not_found", "project 不存在")
			return
		}
		a.log.Error("query records", "project", p.Name, "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "服务内部错误")
		return
	}

	items := make([]recordItem, 0, len(result.Records))
	for _, record := range result.Records {
		item := recordItem{
			Key:         record.Key,
			CreatedAt:   record.CreatedAt,
			UpdatedAt:   record.UpdatedAt,
			Deleted:     record.Deleted,
			ContentType: record.ContentType,
			Size:        record.Size,
			ContentHash: record.ContentHash,
		}
		if query.IncludeValues && !record.Deleted {
			item.ValueBase64 = base64.StdEncoding.EncodeToString(record.Value)
			if utf8.Valid(record.Value) && isTextContentType(record.ContentType) {
				item.ValueText = string(record.Value)
			}
		}
		items = append(items, item)
	}
	nextOffset := query.Offset + len(items)
	response := recordsResponse{
		Items:      items,
		Total:      result.Total,
		NextOffset: nextOffset,
		HasMore:    int64(nextOffset) < result.Total,
	}
	writeJSON(w, http.StatusOK, response)
}

func parseRecordQuery(r *http.Request) (store.RecordQuery, error) {
	values := r.URL.Query()
	query := store.RecordQuery{
		Prefix:  strings.TrimSpace(values.Get("prefix")),
		Pattern: strings.TrimSpace(values.Get("pattern")),
	}
	var err error
	if query.UpdatedAfter, err = parseTimestamp(values.Get("updatedAfter")); err != nil {
		return store.RecordQuery{}, fmt.Errorf("updatedAfter 不合法: %w", err)
	}
	if query.UpdatedBefore, err = parseTimestamp(values.Get("updatedBefore")); err != nil {
		return store.RecordQuery{}, fmt.Errorf("updatedBefore 不合法: %w", err)
	}
	if query.IncludeDeleted, err = parseBool(values.Get("includeDeleted")); err != nil {
		return store.RecordQuery{}, fmt.Errorf("includeDeleted 不合法: %w", err)
	}
	if query.IncludeValues, err = parseBool(values.Get("includeValues")); err != nil {
		return store.RecordQuery{}, fmt.Errorf("includeValues 不合法: %w", err)
	}
	limit, err := parseInt64(values.Get("limit"), 0)
	if err != nil {
		return store.RecordQuery{}, fmt.Errorf("limit 不合法: %w", err)
	}
	if limit < 0 || limit > 500 {
		return store.RecordQuery{}, errors.New("limit 必须在 0 到 500 之间")
	}
	offset, err := parseInt64(values.Get("offset"), 0)
	if err != nil {
		return store.RecordQuery{}, fmt.Errorf("offset 不合法: %w", err)
	}
	if offset < 0 {
		return store.RecordQuery{}, errors.New("offset 不能为负数")
	}
	query.Limit = int(limit)
	query.Offset = int(offset)
	return query, nil
}

func parseTimestamp(value string) (int64, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, nil
	}
	if parsed, err := strconv.ParseInt(value, 10, 64); err == nil {
		if parsed < 0 {
			return 0, errors.New("不能为负数")
		}
		return parsed, nil
	}
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return 0, errors.New("应为 Unix 毫秒或 RFC3339 时间")
	}
	return parsed.UnixMilli(), nil
}

func parseInt64(value string, defaultValue int64) (int64, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return defaultValue, nil
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0, err
	}
	return parsed, nil
}

func parseBool(value string) (bool, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return false, nil
	}
	return strconv.ParseBool(value)
}

func parseUpdatedAt(r *http.Request) (int64, error) {
	value := strings.TrimSpace(r.Header.Get("X-Updated-At"))
	if value == "" {
		return 0, errors.New("X-Updated-At 不能为空")
	}
	updatedAt, err := strconv.ParseInt(value, 10, 64)
	if err != nil || updatedAt <= 0 {
		return 0, errors.New("X-Updated-At 必须是正整数 Unix 毫秒时间戳")
	}
	return updatedAt, nil
}

func parseWriteCondition(r *http.Request) (store.WriteCondition, error) {
	raw := strings.TrimSpace(r.Header.Get("X-KV-Condition"))
	if raw == "" {
		return store.WriteCondition{}, nil
	}
	if len(raw) > 8<<10 {
		return store.WriteCondition{}, errors.New("X-KV-Condition 不能超过 8 KiB")
	}
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.UseNumber()
	decoder.DisallowUnknownFields()
	var condition store.WriteCondition
	if err := decoder.Decode(&condition); err != nil {
		return store.WriteCondition{}, fmt.Errorf("X-KV-Condition 必须是合法 JSON: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return store.WriteCondition{}, errors.New("X-KV-Condition 只能包含一个 JSON 对象")
	}
	if err := store.ValidateWriteCondition(condition); err != nil {
		return store.WriteCondition{}, err
	}
	return condition, nil
}

func writePreconditionFailed(w http.ResponseWriter, err error) {
	var failed *store.PreconditionFailed
	if !errors.As(err, &failed) {
		writeError(w, http.StatusPreconditionFailed, "precondition_failed", "写入前置条件不成立")
		return
	}
	w.Header().Set("X-Updated-At", strconv.FormatInt(failed.UpdatedAt, 10))
	w.Header().Set("X-Created-At", strconv.FormatInt(failed.CreatedAt, 10))
	writeJSON(w, http.StatusPreconditionFailed, map[string]any{
		"error": map[string]any{
			"code":    "precondition_failed",
			"message": failed.Error(),
		},
		"current": failed,
	})
}

func (a *API) writeTableError(w http.ResponseWriter, operation string, p store.Project, err error) {
	switch {
	case errors.Is(err, store.ErrTableNotFound):
		writeError(w, http.StatusNotFound, "table_not_found", "table 不存在")
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "project_not_found", "project 不存在")
	case errors.Is(err, store.ErrInvalid):
		writeError(w, http.StatusBadRequest, "invalid_table", err.Error())
	default:
		a.log.Error(operation, "project", p.Name, "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "服务内部错误")
	}
}

func (a *API) writeStoreError(w http.ResponseWriter, operation string, p store.Project, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "project_not_found", "project 不存在")
	case errors.Is(err, store.ErrInvalid):
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
	default:
		a.log.Error(operation, "project", p.Name, "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "服务内部错误")
	}
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func isTextContentType(contentType string) bool {
	contentType = strings.ToLower(strings.TrimSpace(contentType))
	return strings.HasPrefix(contentType, "text/") ||
		strings.Contains(contentType, "json") ||
		strings.Contains(contentType, "xml") ||
		strings.Contains(contentType, "javascript")
}

func (a *API) cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" {
			if a.allowAll {
				w.Header().Set("Access-Control-Allow-Origin", "*")
			} else if _, ok := a.allowed[origin]; ok {
				w.Header().Set("Access-Control-Allow-Origin", origin)
				w.Header().Add("Vary", "Origin")
			}
			w.Header().Set("Access-Control-Allow-Methods", "GET, HEAD, POST, PUT, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, X-KV-Condition, X-Updated-At")
			w.Header().Set("Access-Control-Expose-Headers", "ETag, Content-Length, Last-Modified, X-Value-Size, X-Created-At, X-Updated-At")
			w.Header().Set("Access-Control-Max-Age", "86400")
		}
		if r.Method == http.MethodOptions {
			w.Header().Set("Allow", "GET, HEAD, POST, PUT, DELETE, OPTIONS")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func etag(hash string) string {
	return `"` + hash + `"`
}

// WriteError 输出与其他 API 错误一致的 JSON 结构，供外层路由拒绝非法路径时复用。
func WriteError(w http.ResponseWriter, status int, code, message string) {
	writeError(w, status, code, message)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, `{"error":{"code":"`+code+`","message":`+jsonString(message)+`}}`)
}

// jsonString 做最小的 JSON 字符串转义，避免为错误响应引入额外依赖。
func jsonString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '\\', '"':
			b.WriteByte('\\')
			b.WriteRune(r)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if r < 0x20 {
				fmt.Fprintf(&b, `\u%04x`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}
