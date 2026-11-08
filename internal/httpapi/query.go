package httpapi

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"unicode/utf8"

	"akkvstore/internal/store"
)

const maxQueryBodyBytes = 1 << 20

type recordsQueryRequest struct {
	Tables         []string                `json:"tables,omitempty"`
	Condition      store.QueryConditionSet `json:"condition,omitempty"`
	IncludeDeleted bool                    `json:"includeDeleted,omitempty"`
	IncludeValues  bool                    `json:"includeValues,omitempty"`
	Limit          int                     `json:"limit,omitempty"`
	Offset         int                     `json:"offset,omitempty"`
}

type singleRecordsQueryRequest struct {
	Condition      store.QueryConditionSet `json:"condition,omitempty"`
	IncludeDeleted bool                    `json:"includeDeleted,omitempty"`
	IncludeValues  bool                    `json:"includeValues,omitempty"`
	Limit          int                     `json:"limit,omitempty"`
	Offset         int                     `json:"offset,omitempty"`
}

func (a *API) handleProjectRecords(w http.ResponseWriter, r *http.Request, p store.Project, _ store.ProjectToken) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST, OPTIONS")
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "不支持的 HTTP 方法")
		return
	}
	var request recordsQueryRequest
	if err := decodeQueryBody(w, r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_query", err.Error())
		return
	}
	result, err := a.store.QueryProjectRecords(p, store.AdvancedRecordQuery{
		Tables:         request.Tables,
		Condition:      request.Condition,
		IncludeDeleted: request.IncludeDeleted,
		IncludeValues:  request.IncludeValues,
		Limit:          request.Limit,
		Offset:         request.Offset,
	})
	if err != nil {
		writeQueryError(w, err)
		return
	}
	writeQueryResult(w, result, request.Offset, request.IncludeValues)
}

func (a *API) handleSingleRecords(w http.ResponseWriter, r *http.Request, p store.Project, _ store.ProjectToken) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST, OPTIONS")
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "不支持的 HTTP 方法")
		return
	}
	var request singleRecordsQueryRequest
	if err := decodeQueryBody(w, r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_query", err.Error())
		return
	}
	result, err := a.store.QueryTableRecords(p, r.PathValue("table"), store.AdvancedRecordQuery{
		Condition:      request.Condition,
		IncludeDeleted: request.IncludeDeleted,
		IncludeValues:  request.IncludeValues,
		Limit:          request.Limit,
		Offset:         request.Offset,
	})
	if err != nil {
		writeQueryError(w, err)
		return
	}
	writeQueryResult(w, result, request.Offset, request.IncludeValues)
}

func decodeQueryBody(w http.ResponseWriter, r *http.Request, dst any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxQueryBodyBytes)
	decoder := json.NewDecoder(r.Body)
	decoder.UseNumber()
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		return fmt.Errorf("请求体必须是合法查询 JSON: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return errors.New("请求体只能包含一个 JSON 对象")
	}
	return nil
}

func writeQueryResult(
	w http.ResponseWriter,
	result store.ProjectRecordQueryResult,
	offset int,
	includeValues bool,
) {
	items := make([]recordItem, 0, len(result.Records))
	for _, projectRecord := range result.Records {
		record := projectRecord.Record
		item := recordItem{
			Table:       projectRecord.Table,
			Key:         record.Key,
			CreatedAt:   record.CreatedAt,
			UpdatedAt:   record.UpdatedAt,
			Deleted:     record.Deleted,
			ContentType: record.ContentType,
			Size:        record.Size,
			ContentHash: record.ContentHash,
		}
		if includeValues && !record.Deleted {
			item.ValueBase64 = base64.StdEncoding.EncodeToString(record.Value)
			if utf8.Valid(record.Value) && isTextContentType(record.ContentType) {
				item.ValueText = string(record.Value)
			}
		}
		items = append(items, item)
	}
	nextOffset := offset + len(items)
	writeJSON(w, http.StatusOK, recordsResponse{
		Items:      items,
		Total:      result.Total,
		NextOffset: nextOffset,
		HasMore:    int64(nextOffset) < result.Total,
	})
}

func writeQueryError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrTableNotFound):
		writeError(w, http.StatusNotFound, "table_not_found", "table 不存在")
	case errors.Is(err, store.ErrQueryValueLimit):
		writeError(w, http.StatusRequestEntityTooLarge, "query_value_limit", err.Error())
	case errors.Is(err, store.ErrInvalid):
		writeError(w, http.StatusBadRequest, "invalid_query", err.Error())
	default:
		writeError(w, http.StatusInternalServerError, "internal", "服务内部错误")
	}
}
