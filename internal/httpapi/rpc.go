package httpapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"akkvstore/internal/store"
)

const maxRPCBodyBytes = maxBatchBodyBytes

const (
	rpcParseError     = -32700
	rpcInvalidRequest = -32600
	rpcMethodNotFound = -32601
	rpcInvalidParams  = -32602
	rpcInternalError  = -32603
	rpcKeyNotFound    = -32002
	rpcTableNotFound  = -32003
	rpcConflict       = -32004
	rpcPrecondition   = -32005
	rpcTooLarge       = -32006
	rpcInvalidQuery   = -32007
	rpcInvalidBatch   = -32008
)

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

type projectParams struct {
	Project string `json:"project"`
}

type tableNameParams struct {
	Project string `json:"project"`
	Table   string `json:"table"`
}

type tableCreateParams struct {
	Project string `json:"project"`
	Name    string `json:"name"`
}

type kvGetParams struct {
	Project string `json:"project"`
	Table   string `json:"table"`
	Key     string `json:"key"`
}

type kvPutParams struct {
	Project   string               `json:"project"`
	Table     string               `json:"table"`
	Key       string               `json:"key"`
	UpdatedAt int64                `json:"updatedAt"`
	Value     *string              `json:"value"`
	Condition store.WriteCondition `json:"condition,omitempty"`
}

type kvDeleteParams struct {
	Project   string               `json:"project"`
	Table     string               `json:"table"`
	Key       string               `json:"key"`
	UpdatedAt int64                `json:"updatedAt"`
	Condition store.WriteCondition `json:"condition,omitempty"`
}

type tablesResponse struct {
	Items []tableItem `json:"items"`
}

