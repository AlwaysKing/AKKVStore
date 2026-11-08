package admin_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"akkvstore/internal/admin"
	"akkvstore/internal/auth"
	"akkvstore/internal/store"
)

var tokenRe = regexp.MustCompile(`kv_[A-Za-z0-9_-]{20,}`)
var loginCodeRe = regexp.MustCompile(`验证码：([0-9]{4})`)

func newAdminServer(t *testing.T) (*httptest.Server, *store.Store, *http.Client) {
	t.Helper()
	ts, st, client, _ := newAdminServerWithLimit(t, 1<<20)
	return ts, st, client
}

func newAdminServerWithLimit(t *testing.T, maxValueBytes int64) (*httptest.Server, *store.Store, *http.Client, string) {
	t.Helper()
	return newAdminServerConfigured(t, maxValueBytes, "secret")
}

func newAdminServerWithoutPassword(t *testing.T) (*httptest.Server, *store.Store, *http.Client) {
	t.Helper()
	ts, st, client, _ := newAdminServerConfigured(t, 1<<20, "")
	return ts, st, client
}

func newAdminServerConfigured(
	t *testing.T,
	maxValueBytes int64,
	password string,
) (*httptest.Server, *store.Store, *http.Client, string) {
	t.Helper()
	dir := t.TempDir()
	dataDir := filepath.Join(dir, "data")
	st, err := store.Open(store.Options{
		SystemDBPath:    filepath.Join(dataDir, "system.db"),
		DataDir:         dataDir,
		InlineThreshold: 16,
		MaxValueBytes:   maxValueBytes,
	})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	a, err := admin.New(st, []byte("0123456789abcdef0123456789abcdef"), time.Hour, false, log)
	if err != nil {
		t.Fatalf("admin.New: %v", err)
	}
	if password != "" {
		hash, err := auth.HashPassword(password)
		if err != nil {
			t.Fatalf("HashPassword: %v", err)
		}
		if _, err := st.UpdateAdminPassword(hash); err != nil {
			t.Fatalf("UpdateAdminPassword: %v", err)
		}
	}

	mux := http.NewServeMux()
	mux.Handle("/admin/", http.StripPrefix("/admin", a.Handler()))
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookiejar: %v", err)
	}
	client := &http.Client{Jar: jar, Timeout: 10 * time.Second}
	return ts, st, client, dataDir
}

func bodyOf(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(b)
}

func login(t *testing.T, ts *httptest.Server, client *http.Client) {
	t.Helper()
	resp, err := client.PostForm(ts.URL+"/admin/login", url.Values{
		"login_method": {"password"},
		"password":     {"secret"},
	})
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	body := bodyOf(t, resp)
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "项目") {
		t.Fatalf("login status=%d body=%s", resp.StatusCode, body)
	}
}

func TestUnauthenticatedRedirectsToLogin(t *testing.T) {
	ts, _, _ := newAdminServer(t)
	noRedirect := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := noRedirect.Get(ts.URL + "/admin/")
	if err != nil {
		t.Fatalf("GET /admin/: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/admin/login" {
		t.Fatalf("Location = %q", loc)
	}

	resp2, err := http.Get(ts.URL + "/admin/login")
	if err != nil {
		t.Fatalf("GET /admin/login: %v", err)
	}
	body := bodyOf(t, resp2)
	if resp2.StatusCode != http.StatusOK || !strings.Contains(body, "请输入管理密码") {
		t.Fatalf("login page status=%d body=%s", resp2.StatusCode, body)
	}
}

func TestAdminTablePrefixPagination(t *testing.T) {
	ts, st, client := newAdminServer(t)
	p, _, err := st.CreateProject("paging")
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	table, err := st.CreateTable(p, "main")
	if err != nil {
		t.Fatalf("CreateTable: %v", err)
	}
	for i := 0; i < 55; i++ {
		if _, err := st.PutKey(p, table.Name, fmt.Sprintf("group/key%02d", i), "text/plain",
			strings.NewReader("value")); err != nil {
			t.Fatalf("put group key %d: %v", i, err)
		}
	}
	for i := 0; i < 5; i++ {
		if _, err := st.PutKey(p, table.Name, fmt.Sprintf("other/key%02d", i), "text/plain",
			strings.NewReader("value")); err != nil {
			t.Fatalf("put other key %d: %v", i, err)
		}
	}
	login(t, ts, client)

	baseURL := ts.URL + "/admin/projects/paging/tables/main"
	resp, err := client.Get(baseURL + "?prefix=group%2F")
	if err != nil {
		t.Fatalf("get filtered page: %v", err)
	}
	body := bodyOf(t, resp)
	if !strings.Contains(body, `name="prefix"`) || !strings.Contains(body, `placeholder="按 key 前缀过滤"`) {
		t.Fatalf("prefix search form missing: %s", body)
	}
	if !strings.Contains(body, "group/key00") || strings.Contains(body, "other/key00") {
		t.Fatalf("first filtered page is incorrect: %s", body)
	}
	if !strings.Contains(body, "page=2") || !strings.Contains(body, "prefix=group%2F") {
		t.Fatalf("next page link does not preserve prefix: %s", body)
	}
	if !strings.Contains(body, "key=group%2Fkey00") || strings.Contains(body, "%252F") {
		t.Fatalf("key URL is not encoded exactly once: %s", body)
	}

	resp, err = client.Get(baseURL + "?page=2&prefix=group%2F")
	if err != nil {
		t.Fatalf("get filtered page 2: %v", err)
	}
	body = bodyOf(t, resp)
	if !strings.Contains(body, "group/key50") || !strings.Contains(body, "group/key54") {
		t.Fatalf("second filtered page is incomplete: %s", body)
	}
	if strings.Contains(body, "other/key") {
		t.Fatalf("second filtered page leaked unfiltered rows: %s", body)
	}
}

func TestInlineMultipartCreate(t *testing.T) {
	ts, st, client := newAdminServer(t)
	p, _, err := st.CreateProject("inline")
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	if _, err := st.CreateTable(p, "main"); err != nil {
		t.Fatalf("CreateTable: %v", err)
	}
	login(t, ts, client)

	submit := func() *http.Response {
		var body bytes.Buffer
		writer := multipart.NewWriter(&body)
		_ = writer.WriteField("action", "save")
		_ = writer.WriteField("key", "new-key")
		_ = writer.WriteField("value", "new value")
		_ = writer.WriteField("content_type", "text/plain")
		_ = writer.Close()
		req, _ := http.NewRequest(http.MethodPost,
			ts.URL+"/admin/projects/inline/tables/main/key?return_to=table&create=1", &body)
		req.Header.Set("Content-Type", writer.FormDataContentType())
		req.Header.Set("X-Inline-Edit", "1")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("inline multipart create: %v", err)
		}
		return resp
	}

	resp := submit()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("inline create status = %d, body = %s", resp.StatusCode, bodyOf(t, resp))
	}
	bodyOf(t, resp)
	meta, rc, err := st.GetKey(p, "main", "new-key")
	if err != nil {
		t.Fatalf("GetKey after inline create: %v", err)
	}
	if meta.ContentType != "text/plain" || readAll(t, rc) != "new value" {
		t.Fatalf("inline created meta = %+v", meta)
	}

	var uploadBody bytes.Buffer
	writer := multipart.NewWriter(&uploadBody)
	_ = writer.WriteField("action", "save")
	_ = writer.WriteField("key", "file-key")
	filePart, err := writer.CreateFormFile("file", "upload.bin")
	if err != nil {
		t.Fatalf("CreateFormFile: %v", err)
	}
	if _, err := filePart.Write([]byte("uploaded-content")); err != nil {
		t.Fatalf("write upload: %v", err)
	}
	_ = writer.WriteField("content_type", "text/custom")
	_ = writer.Close()
	req, _ := http.NewRequest(http.MethodPost,
		ts.URL+"/admin/projects/inline/tables/main/key?return_to=table&create=1", &uploadBody)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("X-Inline-Edit", "1")
	resp = func() *http.Response {
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("inline file create: %v", err)
		}
		return resp
	}()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("inline file create status = %d, body = %s", resp.StatusCode, bodyOf(t, resp))
	}
	bodyOf(t, resp)
	meta, rc, err = st.GetKey(p, "main", "file-key")
	if err != nil {
		t.Fatalf("GetKey uploaded file: %v", err)
	}
	if meta.ContentType != "text/custom" || readAll(t, rc) != "uploaded-content" {
		t.Fatalf("uploaded file meta = %+v", meta)
	}

	resp = submit()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("inline duplicate status = %d, body = %s", resp.StatusCode, bodyOf(t, resp))
	}
	bodyOf(t, resp)
}

