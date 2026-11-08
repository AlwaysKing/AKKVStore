package httpapi

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"akkvstore/internal/store"
)

var testUpdatedAt atomic.Int64

func newTestServer(t *testing.T, apiMax int64, origins []string) (*httptest.Server, *store.Store, store.Project, string) {
	t.Helper()
	dir := t.TempDir()
	dataDir := filepath.Join(dir, "data")
	st, err := store.Open(store.Options{
		SystemDBPath:    filepath.Join(dataDir, "system.db"),
		DataDir:         dataDir,
		InlineThreshold: 16,
		MaxValueBytes:   apiMax,
	})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	p, token, err := st.CreateProject("demo")
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	api := New(st, apiMax, origins, log)

	mux := http.NewServeMux()
	mux.Handle("/v1/", http.StripPrefix("/v1", api.Handler()))
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts, st, p, token
}

func doRequest(t *testing.T, method, rawurl, token string, body []byte, headers map[string]string) *http.Response {
	t.Helper()
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, rawurl, r)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if (method == http.MethodPut || method == http.MethodDelete) && req.Header.Get("X-Updated-At") == "" {
		req.Header.Set("X-Updated-At", strconv.FormatInt(1_760_000_000_000+testUpdatedAt.Add(1), 10))
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, rawurl, err)
	}
	return resp
}