func (a *API) handleRPC(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST, OPTIONS")
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "不支持的 HTTP 方法")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxRPCBodyBytes)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	var request rpcRequest
	if err := decoder.Decode(&request); err != nil {
		writeRPCError(w, http.StatusBadRequest, request.ID, rpcParseError, "JSON-RPC 请求解析失败", nil)
		return
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		writeRPCError(w, http.StatusBadRequest, request.ID, rpcInvalidRequest, "请求只能包含一个 JSON-RPC 对象", nil)
		return
	}
	if request.JSONRPC != "2.0" {
		writeRPCError(w, http.StatusOK, request.ID, rpcInvalidRequest, "jsonrpc 必须是 2.0", nil)
		return
	}
	if strings.TrimSpace(request.Method) == "" {
		writeRPCError(w, http.StatusOK, request.ID, rpcInvalidRequest, "method 不能为空", nil)
		return
	}

	project, err := projectFromParams(request.Params)
	if err != nil {
		writeRPCError(w, http.StatusOK, request.ID, rpcInvalidParams, err.Error(), nil)
		return
	}
	p, token, ok := a.authenticate(w, r, project)
	if !ok {
		return
	}
	a.recordAccess(token, isReadRPCMethod(request.Method))

	var result any
	switch request.Method {
	case "table.list":
		var params projectParams
		if err := decodeParams(request.Params, &params); err != nil {
			writeRPCError(w, http.StatusOK, request.ID, rpcInvalidParams, err.Error(), nil)
			return
		}
		tables, err := a.store.ListTables(p)
		if err != nil {
			writeRPCStoreError(w, request.ID, err)
			return
		}
		items := make([]tableItem, 0, len(tables))
		for _, table := range tables {
			items = append(items, tableItemFromStore(table.Table))
		}
		result = tablesResponse{Items: items}

	case "table.create":
		var params tableCreateParams
		if err := decodeParams(request.Params, &params); err != nil {
			writeRPCError(w, http.StatusOK, request.ID, rpcInvalidParams, err.Error(), nil)
			return
		}
		table, err := a.store.CreateTable(p, params.Name)
		if err != nil {
			writeRPCStoreError(w, request.ID, err)
			return
		}
		result = tableItemFromStore(table)

	case "table.get":
		var params tableNameParams
		if err := decodeParams(request.Params, &params); err != nil {
			writeRPCError(w, http.StatusOK, request.ID, rpcInvalidParams, err.Error(), nil)
			return
		}
		table, err := a.store.GetTableSummary(p, params.Table)
		if err != nil {
			writeRPCStoreError(w, request.ID, err)
			return
		}
		result = tableItemFromStore(table.Table)

	case "table.delete":
		var params tableNameParams
		if err := decodeParams(request.Params, &params); err != nil {
			writeRPCError(w, http.StatusOK, request.ID, rpcInvalidParams, err.Error(), nil)
			return
		}
		if err := a.store.DeleteTable(p, params.Table); err != nil {
			writeRPCStoreError(w, request.ID, err)
			return
		}
		result = map[string]bool{"deleted": true}

	case "kv.get":
		var params kvGetParams
		if err := decodeParams(request.Params, &params); err != nil {
			writeRPCError(w, http.StatusOK, request.ID, rpcInvalidParams, err.Error(), nil)
			return
		}
		item, err := a.getKeyItem(p, params)
		if err != nil {
			writeRPCStoreError(w, request.ID, err)
			return
		}
		result = item

	case "kv.put":
		var params kvPutParams
		if err := decodeParams(request.Params, &params); err != nil {
			writeRPCError(w, http.StatusOK, request.ID, rpcInvalidParams, err.Error(), nil)
			return
		}
		if params.Value == nil {
			writeRPCError(w, http.StatusOK, request.ID, rpcInvalidParams, "value 必须是字符串", nil)
			return
		}
		meta, err := a.store.PutKeyConditional(
			p,
			params.Table,
			params.Key,
			store.ValueTypeString,
			strings.NewReader(*params.Value),
			params.UpdatedAt,
			params.Condition,
		)
		if err != nil {
			writeRPCStoreError(w, request.ID, err)
			return
		}
		a.recordWrites(token, 1)
		result = keyItemFromMeta(params.Table, meta)

	case "kv.delete":
		var params kvDeleteParams
		if err := decodeParams(request.Params, &params); err != nil {
			writeRPCError(w, http.StatusOK, request.ID, rpcInvalidParams, err.Error(), nil)
			return
		}
		meta, err := a.store.DeleteKeyConditional(
			p,
			params.Table,
			params.Key,
			params.UpdatedAt,
			params.Condition,
		)
		if err != nil {
			writeRPCStoreError(w, request.ID, err)
			return
		}
		a.recordWrites(token, 1)
		result = keyItemFromMeta(params.Table, meta)

	case "records.query":
		var params queryParams
		if err := decodeParams(request.Params, &params); err != nil {
			writeRPCError(w, http.StatusOK, request.ID, rpcInvalidParams, err.Error(), nil)
			return
		}
		response, err := a.queryRecords(p, params)
		if err != nil {
			code, message, data := rpcErrorForStore(err)
			if code == rpcInvalidParams {
				code = rpcInvalidQuery
			}
			writeRPCError(w, http.StatusOK, request.ID, code, message, data)
			return
		}
		result = response

	case "batch.execute":
		var params batchParams
		if err := decodeParams(request.Params, &params); err != nil {
			writeRPCError(w, http.StatusOK, request.ID, rpcInvalidParams, err.Error(), nil)
			return
		}
		response, err := a.executeBatch(p, token, params)
		if err != nil {
			writeRPCStoreError(w, request.ID, err)
			return
		}
		result = response

	default:
		writeRPCError(w, http.StatusOK, request.ID, rpcMethodNotFound, "方法不存在", nil)
		return
	}
	writeRPCResult(w, request.ID, result)
}

func (a *API) getKeyItem(p store.Project, params kvGetParams) (keyItem, error) {
	meta, reader, err := a.store.GetKey(p, params.Table, params.Key)
	if err != nil {
		return keyItem{}, err
	}
	defer reader.Close()

	item := keyItemFromMeta(params.Table, meta)
	switch meta.ContentType {
	case store.ValueTypeString:
		value, err := io.ReadAll(reader)
		if err != nil {
			return keyItem{}, err
		}
		text := string(value)
		item.Value = &text
	case store.ValueTypeFile:
		item.ValueOmitted = true
	default:
		return keyItem{}, fmt.Errorf("%w: 不支持的 contentType", store.ErrInvalid)
	}
	return item, nil
}

func keyItemFromMeta(table string, meta store.KeyMeta) keyItem {
	return keyItem{
		Table:       table,
		Key:         meta.Key,
		ContentType: meta.ContentType,
		Size:        meta.Size,
		UpdatedAt:   meta.UpdatedAt,
		Deleted:     meta.Deleted,
	}
}

