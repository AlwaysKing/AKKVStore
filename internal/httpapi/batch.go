package httpapi

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"akkvstore/internal/store"
)

const (
	maxBatchOperations = 500
	maxBatchBodyBytes  = 32 << 20
)

type batchRequest struct {
	Transaction *bool                 `json:"transaction"`
	Operations  []batchOperationInput `json:"operations"`
}

type batchOperationInput struct {
	Op          string               `json:"op"`
	Table       string               `json:"table"`
	Key         string               `json:"key"`
	UpdatedAt   int64                `json:"updatedAt"`
	ContentType string               `json:"contentType,omitempty"`
	Value       *string              `json:"value,omitempty"`
	ValueBase64 *string              `json:"valueBase64,omitempty"`
	Condition   store.WriteCondition `json:"condition,omitempty"`
}

type batchItemResult struct {
	Index         int             `json:"index"`
	Op            string          `json:"op"`
	Table         string          `json:"table"`
	Key           string          `json:"key"`
	OK            bool            `json:"ok"`
	Status        int             `json:"status"`
	CreatedAt     int64           `json:"createdAt,omitempty"`
	UpdatedAt     int64           `json:"updatedAt,omitempty"`
	Deleted       bool            `json:"deleted,omitempty"`
	ContentType   string          `json:"contentType,omitempty"`
	ContentLength int64           `json:"contentLength,omitempty"`
	ContentHash   string          `json:"contentHash,omitempty"`
	ETag          string          `json:"etag,omitempty"`
	Error         *batchItemError `json:"error,omitempty"`
}

type batchItemError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type batchResponse struct {
	Transaction bool              `json:"transaction"`
	Applied     int               `json:"applied"`
	Failed      int               `json:"failed"`
	Results     []batchItemResult `json:"results"`
}

func (a *API) handleBatch(w http.ResponseWriter, r *http.Request, p store.Project, token store.ProjectToken) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST, OPTIONS")
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "不支持的 HTTP 方法")
		return
	}
	request, operations, err := decodeBatchRequest(w, r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_batch", err.Error())
		return
	}
	if err := store.ValidateBatchOperations(operations); err != nil {
		writeBatchStoreError(w, err)
		return
	}

	transaction := *request.Transaction
	if transaction {
		metas, err := a.store.BatchWriteAtomic(p, operations)
		if err != nil {
			writeBatchStoreError(w, err)
			return
		}
		results := make([]batchItemResult, len(operations))
		for i, operation := range operations {
			results[i] = successfulBatchResult(i, operation, metas[i])
		}
		if err := a.store.RecordTokenWrites(token.ID, int64(len(results))); err != nil {
			a.log.Error("record batch token writes", "token_id", token.ID, "err", err)
		}
		writeJSON(w, http.StatusOK, batchResponse{
			Transaction: true,
			Applied:     len(results),
			Results:     results,
		})
		return
	}

	response := batchResponse{
		Transaction: false,
		Results:     make([]batchItemResult, 0, len(operations)),
	}
	for i, operation := range operations {
		var (
			meta store.KeyMeta
			err  error
		)
		if operation.Delete {
			meta, err = a.store.DeleteBatchOperation(p, operation)
		} else {
			meta, err = a.store.PutBatchOperation(p, operation)
		}
		if err != nil {
			status, code, message := batchErrorInfo(err)
			response.Failed++
			response.Results = append(response.Results, batchItemResult{
				Index:  i,
				Op:     batchOperationName(operation),
				Table:  operation.TableName,
				Key:    operation.Key,
				OK:     false,
				Status: status,
				Error:  &batchItemError{Code: code, Message: message},
			})
			continue
		}
		response.Applied++
		response.Results = append(response.Results, successfulBatchResult(i, operation, meta))
	}
	if err := a.store.RecordTokenWrites(token.ID, int64(response.Applied)); err != nil {
		a.log.Error("record batch token writes", "token_id", token.ID, "err", err)
	}
	writeJSON(w, http.StatusOK, response)
}

