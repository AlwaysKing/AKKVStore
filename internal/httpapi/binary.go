package httpapi

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"akkvstore/internal/store"
)

const (
	headerProject   = "AKKV-Index-Project"
	headerTable     = "AKKV-Index-Table"
	headerKey       = "AKKV-Index-Key"
	headerUpdatedAt = "AKKV-Data-Updated-At"
	headerCondition = "AKKV-Data-Condition"

	maxConditionHeaderBytes = 8 << 10
)

func (a *API) handleUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		w.Header().Set("Allow", "PUT, OPTIONS")
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "不支持的 HTTP 方法")
		return
	}
	project := strings.TrimSpace(r.Header.Get(headerProject))
	table := strings.TrimSpace(r.Header.Get(headerTable))
	key, err := decodeIndexKey(r.Header.Get(headerKey))
	if project == "" || table == "" || err != nil {
		writeError(w, http.StatusBadRequest, "invalid_index", "AKKV-Index-* Header 不完整或 key 编码不合法")
		return
	}
	condition, err := parseHeaderCondition(r.Header.Get(headerCondition))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_condition", err.Error())
		return
	}
	updatedAt, err := parseHeaderUpdatedAt(r.Header.Get(headerUpdatedAt))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_updated_at", err.Error())
		return
	}

	p, token, ok := a.authenticate(w, r, project)
	if !ok {
		return
	}
	a.recordAccess(token, false)

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
		table,
		key,
		store.ValueTypeFile,
		r.Body,
		updatedAt,
		condition,
	)
	if err != nil {
		var maxBytesErr *http.MaxBytesError
		switch {
		case errors.As(err, &maxBytesErr), errors.Is(err, store.ErrTooLarge):
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
			a.log.Error("upload file", "project", p.Name, "table", table, "key", key, "err", err)
			writeError(w, http.StatusInternalServerError, "internal", "服务内部错误")
		}
		return
	}

	a.recordWrites(token, 1)
	w.Header().Set("ETag", etag(meta.ContentHash))
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) handleDownload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET, OPTIONS")
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "不支持的 HTTP 方法")
		return
	}
	project := strings.TrimSpace(r.Header.Get(headerProject))
	table := strings.TrimSpace(r.Header.Get(headerTable))
	key, err := decodeIndexKey(r.Header.Get(headerKey))
	if project == "" || table == "" || err != nil {
		writeError(w, http.StatusBadRequest, "invalid_index", "AKKV-Index-* Header 不完整或 key 编码不合法")
		return
	}

	p, token, ok := a.authenticate(w, r, project)
	if !ok {
		return
	}
	a.recordAccess(token, true)

	meta, reader, err := a.store.GetKey(p, table, key)
	if err != nil {
		switch {
		case errors.Is(err, store.ErrTableNotFound):
			writeError(w, http.StatusNotFound, "table_not_found", "table 不存在")
		case errors.Is(err, store.ErrNotFound):
			writeError(w, http.StatusNotFound, "not_found", "key 不存在")
		case errors.Is(err, store.ErrInvalid):
			writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		default:
			a.log.Error("download file", "project", p.Name, "table", table, "key", key, "err", err)
			writeError(w, http.StatusInternalServerError, "internal", "服务内部错误")
		}
		return
	}
	defer reader.Close()

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("ETag", etag(meta.ContentHash))
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "sandbox; default-src 'none'")
	http.ServeContent(w, r, "", time.UnixMilli(meta.UpdatedAt), reader)
}

func decodeIndexKey(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", errors.New("empty key")
	}
	raw, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		raw, err = base64.URLEncoding.DecodeString(value)
	}
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

func parseHeaderUpdatedAt(value string) (int64, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, errors.New(headerUpdatedAt + " 不能为空")
	}
	updatedAt, err := strconv.ParseInt(value, 10, 64)
	if err != nil || updatedAt <= 0 {
		return 0, errors.New(headerUpdatedAt + " 必须是正整数 Unix 毫秒时间戳")
	}
	return updatedAt, nil
}

func parseHeaderCondition(value string) (store.WriteCondition, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return store.WriteCondition{}, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		raw, err = base64.URLEncoding.DecodeString(value)
	}
	if err != nil {
		return store.WriteCondition{}, fmt.Errorf("%s 必须是 Base64URL 编码的 JSON", headerCondition)
	}
	if len(raw) > maxConditionHeaderBytes {
		return store.WriteCondition{}, fmt.Errorf("%s 不能超过 %d 字节", headerCondition, maxConditionHeaderBytes)
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.UseNumber()
	decoder.DisallowUnknownFields()
	var condition store.WriteCondition
	if err := decoder.Decode(&condition); err != nil {
		return store.WriteCondition{}, fmt.Errorf("%s 必须是合法条件 JSON: %w", headerCondition, err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return store.WriteCondition{}, fmt.Errorf("%s 只能包含一个 JSON 对象", headerCondition)
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
	writeJSON(w, http.StatusPreconditionFailed, map[string]any{
		"error": map[string]string{
			"code":    "precondition_failed",
			"message": failed.Error(),
		},
		"current": failed,
	})
}