func tableItemFromStore(table store.Table) tableItem {
	return tableItem{
		Name:      table.Name,
		KeyCount:  table.KeyCount,
		UpdatedAt: table.UpdatedAt,
	}
}

func projectFromParams(raw json.RawMessage) (string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", errors.New("params 必须是对象，并包含 project")
	}
	var params map[string]json.RawMessage
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&params); err != nil {
		return "", errors.New("params 必须是对象，并包含 project")
	}
	value, ok := params["project"]
	if !ok {
		return "", errors.New("params.project 不能为空")
	}
	var project string
	if err := json.Unmarshal(value, &project); err != nil || strings.TrimSpace(project) == "" {
		return "", errors.New("params.project 必须是非空字符串")
	}
	return strings.TrimSpace(project), nil
}

func decodeParams(raw json.RawMessage, dst any) error {
	if len(raw) == 0 || string(raw) == "null" {
		return errors.New("params 必须是对象")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		return fmt.Errorf("params 格式不正确: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return errors.New("params 只能包含一个 JSON 对象")
	}
	return nil
}

func isReadRPCMethod(method string) bool {
	switch method {
	case "table.list", "table.get", "kv.get", "records.query":
		return true
	default:
		return false
	}
}

func writeRPCResult(w http.ResponseWriter, id json.RawMessage, result any) {
	writeJSON(w, http.StatusOK, rpcResponse{
		JSONRPC: "2.0",
		ID:      normalizeRPCID(id),
		Result:  result,
	})
}

func writeRPCError(w http.ResponseWriter, status int, id json.RawMessage, code int, message string, data any) {
	writeJSON(w, status, rpcResponse{
		JSONRPC: "2.0",
		ID:      normalizeRPCID(id),
		Error: &rpcError{
			Code:    code,
			Message: message,
			Data:    data,
		},
	})
}

func normalizeRPCID(id json.RawMessage) json.RawMessage {
	if len(id) == 0 || string(id) == "null" {
		return json.RawMessage("null")
	}
	return id
}

func writeRPCStoreError(w http.ResponseWriter, id json.RawMessage, err error) {
	code, message, data := rpcErrorForStore(err)
	writeRPCError(w, http.StatusOK, id, code, message, data)
}

func rpcErrorForStore(err error) (int, string, any) {
	var validationErr *batchValidationError
	if errors.As(err, &validationErr) {
		return rpcInvalidBatch, validationErr.Error(), nil
	}
	var batchFailure *store.BatchOperationFailure
	if errors.As(err, &batchFailure) {
		code, message, _ := rpcErrorForStore(batchFailure.Err)
		return code, message, map[string]any{
			"index":   batchFailure.Index,
			"table":   batchFailure.Table,
			"key":     batchFailure.Key,
			"message": batchFailure.Error(),
		}
	}
	if store.IsDatabaseBusy(err) {
		return rpcInternalError, "数据库繁忙，请稍后重试", map[string]any{"retryable": true}
	}
	switch {
	case errors.Is(err, store.ErrPreconditionFailed):
		var failed *store.PreconditionFailed
		if errors.As(err, &failed) {
			return rpcPrecondition, failed.Error(), map[string]any{"current": failed}
		}
		return rpcPrecondition, "写入前置条件不成立", nil
	case errors.Is(err, store.ErrTableNotFound):
		return rpcTableNotFound, "table 不存在", map[string]any{"code": "table_not_found"}
	case errors.Is(err, store.ErrNotFound):
		return rpcKeyNotFound, "记录不存在", map[string]any{"code": "not_found"}
	case errors.Is(err, store.ErrTableExists):
		return rpcConflict, "table 已存在", map[string]any{"code": "table_exists"}
	case errors.Is(err, store.ErrKeyExists):
		return rpcConflict, "key 已存在", map[string]any{"code": "key_exists"}
	case errors.Is(err, store.ErrQueryValueLimit):
		return rpcInvalidQuery, err.Error(), nil
	case errors.Is(err, store.ErrTooLarge):
		return rpcTooLarge, err.Error(), nil
	case errors.Is(err, store.ErrInvalid):
		return rpcInvalidParams, err.Error(), nil
	default:
		return rpcInternalError, "服务内部错误", nil
	}
}