func TestAdminMultipartUploadLimit(t *testing.T) {
	ts, st, client, dataDir := newAdminServerWithLimit(t, 10)
	p, _, err := st.CreateProject("limited")
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	if _, err := st.CreateTable(p, "main"); err != nil {
		t.Fatalf("CreateTable: %v", err)
	}
	login(t, ts, client)

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	_ = writer.WriteField("action", "save")
	_ = writer.WriteField("key", "too-large")
	filePart, err := writer.CreateFormFile("file", "large.bin")
	if err != nil {
		t.Fatalf("CreateFormFile: %v", err)
	}
	if _, err := filePart.Write(bytes.Repeat([]byte("x"), 100)); err != nil {
		t.Fatalf("write upload: %v", err)
	}
	_ = writer.WriteField("content_type", "application/octet-stream")
	_ = writer.Close()
	req, _ := http.NewRequest(http.MethodPost,
		ts.URL+"/admin/projects/limited/tables/main/key?return_to=table&create=1", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("X-Inline-Edit", "1")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("upload over limit: %v", err)
	}
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("upload limit status = %d, body = %s", resp.StatusCode, bodyOf(t, resp))
	}
	bodyOf(t, resp)
	if _, _, err := st.GetKey(p, "main", "too-large"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("over-limit key err = %v, want ErrNotFound", err)
	}
	entries, err := os.ReadDir(filepath.Join(dataDir, "tmp"))
	if err != nil {
		t.Fatalf("read managed temp dir: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("managed temp dir is not clean: %+v", entries)
	}
}

func TestAdminBulkDeleteKeys(t *testing.T) {
	ts, st, client := newAdminServer(t)
	p, _, err := st.CreateProject("bulk")
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	table, err := st.CreateTable(p, "main")
	if err != nil {
		t.Fatalf("CreateTable: %v", err)
	}
	for _, key := range []string{"a", "b", "c"} {
		if _, err := st.PutKey(p, table.Name, key, "text/plain", strings.NewReader(key)); err != nil {
			t.Fatalf("PutKey %s: %v", key, err)
		}
	}
	login(t, ts, client)

	resp, err := client.PostForm(ts.URL+"/admin/projects/bulk/tables/main/keys/delete", url.Values{
		"key": {"a", "a", "b"},
	})
	if err != nil {
		t.Fatalf("bulk delete: %v", err)
	}
	if !strings.HasSuffix(resp.Request.URL.Path, "/tables/main") {
		t.Fatalf("bulk delete should return to table, got path %s", resp.Request.URL.Path)
	}
	body := bodyOf(t, resp)
	if !strings.Contains(body, "已删除 2 个 Key") {
		t.Fatalf("bulk delete flash missing: %s", body)
	}
	for _, key := range []string{"a", "b"} {
		if _, _, err := st.GetKey(p, table.Name, key); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("deleted key %s err = %v, want ErrNotFound", key, err)
		}
	}
	if _, _, err := st.GetKey(p, table.Name, "c"); err != nil {
		t.Fatalf("remaining key c: %v", err)
	}
}

func TestAdminKeyWhitespaceNormalization(t *testing.T) {
	ts, st, client := newAdminServer(t)
	p, _, err := st.CreateProject("spaces")
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	table, err := st.CreateTable(p, "main")
	if err != nil {
		t.Fatalf("CreateTable: %v", err)
	}
	login(t, ts, client)

	resp, err := client.PostForm(ts.URL+"/admin/projects/spaces/tables/main/key?return_to=table&create=1", url.Values{
		"action":       {"save"},
		"key":          {"  spaced-key  "},
		"content_type": {"text/plain"},
		"value":        {"value"},
	})
	if err != nil {
		t.Fatalf("create spaced key: %v", err)
	}
	bodyOf(t, resp)
	if _, _, err := st.GetKey(p, table.Name, "spaced-key"); err != nil {
		t.Fatalf("trimmed key was not created: %v", err)
	}
	if _, _, err := st.GetKey(p, table.Name, "  spaced-key  "); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("untrimmed key err = %v, want ErrNotFound", err)
	}

	if _, err := st.PutKey(p, table.Name, "asd ", "text/plain", strings.NewReader("existing")); err != nil {
		t.Fatalf("create trailing-space key: %v", err)
	}
	resp, err = client.Get(ts.URL + "/admin/projects/spaces/tables/main")
	if err != nil {
		t.Fatalf("get table page: %v", err)
	}
	body := bodyOf(t, resp)
	if !strings.Contains(body, "asd␠") {
		t.Fatalf("trailing space is not visible in key label: %s", body)
	}
}

func TestWrongPassword(t *testing.T) {
	ts, _, _ := newAdminServer(t)
	resp, err := http.PostForm(ts.URL+"/admin/login", url.Values{
		"login_method": {"password"},
		"password":     {"wrong"},
	})
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	body := bodyOf(t, resp)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if !strings.Contains(body, "密码不正确") {
		t.Fatalf("body = %s", body)
	}
}