func TestPutGetDelete(t *testing.T) {
	ts, _, _, token := newTestServer(t, 1<<20, []string{"*"})
	base := ts.URL + "/v1/demo/main/kv/" + url.PathEscape("user:1")

	resp := doRequest(t, http.MethodPut, base, token, []byte(`{"hello":"世界"}`), map[string]string{"Content-Type": "application/json"})
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("PUT status = %d, body = %s", resp.StatusCode, readBody(resp))
	}
	if et := resp.Header.Get("ETag"); len(et) != 66 {
		t.Fatalf("ETag = %q", et)
	}
	resp.Body.Close()

	resp = doRequest(t, http.MethodGet, base, token, nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET status = %d, body = %s", resp.StatusCode, readBody(resp))
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q", ct)
	}
	if csp := resp.Header.Get("Content-Security-Policy"); csp != "sandbox; default-src 'none'" {
		t.Fatalf("Content-Security-Policy = %q", csp)
	}
	body := readBody(resp)
	if body != `{"hello":"世界"}` {
		t.Fatalf("body = %q", body)
	}

	// HEAD 不返回 body，但应带 Content-Length。
	req, _ := http.NewRequest(http.MethodHead, base, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	headResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("HEAD: %v", err)
	}
	if headResp.StatusCode != http.StatusOK {
		t.Fatalf("HEAD status = %d", headResp.StatusCode)
	}
	if headResp.ContentLength != int64(len(`{"hello":"世界"}`)) {
		t.Fatalf("HEAD Content-Length = %d", headResp.ContentLength)
	}
	headResp.Body.Close()

	resp = doRequest(t, http.MethodDelete, base, token, nil, nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE status = %d", resp.StatusCode)
	}
	resp.Body.Close()
	resp = doRequest(t, http.MethodDelete, base, token, nil, nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE idempotent status = %d", resp.StatusCode)
	}
	resp.Body.Close()
	resp = doRequest(t, http.MethodGet, base, token, nil, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET after delete status = %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestTableCRUDAndAutoCreate(t *testing.T) {
	ts, _, _, token := newTestServer(t, 1<<20, []string{"*"})
	tablesURL := ts.URL + "/v1/demo"

	resp := doRequest(t, http.MethodGet, tablesURL, token, nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list tables status = %d, body = %s", resp.StatusCode, readBody(resp))
	}
	var list struct {
		Items []struct {
			Name string `json:"name"`
		} `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		t.Fatalf("decode tables: %v", err)
	}
	resp.Body.Close()
	if len(list.Items) != 0 {
		t.Fatalf("initial tables = %+v", list.Items)
	}

	resp = doRequest(t, http.MethodPost, tablesURL, token, []byte(`{"name":"users"}`),
		map[string]string{"Content-Type": "application/json"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create table status = %d, body = %s", resp.StatusCode, readBody(resp))
	}
	if loc := resp.Header.Get("Location"); loc != "/v1/demo/users" {
		t.Fatalf("Location = %q", loc)
	}
	resp.Body.Close()

	resp = doRequest(t, http.MethodPost, tablesURL, token, []byte(`{"name":"users"}`),
		map[string]string{"Content-Type": "application/json"})
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("duplicate table status = %d", resp.StatusCode)
	}
	assertErrorCode(t, resp, "table_exists")

	resp = doRequest(t, http.MethodGet, tablesURL+"/users", token, nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get table status = %d, body = %s", resp.StatusCode, readBody(resp))
	}
	resp.Body.Close()

	autoURL := ts.URL + "/v1/demo/auto/kv/k"
	resp = doRequest(t, http.MethodPut, autoURL, token, []byte("value"),
		map[string]string{"Content-Type": "text/plain"})
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("auto-create put status = %d, body = %s", resp.StatusCode, readBody(resp))
	}
	resp.Body.Close()

	resp = doRequest(t, http.MethodGet, tablesURL, token, nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list tables after put status = %d", resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		t.Fatalf("decode tables after put: %v", err)
	}
	resp.Body.Close()
	if len(list.Items) != 2 {
		t.Fatalf("tables after auto-create = %+v", list.Items)
	}

	resp = doRequest(t, http.MethodGet, tablesURL+"/auto", token, nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get auto table status = %d", resp.StatusCode)
	}
	var detail struct {
		Name     string `json:"name"`
		KeyCount int64  `json:"keyCount"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&detail); err != nil {
		t.Fatalf("decode auto table: %v", err)
	}
	resp.Body.Close()
	if detail.Name != "auto" || detail.KeyCount != 1 {
		t.Fatalf("auto table detail = %+v", detail)
	}

	resp = doRequest(t, http.MethodDelete, tablesURL+"/auto", token, nil, nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete table status = %d, body = %s", resp.StatusCode, readBody(resp))
	}
	resp.Body.Close()
	resp = doRequest(t, http.MethodGet, autoURL, token, nil, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("get deleted table key status = %d", resp.StatusCode)
	}
	assertErrorCode(t, resp, "table_not_found")

	resp = doRequest(t, http.MethodDelete, tablesURL+"/auto", token, nil, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("delete missing table status = %d", resp.StatusCode)
	}
	assertErrorCode(t, resp, "table_not_found")

	resp = doRequest(t, http.MethodPost, tablesURL, token, []byte(`{"name":"Bad Table"}`),
		map[string]string{"Content-Type": "application/json"})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid table status = %d", resp.StatusCode)
	}
	assertErrorCode(t, resp, "invalid_table")
}

