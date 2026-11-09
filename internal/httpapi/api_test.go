package httpapi

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"akkvstore/internal/store"
)

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

func doRequest(t *testing.T, method, rawURL, token string, body []byte, headers map[string]string) *http.Response {
	t.Helper()
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, rawURL, reader)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, rawURL, err)
	}
	return resp
}

func rpcCall(t *testing.T, ts *httptest.Server, token, method string, params map[string]any) (int, map[string]any) {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  method,
		"params":  params,
	})
	if err != nil {
		t.Fatalf("marshal rpc: %v", err)
	}
	resp := doRequest(t, http.MethodPost, ts.URL+"/v1/api", token, payload, map[string]string{
		"Content-Type": "application/json",
	})
	defer resp.Body.Close()
	var envelope map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		t.Fatalf("decode rpc response: %v", err)
	}
	return resp.StatusCode, envelope
}

func resultMap(t *testing.T, envelope map[string]any) map[string]any {
	t.Helper()
	if rpcErr, ok := envelope["error"]; ok {
		t.Fatalf("unexpected rpc error: %#v", rpcErr)
	}
	result, ok := envelope["result"].(map[string]any)
	if !ok {
		t.Fatalf("result is not an object: %#v", envelope)
	}
	return result
}

func TestServerTime(t *testing.T) {
	ts, _, _, _ := newTestServer(t, 1<<20, []string{"*"})
	resp := doRequest(t, http.MethodGet, ts.URL+"/v1/time", "", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /v1/time status = %d", resp.StatusCode)
	}
	defer resp.Body.Close()
	var result struct {
		ServerTime int64  `json:"serverTime"`
		ISO        string `json:"iso"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("decode time: %v", err)
	}
	if result.ServerTime <= 0 || result.ISO == "" {
		t.Fatalf("time response = %+v", result)
	}

	resp = doRequest(t, http.MethodPost, ts.URL+"/v1/time", "", nil, nil)
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST /v1/time status = %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestJSONRPCStringLifecycle(t *testing.T) {
	ts, _, _, token := newTestServer(t, 1<<20, []string{"*"})
	project := "demo"

	status, envelope := rpcCall(t, ts, token, "kv.put", map[string]any{
		"project":   project,
		"table":     "main",
		"key":       "user:1",
		"updatedAt": int64(1760000000001),
		"value":     `{"hello":"世界"}`,
	})
	if status != http.StatusOK {
		t.Fatalf("kv.put status = %d, envelope = %#v", status, envelope)
	}
	if _, ok := envelope["error"]; ok {
		t.Fatalf("kv.put error = %#v", envelope)
	}

	status, envelope = rpcCall(t, ts, token, "kv.get", map[string]any{
		"project": project,
		"table":   "main",
		"key":     "user:1",
	})
	if status != http.StatusOK {
		t.Fatalf("kv.get status = %d", status)
	}
	got := resultMap(t, envelope)
	if got["contentType"] != float64(0) || got["value"] != `{"hello":"世界"}` {
		t.Fatalf("kv.get result = %#v", got)
	}

	status, envelope = rpcCall(t, ts, token, "kv.delete", map[string]any{
		"project":   project,
		"table":     "main",
		"key":       "user:1",
		"updatedAt": int64(1760000000002),
	})
	if status != http.StatusOK {
		t.Fatalf("kv.delete status = %d", status)
	}
	resultMap(t, envelope)

	status, envelope = rpcCall(t, ts, token, "kv.get", map[string]any{
		"project": project,
		"table":   "main",
		"key":     "user:1",
	})
	if status != http.StatusOK {
		t.Fatalf("deleted kv.get status = %d", status)
	}
	rpcErr, ok := envelope["error"].(map[string]any)
	if !ok || rpcErr["code"] != float64(-32002) {
		t.Fatalf("deleted kv.get envelope = %#v", envelope)
	}
}

func TestJSONRPCTableAndBatch(t *testing.T) {
	ts, _, _, token := newTestServer(t, 1<<20, []string{"*"})

	status, envelope := rpcCall(t, ts, token, "table.create", map[string]any{
		"project": "demo",
		"name":    "batch",
	})
	if status != http.StatusOK {
		t.Fatalf("table.create status = %d", status)
	}
	resultMap(t, envelope)

	status, envelope = rpcCall(t, ts, token, "table.list", map[string]any{"project": "demo"})
	if status != http.StatusOK {
		t.Fatalf("table.list status = %d", status)
	}
	result := resultMap(t, envelope)
	if items, ok := result["items"].([]any); !ok || len(items) != 1 {
		t.Fatalf("table.list result = %#v", result)
	}

	status, envelope = rpcCall(t, ts, token, "batch.execute", map[string]any{
		"project":     "demo",
		"transaction": true,
		"operations": []any{
			map[string]any{
				"op":        "put",
				"table":     "batch",
				"key":       "one",
				"updatedAt": int64(1760000000100),
				"value":     "first",
			},
		},
	})
	if status != http.StatusOK {
		t.Fatalf("batch.execute status = %d", status)
	}
	batch := resultMap(t, envelope)
	if batch["applied"] != float64(1) {
		t.Fatalf("batch result = %#v", batch)
	}

	status, envelope = rpcCall(t, ts, token, "kv.get", map[string]any{
		"project": "demo",
		"table":   "batch",
		"key":     "one",
	})
	if status != http.StatusOK {
		t.Fatalf("batch kv.get status = %d", status)
	}
	if resultMap(t, envelope)["value"] != "first" {
		t.Fatalf("batch kv.get envelope = %#v", envelope)
	}
}

func TestFileUploadAndDownload(t *testing.T) {
	ts, _, _, token := newTestServer(t, 1<<20, []string{"*"})
	key := base64.RawURLEncoding.EncodeToString([]byte("dir/report.bin"))
	headers := map[string]string{
		"AKKV-Index-Project":   "demo",
		"AKKV-Index-Table":     "files",
		"AKKV-Index-Key":       key,
		"AKKV-Data-Updated-At": "1760000000010",
		"Content-Type":         "application/pdf",
	}
	content := []byte{0x00, 0x01, 0x02, 'A', 'B'}

	resp := doRequest(t, http.MethodPut, ts.URL+"/v1/upload", token, content, headers)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("upload status = %d, body = %s", resp.StatusCode, readBody(resp))
	}
	if et := resp.Header.Get("ETag"); et == "" {
		t.Fatal("upload ETag is empty")
	}
	resp.Body.Close()

	status, envelope := rpcCall(t, ts, token, "kv.get", map[string]any{
		"project": "demo",
		"table":   "files",
		"key":     "dir/report.bin",
	})
	if status != http.StatusOK {
		t.Fatalf("file kv.get status = %d", status)
	}
	got := resultMap(t, envelope)
	if got["contentType"] != float64(1) || got["valueOmitted"] != true {
		t.Fatalf("file kv.get result = %#v", got)
	}

	resp = doRequest(t, http.MethodGet, ts.URL+"/v1/download", token, nil, headers)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("download status = %d, body = %s", resp.StatusCode, readBody(resp))
	}
	if got := readBodyBytes(resp); !bytes.Equal(got, content) {
		t.Fatalf("download body = %v, want %v", got, content)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/octet-stream" {
		t.Fatalf("download Content-Type = %q", ct)
	}

	head := doRequest(t, http.MethodHead, ts.URL+"/v1/download", token, nil, headers)
	if head.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("HEAD download status = %d", head.StatusCode)
	}
	head.Body.Close()
}

func TestUploadConditionAndQuery(t *testing.T) {
	ts, _, _, token := newTestServer(t, 1<<20, []string{"*"})
	key := base64.RawURLEncoding.EncodeToString([]byte("file"))
	headers := map[string]string{
		"AKKV-Index-Project":   "demo",
		"AKKV-Index-Table":     "main",
		"AKKV-Index-Key":       key,
		"AKKV-Data-Updated-At": "1760000000020",
		"AKKV-Data-Condition":  base64.RawURLEncoding.EncodeToString([]byte(`{"all":[{"field":"exists","op":"eq","value":true}]}`)),
	}
	resp := doRequest(t, http.MethodPut, ts.URL+"/v1/upload", token, []byte("file-body"), headers)
	if resp.StatusCode != http.StatusPreconditionFailed {
		t.Fatalf("condition upload status = %d, body = %s", resp.StatusCode, readBody(resp))
	}
	resp.Body.Close()

	_, envelope := rpcCall(t, ts, token, "kv.put", map[string]any{
		"project":   "demo",
		"table":     "main",
		"key":       "a",
		"updatedAt": int64(1760000000021),
		"value":     "alpha",
	})
	resultMap(t, envelope)
	_, envelope = rpcCall(t, ts, token, "kv.put", map[string]any{
		"project":   "demo",
		"table":     "main",
		"key":       "b",
		"updatedAt": int64(1760000000022),
		"value":     "beta",
	})
	resultMap(t, envelope)

	_, envelope = rpcCall(t, ts, token, "records.query", map[string]any{
		"project":       "demo",
		"tables":        []string{"main"},
		"includeValues": true,
		"limit":         10,
	})
	result := resultMap(t, envelope)
	items, ok := result["items"].([]any)
	if !ok || len(items) != 2 {
		t.Fatalf("query result = %#v", result)
	}
	for _, raw := range items {
		item, ok := raw.(map[string]any)
		if !ok || item["contentType"] != float64(0) || item["value"] == nil {
			t.Fatalf("query item = %#v", raw)
		}
	}
}

func TestJSONRPCAuthAndCORS(t *testing.T) {
	ts, _, _, token := newTestServer(t, 1<<20, []string{"*"})
	payload := []byte(`{"jsonrpc":"2.0","id":1,"method":"table.list","params":{"project":"demo"}}`)
	resp := doRequest(t, http.MethodPost, ts.URL+"/v1/api", "", payload, map[string]string{"Content-Type": "application/json"})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthorized rpc status = %d", resp.StatusCode)
	}
	resp.Body.Close()

	req, _ := http.NewRequest(http.MethodOptions, ts.URL+"/v1/upload", nil)
	req.Header.Set("Origin", "https://example.com")
	req.Header.Set("Access-Control-Request-Headers", "AKKV-Index-Project,AKKV-Index-Table,AKKV-Index-Key,AKKV-Data-Updated-At")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("OPTIONS: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("OPTIONS status = %d", resp.StatusCode)
	}
	allow := resp.Header.Get("Access-Control-Allow-Headers")
	if !strings.Contains(allow, "AKKV-Index-Key") || !strings.Contains(allow, "AKKV-Data-Updated-At") {
		t.Fatalf("Access-Control-Allow-Headers = %q", allow)
	}

	status, envelope := rpcCall(t, ts, token, "table.list", map[string]any{"project": "demo"})
	if status != http.StatusOK {
		t.Fatalf("authenticated table.list status = %d", status)
	}
	resultMap(t, envelope)
}

func readBody(resp *http.Response) string {
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	return string(body)
}

func readBodyBytes(resp *http.Response) []byte {
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	return body
}