func TestLoginMethodsRespectEnabledSettings(t *testing.T) {
	ts, st, client := newAdminServer(t)
	if _, err := st.UpdateAdminLoginSettings(false, true, "https://bark.example.com/device-key"); err != nil {
		t.Fatalf("enable Bark-only login: %v", err)
	}

	resp, err := client.Get(ts.URL + "/admin/login")
	if err != nil {
		t.Fatalf("get Bark-only login page: %v", err)
	}
	body := bodyOf(t, resp)
	if !strings.Contains(body, `name="code"`) || !strings.Contains(body, "发送验证码") ||
		strings.Contains(body, `name="password"`) {
		t.Fatalf("Bark-only login page is incorrect: %s", body)
	}

	resp, err = client.PostForm(ts.URL+"/admin/login", url.Values{
		"login_method": {"password"},
		"password":     {"secret"},
	})
	if err != nil {
		t.Fatalf("disabled password login: %v", err)
	}
	body = bodyOf(t, resp)
	if resp.StatusCode != http.StatusForbidden || !strings.Contains(body, "管理员密码登录未启用") {
		t.Fatalf("disabled password login status=%d body=%s", resp.StatusCode, body)
	}

	if _, err := st.UpdateAdminLoginSettings(true, false, "https://bark.example.com/device-key"); err != nil {
		t.Fatalf("enable password-only login: %v", err)
	}
	resp, err = client.Get(ts.URL + "/admin/login")
	if err != nil {
		t.Fatalf("get password-only login page: %v", err)
	}
	body = bodyOf(t, resp)
	if !strings.Contains(body, `name="password"`) || strings.Contains(body, `name="code"`) {
		t.Fatalf("password-only login page is incorrect: %s", body)
	}
	resp, err = client.PostForm(ts.URL+"/admin/login", url.Values{
		"login_method": {"bark"},
		"code":         {"1234"},
	})
	if err != nil {
		t.Fatalf("disabled Bark login: %v", err)
	}
	body = bodyOf(t, resp)
	if resp.StatusCode != http.StatusForbidden || !strings.Contains(body, "Bark 验证码登录未启用") {
		t.Fatalf("disabled Bark login status=%d body=%s", resp.StatusCode, body)
	}
}

func TestAdminNewUsesPersistedPassword(t *testing.T) {
	_, st, _ := newAdminServer(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if _, err := admin.New(st, []byte("0123456789abcdef0123456789abcdef"), time.Hour, false, log); err != nil {
		t.Fatalf("admin.New with persisted password: %v", err)
	}
}

func TestSessionRejectedBeforeAdminPasswordSetup(t *testing.T) {
	ts, st, _ := newAdminServerWithoutPassword(t)
	settings, err := st.GetAdminSettings()
	if err != nil {
		t.Fatalf("GetAdminSettings: %v", err)
	}
	signer := auth.NewCookieSigner([]byte("0123456789abcdef0123456789abcdef"))
	value, err := signer.Sign(auth.NewSessionWithEpoch(time.Hour, settings.AuthEpoch))
	if err != nil {
		t.Fatalf("sign session: %v", err)
	}
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/admin/", nil)
	req.AddCookie(&http.Cookie{Name: "akkv_admin", Value: value, Path: "/admin"})
	client := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("request dashboard with pre-setup session: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/admin/login" {
		t.Fatalf("pre-setup session status=%d location=%q", resp.StatusCode, resp.Header.Get("Location"))
	}
}

func TestInitialAdminPasswordSetup(t *testing.T) {
	ts, _, client := newAdminServerWithoutPassword(t)

	resp, err := client.Get(ts.URL + "/admin/login")
	if err != nil {
		t.Fatalf("get initial password page: %v", err)
	}
	body := bodyOf(t, resp)
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "首次使用，请设置管理员初始密码") ||
		!strings.Contains(body, `action="/admin/login/setup"`) {
		t.Fatalf("initial password setup page is missing: status=%d body=%s", resp.StatusCode, body)
	}

	for name, form := range map[string]url.Values{
		"too short": {
			"new_password":     {"short"},
			"confirm_password": {"short"},
		},
		"mismatch": {
			"new_password":     {"secure-password"},
			"confirm_password": {"other-password"},
		},
		"legacy placeholder": {
			"new_password":     {"change-this-to-a-long-password"},
			"confirm_password": {"change-this-to-a-long-password"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			resp, err := client.PostForm(ts.URL+"/admin/login/setup", form)
			if err != nil {
				t.Fatalf("post invalid initial password: %v", err)
			}
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("invalid setup status = %d, body = %s", resp.StatusCode, bodyOf(t, resp))
			}
			bodyOf(t, resp)
		})
	}

	resp, err = client.PostForm(ts.URL+"/admin/login/setup", url.Values{
		"new_password":     {"secure-password"},
		"confirm_password": {"secure-password"},
	})
	if err != nil {
		t.Fatalf("set initial password: %v", err)
	}
	body = bodyOf(t, resp)
	if resp.StatusCode != http.StatusOK || !strings.HasSuffix(resp.Request.URL.Path, "/admin/") ||
		!strings.Contains(body, "初始管理员密码已设置") {
		t.Fatalf("initial setup did not log in: status=%d path=%s body=%s", resp.StatusCode, resp.Request.URL.Path, body)
	}

	resp, err = client.Get(ts.URL + "/admin/")
	if err != nil {
		t.Fatalf("dashboard after initial setup: %v", err)
	}
	if resp.StatusCode != http.StatusOK || !strings.HasSuffix(resp.Request.URL.Path, "/admin/") {
		t.Fatalf("dashboard after initial setup status=%d path=%s", resp.StatusCode, resp.Request.URL.Path)
	}
	bodyOf(t, resp)

	jar, _ := cookiejar.New(nil)
	client2 := &http.Client{Jar: jar, Timeout: 10 * time.Second}
	resp, err = client2.PostForm(ts.URL+"/admin/login/setup", url.Values{
		"new_password":     {"replacement-password"},
		"confirm_password": {"replacement-password"},
	})
	if err != nil {
		t.Fatalf("second initial setup: %v", err)
	}
	body = bodyOf(t, resp)
	if !strings.Contains(body, "管理员初始密码已经设置") {
		t.Fatalf("second initial setup was not rejected: %s", body)
	}

	resp, err = client2.Get(ts.URL + "/admin/login")
	if err != nil {
		t.Fatalf("get login after setup: %v", err)
	}
	body = bodyOf(t, resp)
	if strings.Contains(body, "首次使用，请设置管理员初始密码") || !strings.Contains(body, "请输入管理密码") {
		t.Fatalf("login page did not switch to normal password mode: %s", body)
	}

	resp, err = client2.PostForm(ts.URL+"/admin/login", url.Values{
		"login_method": {"password"},
		"password":     {"secure-password"},
	})
	if err != nil {
		t.Fatalf("login with initialized password: %v", err)
	}
	if resp.StatusCode != http.StatusOK || !strings.HasSuffix(resp.Request.URL.Path, "/admin/") {
		t.Fatalf("initialized password login status=%d path=%s body=%s",
			resp.StatusCode, resp.Request.URL.Path, bodyOf(t, resp))
	}
	bodyOf(t, resp)
}

