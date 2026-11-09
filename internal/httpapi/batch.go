package httpapi

import (
	"errors"
	"fmt"
	"strings"

	"akkvstore/internal/store"
)

const (
	maxBatchOperations = 500
	maxBatchBodyBytes  = 32 << 20
)

type batchParams struct {
	Project     string                `json:"project"`
	Transaction *bool                 `json:"transaction"`
	Operations  []batchOperationInput `json:"operations"`
}

type batchOperationInput struct {
	Op        string               `json:"op"`
	Table     string               `json:"table"`
	Key       string               `json:"key"`
	UpdatedAt int64                `json:"updatedAt"`
	Value     *string              `json:"value,omitempty"`
	Condition store.WriteCondition `json:"condition,omitempty"`
}

type batchItemResult struct {
	Index       int             `json:"index"`
	Op          string          `json:"op"`
	Table       string          `json:"table"`
	Key         string          `json:"key"`
	OK          bool            `json:"ok"`
	Status      int             `json:"status"`
	UpdatedAt   int64           `json:"updatedAt,omitempty"`
	Deleted     bool            `json:"deleted,omitempty"`
	ContentType store.ValueType `json:"contentType"`
	Size        int64           `json:"size,omitempty"`
	Error       *batchItemError `json:"error,omitempty"`
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

type batchValidationError struct {
	err error
}

func (e *batchValidationError) Error() string { return e.err.Error() }
func (e *batchValidationError) Unwrap() error { return e.err }

func (a *API) executeBatch(
	p store.Project,
	token store.ProjectToken,
	params batchParams,
) (batchResponse, error) {
	if params.Transaction == nil {
		return batchResponse{}, &batchValidationError{err: fmt.Errorf("transaction 必须显式设置为 true 或 false")}
	}
	if len(params.Operations) == 0 {
		return batchResponse{}, &batchValidationError{err: fmt.Errorf("operations 不能为空")}
	}
	if len(params.Operations) > maxBatchOperations {
		return batchResponse{}, &batchValidationError{err: fmt.Errorf("operations 不能超过 %d 个", maxBatchOperations)}
	}

	operations, err := buildBatchOperations(params.Operations)
	if err != nil {
		return batchResponse{}, &batchValidationError{err: err}
	}
	if err := store.ValidateBatchOperations(operations); err != nil {
		return batchResponse{}, &batchValidationError{err: err}
	}

	transaction := *params.Transaction
	if transaction {
		metas, err := a.store.BatchWriteAtomic(p, operations)
		if err != nil {
			return batchResponse{}, err
		}
		results := make([]batchItemResult, len(operations))
		for i, operation := range operations {
			results[i] = successfulBatchResult(i, operation, metas[i])
		}
		a.recordWrites(token, int64(len(results)))
		return batchResponse{
			Transaction: true,
			Applied:     len(results),
			Results:     results,
		}, nil
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
	a.recordWrites(token, int64(response.Applied))
	return response, nil
}

func buildBatchOperations(inputs []batchOperationInput) ([]store.BatchWriteOperation, error) {
	operations := make([]store.BatchWriteOperation, 0, len(inputs))
	for i, input := range inputs {
		op := strings.ToLower(strings.TrimSpace(input.Op))
		switch op {
		case "put":
			if input.Value == nil {
				return nil, fmt.Errorf("operation %d: put 必须提供 value", i)
			}
			operations = append(operations, store.BatchWriteOperation{
				TableName:   input.Table,
				Key:         input.Key,
				ContentType: store.ValueTypeString,
				Value:       []byte(*input.Value),
				UpdatedAt:   input.UpdatedAt,
				Condition:   input.Condition,
			})
		case "delete":
			if input.Value != nil {
				return nil, fmt.Errorf("operation %d: delete 不能提供 value", i)
			}
			operations = append(operations, store.BatchWriteOperation{
				TableName: input.Table,
				Key:       input.Key,
				Delete:    true,
				UpdatedAt: input.UpdatedAt,
				Condition: input.Condition,
			})
		default:
			return nil, fmt.Errorf("operation %d: op 只支持 put 或 delete", i)
		}
	}
	return operations, nil
}

func successfulBatchResult(index int, operation store.BatchWriteOperation, meta store.KeyMeta) batchItemResult {
	return batchItemResult{
		Index:       index,
		Op:          batchOperationName(operation),
		Table:       operation.TableName,
		Key:         operation.Key,
		OK:          true,
		Status:      200,
		UpdatedAt:   meta.UpdatedAt,
		Deleted:     meta.Deleted,
		ContentType: meta.ContentType,
		Size:        meta.Size,
	}
}

func batchOperationName(operation store.BatchWriteOperation) string {
	if operation.Delete {
		return "delete"
	}
	return "put"
}

func batchErrorInfo(err error) (int, string, string) {
	switch {
	case store.IsDatabaseBusy(err):
		return 503, "database_busy", "数据库繁忙，请稍后重试"
	case errors.Is(err, store.ErrPreconditionFailed):
		var failed *store.PreconditionFailed
		if errors.As(err, &failed) {
			return 412, "precondition_failed", failed.Error()
		}
		return 412, "precondition_failed", "写入前置条件不成立"
	case errors.Is(err, store.ErrTableNotFound):
		return 404, "table_not_found", "table 不存在"
	case errors.Is(err, store.ErrNotFound):
		return 404, "not_found", "记录不存在"
	case errors.Is(err, store.ErrTooLarge):
		return 413, "too_large", err.Error()
	case errors.Is(err, store.ErrKeyExists):
		return 409, "key_exists", err.Error()
	case errors.Is(err, store.ErrInvalid):
		return 400, "invalid_operation", err.Error()
	default:
		return 500, "internal", "服务内部错误"
	}
}