func decodeBatchRequest(
	w http.ResponseWriter,
	r *http.Request,
) (batchRequest, []store.BatchWriteOperation, error) {
	var request batchRequest
	r.Body = http.MaxBytesReader(w, r.Body, maxBatchBodyBytes)
	decoder := json.NewDecoder(r.Body)
	decoder.UseNumber()
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		return request, nil, fmt.Errorf("请求体必须是合法批量 JSON: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return request, nil, errors.New("请求体只能包含一个 JSON 对象")
	}
	if request.Transaction == nil {
		return request, nil, errors.New("transaction 必须显式设置为 true 或 false")
	}
	if len(request.Operations) == 0 {
		return request, nil, errors.New("operations 不能为空")
	}
	if len(request.Operations) > maxBatchOperations {
		return request, nil, fmt.Errorf("operations 不能超过 %d 个", maxBatchOperations)
	}
	operations := make([]store.BatchWriteOperation, 0, len(request.Operations))
	for i, input := range request.Operations {
		op := strings.ToLower(strings.TrimSpace(input.Op))
		operation := store.BatchWriteOperation{
			TableName: strings.TrimSpace(input.Table),
			Key:       input.Key,
			UpdatedAt: input.UpdatedAt,
			Condition: input.Condition,
		}
		switch op {
		case "put":
			operation.ContentType = input.ContentType
			switch {
			case input.Value != nil && input.ValueBase64 != nil:
				return request, nil, fmt.Errorf("operation %d: value 和 valueBase64 只能二选一", i)
			case input.Value != nil:
				operation.Value = []byte(*input.Value)
			case input.ValueBase64 != nil:
				value, err := base64.StdEncoding.DecodeString(*input.ValueBase64)
				if err != nil {
					value, err = base64.RawStdEncoding.DecodeString(*input.ValueBase64)
				}
				if err != nil {
					return request, nil, fmt.Errorf("operation %d: valueBase64 不合法", i)
				}
				operation.Value = value
			default:
				return request, nil, fmt.Errorf("operation %d: put 必须提供 value 或 valueBase64", i)
			}
		case "delete":
			if input.Value != nil || input.ValueBase64 != nil {
				return request, nil, fmt.Errorf("operation %d: delete 不能包含 value", i)
			}
			operation.Delete = true
		default:
			return request, nil, fmt.Errorf("operation %d: op 必须是 put 或 delete", i)
		}
		operations = append(operations, operation)
	}
	return request, operations, nil
}

func successfulBatchResult(index int, operation store.BatchWriteOperation, meta store.KeyMeta) batchItemResult {
	result := batchItemResult{
		Index:         index,
		Op:            batchOperationName(operation),
		Table:         operation.TableName,
		Key:           operation.Key,
		OK:            true,
		Status:        http.StatusNoContent,
		CreatedAt:     meta.CreatedAt,
		UpdatedAt:     meta.UpdatedAt,
		Deleted:       meta.Deleted,
		ContentType:   meta.ContentType,
		ContentLength: meta.Size,
		ContentHash:   meta.ContentHash,
	}
	if meta.ContentHash != "" {
		result.ETag = etag(meta.ContentHash)
	}
	return result
}

func batchOperationName(operation store.BatchWriteOperation) string {
	if operation.Delete {
		return "delete"
	}
	return "put"
}

func writeBatchStoreError(w http.ResponseWriter, err error) {
	var failure *store.BatchOperationFailure
	if errors.As(err, &failure) {
		if current, ok := failure.Err.(*store.PreconditionFailed); ok {
			writeJSON(w, http.StatusPreconditionFailed, map[string]any{
				"error": map[string]any{
					"code":    "precondition_failed",
					"message": failure.Error(),
				},
				"failedOperation": map[string]any{
					"index": failure.Index,
					"table": failure.Table,
					"key":   failure.Key,
				},
				"current": current,
			})
			return
		}
	}
	status, code, message := batchErrorInfo(err)
	writeError(w, status, code, message)
}

func batchErrorInfo(err error) (int, string, string) {
	switch {
	case errors.Is(err, store.ErrPreconditionFailed):
		return http.StatusPreconditionFailed, "precondition_failed", "写入前置条件不成立"
	case errors.Is(err, store.ErrTooLarge):
		return http.StatusRequestEntityTooLarge, "too_large", err.Error()
	case errors.Is(err, store.ErrTableNotFound):
		return http.StatusNotFound, "table_not_found", "table 不存在"
	case errors.Is(err, store.ErrInvalid):
		return http.StatusBadRequest, "invalid_operation", err.Error()
	default:
		return http.StatusInternalServerError, "internal", "服务内部错误"
	}
}