func TestSystemSettingsAndPasswordChange(t *testing.T) {
	ts, _, client := newAdminServer(t)
	login(t, ts, client)
	jar2, _ := cookiejar.New(nil)
	client2 := &http.Client{Jar: jar2, Timeout: 10 * time.Second}
	login(t, ts, client2)

	resp, err := client.Get(ts.URL + "/admin/")
	if err != nil {
		t.Fatalf("get dashboard: %v", err)
	}
	body := bodyOf(t, resp)
	if !strings.Contains(body, `href="/admin/system"`) {
		t.Fatalf("system settings entry missing: %s", body)
	}

	resp, err = client.Get(ts.URL + "/admin/system")
	if err != nil {
		t.Fatalf("get system settings: %v", err)
	}
	body = bodyOf(t, resp)
	if resp.StatusCode != http.StatusOK ||
		!strings.HasSuffix(resp.Request.URL.Path, "/admin/system") ||
		!strings.Contains(body, "认证方式") ||
		!strings.Contains(body, "密码认证") ||
		!strings.Contains(body, "Bark 推送认证") ||
		!strings.Contains(body, `class="workspace-sidebar"`) ||
		!strings.Contains(body, "system-nav-icon") ||
		!strings.Contains(body, `data-auth-toggle="password"`) ||
		!strings.Contains(body, `data-auth-toggle="bark"`) ||
		!strings.Contains(body, "data-bark-address") ||
		!strings.Contains(body, "data-bark-verify") ||
		!strings.Contains(body, `id="bark-verify-dialog"`) ||
		strings.Contains(body, `name="current_password"`) ||
		strings.Contains(body, `name="confirm_password"`) ||
		!strings.Contains(body, "输入新密码") {
		t.Fatalf("auth settings page incomplete: %s", body)
	}

	resp, err = client.PostForm(ts.URL+"/admin/system/auth/method", url.Values{
		"method":  {"password"},
		"enabled": {"0"},
	})
	if err != nil {
		t.Fatalf("disable last auth method: %v", err)
	}
	body = bodyOf(t, resp)
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(body, "必须至少启用一种认证方式") {
		t.Fatalf("disable last auth method status=%d body=%s", resp.StatusCode, body)
	}
	resp, err = client.PostForm(ts.URL+"/admin/system/auth/method", url.Values{
		"method":  {"bark"},
		"enabled": {"1"},
	})
	if err != nil {
		t.Fatalf("enable unverified Bark auth: %v", err)
	}
	body = bodyOf(t, resp)
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(body, "请先验证 Bark 地址") {
		t.Fatalf("enable unverified Bark auth status=%d body=%s", resp.StatusCode, body)
	}
	resp, err = client.PostForm(ts.URL+"/admin/system/password", url.Values{
		"new_password": {"change-this-to-a-long-password"},
	})
	if err != nil {
		t.Fatalf("set legacy placeholder password: %v", err)
	}
	body = bodyOf(t, resp)
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(body, "不能使用旧版默认占位密码") {
		t.Fatalf("legacy placeholder update status=%d body=%s", resp.StatusCode, body)
	}
	resp, err = client.PostForm(ts.URL+"/admin/system/password", url.Values{
		"new_password": {"new-secret"},
	})
	if err != nil {
		t.Fatalf("update password: %v", err)
	}
	body = bodyOf(t, resp)
	if !strings.Contains(body, "管理员密码已更新") {
		t.Fatalf("password update flash missing: %s", body)
	}
	resp, err = client2.Get(ts.URL + "/admin/")
	if err != nil {
		t.Fatalf("stale session request: %v", err)
	}
	if !strings.HasSuffix(resp.Request.URL.Path, "/admin/login") {
		t.Fatalf("stale session should be invalidated, path = %s", resp.Request.URL.Path)
	}
	bodyOf(t, resp)
	resp, err = client.PostForm(ts.URL+"/admin/logout", nil)
	if err != nil {
		t.Fatalf("logout: %v", err)
	}
	bodyOf(t, resp)

	resp, err = client.PostForm(ts.URL+"/admin/login", url.Values{
		"login_method": {"password"},
		"password":     {"secret"},
	})
	if err != nil {
		t.Fatalf("old password login: %v", err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("old password status = %d, body = %s", resp.StatusCode, bodyOf(t, resp))
	}
	bodyOf(t, resp)
	resp, err = client.PostForm(ts.URL+"/admin/login", url.Values{
		"login_method": {"password"},
		"password":     {"new-secret"},
	})
	if err != nil {
		t.Fatalf("new password login: %v", err)
	}
	if resp.StatusCode != http.StatusOK || !strings.HasSuffix(resp.Request.URL.Path, "/admin/") {
		t.Fatalf("new password login status = %d, path = %s", resp.StatusCode, resp.Request.URL.Path)
	}
	bodyOf(t, resp)
}

func TestBarkCodeLogin(t *testing.T) {
	codeCh := make(chan string, 2)
	bark := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		match := loginCodeRe.FindStringSubmatch(r.URL.Path)
		if len(match) != 2 {
			t.Errorf("Bark path missing 4 digit code: %s", r.URL.Path)
			codeCh <- ""
			http.Error(w, "bad code", http.StatusBadRequest)
			return
		}
		codeCh <- match[1]
		w.WriteHeader(http.StatusOK)
	}))
	defer bark.Close()

	ts, st, client := newAdminServer(t)
	login(t, ts, client)
	barkAddress := bark.URL + "/device-key"
	resp, err := client.PostForm(ts.URL+"/admin/system/auth/bark/send", url.Values{
		"bark_address": {barkAddress},
	})
	if err != nil {
		t.Fatalf("send Bark setup code: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("send Bark setup code status = %d, body = %s", resp.StatusCode, bodyOf(t, resp))
	}
	bodyOf(t, resp)
	setupCode := <-codeCh
	if len(setupCode) != 4 {
		t.Fatalf("Bark setup code = %q", setupCode)
	}
	wrongCode := "0000"
	if setupCode == wrongCode {
		wrongCode = "0001"
	}
	resp, err = client.PostForm(ts.URL+"/admin/system/auth/bark/verify", url.Values{
		"bark_address": {barkAddress},
		"bark_code":    {wrongCode},
	})
	if err != nil {
		t.Fatalf("verify wrong Bark code: %v", err)
	}
	bodyOf(t, resp)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("wrong Bark code status = %d", resp.StatusCode)
	}
	settings, err := st.GetAdminSettings()
	if err != nil {
		t.Fatalf("GetAdminSettings after wrong Bark code: %v", err)
	}
	if settings.BarkLoginEnabled {
		t.Fatal("wrong Bark code enabled Bark login")
	}

	resp, err = client.PostForm(ts.URL+"/admin/system/auth/bark/verify", url.Values{
		"bark_address": {barkAddress},
		"bark_code":    {setupCode},
	})
	if err != nil {
		t.Fatalf("verify Bark login address: %v", err)
	}
	var verifyResult struct {
		OK      bool   `json:"ok"`
		Address string `json:"address"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&verifyResult); err != nil {
		resp.Body.Close()
		t.Fatalf("decode Bark verify response: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !verifyResult.OK || verifyResult.Address != barkAddress {
		t.Fatalf("verify Bark login status=%d result=%+v", resp.StatusCode, verifyResult)
	}
	settings, err = st.GetAdminSettings()
	if err != nil {
		t.Fatalf("GetAdminSettings after Bark verification: %v", err)
	}
	if settings.BarkAddress != barkAddress {
		t.Fatalf("verified Bark settings = %+v", settings)
	}
	resp, err = client.PostForm(ts.URL+"/admin/system/auth/method", url.Values{
		"method":  {"bark"},
		"enabled": {"1"},
	})
	if err != nil {
		t.Fatalf("enable Bark auth after verification: %v", err)
	}
	bodyOf(t, resp)
	settings, err = st.GetAdminSettings()
	if err != nil {
		t.Fatalf("GetAdminSettings after enabling Bark: %v", err)
	}
	if !settings.BarkLoginEnabled {
		t.Fatalf("Bark auth not enabled after toggle: %+v", settings)
	}

	resp, err = client.PostForm(ts.URL+"/admin/logout", nil)
	if err != nil {
		t.Fatalf("logout: %v", err)
	}
	bodyOf(t, resp)
	resp, err = client.Get(ts.URL + "/admin/login")
	if err != nil {
		t.Fatalf("get login methods: %v", err)
	}
	body := bodyOf(t, resp)
	if !strings.Contains(body, `data-login-tab="password"`) ||
		!strings.Contains(body, `data-login-tab="bark"`) ||
		!strings.Contains(body, "发送验证码") {
		t.Fatalf("enabled login methods missing: %s", body)
	}
	resp, err = client.PostForm(ts.URL+"/admin/login", url.Values{
		"login_method": {"password"},
		"password":     {"secret"},
	})
	if err != nil {
		t.Fatalf("password login with Bark enabled: %v", err)
	}
	if resp.StatusCode != http.StatusOK || !strings.HasSuffix(resp.Request.URL.Path, "/admin/") {
		t.Fatalf("password login with Bark enabled status = %d, path = %s", resp.StatusCode, resp.Request.URL.Path)
	}
	bodyOf(t, resp)
	resp, err = client.PostForm(ts.URL+"/admin/logout", nil)
	if err != nil {
		t.Fatalf("logout after password login: %v", err)
	}
	bodyOf(t, resp)
	resp, err = client.PostForm(ts.URL+"/admin/login/code", nil)
	if err != nil {
		t.Fatalf("send login code: %v", err)
	}
	bodyOf(t, resp)

	code := <-codeCh
	if len(code) != 4 {
		t.Fatalf("login code = %q", code)
	}
	resp, err = client.PostForm(ts.URL+"/admin/login", url.Values{
		"login_method": {"bark"},
		"code":         {code},
	})
	if err != nil {
		t.Fatalf("code login: %v", err)
	}
	if resp.StatusCode != http.StatusOK || !strings.HasSuffix(resp.Request.URL.Path, "/admin/") {
		t.Fatalf("code login status = %d, path = %s, body = %s", resp.StatusCode, resp.Request.URL.Path, bodyOf(t, resp))
	}
	bodyOf(t, resp)
}

func TestBarkAddressRequiresVerificationForEnable(t *testing.T) {
	ts, st, client := newAdminServer(t)
	login(t, ts, client)

	resp, err := client.PostForm(ts.URL+"/admin/system/auth/method", url.Values{
		"method":  {"bark"},
		"enabled": {"1"},
	})
	if err != nil {
		t.Fatalf("enable unverified Bark: %v", err)
	}
	body := bodyOf(t, resp)
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(body, "请先验证 Bark 地址") {
		t.Fatalf("enable unverified Bark status=%d body=%s", resp.StatusCode, body)
	}
	settings, err := st.GetAdminSettings()
	if err != nil {
		t.Fatalf("GetAdminSettings after rejected Bark enable: %v", err)
	}
	if settings.BarkLoginEnabled {
		t.Fatalf("rejected Bark enable changed settings: %+v", settings)
	}
}

func TestLoginCodeJSONCooldown(t *testing.T) {
	var calls atomic.Int32
	bark := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer bark.Close()

	ts, st, client := newAdminServer(t)
	if _, err := st.UpdateAdminLoginSettings(true, true, bark.URL+"/device-key"); err != nil {
		t.Fatalf("enable Bark login: %v", err)
	}
	login(t, ts, client)

	send := func() (*http.Response, struct {
		Error      string `json:"error"`
		RetryAfter int    `json:"retry_after"`
	}) {
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/admin/login/code", nil)
		req.Header.Set("Accept", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("send login code: %v", err)
		}
		var payload struct {
			Error      string `json:"error"`
			RetryAfter int    `json:"retry_after"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
			resp.Body.Close()
			t.Fatalf("decode login code response: %v", err)
		}
		resp.Body.Close()
		return resp, payload
	}

	resp, payload := send()
	if resp.StatusCode != http.StatusOK || payload.RetryAfter <= 0 {
		t.Fatalf("first JSON login code status=%d payload=%+v", resp.StatusCode, payload)
	}
	resp, payload = send()
	if resp.StatusCode != http.StatusTooManyRequests || payload.RetryAfter <= 0 ||
		!strings.Contains(payload.Error, "验证码发送过于频繁") {
		t.Fatalf("cooldown JSON status=%d payload=%+v", resp.StatusCode, payload)
	}
	if calls.Load() != 1 {
		t.Fatalf("Bark calls = %d, want 1", calls.Load())
	}
}