func TestRecordsQueryConditionsAndTombstones(t *testing.T) {
	ts, _, _, token := newTestServer(t, 1<<20, []string{"*"})
	rawurl := ts.URL + "/v1/demo/main/kv/" + url.PathEscape("watchLater/series:1")
	const (
		t1 = "1760000000001"
		t2 = "1760000000002"
		t3 = "1760000000003"
		t4 = "1760000000004"
	)

	resp := doRequest(t, http.MethodPut, rawurl, token, []byte(`{"v":1}`), map[string]string{
		"Content-Type":   "application/json",
		"X-Updated-At":   t1,
		"X-KV-Condition": `{"all":[{"field":"exists","op":"eq","value":false}]}`,
	})
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("create status = %d, body = %s", resp.StatusCode, readBody(resp))
	}
	if got := resp.Header.Get("X-Updated-At"); got != t1 {
		t.Fatalf("create updatedAt = %q", got)
	}
	resp.Body.Close()

	resp = doRequest(t, http.MethodPut, rawurl, token, []byte(`{"v":2}`), map[string]string{
		"Content-Type":   "application/json",
		"X-Updated-At":   t2,
		"X-KV-Condition": `{"all":[{"field":"updatedAt","op":"lt","value":1760000000002},{"field":"value","op":"endswith","value":"1}"}]}`,
	})
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("update status = %d, body = %s", resp.StatusCode, readBody(resp))
	}
	if got := resp.Header.Get("X-Updated-At"); got != t2 {
		t.Fatalf("update updatedAt = %q", got)
	}
	resp.Body.Close()

	resp = doRequest(t, http.MethodPut, rawurl, token, []byte(`{"v":"duplicate"}`), map[string]string{
		"Content-Type":   "application/json",
		"X-Updated-At":   t3,
		"X-KV-Condition": `{"all":[{"field":"exists","op":"eq","value":false}]}`,
	})
	if resp.StatusCode != http.StatusPreconditionFailed {
		t.Fatalf("duplicate create status = %d, body = %s", resp.StatusCode, readBody(resp))
	}
	if got := resp.Header.Get("X-Updated-At"); got != t2 {
		t.Fatalf("precondition current updatedAt = %q", got)
	}
	assertErrorCode(t, resp, "precondition_failed")

	resp = doRequest(t, http.MethodPut, rawurl, token, []byte(`{"v":3}`), map[string]string{
		"Content-Type":   "application/json",
		"X-Updated-At":   t3,
		"X-KV-Condition": `{"all":[{"field":"updatedAt","op":"lt","value":1760000000002}]}`,
	})
	if resp.StatusCode != http.StatusPreconditionFailed {
		t.Fatalf("stale update status = %d, body = %s", resp.StatusCode, readBody(resp))
	}
	assertErrorCode(t, resp, "precondition_failed")

	resp = doRequest(t, http.MethodGet, rawurl, token, nil, nil)
	if resp.StatusCode != http.StatusOK || readBody(resp) != `{"v":2}` {
		t.Fatalf("failed writes changed value")
	}

	recordsURL := ts.URL + "/v1/demo/main/records?prefix=" + url.QueryEscape("watchLater/") + "&includeValues=true"
	resp = doRequest(t, http.MethodGet, recordsURL, token, nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("records status = %d, body = %s", resp.StatusCode, readBody(resp))
	}
	var page struct {
		Items []struct {
			Key         string `json:"key"`
			UpdatedAt   int64  `json:"updatedAt"`
			ValueBase64 string `json:"valueBase64"`
			ValueText   string `json:"valueText"`
			Deleted     bool   `json:"deleted"`
		} `json:"items"`
		Total int64 `json:"total"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&page); err != nil {
		t.Fatalf("decode records: %v", err)
	}
	resp.Body.Close()
	if page.Total != 1 || len(page.Items) != 1 {
		t.Fatalf("records page = %+v", page)
	}
	item := page.Items[0]
	if item.Key != "watchLater/series:1" || item.UpdatedAt != 1_760_000_000_002 {
		t.Fatalf("record item = %+v", item)
	}
	if item.ValueText != `{"v":2}` || item.ValueBase64 == "" {
		t.Fatalf("record value = %+v", item)
	}

	resp = doRequest(t, http.MethodDelete, rawurl, token, nil, map[string]string{
		"X-Updated-At":   t4,
		"X-KV-Condition": `{"all":[{"field":"updatedAt","op":"eq","value":1760000000002}]}`,
	})
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete status = %d, body = %s", resp.StatusCode, readBody(resp))
	}
	if got := resp.Header.Get("X-Updated-At"); got != t4 {
		t.Fatalf("delete updatedAt = %q", got)
	}
	resp.Body.Close()

	deletedURL := ts.URL + "/v1/demo/main/records?includeDeleted=true&updatedAfter=1760000000003"
	resp = doRequest(t, http.MethodGet, deletedURL, token, nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("deleted records status = %d, body = %s", resp.StatusCode, readBody(resp))
	}
	if err := json.NewDecoder(resp.Body).Decode(&page); err != nil {
		t.Fatalf("decode deleted records: %v", err)
	}
	resp.Body.Close()
	if page.Total != 1 || len(page.Items) != 1 || !page.Items[0].Deleted {
		t.Fatalf("deleted page = %+v", page)
	}
	if page.Items[0].UpdatedAt != 1_760_000_000_004 {
		t.Fatalf("deleted item = %+v", page.Items[0])
	}
}

func TestProjectPathCaseInsensitive(t *testing.T) {
	ts, _, _, token := newTestServer(t, 1<<20, []string{"*"})
	for _, project := range []string{"demo", "DEMO", "DeMo"} {
		resp := doRequest(t, http.MethodGet, ts.URL+"/v1/"+project, token, nil, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("project path %q status = %d, body = %s", project, resp.StatusCode, readBody(resp))
		}
		resp.Body.Close()
	}
}

func TestUnconditionalWritesUseClientTimestamp(t *testing.T) {
	ts, _, _, token := newTestServer(t, 1<<20, []string{"*"})
	rawurl := ts.URL + "/v1/demo/main/kv/k"

	resp := doRequest(t, http.MethodPut, rawurl, token, []byte("newer"), map[string]string{
		"Content-Type": "text/plain",
		"X-Updated-At": "1760000000100",
	})
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("first put status = %d, body = %s", resp.StatusCode, readBody(resp))
	}
	resp.Body.Close()

	resp = doRequest(t, http.MethodPut, rawurl, token, []byte("older-arriving-later"), map[string]string{
		"Content-Type": "text/plain",
		"X-Updated-At": "1760000000050",
	})
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("second put status = %d, body = %s", resp.StatusCode, readBody(resp))
	}
	if got := resp.Header.Get("X-Updated-At"); got != "1760000000050" {
		t.Fatalf("updatedAt = %q", got)
	}
	resp.Body.Close()

	resp = doRequest(t, http.MethodGet, rawurl, token, nil, nil)
	if resp.StatusCode != http.StatusOK || readBody(resp) != "older-arriving-later" {
		t.Fatalf("server should overwrite without a condition")
	}
}

func TestTokenUsageStats(t *testing.T) {
	ts, st, p, token := newTestServer(t, 1<<20, []string{"*"})
	rawurl := ts.URL + "/v1/demo/main/kv/k"

	resp := doRequest(t, http.MethodPut, rawurl, token, []byte("value"),
		map[string]string{"Content-Type": "text/plain"})
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("PUT status = %d, body = %s", resp.StatusCode, readBody(resp))
	}
	resp.Body.Close()
	resp = doRequest(t, http.MethodGet, rawurl, token, nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET status = %d, body = %s", resp.StatusCode, readBody(resp))
	}
	resp.Body.Close()

	tokens, err := st.ListProjectTokens(p.Name)
	if err != nil {
		t.Fatalf("ListProjectTokens: %v", err)
	}
	if len(tokens) != 1 || tokens[0].RequestCount != 2 || tokens[0].ReadCount != 1 ||
		tokens[0].WriteCount != 1 || tokens[0].LastUsedAt == 0 {
		t.Fatalf("token stats = %+v", tokens)
	}
}

func TestWriteConditionValidation(t *testing.T) {
	ts, _, _, token := newTestServer(t, 1<<20, []string{"*"})
	rawurl := ts.URL + "/v1/demo/main/kv/k"

	req, _ := http.NewRequest(http.MethodPut, rawurl, strings.NewReader("value"))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "text/plain")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT without updatedAt: %v", err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("missing updatedAt status = %d, body = %s", resp.StatusCode, readBody(resp))
	}
	assertErrorCode(t, resp, "invalid_updated_at")

	for _, header := range []string{
		`{"all":[{"field":"unknown","op":"eq","value":true}]}`,
		`{"all":[{"field":"updatedAt","op":"contains","value":1}]}`,
		`not-json`,
		`{"all":[{"field":"updatedAt","op":"lt","value":1}],"unknown":true}`,
	} {
		resp = doRequest(t, http.MethodPut, rawurl, token, []byte("value"), map[string]string{
			"Content-Type":   "text/plain",
			"X-KV-Condition": header,
		})
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("condition %s status = %d, body = %s", header, resp.StatusCode, readBody(resp))
		}
		assertErrorCode(t, resp, "invalid_condition")
	}
}

func TestRecordsValueBudget(t *testing.T) {
	ts, _, _, token := newTestServer(t, 64<<20, []string{"*"})
	value := strings.Repeat("x", 17<<20)
	for _, key := range []string{"a", "b"} {
		resp := doRequest(t, http.MethodPut, ts.URL+"/v1/demo/large/kv/"+key, token,
			[]byte(value), map[string]string{"Content-Type": "text/plain"})
		if resp.StatusCode != http.StatusNoContent {
			t.Fatalf("put %s status = %d, body = %s", key, resp.StatusCode, readBody(resp))
		}
		resp.Body.Close()
	}
	resp := doRequest(t, http.MethodGet,
		ts.URL+"/v1/demo/large/records?includeValues=true", token, nil, nil)
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("records value budget status = %d, body = %s", resp.StatusCode, readBody(resp))
	}
	assertErrorCode(t, resp, "query_value_limit")
}

func TestAuthFailures(t *testing.T) {
	ts, _, _, token := newTestServer(t, 1<<20, []string{"*"})
	rawurl := ts.URL + "/v1/demo/main/kv/k"

	resp := doRequest(t, http.MethodGet, rawurl, "", nil, nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no token status = %d", resp.StatusCode)
	}
	if resp.Header.Get("WWW-Authenticate") == "" {
		t.Fatal("missing WWW-Authenticate")
	}
	resp.Body.Close()

	resp = doRequest(t, http.MethodGet, rawurl, "kv_wrong", nil, nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong token status = %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp = doRequest(t, http.MethodGet, ts.URL+"/v1/missing/main/kv/k", token, nil, nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("missing project status = %d", resp.StatusCode)
	}
	resp.Body.Close()

	// 项目路径与 token 不匹配
	resp = doRequest(t, http.MethodGet, ts.URL+"/v1/demo/main/kv/k", token, nil, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("missing table status = %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestBinaryAndKeyWithSlash(t *testing.T) {
	ts, _, _, token := newTestServer(t, 1<<20, []string{"*"})
	body := []byte{0x00, 0x01, 0x02, 0xff, 'h', 'i'}
	rawurl := ts.URL + "/v1/demo/main/kv/dir/sub%2Fkey"

	resp := doRequest(t, http.MethodPut, rawurl, token, body, map[string]string{"Content-Type": "application/octet-stream"})
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("PUT status = %d, body = %s", resp.StatusCode, readBody(resp))
	}
	resp.Body.Close()

	resp = doRequest(t, http.MethodGet, rawurl, token, nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET status = %d", resp.StatusCode)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !bytes.Equal(got, body) {
		t.Fatalf("body = %v, want %v", got, body)
	}
}

func TestTooLarge(t *testing.T) {
	ts, _, _, token := newTestServer(t, 10, []string{"*"})
	rawurl := ts.URL + "/v1/demo/main/kv/k"

	resp := doRequest(t, http.MethodPut, rawurl, token, []byte("01234567890"), map[string]string{"Content-Type": "text/plain"})
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("known length status = %d, body = %s", resp.StatusCode, readBody(resp))
	}
	resp.Body.Close()

	// 未知长度走 MaxBytesReader
	req, _ := http.NewRequest(http.MethodPut, rawurl, bytes.NewReader([]byte("01234567890")))
	req.ContentLength = -1
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "text/plain")
	req.Header.Set("X-Updated-At", "1760000000001")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT chunked: %v", err)
	}
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("chunked status = %d, body = %s", resp.StatusCode, readBody(resp))
	}
	resp.Body.Close()
}

func TestCORS(t *testing.T) {
	ts, _, _, token := newTestServer(t, 1<<20, []string{"https://app.example.com"})
	rawurl := ts.URL + "/v1/demo/main/kv/k"

	req, _ := http.NewRequest(http.MethodOptions, rawurl, nil)
	req.Header.Set("Origin", "https://app.example.com")
	req.Header.Set("Access-Control-Request-Method", "PUT")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("OPTIONS: %v", err)
	}
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("OPTIONS status = %d", resp.StatusCode)
	}
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "https://app.example.com" {
		t.Fatalf("allow origin = %q", got)
	}
	allowHeaders := resp.Header.Get("Access-Control-Allow-Headers")
	if !strings.Contains(allowHeaders, "X-KV-Condition") || !strings.Contains(allowHeaders, "X-Updated-At") {
		t.Fatalf("allow headers = %q", allowHeaders)
	}
	if exposeHeaders := resp.Header.Get("Access-Control-Expose-Headers"); strings.Contains(exposeHeaders, "X-Revision") ||
		strings.Contains(exposeHeaders, "X-Server-Seq") {
		t.Fatalf("expose headers still contain removed fields: %q", exposeHeaders)
	}
	resp.Body.Close()

	resp = doRequest(t, http.MethodGet, rawurl, token, nil, map[string]string{"Origin": "https://evil.example.com"})
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("unexpected allow origin = %q", got)
	}
	resp.Body.Close()

	resp = doRequest(t, http.MethodGet, rawurl, token, nil, map[string]string{"Origin": "https://app.example.com"})
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "https://app.example.com" {
		t.Fatalf("allow origin = %q", got)
	}
	resp.Body.Close()
}

func TestMethodAndPathErrorsAreJSON(t *testing.T) {
	ts, _, _, token := newTestServer(t, 1<<20, []string{"*"})

	resp := doRequest(t, http.MethodPost, ts.URL+"/v1/demo/main/kv/k", token, []byte("x"),
		map[string]string{"Content-Type": "text/plain"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST status = %d", resp.StatusCode)
	}
	if resp.Header.Get("Allow") == "" {
		t.Fatal("missing Allow header")
	}
	assertErrorCode(t, resp, "method_not_allowed")

	resp = doRequest(t, http.MethodGet, ts.URL+"/v1/demo/main/unknown", token, nil, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown path status = %d", resp.StatusCode)
	}
	assertErrorCode(t, resp, "not_found")

	// %2F 解码后是 leading slash，属于会被路径清理改写的 key，必须拒绝。
	resp = doRequest(t, http.MethodPut, ts.URL+"/v1/demo/main/kv/%2Flead", token, []byte("x"),
		map[string]string{"Content-Type": "text/plain"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("leading slash key status = %d, body = %s", resp.StatusCode, readBody(resp))
	}

	invalidTableURL := ts.URL + "/v1/demo/Bad%20Table/kv/k"
	resp = doRequest(t, http.MethodGet, invalidTableURL, token, nil, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid table GET status = %d, body = %s", resp.StatusCode, readBody(resp))
	}
	assertErrorCode(t, resp, "invalid_table")

	resp = doRequest(t, http.MethodDelete, invalidTableURL, token, nil, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid table DELETE status = %d, body = %s", resp.StatusCode, readBody(resp))
	}
	assertErrorCode(t, resp, "invalid_table")
}

func assertErrorCode(t *testing.T, resp *http.Response, want string) {
	t.Helper()
	var payload struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatalf("decode error response: %v", err)
	}
	if payload.Error.Code != want {
		t.Fatalf("error code = %q, want %q", payload.Error.Code, want)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("Content-Type = %q", ct)
	}
}

func TestErrorJSON(t *testing.T) {
	ts, _, _, token := newTestServer(t, 1<<20, []string{"*"})
	resp := doRequest(t, http.MethodGet, ts.URL+"/v1/demo/main/kv/missing", token, nil, nil)
	defer resp.Body.Close()
	var payload struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if payload.Error.Code != "table_not_found" || payload.Error.Message == "" {
		t.Fatalf("payload = %+v", payload)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("Content-Type = %q", ct)
	}
}

func readBody(resp *http.Response) string {
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return string(b)
}