func TestBarkSendFailureCooldown(t *testing.T) {
	var calls atomic.Int32
	bark := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	defer bark.Close()

	ts, _, client := newAdminServer(t)
	login(t, ts, client)
	barkAddress := bark.URL + "/device-key"
	resp, err := client.PostForm(ts.URL+"/admin/system/auth/bark/send", url.Values{
		"bark_address": {barkAddress},
	})
	if err != nil {
		t.Fatalf("first Bark setup send: %v", err)
	}
	body := bodyOf(t, resp)
	if !strings.Contains(body, "Bark 推送失败") {
		t.Fatalf("first Bark setup error missing: %s", body)
	}
	resp, err = client.PostForm(ts.URL+"/admin/system/auth/bark/send", url.Values{
		"bark_address": {barkAddress},
	})
	if err != nil {
		t.Fatalf("second Bark setup send: %v", err)
	}
	body = bodyOf(t, resp)
	if !strings.Contains(body, "验证码发送过于频繁") {
		t.Fatalf("cooldown error missing: %s", body)
	}
	if calls.Load() != 1 {
		t.Fatalf("Bark calls = %d, want 1", calls.Load())
	}
}

func TestAdminEndToEnd(t *testing.T) {
	ts, st, client := newAdminServer(t)
	login(t, ts, client)

	// Origin 不再参与管理后台请求鉴权，代理后的 Host 差异不应阻止 POST。
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/admin/projects", strings.NewReader("name=evil"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "https://evil.example.com")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("cross-origin post: %v", err)
	}
	body := bodyOf(t, resp)
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "evil") {
		t.Fatalf("cross-origin POST was not allowed: status=%d body=%s", resp.StatusCode, body)
	}

	resp, err = client.PostForm(ts.URL+"/admin/projects", url.Values{"name": {"demo"}})
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	body = bodyOf(t, resp)
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "demo") {
		t.Fatalf("create project status=%d body=%s", resp.StatusCode, body)
	}
	if !strings.Contains(body, "workspace-sidebar") || !strings.Contains(body, "table-picker") ||
		!strings.Contains(body, "new-table-dialog") || !strings.Contains(body, "/settings") {
		t.Fatalf("project workspace layout missing: %s", body)
	}
	token1 := tokenRe.FindString(body)
	if token1 == "" {
		t.Fatalf("token not found in body: %s", body)
	}
	if !strings.Contains(body, `id="token-result-dialog"`) || !strings.Contains(body, `data-auto-open="true"`) {
		t.Fatalf("token result dialog missing: %s", body)
	}
	if !strings.Contains(body, `data-copy="flash-token"`) || !strings.Contains(body, "copy-check-icon") {
		t.Fatalf("token result copy feedback missing: %s", body)
	}
	if _, _, err := st.VerifyProjectToken("demo", token1); err != nil {
		t.Fatalf("created token invalid: %v", err)
	}

	resp, err = client.PostForm(ts.URL+"/admin/projects", url.Values{"name": {"MixedCase"}})
	if err != nil {
		t.Fatalf("create mixed-case project: %v", err)
	}
	body = bodyOf(t, resp)
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "MixedCase") {
		t.Fatalf("mixed-case project page status=%d body=%s", resp.StatusCode, body)
	}
	resp, err = client.Get(ts.URL + "/admin/projects/mixedcase")
	if err != nil {
		t.Fatalf("get mixed-case project with different casing: %v", err)
	}
	body = bodyOf(t, resp)
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "MixedCase") {
		t.Fatalf("case-insensitive project route status=%d body=%s", resp.StatusCode, body)
	}

	// 创建 Table
	resp, err = client.PostForm(ts.URL+"/admin/projects/demo/tables", url.Values{"table": {"main"}})
	if err != nil {
		t.Fatalf("create table: %v", err)
	}
	body = bodyOf(t, resp)
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "main") {
		t.Fatalf("create table status=%d body=%s", resp.StatusCode, body)
	}
	if !strings.HasSuffix(resp.Request.URL.Path, "/tables/main") {
		t.Fatalf("project should open first table, got path %s", resp.Request.URL.Path)
	}

	resp, err = client.Get(ts.URL + "/admin/")
	if err != nil {
		t.Fatalf("get dashboard: %v", err)
	}
	body = bodyOf(t, resp)
	if !strings.Contains(body, "project-grid") || !strings.Contains(body, "project-card") ||
		!strings.Contains(body, "project-create-card") || !strings.Contains(body, "new-project-dialog") {
		t.Fatalf("dashboard project cards missing: %s", body)
	}
	if !strings.Contains(body, "访问量") || !strings.Contains(body, "访问压力（5 分钟）") ||
		strings.Contains(body, "创建时间") {
		t.Fatalf("project access stats missing: %s", body)
	}

	resp, err = client.Get(ts.URL + "/admin/projects/demo/tables/main")
	if err != nil {
		t.Fatalf("get table page: %v", err)
	}
	body = bodyOf(t, resp)
	if !strings.Contains(body, "data-panel") || !strings.Contains(body, "workspace-sidebar") {
		t.Fatalf("table workspace layout missing: %s", body)
	}
	if !strings.Contains(body, "table-action-row") {
		t.Fatalf("new key table row missing: %s", body)
	}
	if !strings.Contains(body, "inline-key-rename-form") || !strings.Contains(body, "inline-value-edit-form") ||
		!strings.Contains(body, "inline-content-type-edit-form") {
		t.Fatalf("inline cell editing forms missing: %s", body)
	}

	// URL 编码表单保存文本
	resp, err = client.PostForm(ts.URL+"/admin/projects/demo/tables/main/key?return_to=table&create=1", url.Values{
		"action":       {"save"},
		"key":          {"hello"},
		"content_type": {"application/json"},
		"value":        {`{"a":1}`},
	})
	if err != nil {
		t.Fatalf("save key: %v", err)
	}
	body = bodyOf(t, resp)
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "已保存") {
		t.Fatalf("save key status=%d body=%s", resp.StatusCode, body)
	}
	if !strings.HasSuffix(resp.Request.URL.Path, "/tables/main") {
		t.Fatalf("inline save should return to table, got path %s", resp.Request.URL.Path)
	}
	if !strings.Contains(body, "new-key-inline-form") || !strings.Contains(body, "new-key-editor-row") {
		t.Fatalf("inline key editor missing: %s", body)
	}
	if !strings.Contains(body, "inline-value-editor") || !strings.Contains(body, "inline-file-button") {
		t.Fatalf("inline value and upload controls are not grouped: %s", body)
	}
	if !strings.Contains(body, "data-cancel-new-key") {
		t.Fatalf("inline new key row is missing cancel action: %s", body)
	}
	if !strings.Contains(body, "create=1") {
		t.Fatalf("new key form is missing create-only mode: %s", body)
	}
	if !strings.Contains(body, `data-cell="key"`) || !strings.Contains(body, `data-cell="value"`) ||
		!strings.Contains(body, `data-cell="content-type"`) ||
		!strings.Contains(body, "data-edit-url") || !strings.Contains(body, "data-content-url") {
		t.Fatalf("inline cell editing data missing: %s", body)
	}
	if !strings.Contains(body, "row-delete-form") || !strings.Contains(body, `data-confirm="确定删除 Key hello？"`) ||
		!strings.Contains(body, "return_to=table&key=hello") {
		t.Fatalf("row delete action missing: %s", body)
	}
	if !strings.Contains(body, "data-toggle-selection") || !strings.Contains(body, "data-bulk-delete") ||
		!strings.Contains(body, "data-select-key") || !strings.Contains(body, `type="checkbox"`) {
		t.Fatalf("bulk selection controls missing: %s", body)
	}
	if !strings.Contains(body, "table-picker-delete-form") ||
		!strings.Contains(body, `data-confirm="确定删除 Table main？"`) {
		t.Fatalf("table picker delete action missing: %s", body)
	}
	if !strings.Contains(body, `id="confirm-dialog"`) || !strings.Contains(body, "data-confirm-accept") {
		t.Fatalf("custom confirmation dialog missing: %s", body)
	}
	resp, err = client.Get(ts.URL + "/admin/projects/demo/tables/main/key")
	if err != nil {
		t.Fatalf("get new key page: %v", err)
	}
	body = bodyOf(t, resp)
	if !strings.Contains(body, "?create=1") {
		t.Fatalf("new key page is missing create-only mode: %s", body)
	}

	resp, err = client.PostForm(ts.URL+"/admin/projects/demo/tables/main/key?return_to=table&create=1", url.Values{
		"action":       {"save"},
		"key":          {"hello"},
		"content_type": {"text/plain"},
		"value":        {"duplicate"},
	})
	if err != nil {
		t.Fatalf("duplicate create: %v", err)
	}
	body = bodyOf(t, resp)
	if !strings.Contains(body, "Key 已存在，请使用其他名称") {
		t.Fatalf("duplicate key error missing: %s", body)
	}
	duplicateForm := url.Values{
		"action":       {"save"},
		"key":          {"hello"},
		"content_type": {"text/plain"},
		"value":        {"duplicate-inline"},
	}
	req, _ = http.NewRequest(http.MethodPost,
		ts.URL+"/admin/projects/demo/tables/main/key?return_to=table&create=1",
		strings.NewReader(duplicateForm.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Inline-Edit", "1")
	resp, err = client.Do(req)
	if err != nil {
		t.Fatalf("inline duplicate create: %v", err)
	}
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("inline duplicate status = %d, body = %s", resp.StatusCode, bodyOf(t, resp))
	}
	var inlineError struct {
		Error string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&inlineError); err != nil {
		resp.Body.Close()
		t.Fatalf("decode inline duplicate error: %v", err)
	}
	resp.Body.Close()
	if inlineError.Error != "Key 已存在，请使用其他名称" {
		t.Fatalf("inline duplicate error = %q", inlineError.Error)
	}
	resp, err = client.PostForm(ts.URL+"/admin/projects/demo/tables/main/key?return_to=table&create=1", url.Values{
		"action":       {"save"},
		"key":          {"occupied"},
		"content_type": {"text/plain"},
		"value":        {"occupied-value"},
	})
	if err != nil {
		t.Fatalf("create occupied key: %v", err)
	}
	bodyOf(t, resp)
	req, _ = http.NewRequest(http.MethodPost,
		ts.URL+"/admin/projects/demo/tables/main/key/rename",
		strings.NewReader(url.Values{"old_key": {"hello"}, "new_key": {"occupied"}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Inline-Edit", "1")
	resp, err = client.Do(req)
	if err != nil {
		t.Fatalf("inline duplicate rename: %v", err)
	}
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("inline duplicate rename status = %d, body = %s", resp.StatusCode, bodyOf(t, resp))
	}
	if err := json.NewDecoder(resp.Body).Decode(&inlineError); err != nil {
		resp.Body.Close()
		t.Fatalf("decode inline duplicate rename error: %v", err)
	}
	resp.Body.Close()
	if inlineError.Error != "Key 已存在，请使用其他名称" {
		t.Fatalf("inline duplicate rename error = %q", inlineError.Error)
	}
	req, _ = http.NewRequest(http.MethodPost,
		ts.URL+"/admin/projects/demo/tables/main/key/rename",
		strings.NewReader(url.Values{"old_key": {"missing"}, "new_key": {"renamed"}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Inline-Edit", "1")
	resp, err = client.Do(req)
	if err != nil {
		t.Fatalf("inline missing rename: %v", err)
	}
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("inline missing rename status = %d, body = %s", resp.StatusCode, bodyOf(t, resp))
	}
	if err := json.NewDecoder(resp.Body).Decode(&inlineError); err != nil {
		resp.Body.Close()
		t.Fatalf("decode inline missing rename error: %v", err)
	}
	resp.Body.Close()
	if inlineError.Error != "Key 不存在" {
		t.Fatalf("inline missing rename error = %q", inlineError.Error)
	}
	resp, err = client.PostForm(ts.URL+"/admin/projects/demo/tables/main/key/rename",
		url.Values{"old_key": {"missing"}, "new_key": {"renamed"}})
	if err != nil {
		t.Fatalf("missing rename form: %v", err)
	}
	body = bodyOf(t, resp)
	if !strings.Contains(body, "Key 不存在") {
		t.Fatalf("missing rename flash = %s", body)
	}

	resp, err = client.Get(ts.URL + "/admin/projects/demo/tables/main/key/content?key=hello")
	if err != nil {
		t.Fatalf("get inline key content: %v", err)
	}
	var inlineContent struct {
		Editable    bool   `json:"editable"`
		ContentType string `json:"contentType"`
		Value       string `json:"value"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&inlineContent); err != nil {
		resp.Body.Close()
		t.Fatalf("decode inline key content: %v", err)
	}
	resp.Body.Close()
	if !inlineContent.Editable || inlineContent.ContentType != "application/json" || inlineContent.Value != `{"a":1}` {
		t.Fatalf("inline content = %+v", inlineContent)
	}
	resp, err = client.PostForm(ts.URL+"/admin/projects/demo/tables/main/key/content-type", url.Values{
		"key":          {"hello"},
		"content_type": {"text/plain; charset=utf-8"},
	})
	if err != nil {
		t.Fatalf("update content type: %v", err)
	}
	body = bodyOf(t, resp)
	if !strings.Contains(body, "Content-Type 已更新") {
		t.Fatalf("content type update flash missing: %s", body)
	}
	if !strings.Contains(body, "flash-stack") {
		t.Fatalf("content type update flash is not rendered as a floating stack: %s", body)
	}

	resp, err = client.PostForm(ts.URL+"/admin/projects/demo/tables/main/key/rename", url.Values{
		"old_key": {"hello"},
		"new_key": {"hello-renamed"},
	})
	if err != nil {
		t.Fatalf("rename key: %v", err)
	}
	body = bodyOf(t, resp)
	if !strings.Contains(body, "Key 已重命名") || !strings.Contains(body, "hello-renamed") {
		t.Fatalf("rename key response body=%s", body)
	}
	resp, err = client.PostForm(ts.URL+"/admin/projects/demo/tables/main/key/rename", url.Values{
		"old_key": {"hello-renamed"},
		"new_key": {"hello"},
	})
	if err != nil {
		t.Fatalf("rename key back: %v", err)
	}
	bodyOf(t, resp)

	p, err := st.GetProject("demo")
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	table, err := st.GetTable(p, "main")
	if err != nil {
		t.Fatalf("GetTable: %v", err)
	}
	meta, rc, err := st.GetKey(p, table.Name, "hello")
	if err != nil {
		t.Fatalf("GetKey hello: %v", err)
	}
	if meta.ContentType != "text/plain; charset=utf-8" {
		t.Fatalf("content type = %q", meta.ContentType)
	}
	if got := readAll(t, rc); got != `{"a":1}` {
		t.Fatalf("value = %q", got)
	}

	resp, err = client.Get(ts.URL + "/admin/projects/demo/tables/main")
	if err != nil {
		t.Fatalf("get table data page: %v", err)
	}
	body = bodyOf(t, resp)
	if !strings.Contains(body, "value-preview") || !strings.Contains(body, "hello") {
		t.Fatalf("table value preview missing: %s", body)
	}
	for _, removed := range []string{"Content Hash", "Server Seq", "Created At", "Revision", "Storage", "SQLite"} {
		if strings.Contains(body, removed) {
			t.Fatalf("table page still contains removed column %q: %s", removed, body)
		}
	}
	if !strings.Contains(body, "col-content-type") || !strings.Contains(body, "col-updated-at") ||
		!strings.Contains(body, "updated-at-cell") {
		t.Fatalf("fixed table column classes missing: %s", body)
	}

	resp, err = client.Get(ts.URL + "/admin/projects/demo/settings?table=main")
	if err != nil {
		t.Fatalf("get project settings page: %v", err)
	}
	body = bodyOf(t, resp)
	if !strings.Contains(body, "settings-panel") || strings.Contains(body, "删除当前 Table") ||
		!strings.Contains(body, "删除项目") {
		t.Fatalf("settings page layout missing: %s", body)
	}

	// multipart 文件上传（二进制）
	binary := []byte{0x00, 0x01, 0xff, 'A', 'B'}
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("action", "save")
	_ = mw.WriteField("key", "bin")
	_ = mw.WriteField("content_type", "application/octet-stream")
	fw, err := mw.CreateFormFile("file", "data.bin")
	if err != nil {
		t.Fatalf("CreateFormFile: %v", err)
	}
	if _, err := fw.Write(binary); err != nil {
		t.Fatalf("write file: %v", err)
	}
	_ = mw.Close()

	req, _ = http.NewRequest(http.MethodPost, ts.URL+"/admin/projects/demo/tables/main/key?key=bin", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err = client.Do(req)
	if err != nil {
		t.Fatalf("upload binary: %v", err)
	}
	body = bodyOf(t, resp)
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "已保存") {
		t.Fatalf("upload binary status=%d body=%s", resp.StatusCode, body)
	}
	_, rc, err = st.GetKey(p, table.Name, "bin")
	if err != nil {
		t.Fatalf("GetKey bin: %v", err)
	}
	if got := readAllBytes(t, rc); !bytes.Equal(got, binary) {
		t.Fatalf("binary = %v, want %v", got, binary)
	}
	resp, err = client.Get(ts.URL + "/admin/projects/demo/tables/main/key/content?key=bin")
	if err != nil {
		t.Fatalf("get binary inline metadata: %v", err)
	}
	var binaryInline struct {
		Editable bool   `json:"editable"`
		FileName string `json:"fileName"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&binaryInline); err != nil {
		resp.Body.Close()
		t.Fatalf("decode binary inline metadata: %v", err)
	}
	resp.Body.Close()
	if binaryInline.Editable || binaryInline.FileName != "bin" {
		t.Fatalf("binary inline metadata = %+v", binaryInline)
	}

	// 二进制 Key 页面不预览，但可下载
	resp, err = client.Get(ts.URL + "/admin/projects/demo/tables/main/key?key=bin")
	if err != nil {
		t.Fatalf("get bin page: %v", err)
	}
	body = bodyOf(t, resp)
	if !strings.Contains(body, "不预览") {
		t.Fatalf("binary page should warn: %s", body)
	}
	if !strings.Contains(body, "key-editor") {
		t.Fatalf("key editor layout missing: %s", body)
	}
	resp, err = client.Get(ts.URL + "/admin/projects/demo/tables/main/download?key=bin")
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("download status = %d", resp.StatusCode)
	}
	if got := readAllBytes(t, resp.Body); !bytes.Equal(got, binary) {
		t.Fatalf("download = %v, want %v", got, binary)
	}
	if cd := resp.Header.Get("Content-Disposition"); !strings.Contains(cd, "attachment") {
		t.Fatalf("Content-Disposition = %q", cd)
	}

	// 缺少 key 时返回 400，而不是 500。
	resp, err = client.Get(ts.URL + "/admin/projects/demo/tables/main/download")
	if err != nil {
		t.Fatalf("download without key: %v", err)
	}
	bodyOf(t, resp)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("download without key status = %d", resp.StatusCode)
	}

	// 新建和删除访问 Token
	resp, err = client.PostForm(ts.URL+"/admin/projects/demo/tokens", url.Values{"token_name": {"second"}})
	if err != nil {
		t.Fatalf("create token: %v", err)
	}
	body = bodyOf(t, resp)
	token2 := tokenRe.FindString(body)
	if token2 == "" || token2 == token1 {
		t.Fatalf("second token = %q (first %q)", token2, token1)
	}
	if _, _, err := st.VerifyProjectToken("demo", token1); err != nil {
		t.Fatalf("first token after add: %v", err)
	}
	if _, _, err := st.VerifyProjectToken("demo", token2); err != nil {
		t.Fatalf("second token: %v", err)
	}
	tokens, err := st.ListProjectTokens("demo")
	if err != nil {
		t.Fatalf("ListProjectTokens: %v", err)
	}
	if len(tokens) != 2 || tokens[0].Name != "默认 Token" || tokens[1].Name != "second" {
		t.Fatalf("project tokens = %+v", tokens)
	}
	if err := st.RecordTokenAccess(p.ID, tokens[0].ID, true); err != nil {
		t.Fatalf("record token access: %v", err)
	}
	if err := st.RecordTokenWrite(tokens[0].ID); err != nil {
		t.Fatalf("record token write: %v", err)
	}
	resp, err = client.Get(ts.URL + "/admin/projects/demo/settings?table=main")
	if err != nil {
		t.Fatalf("get token settings: %v", err)
	}
	body = bodyOf(t, resp)
	if !strings.Contains(body, "second") || !strings.Contains(body, "访问量") ||
		!strings.Contains(body, "修改量") {
		t.Fatalf("token list or stats missing: %s", body)
	}
	if !strings.Contains(body, "Project API 地址") ||
		!strings.Contains(body, `id="project-api-url"`) ||
		!strings.Contains(body, `data-api-path="/v1/demo"`) ||
		!strings.Contains(body, `data-copy="project-api-url"`) {
		t.Fatalf("project API address control missing: %s", body)
	}
	if !strings.Contains(body, `id="new-token-dialog"`) || !strings.Contains(body, "data-token-toggle") ||
		!strings.Contains(body, "data-token-copy") || !strings.Contains(body, strings.Repeat("*", len(token2))) ||
		!strings.Contains(body, `aria-label="显示 Token"`) || !strings.Contains(body, `aria-label="复制 Token"`) ||
		!strings.Contains(body, "copy-check-icon") {
		t.Fatalf("token creation/reveal controls missing: %s", body)
	}
	resp, err = client.PostForm(ts.URL+fmt.Sprintf("/admin/projects/demo/tokens/%d/delete", tokens[1].ID),
		url.Values{"table": {"main"}})
	if err != nil {
		t.Fatalf("delete token: %v", err)
	}
	bodyOf(t, resp)
	if _, _, err := st.VerifyProjectToken("demo", token2); !errors.Is(err, store.ErrUnauthorized) {
		t.Fatalf("deleted token err = %v, want ErrUnauthorized", err)
	}
	if _, _, err := st.VerifyProjectToken("demo", token1); err != nil {
		t.Fatalf("remaining token: %v", err)
	}

	// 删除 Key
	resp, err = client.PostForm(ts.URL+"/admin/projects/demo/tables/main/key?return_to=table&key=hello", url.Values{"action": {"delete"}})
	if err != nil {
		t.Fatalf("delete key: %v", err)
	}
	if !strings.HasSuffix(resp.Request.URL.Path, "/tables/main") {
		t.Fatalf("delete key should return to table, got path %s", resp.Request.URL.Path)
	}
	bodyOf(t, resp)
	if _, _, err := st.GetKey(p, table.Name, "hello"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("hello err = %v, want ErrNotFound", err)
	}
	resp, err = client.Get(ts.URL + "/admin/projects/demo/tables/main/key?key=hello")
	if err != nil {
		t.Fatalf("get deleted key page: %v", err)
	}
	if !strings.HasSuffix(resp.Request.URL.Path, "/tables/main") {
		t.Fatalf("deleted key page should return to table, got path %s", resp.Request.URL.Path)
	}
	body = bodyOf(t, resp)
	if !strings.Contains(body, "Key 不存在") || strings.Contains(body, `class="data-panel key-editor"`) {
		t.Fatalf("deleted key page should not render new key form: %s", body)
	}

	// 删除 Table
	resp, err = client.PostForm(ts.URL+"/admin/projects/demo/tables/main/delete", url.Values{"confirm": {"main"}})
	if err != nil {
		t.Fatalf("delete table: %v", err)
	}
	body = bodyOf(t, resp)
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "已删除") {
		t.Fatalf("delete table status=%d body=%s", resp.StatusCode, body)
	}
	if _, err := st.GetTable(p, "main"); !errors.Is(err, store.ErrTableNotFound) {
		t.Fatalf("table err = %v, want ErrTableNotFound", err)
	}

	// 删除项目
	resp, err = client.PostForm(ts.URL+"/admin/projects/demo/delete", url.Values{"confirm": {"demo"}})
	if err != nil {
		t.Fatalf("delete project: %v", err)
	}
	body = bodyOf(t, resp)
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "已删除") {
		t.Fatalf("delete project status=%d body=%s", resp.StatusCode, body)
	}
	if _, err := st.GetProject("demo"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("project err = %v, want ErrNotFound", err)
	}
}

func readAll(t *testing.T, rc store.ReadSeekCloser) string {
	t.Helper()
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return string(b)
}

func readAllBytes(t *testing.T, r io.Reader) []byte {
	t.Helper()
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return b
}
