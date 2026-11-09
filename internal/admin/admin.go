package admin

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"log/slog"
	"math"
	"math/big"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"akkvstore/internal/auth"
	"akkvstore/internal/store"
)

//go:embed templates/*.html static/*
var content embed.FS

const (
	sessionCookie = "akkv_admin"
	flashCookie   = "akkv_flash"
	pageSize      = 50
	previewLimit  = 1 << 20
	tablePreview  = 256
)

// Admin 提供中文管理后台。
type Admin struct {
	store           *store.Store
	signer          *auth.CookieSigner
	ttl             time.Duration
	cookieSecure    bool
	log             *slog.Logger
	tmpl            *template.Template
	static          fs.FS
	barkClient      *http.Client
	verificationKey []byte

	loginCodeMu       sync.Mutex
	loginCodeDigest   []byte
	loginCodeExpires  int64
	loginCodeAttempts int
	loginCodeSentAt   int64

	barkSetupCodeMu        sync.Mutex
	barkSetupCodeDigest    []byte
	barkSetupAddressDigest []byte
	barkSetupCodeExpires   int64
	barkSetupCodeAttempts  int
	barkSetupCodeSentAt    int64
}

// New 构造管理后台。
func New(st *store.Store, secret []byte, ttl time.Duration, cookieSecure bool, log *slog.Logger) (*Admin, error) {
	if err := st.EnsureAdminSettings(); err != nil {
		return nil, fmt.Errorf("ensure admin settings: %w", err)
	}
	settings, err := st.GetAdminSettings()
	if err != nil {
		return nil, fmt.Errorf("read admin settings: %w", err)
	}
	if settings.PasswordHash == "" {
		log.Warn("管理员密码尚未初始化；首次访问登录页可设置密码，完成前请勿将服务暴露到不可信网络")
	}
	funcMap := template.FuncMap{
		"formatTime": func(ts int64) string {
			if ts == 0 {
				return "-"
			}
			// 项目元数据仍使用秒；记录数据使用毫秒。
			if ts < 1_000_000_000_000 {
				return time.Unix(ts, 0).Format("2006-01-02 15:04:05")
			}
			return time.UnixMilli(ts).Format("2006-01-02 15:04:05")
		},
		"humanSize":      humanSize,
		"compactCount":   compactCount,
		"displayKey":     displayKey,
		"valueTypeLabel": valueTypeLabel,
		"tokenMask": func(token string) string {
			if token == "" {
				return ""
			}
			return strings.Repeat("*", utf8.RuneCountInString(token))
		},
		"path": url.PathEscape,
		"add":  func(a, b int) int { return a + b },
		"sub":  func(a, b int) int { return a - b },
	}
	tmpl, err := template.New("").Funcs(funcMap).ParseFS(content, "templates/*.html")
	if err != nil {
		return nil, fmt.Errorf("parse templates: %w", err)
	}
	staticFS, err := fs.Sub(content, "static")
	if err != nil {
		return nil, fmt.Errorf("static fs: %w", err)
	}
	verificationKey := sha256.Sum256(secret)
	return &Admin{
		store:           st,
		signer:          auth.NewCookieSigner(secret),
		ttl:             ttl,
		cookieSecure:    cookieSecure,
		log:             log,
		tmpl:            tmpl,
		static:          staticFS,
		barkClient:      &http.Client{Timeout: 10 * time.Second},
		verificationKey: verificationKey[:],
	}, nil
}

// Handler 返回挂载在 /admin 前缀下的处理器（由调用方 StripPrefix）。
func (a *Admin) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.FS(a.static))))

	mux.HandleFunc("GET /login", a.getLogin)
	mux.HandleFunc("POST /login", a.postLogin)
	mux.HandleFunc("POST /login/setup", a.postInitialAdminPassword)
	mux.HandleFunc("POST /login/code", a.postSendLoginCode)
	mux.HandleFunc("POST /logout", a.requireAuth(a.postLogout))

	mux.HandleFunc("GET /{$}", a.requireAuth(a.getProjects))
	mux.HandleFunc("GET /system", a.requireAuth(a.getSystemSettings))
	mux.HandleFunc("GET /system/password", a.requireAuth(a.redirectToSystemAuth))
	mux.HandleFunc("GET /system/login", a.requireAuth(a.redirectToSystemAuth))
	mux.HandleFunc("POST /system/password", a.requireAuth(a.postUpdateAdminPassword))
	mux.HandleFunc("POST /system/auth/method", a.requireAuth(a.postToggleAuthMethod))
	mux.HandleFunc("POST /system/auth/bark/send", a.requireAuth(a.postSendBarkSetupCode))
	mux.HandleFunc("POST /system/auth/bark/verify", a.requireAuth(a.postVerifyBarkSetup))
	mux.HandleFunc("POST /projects", a.requireAuth(a.postCreateProject))
	mux.HandleFunc("GET /projects/{name}", a.requireAuth(a.getProject))
	mux.HandleFunc("POST /projects/{name}/delete", a.requireAuth(a.postDeleteProject))
	mux.HandleFunc("POST /projects/{name}/tokens", a.requireAuth(a.postCreateProjectToken))
	mux.HandleFunc("POST /projects/{name}/tokens/{id}/delete", a.requireAuth(a.postDeleteProjectToken))
	mux.HandleFunc("POST /projects/{name}/tables", a.requireAuth(a.postCreateTable))
	mux.HandleFunc("POST /projects/{name}/tables/{table}/delete", a.requireAuth(a.postDeleteTable))
	mux.HandleFunc("GET /projects/{name}/tables/{table}", a.requireAuth(a.getTable))
	mux.HandleFunc("GET /projects/{name}/tables/{table}/key/content", a.requireAuth(a.getKeyContent))
	mux.HandleFunc("GET /projects/{name}/tables/{table}/key", a.requireAuth(a.getKey))
	mux.HandleFunc("POST /projects/{name}/tables/{table}/key", a.requireAuth(a.postPutKey))
	mux.HandleFunc("POST /projects/{name}/tables/{table}/key/rename", a.requireAuth(a.postRenameKey))
	mux.HandleFunc("POST /projects/{name}/tables/{table}/keys/delete", a.requireAuth(a.postBulkDeleteKeys))
	mux.HandleFunc("GET /projects/{name}/tables/{table}/download", a.requireAuth(a.getDownload))
	mux.HandleFunc("GET /projects/{name}/settings", a.requireAuth(a.getProjectSettings))

	return a.securityHeaders(mux)
}

// ---------- 会话与基础渲染 ----------

type pageBase struct {
	Title string
	Flash auth.Flash
	Now   time.Time
}

type loginData struct {
	pageBase
	Error                string
	PasswordLoginEnabled bool
	BarkLoginEnabled     bool
	NeedsSetup           bool
	ActiveMethod         string
}

type systemSettingsData struct {
	pageBase
	Settings             store.AdminSettings
	PasswordLoginEnabled bool
	BarkLoginEnabled     bool
	BarkAddress          string
	Error                string
}

type projectsData struct {
	pageBase
	Projects []store.ProjectSummary
	Error    string
}

type projectData struct {
	pageBase
	Project     store.Project
	Tables      []store.TableSummary
	ActiveTable string
	Error       string
}

type tableData struct {
	pageBase
	Project     store.Project
	Tables      []store.TableSummary
	ActiveTable string
	Table       store.Table
	Keys        []keyRow
	Prefix      string
	Page        int
	Pages       int
	Total       int64
	Error       string
}

type keyRow struct {
	store.KeyMeta
	ValuePreview string
}

type projectSettingsData struct {
	pageBase
	Project     store.Project
	Tables      []store.TableSummary
	Tokens      []store.ProjectToken
	ActiveTable string
	APIBasePath string
	Error       string
}

type keyData struct {
	pageBase
	Project     store.Project
	Tables      []store.TableSummary
	ActiveTable string
	Table       store.Table
	Key         string
	ContentType store.ValueType
	Exists      bool
	Size        int64
	UpdatedAt   int64
	Value       string
	Binary      bool
	Previewable bool
	Error       string
}

type errorData struct {
	pageBase
	Message string
}

func (a *Admin) base(title string, _ auth.Session) pageBase {
	return pageBase{Title: title, Now: time.Now()}
}

func (a *Admin) currentSession(r *http.Request) (auth.Session, bool) {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return auth.Session{}, false
	}
	var s auth.Session
	if err := a.signer.Unsign(c.Value, &s); err != nil || !s.Valid() {
		return auth.Session{}, false
	}
	settings, err := a.store.GetAdminSettings()
	if err != nil || s.Epoch != settings.AuthEpoch {
		if err != nil {
			a.log.Error("validate admin session epoch", "err", err)
		}
		return auth.Session{}, false
	}
	if settings.PasswordHash == "" {
		return auth.Session{}, false
	}
	return s, true
}

func (a *Admin) setSession(w http.ResponseWriter, s auth.Session) {
	v, err := a.signer.Sign(s)
	if err != nil {
		a.log.Error("sign session", "err", err)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    v,
		Path:     "/admin",
		HttpOnly: true,
		Secure:   a.cookieSecure,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   int(a.ttl.Seconds()),
		Expires:  time.Now().Add(a.ttl),
	})
}

func (a *Admin) clearSession(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    "",
		Path:     "/admin",
		HttpOnly: true,
		Secure:   a.cookieSecure,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   -1,
		Expires:  time.Unix(0, 0),
	})
}

func (a *Admin) setFlash(w http.ResponseWriter, f auth.Flash) {
	if f.ExpiresAt == 0 {
		f.ExpiresAt = time.Now().Add(10 * time.Minute).Unix()
	}
	v, err := a.signer.Sign(f)
	if err != nil {
		a.log.Error("sign flash", "err", err)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     flashCookie,
		Value:    v,
		Path:     "/admin",
		HttpOnly: true,
		Secure:   a.cookieSecure,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   600,
		Expires:  time.Now().Add(10 * time.Minute),
	})
}

func (a *Admin) takeFlash(w http.ResponseWriter, r *http.Request) auth.Flash {
	c, err := r.Cookie(flashCookie)
	if err != nil {
		return auth.Flash{}
	}
	var f auth.Flash
	if err := a.signer.Unsign(c.Value, &f); err != nil || !f.Valid() {
		return auth.Flash{}
	}
	http.SetCookie(w, &http.Cookie{
		Name:     flashCookie,
		Value:    "",
		Path:     "/admin",
		HttpOnly: true,
		Secure:   a.cookieSecure,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   -1,
		Expires:  time.Unix(0, 0),
	})
	return f
}

func (a *Admin) requireAuth(next func(http.ResponseWriter, *http.Request, auth.Session)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess, ok := a.currentSession(r)
		if !ok {
			if strings.HasPrefix(r.URL.Path, "/projects") || r.URL.Path == "/" {
				a.setFlash(w, auth.Flash{ExpiresAt: time.Now().Add(10 * time.Minute).Unix(), Error: "请先登录"})
			}
			http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
			return
		}
		next(w, r, sess)
	}
}

func (a *Admin) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "same-origin")
		w.Header().Set("Content-Security-Policy",
			"default-src 'self'; style-src 'self'; script-src 'self'; img-src 'self' data:; form-action 'self'; frame-ancestors 'none'; base-uri 'none'")
		next.ServeHTTP(w, r)
	})
}

func (a *Admin) render(w http.ResponseWriter, status int, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if err := a.tmpl.ExecuteTemplate(w, name, data); err != nil {
		a.log.Error("render template", "template", name, "err", err)
	}
}

func (a *Admin) renderError(w http.ResponseWriter, status int, sess auth.Session, msg string) {
	data := errorData{
		pageBase: a.base("出错了", sess),
		Message:  msg,
	}
	a.render(w, status, "error", data)
}

// ---------- 登录 ----------

func (a *Admin) getLogin(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.currentSession(r); ok {
		http.Redirect(w, r, "/admin/", http.StatusSeeOther)
		return
	}
	settings, err := a.store.GetAdminSettings()
	if err != nil {
		a.log.Error("read admin settings", "err", err)
		a.renderError(w, http.StatusInternalServerError, auth.Session{}, "读取登录配置失败")
		return
	}
	data := loginData{
		pageBase:             a.base("登录", auth.Session{}),
		PasswordLoginEnabled: settings.PasswordLoginEnabled,
		BarkLoginEnabled:     settings.BarkLoginEnabled,
		NeedsSetup:           settings.PasswordHash == "",
		ActiveMethod:         activeLoginMethod(r.URL.Query().Get("method"), settings),
	}
	data.Flash = a.takeFlash(w, r)
	a.render(w, http.StatusOK, "login", data)
}

func (a *Admin) postInitialAdminPassword(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "请求格式错误", http.StatusBadRequest)
		return
	}
	settings, err := a.store.GetAdminSettings()
	if err != nil {
		a.log.Error("read admin settings", "err", err)
		a.renderError(w, http.StatusInternalServerError, auth.Session{}, "读取登录配置失败")
		return
	}
	if settings.PasswordHash != "" {
		a.setFlash(w, auth.Flash{
			ExpiresAt: time.Now().Add(10 * time.Minute).Unix(),
			Error:     "管理员初始密码已经设置",
		})
		http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
		return
	}
	password := r.FormValue("new_password")
	confirm := r.FormValue("confirm_password")
	message := ""
	switch {
	case len(password) < 8:
		message = "管理员密码至少需要 8 个字符"
	case len(password) > 72:
		message = "管理员密码不能超过 72 字节"
	case store.IsLegacyDefaultAdminPassword(password):
		message = "不能使用旧版默认占位密码，请设置新的管理员密码"
	case password != confirm:
		message = "两次输入的密码不一致"
	}
	if message != "" {
		data := loginData{
			pageBase:   a.base("登录", auth.Session{}),
			Error:      message,
			NeedsSetup: true,
		}
		a.render(w, http.StatusBadRequest, "login", data)
		return
	}
	hash, err := auth.HashPassword(password)
	if err != nil {
		a.log.Error("hash initial admin password", "err", err)
		data := loginData{
			pageBase:   a.base("登录", auth.Session{}),
			Error:      "设置初始密码失败",
			NeedsSetup: true,
		}
		a.render(w, http.StatusInternalServerError, "login", data)
		return
	}
	epoch, err := a.store.InitializeAdminPassword(hash)
	if err != nil {
		a.log.Error("initialize admin password", "err", err)
		data := loginData{
			pageBase:   a.base("登录", auth.Session{}),
			Error:      "设置初始密码失败",
			NeedsSetup: true,
		}
		a.render(w, http.StatusConflict, "login", data)
		return
	}
	a.resetLoginCode()
	a.setSession(w, auth.NewSessionWithEpoch(a.ttl, epoch))
	a.setFlash(w, auth.Flash{
		ExpiresAt: time.Now().Add(10 * time.Minute).Unix(),
		Message:   "初始管理员密码已设置",
	})
	http.Redirect(w, r, "/admin/", http.StatusSeeOther)
}

func (a *Admin) postLogin(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "请求格式错误", http.StatusBadRequest)
		return
	}
	settings, err := a.store.GetAdminSettings()
	if err != nil {
		a.log.Error("read admin settings", "err", err)
		a.renderError(w, http.StatusInternalServerError, auth.Session{}, "读取登录配置失败")
		return
	}
	if settings.PasswordHash == "" {
		a.setFlash(w, auth.Flash{
			ExpiresAt: time.Now().Add(10 * time.Minute).Unix(),
			Error:     "请先设置初始管理员密码",
		})
		http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
		return
	}
	method := strings.TrimSpace(r.FormValue("login_method"))
	if method == "" {
		if strings.TrimSpace(r.FormValue("code")) != "" {
			method = "bark"
		} else {
			method = "password"
		}
	}
	switch method {
	case "password":
		if !settings.PasswordLoginEnabled {
			a.renderLoginError(w, http.StatusForbidden, settings, method, "管理员密码登录未启用")
			return
		}
		password := r.FormValue("password")
		if password == "" || !auth.CheckPassword(settings.PasswordHash, password) {
			time.Sleep(300 * time.Millisecond)
			a.renderLoginError(w, http.StatusUnauthorized, settings, method, "密码不正确")
			return
		}
	case "bark":
		if !settings.BarkConfigured() {
			a.renderLoginError(w, http.StatusForbidden, settings, method, "Bark 验证码登录未启用")
			return
		}
		code := strings.TrimSpace(r.FormValue("code"))
		if err := a.verifyLoginCode(code); err != nil {
			time.Sleep(300 * time.Millisecond)
			a.renderLoginError(w, http.StatusUnauthorized, settings, method, err.Error())
			return
		}
	default:
		a.renderLoginError(w, http.StatusBadRequest, settings, method, "请选择可用的登录方式")
		return
	}
	a.resetLoginCode()
	sess := auth.NewSessionWithEpoch(a.ttl, settings.AuthEpoch)
	a.setSession(w, sess)
	a.setFlash(w, auth.Flash{
		ExpiresAt: time.Now().Add(10 * time.Minute).Unix(),
		Message:   "登录成功",
	})
	http.Redirect(w, r, "/admin/", http.StatusSeeOther)
}

func (a *Admin) renderLoginError(
	w http.ResponseWriter,
	status int,
	settings store.AdminSettings,
	method string,
	message string,
) {
	data := loginData{
		pageBase:             a.base("登录", auth.Session{}),
		Error:                message,
		PasswordLoginEnabled: settings.PasswordLoginEnabled,
		BarkLoginEnabled:     settings.BarkLoginEnabled,
		ActiveMethod:         activeLoginMethod(method, settings),
	}
	a.render(w, status, "login", data)
}

func activeLoginMethod(preferred string, settings store.AdminSettings) string {
	if preferred == "password" && settings.PasswordLoginEnabled {
		return "password"
	}
	if preferred == "bark" && settings.BarkConfigured() {
		return "bark"
	}
	if settings.PasswordLoginEnabled {
		return "password"
	}
	return "bark"
}

const (
	loginCodeTTL         = 5 * time.Minute
	loginCodeCooldown    = time.Minute
	maxLoginCodeAttempts = 5
)

var errLoginCodeCooldown = errors.New("验证码发送过于频繁，请稍后再试")

func (a *Admin) postSendLoginCode(w http.ResponseWriter, r *http.Request) {
	jsonRequested := strings.Contains(strings.ToLower(r.Header.Get("Accept")), "application/json")
	settings, err := a.store.GetAdminSettings()
	if err != nil {
		a.log.Error("read admin settings", "err", err)
		if jsonRequested {
			writeAdminJSON(w, http.StatusInternalServerError, map[string]string{"error": "读取登录配置失败"})
			return
		}
		http.Error(w, "读取登录配置失败", http.StatusInternalServerError)
		return
	}
	if settings.PasswordHash == "" {
		if jsonRequested {
			writeAdminJSON(w, http.StatusConflict, map[string]string{"error": "请先设置初始管理员密码"})
			return
		}
		a.setFlash(w, auth.Flash{
			ExpiresAt: time.Now().Add(10 * time.Minute).Unix(),
			Error:     "请先设置初始管理员密码",
		})
		http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
		return
	}
	if !settings.BarkConfigured() {
		if jsonRequested {
			writeAdminJSON(w, http.StatusBadRequest, map[string]string{"error": "Bark 验证码登录尚未配置完成"})
			return
		}
		a.setFlash(w, auth.Flash{
			ExpiresAt: time.Now().Add(10 * time.Minute).Unix(),
			Error:     "Bark 验证码登录尚未配置完成",
		})
		http.Redirect(w, r, "/admin/login?method=bark", http.StatusSeeOther)
		return
	}
	retryAfter, err := a.issueAndSendLoginCode(settings)
	if err != nil {
		a.log.Error("send Bark login code", "err", err)
		if jsonRequested {
			status := http.StatusBadGateway
			if errors.Is(err, errLoginCodeCooldown) {
				status = http.StatusTooManyRequests
			}
			writeAdminJSON(w, status, map[string]any{
				"error":       err.Error(),
				"retry_after": retryAfter,
			})
			return
		}
		a.setFlash(w, auth.Flash{
			ExpiresAt: time.Now().Add(10 * time.Minute).Unix(),
			Error:     err.Error(),
		})
		http.Redirect(w, r, "/admin/login?method=bark", http.StatusSeeOther)
		return
	}
	if jsonRequested {
		writeAdminJSON(w, http.StatusOK, map[string]any{
			"ok":          true,
			"retry_after": retryAfter,
		})
		return
	}
	a.setFlash(w, auth.Flash{
		ExpiresAt: time.Now().Add(10 * time.Minute).Unix(),
		Message:   "4 位登录验证码已通过 Bark 发送，请在 5 分钟内输入",
	})
	http.Redirect(w, r, "/admin/login?method=bark", http.StatusSeeOther)
}

func (a *Admin) issueAndSendLoginCode(settings store.AdminSettings) (int, error) {
	a.loginCodeMu.Lock()
	defer a.loginCodeMu.Unlock()
	now := time.Now()
	if a.loginCodeSentAt > 0 && now.Unix()-a.loginCodeSentAt < int64(loginCodeCooldown/time.Second) {
		retryAfter := int64(loginCodeCooldown/time.Second) - (now.Unix() - a.loginCodeSentAt)
		return int(retryAfter), errLoginCodeCooldown
	}
	limit := big.NewInt(10000)
	number, err := rand.Int(rand.Reader, limit)
	if err != nil {
		return 0, fmt.Errorf("生成验证码失败")
	}
	code := fmt.Sprintf("%04d", number.Int64())
	a.clearLoginCodeLocked()
	a.loginCodeSentAt = now.Unix()
	if err := a.sendBarkCode(
		settings.BarkAddress,
		"AKKV Store 登录",
		"验证码："+code+"，5 分钟内有效",
	); err != nil {
		return int(loginCodeCooldown / time.Second), fmt.Errorf("Bark 推送失败: %w", err)
	}
	mac := hmac.New(sha256.New, a.verificationKey)
	_, _ = mac.Write([]byte(code))
	a.loginCodeDigest = mac.Sum(nil)
	a.loginCodeExpires = now.Add(loginCodeTTL).Unix()
	a.loginCodeAttempts = 0
	return int(loginCodeCooldown / time.Second), nil
}

func normalizeBarkAddress(address string) (string, error) {
	address = strings.TrimRight(strings.TrimSpace(address), "/")
	if address == "" || len(address) > 2048 {
		return "", errors.New("Bark 地址不能为空且不能超过 2048 字符")
	}
	for _, r := range address {
		if unicode.IsControl(r) {
			return "", errors.New("Bark 地址不能包含控制字符")
		}
	}
	parsed, err := url.Parse(address)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") ||
		parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", errors.New("Bark 地址必须是合法的 HTTP/HTTPS 地址，且不能包含凭据、查询参数或片段")
	}
	return address, nil
}

func (a *Admin) sendBarkCode(address, title, body string) error {
	address, err := normalizeBarkAddress(address)
	if err != nil {
		return err
	}
	endpoint := address + "/" + url.PathEscape(title) + "/" + url.PathEscape(body)
	req, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	resp, err := a.barkClient.Do(req)
	if err != nil {
		return errors.New("无法连接 Bark 服务")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("Bark 返回 HTTP %d", resp.StatusCode)
	}
	return nil
}

func (a *Admin) verifyLoginCode(code string) error {
	if err := validateFourDigitCode(code); err != nil {
		return err
	}
	a.loginCodeMu.Lock()
	defer a.loginCodeMu.Unlock()
	if len(a.loginCodeDigest) == 0 {
		return errors.New("请先获取登录验证码")
	}
	if time.Now().Unix() > a.loginCodeExpires {
		a.clearLoginCodeLocked()
		return errors.New("验证码已过期，请重新获取")
	}
	if a.loginCodeAttempts >= maxLoginCodeAttempts {
		a.clearLoginCodeLocked()
		return errors.New("验证码错误次数过多，请重新获取")
	}
	a.loginCodeAttempts++
	mac := hmac.New(sha256.New, a.verificationKey)
	_, _ = mac.Write([]byte(code))
	if subtle.ConstantTimeCompare(mac.Sum(nil), a.loginCodeDigest) != 1 {
		if a.loginCodeAttempts >= maxLoginCodeAttempts {
			a.clearLoginCodeLocked()
			return errors.New("验证码错误次数过多，请重新获取")
		}
		return errors.New("验证码不正确")
	}
	a.clearLoginCodeLocked()
	a.loginCodeSentAt = 0
	return nil
}

func validateFourDigitCode(code string) error {
	if len(code) != 4 {
		return errors.New("请输入 4 位验证码")
	}
	for _, r := range code {
		if r < '0' || r > '9' {
			return errors.New("请输入 4 位数字验证码")
		}
	}
	return nil
}

func (a *Admin) clearLoginCodeLocked() {
	a.loginCodeDigest = nil
	a.loginCodeExpires = 0
	a.loginCodeAttempts = 0
}

func (a *Admin) resetLoginCode() {
	a.loginCodeMu.Lock()
	defer a.loginCodeMu.Unlock()
	a.clearLoginCodeLocked()
	a.loginCodeSentAt = 0
}

func (a *Admin) issueBarkSetupCode(address string) error {
	address, err := normalizeBarkAddress(address)
	if err != nil {
		return err
	}
	a.barkSetupCodeMu.Lock()
	defer a.barkSetupCodeMu.Unlock()
	now := time.Now()
	if a.barkSetupCodeSentAt > 0 &&
		now.Unix()-a.barkSetupCodeSentAt < int64(loginCodeCooldown/time.Second) {
		return errors.New("验证码发送过于频繁，请稍后再试")
	}
	limit := big.NewInt(10000)
	number, err := rand.Int(rand.Reader, limit)
	if err != nil {
		return errors.New("生成验证码失败")
	}
	code := fmt.Sprintf("%04d", number.Int64())
	a.clearBarkSetupCodeLocked()
	a.barkSetupCodeSentAt = now.Unix()
	if err := a.sendBarkCode(
		address,
		"AKKV Store 配置验证",
		"验证码："+code+"，5 分钟内有效",
	); err != nil {
		return fmt.Errorf("Bark 推送失败: %w", err)
	}
	a.barkSetupCodeDigest = a.setupVerificationDigest("code", code)
	a.barkSetupAddressDigest = a.setupVerificationDigest("address", address)
	a.barkSetupCodeExpires = now.Add(loginCodeTTL).Unix()
	a.barkSetupCodeAttempts = 0
	return nil
}

func (a *Admin) verifyBarkSetupCode(address, code string) error {
	address, err := normalizeBarkAddress(address)
	if err != nil {
		return err
	}
	if err := validateFourDigitCode(code); err != nil {
		return err
	}
	a.barkSetupCodeMu.Lock()
	defer a.barkSetupCodeMu.Unlock()
	if len(a.barkSetupCodeDigest) == 0 || len(a.barkSetupAddressDigest) == 0 {
		return errors.New("请先发送 Bark 验证码")
	}
	if time.Now().Unix() > a.barkSetupCodeExpires {
		a.clearBarkSetupCodeLocked()
		return errors.New("验证码已过期，请重新发送")
	}
	if a.barkSetupCodeAttempts >= maxLoginCodeAttempts {
		a.clearBarkSetupCodeLocked()
		return errors.New("验证码错误次数过多，请重新发送")
	}
	a.barkSetupCodeAttempts++
	codeMatches := subtle.ConstantTimeCompare(
		a.setupVerificationDigest("code", code),
		a.barkSetupCodeDigest,
	) == 1
	addressMatches := subtle.ConstantTimeCompare(
		a.setupVerificationDigest("address", address),
		a.barkSetupAddressDigest,
	) == 1
	if !codeMatches || !addressMatches {
		if a.barkSetupCodeAttempts >= maxLoginCodeAttempts {
			a.clearBarkSetupCodeLocked()
			return errors.New("验证码错误次数过多，请重新发送")
		}
		return errors.New("Bark 验证码不正确，或地址已发生变化")
	}
	a.clearBarkSetupCodeLocked()
	a.barkSetupCodeSentAt = 0
	return nil
}

func (a *Admin) setupVerificationDigest(kind, value string) []byte {
	mac := hmac.New(sha256.New, a.verificationKey)
	_, _ = mac.Write([]byte("bark-setup:" + kind + ":" + value))
	return mac.Sum(nil)
}

func (a *Admin) clearBarkSetupCodeLocked() {
	a.barkSetupCodeDigest = nil
	a.barkSetupAddressDigest = nil
	a.barkSetupCodeExpires = 0
	a.barkSetupCodeAttempts = 0
}

func (a *Admin) resetBarkSetupCode() {
	a.barkSetupCodeMu.Lock()
	defer a.barkSetupCodeMu.Unlock()
	a.clearBarkSetupCodeLocked()
	a.barkSetupCodeSentAt = 0
}

func (a *Admin) postLogout(w http.ResponseWriter, r *http.Request, _ auth.Session) {
	a.clearSession(w)
	http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
}

// ---------- 系统配置 ----------

func (a *Admin) redirectToSystemAuth(w http.ResponseWriter, r *http.Request, _ auth.Session) {
	http.Redirect(w, r, "/admin/system", http.StatusSeeOther)
}

func (a *Admin) getSystemSettings(w http.ResponseWriter, r *http.Request, sess auth.Session) {
	settings, err := a.store.GetAdminSettings()
	if err != nil {
		a.log.Error("read admin settings", "err", err)
		a.renderError(w, http.StatusInternalServerError, sess, "读取系统配置失败")
		return
	}
	data := systemSettingsData{
		pageBase:             a.base("认证方式", sess),
		Settings:             settings,
		PasswordLoginEnabled: settings.PasswordLoginEnabled,
		BarkLoginEnabled:     settings.BarkLoginEnabled,
		BarkAddress:          settings.BarkAddress,
	}
	data.Flash = a.takeFlash(w, r)
	a.render(w, http.StatusOK, "systemAuth", data)
}

func (a *Admin) postUpdateAdminPassword(w http.ResponseWriter, r *http.Request, sess auth.Session) {
	if err := r.ParseForm(); err != nil {
		a.renderError(w, http.StatusBadRequest, sess, "请求格式错误")
		return
	}
	password := r.FormValue("new_password")
	if len(password) < 8 {
		a.renderSystemError(w, sess, "管理员密码至少需要 8 个字符")
		return
	}
	if len(password) > 72 {
		a.renderSystemError(w, sess, "管理员密码不能超过 72 字节")
		return
	}
	if store.IsLegacyDefaultAdminPassword(password) {
		a.renderSystemError(w, sess, "不能使用旧版默认占位密码，请设置新的管理员密码")
		return
	}
	hash, err := auth.HashPassword(password)
	if err != nil {
		a.log.Error("hash admin password", "err", err)
		a.renderSystemError(w, sess, "更新管理员密码失败")
		return
	}
	epoch, err := a.store.UpdateAdminPassword(hash)
	if err != nil {
		a.log.Error("update admin password", "err", err)
		a.renderSystemError(w, sess, "更新管理员密码失败")
		return
	}
	a.resetLoginCode()
	a.resetBarkSetupCode()
	a.setSession(w, auth.NewSessionWithEpoch(a.ttl, epoch))
	a.setFlash(w, auth.Flash{
		ExpiresAt: time.Now().Add(10 * time.Minute).Unix(),
		Message:   "管理员密码已更新",
	})
	http.Redirect(w, r, "/admin/system", http.StatusSeeOther)
}

func (a *Admin) postToggleAuthMethod(w http.ResponseWriter, r *http.Request, _ auth.Session) {
	if err := r.ParseForm(); err != nil {
		writeAdminJSON(w, http.StatusBadRequest, map[string]string{"error": "请求格式错误"})
		return
	}
	method := strings.TrimSpace(r.FormValue("method"))
	enabled := r.FormValue("enabled") == "1"
	settings, err := a.store.GetAdminSettings()
	if err != nil {
		a.log.Error("read admin settings for toggle", "err", err)
		writeAdminJSON(w, http.StatusInternalServerError, map[string]string{"error": "读取认证配置失败"})
		return
	}
	passwordEnabled := settings.PasswordLoginEnabled
	barkEnabled := settings.BarkLoginEnabled
	switch method {
	case "password":
		passwordEnabled = enabled
	case "bark":
		barkEnabled = enabled
	default:
		writeAdminJSON(w, http.StatusBadRequest, map[string]string{"error": "不支持的认证方式"})
		return
	}
	if !passwordEnabled && !barkEnabled {
		writeAdminJSON(w, http.StatusBadRequest, map[string]string{"error": "必须至少启用一种认证方式"})
		return
	}
	if barkEnabled && settings.BarkAddress == "" {
		writeAdminJSON(w, http.StatusBadRequest, map[string]string{"error": "请先验证 Bark 地址"})
		return
	}
	epoch, err := a.store.UpdateAdminLoginSettings(passwordEnabled, barkEnabled, settings.BarkAddress)
	if err != nil {
		a.log.Error("update auth methods", "err", err)
		writeAdminJSON(w, http.StatusBadRequest, map[string]string{"error": "更新认证方式失败"})
		return
	}
	a.resetLoginCode()
	a.resetBarkSetupCode()
	a.setSession(w, auth.NewSessionWithEpoch(a.ttl, epoch))
	writeAdminJSON(w, http.StatusOK, map[string]any{
		"ok":                     true,
		"password_login_enabled": passwordEnabled,
		"bark_login_enabled":     barkEnabled,
	})
}

func (a *Admin) postSendBarkSetupCode(w http.ResponseWriter, r *http.Request, _ auth.Session) {
	if err := r.ParseForm(); err != nil {
		writeAdminJSON(w, http.StatusBadRequest, map[string]string{"error": "请求格式错误"})
		return
	}
	addressInput := strings.TrimSpace(r.FormValue("bark_address"))
	address, err := normalizeBarkAddress(addressInput)
	if err != nil {
		writeAdminJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if err := a.issueBarkSetupCode(address); err != nil {
		a.log.Error("send Bark setup code", "err", err)
		writeAdminJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	writeAdminJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"message": "验证码已发送，请在 5 分钟内完成验证",
	})
}

func (a *Admin) postVerifyBarkSetup(w http.ResponseWriter, r *http.Request, _ auth.Session) {
	if err := r.ParseForm(); err != nil {
		writeAdminJSON(w, http.StatusBadRequest, map[string]string{"error": "请求格式错误"})
		return
	}
	address, err := normalizeBarkAddress(r.FormValue("bark_address"))
	if err != nil {
		writeAdminJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	code := strings.TrimSpace(r.FormValue("bark_code"))
	if err := a.verifyBarkSetupCode(address, code); err != nil {
		writeAdminJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	settings, err := a.store.GetAdminSettings()
	if err != nil {
		a.log.Error("read admin settings for Bark verify", "err", err)
		writeAdminJSON(w, http.StatusInternalServerError, map[string]string{"error": "读取认证配置失败"})
		return
	}
	epoch, err := a.store.UpdateAdminLoginSettings(
		settings.PasswordLoginEnabled,
		settings.BarkLoginEnabled,
		address,
	)
	if err != nil {
		a.log.Error("save verified Bark settings", "err", err)
		writeAdminJSON(w, http.StatusInternalServerError, map[string]string{"error": "保存 Bark 地址失败"})
		return
	}
	a.resetLoginCode()
	a.resetBarkSetupCode()
	a.setSession(w, auth.NewSessionWithEpoch(a.ttl, epoch))
	writeAdminJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"message": "验证完成，Bark 地址已保存",
		"address": address,
	})
}

func (a *Admin) renderSystemError(w http.ResponseWriter, sess auth.Session, message string) {
	settings, err := a.store.GetAdminSettings()
	if err != nil {
		a.log.Error("read admin settings", "err", err)
		a.renderError(w, http.StatusInternalServerError, sess, "读取系统配置失败")
		return
	}
	page := a.base("认证方式", sess)
	page.Flash.Error = message
	data := systemSettingsData{
		pageBase:             page,
		Settings:             settings,
		PasswordLoginEnabled: settings.PasswordLoginEnabled,
		BarkLoginEnabled:     settings.BarkLoginEnabled,
		BarkAddress:          settings.BarkAddress,
	}
	a.render(w, http.StatusBadRequest, "systemAuth", data)
}

// ---------- 项目列表 ----------

func (a *Admin) getProjects(w http.ResponseWriter, r *http.Request, sess auth.Session) {
	a.renderProjects(w, r, http.StatusOK, sess, "")
}

func (a *Admin) renderProjects(w http.ResponseWriter, r *http.Request, status int, sess auth.Session, errMsg string) {
	projects, err := a.store.ListProjects()
	if err != nil {
		a.log.Error("list projects", "err", err)
		a.renderError(w, http.StatusInternalServerError, sess, "读取项目列表失败")
		return
	}
	data := projectsData{
		pageBase: a.base("项目", sess),
		Projects: projects,
		Error:    errMsg,
	}
	if status == http.StatusOK {
		data.Flash = a.takeFlash(w, r)
	}
	a.render(w, status, "projects", data)
}

func (a *Admin) postCreateProject(w http.ResponseWriter, r *http.Request, sess auth.Session) {
	name := strings.TrimSpace(r.FormValue("name"))
	p, token, err := a.store.CreateProject(name)
	if err != nil {
		msg := err.Error()
		if errors.Is(err, store.ErrInvalid) {
			msg = strings.TrimPrefix(msg, "invalid input: ")
		}
		a.renderProjects(w, r, http.StatusBadRequest, sess, msg)
		return
	}
	a.setFlash(w, auth.Flash{
		ExpiresAt: time.Now().Add(10 * time.Minute).Unix(),
		Message:   "项目创建成功。默认 Token 已保存，可随时在项目设置中查看和复制。",
		Token:     token,
	})
	http.Redirect(w, r, "/admin/projects/"+url.PathEscape(p.Name), http.StatusSeeOther)
}

// ---------- 单个项目 ----------

func (a *Admin) getProject(w http.ResponseWriter, r *http.Request, sess auth.Session) {
	name := r.PathValue("name")
	p, err := a.store.GetProject(name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			a.renderError(w, http.StatusNotFound, sess, "项目不存在")
			return
		}
		a.log.Error("get project", "err", err)
		a.renderError(w, http.StatusInternalServerError, sess, "读取项目失败")
		return
	}

	tables, err := a.store.ListTables(p)
	if err != nil {
		a.log.Error("list tables", "err", err)
		a.renderError(w, http.StatusInternalServerError, sess, "读取 Table 列表失败")
		return
	}
	if len(tables) > 0 {
		location := "/admin/projects/" + url.PathEscape(p.Name) + "/tables/" + url.PathEscape(tables[0].Name)
		http.Redirect(w, r, location, http.StatusSeeOther)
		return
	}
	data := projectData{
		pageBase: a.base(p.Name, sess),
		Project:  p,
		Tables:   tables,
	}
	data.Flash = a.takeFlash(w, r)
	a.render(w, http.StatusOK, "project", data)
}

func (a *Admin) renderProjectError(w http.ResponseWriter, status int, sess auth.Session, name, msg string) {
	p, err := a.store.GetProject(name)
	if err != nil {
		a.renderError(w, status, sess, msg)
		return
	}
	tables, _ := a.store.ListTables(p)
	data := projectData{
		pageBase: a.base(p.Name, sess),
		Project:  p,
		Tables:   tables,
		Error:    msg,
	}
	a.render(w, status, "project", data)
}

func (a *Admin) postCreateTable(w http.ResponseWriter, r *http.Request, sess auth.Session) {
	name := r.PathValue("name")
	tableName := strings.TrimSpace(r.FormValue("table"))
	p, err := a.store.GetProject(name)
	if err != nil {
		a.renderError(w, http.StatusNotFound, sess, "项目不存在")
		return
	}
	table, err := a.store.CreateTable(p, tableName)
	if err != nil {
		msg := err.Error()
		if errors.Is(err, store.ErrInvalid) {
			msg = strings.TrimPrefix(msg, "invalid input: ")
		}
		if errors.Is(err, store.ErrTableExists) {
			msg = "Table 已存在"
		}
		a.renderProjectError(w, http.StatusBadRequest, sess, name, msg)
		return
	}
	a.setFlash(w, auth.Flash{
		ExpiresAt: time.Now().Add(10 * time.Minute).Unix(),
		Message:   "Table " + tableName + " 已创建",
	})
	http.Redirect(w, r, "/admin/projects/"+url.PathEscape(name)+"/tables/"+url.PathEscape(table.Name), http.StatusSeeOther)
}

func (a *Admin) postDeleteTable(w http.ResponseWriter, r *http.Request, sess auth.Session) {
	name := r.PathValue("name")
	tableName := r.PathValue("table")
	p, err := a.store.GetProject(name)
	if err != nil {
		a.renderError(w, http.StatusNotFound, sess, "项目不存在")
		return
	}
	if r.FormValue("confirm") != tableName {
		a.renderProjectError(w, http.StatusBadRequest, sess, name, "Table 名输入不匹配，未执行删除")
		return
	}
	if err := a.store.DeleteTable(p, tableName); err != nil {
		msg := "删除 Table 失败"
		status := http.StatusInternalServerError
		if errors.Is(err, store.ErrTableNotFound) {
			msg = "Table 不存在"
			status = http.StatusNotFound
		} else if errors.Is(err, store.ErrInvalid) {
			msg = strings.TrimPrefix(err.Error(), "invalid input: ")
			status = http.StatusBadRequest
		}
		a.renderProjectError(w, status, sess, name, msg)
		return
	}
	a.setFlash(w, auth.Flash{
		ExpiresAt: time.Now().Add(10 * time.Minute).Unix(),
		Message:   "Table " + tableName + " 已删除",
	})
	http.Redirect(w, r, "/admin/projects/"+url.PathEscape(name), http.StatusSeeOther)
}

func (a *Admin) getTable(w http.ResponseWriter, r *http.Request, sess auth.Session) {
	name := r.PathValue("name")
	p, err := a.store.GetProject(name)
	if err != nil {
		a.renderError(w, http.StatusNotFound, sess, "项目不存在")
		return
	}
	tableName := r.PathValue("table")
	table, err := a.store.GetTable(p, tableName)
	if err != nil {
		if errors.Is(err, store.ErrTableNotFound) {
			a.renderError(w, http.StatusNotFound, sess, "Table 不存在")
			return
		}
		a.log.Error("get table", "err", err)
		a.renderError(w, http.StatusInternalServerError, sess, "读取 Table 失败")
		return
	}
	tables, err := a.store.ListTables(p)
	if err != nil {
		a.log.Error("list tables", "err", err)
		a.renderError(w, http.StatusInternalServerError, sess, "读取 Table 列表失败")
		return
	}

	prefix := r.URL.Query().Get("prefix")
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if page < 1 {
		page = 1
	}
	keys, total, err := a.store.ListKeysWithPreview(
		p, table.Name, prefix, pageSize, (page-1)*pageSize, tablePreview,
	)
	if err != nil {
		a.log.Error("list keys", "err", err)
		a.renderError(w, http.StatusInternalServerError, sess, "读取 Key 列表失败")
		return
	}
	pages := int((total + int64(pageSize) - 1) / int64(pageSize))
	if pages == 0 {
		pages = 1
	}
	if page > pages {
		page = pages
		keys, total, err = a.store.ListKeysWithPreview(
			p, table.Name, prefix, pageSize, (page-1)*pageSize, tablePreview,
		)
		if err != nil {
			a.log.Error("list keys", "err", err)
			a.renderError(w, http.StatusInternalServerError, sess, "读取 Key 列表失败")
			return
		}
	}
	rows := make([]keyRow, 0, len(keys))
	for _, key := range keys {
		rows = append(rows, keyRow{
			KeyMeta:      key.KeyMeta,
			ValuePreview: keyPreview(key),
		})
	}
	data := tableData{
		pageBase:    a.base(p.Name+" / "+table.Name, sess),
		Project:     p,
		Tables:      tables,
		ActiveTable: table.Name,
		Table:       table,
		Keys:        rows,
		Prefix:      prefix,
		Page:        page,
		Pages:       pages,
		Total:       total,
	}
	data.Flash = a.takeFlash(w, r)
	a.render(w, http.StatusOK, "table", data)
}

func (a *Admin) getProjectSettings(w http.ResponseWriter, r *http.Request, sess auth.Session) {
	name := r.PathValue("name")
	p, err := a.store.GetProject(name)
	if err != nil {
		a.renderError(w, http.StatusNotFound, sess, "项目不存在")
		return
	}
	tables, err := a.store.ListTables(p)
	if err != nil {
		a.log.Error("list tables", "err", err)
		a.renderError(w, http.StatusInternalServerError, sess, "读取 Table 列表失败")
		return
	}
	activeTable := r.URL.Query().Get("table")
	if !store.ValidateTableName(activeTable) {
		activeTable = ""
	} else {
		found := false
		for _, table := range tables {
			if table.Name == activeTable {
				found = true
				break
			}
		}
		if !found {
			activeTable = ""
		}
	}
	tokens, err := a.store.ListProjectTokens(p.Name)
	if err != nil {
		a.log.Error("list project tokens", "project", p.Name, "err", err)
		a.renderError(w, http.StatusInternalServerError, sess, "读取 Token 列表失败")
		return
	}
	data := projectSettingsData{
		pageBase:    a.base("项目设置", sess),
		Project:     p,
		Tables:      tables,
		Tokens:      tokens,
		ActiveTable: activeTable,
		APIBasePath: "/v1/api",
	}
	data.Flash = a.takeFlash(w, r)
	a.render(w, http.StatusOK, "settings", data)
}

func keyPreview(key store.Record) string {
	if key.ValuePreviewError != nil {
		return "[读取失败]"
	}
	if key.Size == 0 {
		return "(空)"
	}
	if key.ContentType != store.ValueTypeString {
		return "[二进制 " + humanSize(key.Size) + "]"
	}
	value := key.Value
	if !utf8.Valid(value) {
		for len(value) > 0 && !utf8.Valid(value) {
			value = value[:len(value)-1]
		}
	}
	preview := strings.TrimSpace(strings.Join(strings.Fields(string(value)), " "))
	if preview == "" {
		return "(空)"
	}
	truncated := key.Size > int64(len(value))
	runes := []rune(preview)
	if len(runes) > 100 {
		preview = string(runes[:100])
		truncated = true
	}
	if truncated {
		preview += "…"
	}
	return preview
}

func (a *Admin) postDeleteProject(w http.ResponseWriter, r *http.Request, sess auth.Session) {
	name := r.PathValue("name")
	if r.FormValue("confirm") != name {
		a.renderProjectError(w, http.StatusBadRequest, sess, name, "项目名输入不匹配，未执行删除")
		return
	}
	if err := a.store.DeleteProject(name); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			a.renderError(w, http.StatusNotFound, sess, "项目不存在")
			return
		}
		a.log.Error("delete project", "err", err)
		a.renderProjectError(w, http.StatusInternalServerError, sess, name, "删除项目失败")
		return
	}
	a.setFlash(w, auth.Flash{
		ExpiresAt: time.Now().Add(10 * time.Minute).Unix(),
		Message:   "项目 " + name + " 已删除",
	})
	http.Redirect(w, r, "/admin/", http.StatusSeeOther)
}

func (a *Admin) postCreateProjectToken(w http.ResponseWriter, r *http.Request, sess auth.Session) {
	name := r.PathValue("name")
	settingsURL := "/admin/projects/" + url.PathEscape(name) + "/settings"
	if activeTable := r.FormValue("table"); store.ValidateTableName(activeTable) {
		settingsURL += "?table=" + url.QueryEscape(activeTable)
	}
	tokenMeta, token, err := a.store.CreateProjectToken(name, r.FormValue("token_name"))
	if err != nil {
		msg := "创建 Token 失败"
		if errors.Is(err, store.ErrNotFound) {
			msg = "项目不存在"
		} else if errors.Is(err, store.ErrInvalid) {
			msg = strings.TrimPrefix(err.Error(), "invalid input: ")
		}
		a.setFlash(w, auth.Flash{
			ExpiresAt: time.Now().Add(10 * time.Minute).Unix(),
			Error:     msg,
		})
		http.Redirect(w, r, settingsURL, http.StatusSeeOther)
		return
	}
	a.setFlash(w, auth.Flash{
		ExpiresAt: time.Now().Add(10 * time.Minute).Unix(),
		Message:   "Token " + tokenMeta.Name + " 创建成功，已保存，可在列表中查看和复制。",
		Token:     token,
	})
	http.Redirect(w, r, settingsURL, http.StatusSeeOther)
}

func (a *Admin) postDeleteProjectToken(w http.ResponseWriter, r *http.Request, sess auth.Session) {
	name := r.PathValue("name")
	settingsURL := "/admin/projects/" + url.PathEscape(name) + "/settings"
	if activeTable := r.FormValue("table"); store.ValidateTableName(activeTable) {
		settingsURL += "?table=" + url.QueryEscape(activeTable)
	}
	tokenID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || tokenID <= 0 {
		a.setFlash(w, auth.Flash{
			ExpiresAt: time.Now().Add(10 * time.Minute).Unix(),
			Error:     "Token 不存在",
		})
		http.Redirect(w, r, settingsURL, http.StatusSeeOther)
		return
	}
	if err := a.store.DeleteProjectToken(name, tokenID); err != nil {
		message := "删除 Token 失败"
		if errors.Is(err, store.ErrNotFound) {
			message = "Token 不存在"
		}
		a.setFlash(w, auth.Flash{
			ExpiresAt: time.Now().Add(10 * time.Minute).Unix(),
			Error:     message,
		})
		http.Redirect(w, r, settingsURL, http.StatusSeeOther)
		return
	}
	a.setFlash(w, auth.Flash{
		ExpiresAt: time.Now().Add(10 * time.Minute).Unix(),
		Message:   "Token 已删除",
	})
	http.Redirect(w, r, settingsURL, http.StatusSeeOther)
}

type keyContentData struct {
	Editable    bool            `json:"editable"`
	ContentType store.ValueType `json:"contentType"`
	Size        int64           `json:"size"`
	External    bool            `json:"external"`
	FileName    string          `json:"fileName,omitempty"`
	Value       string          `json:"value,omitempty"`
}

func (a *Admin) getKeyContent(w http.ResponseWriter, r *http.Request, _ auth.Session) {
	name := r.PathValue("name")
	p, err := a.store.GetProject(name)
	if err != nil {
		writeAdminJSON(w, http.StatusNotFound, map[string]string{"error": "项目不存在"})
		return
	}
	table, err := a.store.GetTable(p, r.PathValue("table"))
	if err != nil {
		writeAdminJSON(w, http.StatusNotFound, map[string]string{"error": "Table 不存在"})
		return
	}
	key := r.URL.Query().Get("key")
	if err := store.ValidateKey(key); err != nil {
		writeAdminJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	meta, rc, err := a.store.GetKey(p, table.Name, key)
	if err != nil {
		writeAdminJSON(w, http.StatusNotFound, map[string]string{"error": "Key 不存在"})
		return
	}
	defer rc.Close()

	result := keyContentData{
		ContentType: meta.ContentType,
		Size:        meta.Size,
		External:    meta.External,
		FileName:    path.Base(key),
	}
	if meta.ContentType != store.ValueTypeString || meta.Size > previewLimit {
		writeAdminJSON(w, http.StatusOK, result)
		return
	}
	value, err := io.ReadAll(io.LimitReader(rc, previewLimit+1))
	if err != nil {
		writeAdminJSON(w, http.StatusInternalServerError, map[string]string{"error": "读取内容失败"})
		return
	}
	if int64(len(value)) > previewLimit || !utf8.Valid(value) {
		writeAdminJSON(w, http.StatusOK, result)
		return
	}
	result.Editable = true
	result.Value = string(value)
	writeAdminJSON(w, http.StatusOK, result)
}

func writeAdminJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeInlineWriteError(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	switch {
	case errors.Is(err, store.ErrKeyExists):
		status = http.StatusConflict
	case errors.Is(err, store.ErrInvalid):
		status = http.StatusBadRequest
	case errors.Is(err, store.ErrTooLarge):
		status = http.StatusRequestEntityTooLarge
	case errors.Is(err, store.ErrTableNotFound):
		status = http.StatusNotFound
	case errors.Is(err, store.ErrNotFound):
		status = http.StatusNotFound
	}
	writeAdminJSON(w, status, map[string]string{"error": publicStoreError(err)})
}

// ---------- Key ----------

func (a *Admin) getKey(w http.ResponseWriter, r *http.Request, sess auth.Session) {
	name := r.PathValue("name")
	p, err := a.store.GetProject(name)
	if err != nil {
		a.renderError(w, http.StatusNotFound, sess, "项目不存在")
		return
	}
	tableName := r.PathValue("table")
	table, err := a.store.GetTable(p, tableName)
	if err != nil {
		if errors.Is(err, store.ErrTableNotFound) {
			a.renderError(w, http.StatusNotFound, sess, "Table 不存在")
			return
		}
		a.log.Error("get table", "err", err)
		a.renderError(w, http.StatusInternalServerError, sess, "读取 Table 失败")
		return
	}
	tables, err := a.store.ListTables(p)
	if err != nil {
		a.log.Error("list tables", "err", err)
		a.renderError(w, http.StatusInternalServerError, sess, "读取 Table 列表失败")
		return
	}
	key := r.URL.Query().Get("key")
	data := keyData{
		pageBase:    a.base("Key", sess),
		Project:     p,
		Tables:      tables,
		ActiveTable: table.Name,
		Table:       table,
		Key:         key,
		ContentType: store.ValueTypeString,
	}
	data.Flash = a.takeFlash(w, r)

	if key != "" {
		if err := store.ValidateKey(key); err != nil {
			a.renderError(w, http.StatusBadRequest, sess, "Key 不合法："+err.Error())
			return
		}
		meta, rc, err := a.store.GetKey(p, table.Name, key)
		if errors.Is(err, store.ErrNotFound) {
			a.setFlash(w, auth.Flash{
				ExpiresAt: time.Now().Add(10 * time.Minute).Unix(),
				Error:     "Key 不存在",
			})
			http.Redirect(w, r, "/admin/projects/"+url.PathEscape(name)+"/tables/"+url.PathEscape(table.Name), http.StatusSeeOther)
			return
		}
		if err != nil {
			a.log.Error("get key", "err", err)
			a.renderError(w, http.StatusInternalServerError, sess, "读取 Key 失败")
			return
		}
		if err == nil {
			defer rc.Close()
			data.Exists = true
			data.ContentType = meta.ContentType
			data.Size = meta.Size
			data.UpdatedAt = meta.UpdatedAt
			if meta.ContentType == store.ValueTypeString && meta.Size <= previewLimit {
				b, err := io.ReadAll(io.LimitReader(rc, previewLimit+1))
				if err == nil && int64(len(b)) <= previewLimit && utf8.Valid(b) {
					data.Value = string(b)
					data.Previewable = true
				}
			}
			data.Binary = !data.Previewable
		}
	}
	a.render(w, http.StatusOK, "key", data)
}

func (a *Admin) postPutKey(w http.ResponseWriter, r *http.Request, sess auth.Session) {
	name := r.PathValue("name")
	p, err := a.store.GetProject(name)
	if err != nil {
		a.renderError(w, http.StatusNotFound, sess, "项目不存在")
		return
	}
	tableName := r.PathValue("table")
	table, err := a.store.GetTable(p, tableName)
	if err != nil {
		if errors.Is(err, store.ErrTableNotFound) {
			a.renderError(w, http.StatusNotFound, sess, "Table 不存在")
			return
		}
		a.log.Error("get table", "err", err)
		a.renderError(w, http.StatusInternalServerError, sess, "读取 Table 失败")
		return
	}

	key := r.URL.Query().Get("key")
	returnToTable := r.URL.Query().Get("return_to") == "table"
	createOnly := r.URL.Query().Get("create") == "1"
	inlineRequest := strings.TrimSpace(r.Header.Get("X-Inline-Edit")) == "1"
	redirectToKey := func(msg, errMsg string) {
		f := auth.Flash{ExpiresAt: time.Now().Add(10 * time.Minute).Unix(), Message: msg, Error: errMsg}
		a.setFlash(w, f)
		loc := "/admin/projects/" + url.PathEscape(name) + "/tables/" + url.PathEscape(table.Name)
		if !returnToTable {
			loc += "/key"
			if key != "" {
				loc += "?key=" + url.QueryEscape(key)
			}
		}
		http.Redirect(w, r, loc, http.StatusSeeOther)
	}

	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
		if err := a.saveMultipart(p, table.Name, r, &key, createOnly); err != nil {
			if inlineRequest {
				writeInlineWriteError(w, err)
				return
			}
			a.log.Error("save multipart key", "err", err)
			redirectToKey("", publicStoreError(err))
			return
		}
		if inlineRequest {
			writeAdminJSON(w, http.StatusOK, map[string]string{"message": "已保存"})
			return
		}
		redirectToKey("已保存", "")
		return
	}

	if err := r.ParseForm(); err != nil {
		http.Error(w, "请求格式错误", http.StatusBadRequest)
		return
	}
	if key == "" {
		key = r.FormValue("key")
	}
	if createOnly {
		key = strings.TrimSpace(key)
	}
	if r.FormValue("action") == "delete" {
		if key == "" {
			redirectToKey("", "缺少 Key")
			return
		}
		if err := a.store.DeleteKey(p, table.Name, key); err != nil {
			a.log.Error("delete key", "err", err)
			redirectToKey("", publicStoreError(err))
			return
		}
		redirectToKey("Key 已删除", "")
		return
	}
	if createOnly {
		_, err = a.store.PutKeyCreate(p, table.Name, key, store.ValueTypeString, strings.NewReader(r.FormValue("value")))
	} else {
		_, err = a.store.PutKey(p, table.Name, key, store.ValueTypeString, strings.NewReader(r.FormValue("value")))
	}
	if err != nil {
		if inlineRequest {
			writeInlineWriteError(w, err)
			return
		}
		a.log.Error("put key", "err", err)
		redirectToKey("", publicStoreError(err))
		return
	}
	if inlineRequest {
		writeAdminJSON(w, http.StatusOK, map[string]string{"message": "已保存"})
		return
	}
	redirectToKey("已保存", "")
}

func (a *Admin) postRenameKey(w http.ResponseWriter, r *http.Request, sess auth.Session) {
	name := r.PathValue("name")
	p, err := a.store.GetProject(name)
	if err != nil {
		a.renderError(w, http.StatusNotFound, sess, "项目不存在")
		return
	}
	table, err := a.store.GetTable(p, r.PathValue("table"))
	if err != nil {
		a.renderError(w, http.StatusNotFound, sess, "Table 不存在")
		return
	}
	oldKey := r.FormValue("old_key")
	newKey := strings.TrimSpace(r.FormValue("new_key"))
	inlineRequest := strings.TrimSpace(r.Header.Get("X-Inline-Edit")) == "1"
	_, err = a.store.RenameKey(p, table.Name, oldKey, newKey)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			if inlineRequest {
				writeAdminJSON(w, http.StatusNotFound, map[string]string{"error": "Key 不存在"})
				return
			}
			a.setFlash(w, auth.Flash{
				ExpiresAt: time.Now().Add(10 * time.Minute).Unix(),
				Error:     "Key 不存在",
			})
			http.Redirect(w, r, "/admin/projects/"+url.PathEscape(name)+"/tables/"+url.PathEscape(table.Name), http.StatusSeeOther)
			return
		}
		if inlineRequest {
			writeInlineWriteError(w, err)
			return
		}
		a.log.Error("rename key", "project", p.Name, "table", table.Name, "old_key", oldKey, "new_key", newKey, "err", err)
		a.setFlash(w, auth.Flash{
			ExpiresAt: time.Now().Add(10 * time.Minute).Unix(),
			Error:     publicStoreError(err),
		})
	} else {
		a.setFlash(w, auth.Flash{
			ExpiresAt: time.Now().Add(10 * time.Minute).Unix(),
			Message:   "Key 已重命名",
		})
	}
	if inlineRequest {
		writeAdminJSON(w, http.StatusOK, map[string]string{"message": "Key 已重命名"})
		return
	}
	http.Redirect(w, r, "/admin/projects/"+url.PathEscape(name)+"/tables/"+url.PathEscape(table.Name), http.StatusSeeOther)
}

func (a *Admin) postBulkDeleteKeys(w http.ResponseWriter, r *http.Request, sess auth.Session) {
	name := r.PathValue("name")
	p, err := a.store.GetProject(name)
	if err != nil {
		a.renderError(w, http.StatusNotFound, sess, "项目不存在")
		return
	}
	table, err := a.store.GetTable(p, r.PathValue("table"))
	if err != nil {
		a.renderError(w, http.StatusNotFound, sess, "Table 不存在")
		return
	}
	if err := r.ParseForm(); err != nil {
		a.renderError(w, http.StatusBadRequest, sess, "请求格式错误")
		return
	}
	seen := make(map[string]struct{})
	keys := make([]string, 0, len(r.PostForm["key"]))
	for _, key := range r.PostForm["key"] {
		if _, ok := seen[key]; ok {
			continue
		}
		if err := store.ValidateKey(key); err != nil {
			a.renderError(w, http.StatusBadRequest, sess, "Key 不合法："+err.Error())
			return
		}
		seen[key] = struct{}{}
		keys = append(keys, key)
	}
	if len(keys) == 0 {
		a.setFlash(w, auth.Flash{
			ExpiresAt: time.Now().Add(10 * time.Minute).Unix(),
			Error:     "请先选择要删除的 Key",
		})
		http.Redirect(w, r, "/admin/projects/"+url.PathEscape(name)+"/tables/"+url.PathEscape(table.Name), http.StatusSeeOther)
		return
	}

	deleted := 0
	for _, key := range keys {
		if err := a.store.DeleteKey(p, table.Name, key); err != nil {
			a.log.Error("bulk delete key", "project", p.Name, "table", table.Name, "key", key, "err", err)
			a.setFlash(w, auth.Flash{
				ExpiresAt: time.Now().Add(10 * time.Minute).Unix(),
				Error:     fmt.Sprintf("已删除 %d 个 Key，后续删除失败", deleted),
			})
			http.Redirect(w, r, "/admin/projects/"+url.PathEscape(name)+"/tables/"+url.PathEscape(table.Name), http.StatusSeeOther)
			return
		}
		deleted++
	}
	a.setFlash(w, auth.Flash{
		ExpiresAt: time.Now().Add(10 * time.Minute).Unix(),
		Message:   fmt.Sprintf("已删除 %d 个 Key", deleted),
	})
	http.Redirect(w, r, "/admin/projects/"+url.PathEscape(name)+"/tables/"+url.PathEscape(table.Name), http.StatusSeeOther)
}

func (a *Admin) saveMultipart(p store.Project, tableName string, r *http.Request, key *string, createOnly bool) error {
	mr, err := r.MultipartReader()
	if err != nil {
		return fmt.Errorf("解析上传表单失败: %w", err)
	}
	fields := make(map[string]string)
	var uploadedFile string

	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("读取上传表单失败: %w", err)
		}
		name := part.FormName()
		switch {
		case name == "file" && part.FileName() != "":
			if uploadedFile != "" {
				part.Close()
				return fmt.Errorf("%w: 一次只能上传一个文件", store.ErrInvalid)
			}
			tmp, err := a.store.CreateAdminTempFile("admin-upload-*")
			if err != nil {
				part.Close()
				return fmt.Errorf("创建上传临时文件失败: %w", err)
			}
			uploadedFile = tmp.Name()
			defer os.Remove(uploadedFile)
			var src io.Reader = part
			maxValueBytes := a.store.MaxValueBytes()
			if maxValueBytes > 0 {
				src = io.LimitReader(part, maxValueBytes+1)
			}
			written, copyErr := io.Copy(tmp, src)
			closeErr := tmp.Close()
			part.Close()
			if copyErr != nil {
				_ = os.Remove(uploadedFile)
				return fmt.Errorf("读取上传文件失败: %w", copyErr)
			}
			if closeErr != nil {
				_ = os.Remove(uploadedFile)
				return closeErr
			}
			if maxValueBytes > 0 && written > maxValueBytes {
				return store.ErrTooLarge
			}
		case name == "value":
			b, err := readPart(part, 2<<20)
			if err != nil {
				return err
			}
			fields[name] = string(b)
		default:
			b, err := readPart(part, 64<<10)
			if err != nil {
				return err
			}
			fields[name] = string(b)
			if name == "key" {
				keyValue := string(b)
				if createOnly {
					keyValue = strings.TrimSpace(keyValue)
				}
				*key = keyValue
			}
		}
	}
	if uploadedFile != "" {
		if createOnly {
			*key = strings.TrimSpace(*key)
		}
		if *key == "" {
			return fmt.Errorf("缺少 Key")
		}
		f, err := os.Open(uploadedFile)
		if err != nil {
			return err
		}
		defer f.Close()
		if createOnly {
			_, err = a.store.PutKeyCreate(p, tableName, *key, store.ValueTypeFile, f)
		} else {
			_, err = a.store.PutKey(p, tableName, *key, store.ValueTypeFile, f)
		}
		return err
	}
	if fields["require_file"] == "1" {
		return fmt.Errorf("%w: 该 Key 的内容无法在页面预览，请选择文件覆盖", store.ErrInvalid)
	}
	return a.putFromFields(p, tableName, *key, fields, createOnly)
}

func (a *Admin) putFromFields(p store.Project, tableName, key string, fields map[string]string, createOnly bool) error {
	if createOnly {
		key = strings.TrimSpace(key)
	}
	var err error
	if createOnly {
		_, err = a.store.PutKeyCreate(p, tableName, key, store.ValueTypeString, strings.NewReader(fields["value"]))
	} else {
		_, err = a.store.PutKey(p, tableName, key, store.ValueTypeString, strings.NewReader(fields["value"]))
	}
	return err
}

func readPart(part io.Reader, limit int64) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(part, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, store.ErrTooLarge
	}
	return b, nil
}

func (a *Admin) getDownload(w http.ResponseWriter, r *http.Request, sess auth.Session) {
	name := r.PathValue("name")
	p, err := a.store.GetProject(name)
	if err != nil {
		a.renderError(w, http.StatusNotFound, sess, "项目不存在")
		return
	}
	tableName := r.PathValue("table")
	table, err := a.store.GetTable(p, tableName)
	if err != nil {
		if errors.Is(err, store.ErrTableNotFound) {
			a.renderError(w, http.StatusNotFound, sess, "Table 不存在")
			return
		}
		a.renderError(w, http.StatusInternalServerError, sess, "读取 Table 失败")
		return
	}
	key := r.URL.Query().Get("key")
	if key == "" {
		a.renderError(w, http.StatusBadRequest, sess, "缺少 Key")
		return
	}
	if err := store.ValidateKey(key); err != nil {
		a.renderError(w, http.StatusBadRequest, sess, "Key 不合法："+err.Error())
		return
	}
	meta, rc, err := a.store.GetKey(p, table.Name, key)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			a.renderError(w, http.StatusNotFound, sess, "Key 不存在")
			return
		}
		a.renderError(w, http.StatusInternalServerError, sess, "读取 Key 失败")
		return
	}
	defer rc.Close()

	filename := path.Base(key)
	if filename == "." || filename == "/" || filename == "" {
		filename = "value"
	}
	disposition := mime.FormatMediaType("attachment", map[string]string{"filename": filename})
	if disposition == "" {
		disposition = "attachment"
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", disposition)
	w.Header().Set("ETag", `"`+meta.ContentHash+`"`)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	http.ServeContent(w, r, "", time.UnixMilli(meta.UpdatedAt), rc)
}

func publicStoreError(err error) string {
	switch {
	case errors.Is(err, store.ErrTooLarge):
		return "内容超过服务端配置的大小上限"
	case errors.Is(err, store.ErrTableNotFound):
		return "Table 不存在"
	case errors.Is(err, store.ErrKeyExists):
		return "Key 已存在，请使用其他名称"
	case errors.Is(err, store.ErrNotFound):
		return "项目已被删除"
	case errors.Is(err, store.ErrInvalid):
		return strings.TrimPrefix(err.Error(), "invalid input: ")
	default:
		return "操作失败，请查看服务端日志"
	}
}

func valueTypeLabel(valueType store.ValueType) string {
	if valueType == store.ValueTypeFile {
		return "file"
	}
	return "string"
}

func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}

func compactCount(n int64) string {
	if n < 1000 {
		return strconv.FormatInt(n, 10)
	}
	units := []string{"K", "M", "B", "T"}
	divisor := float64(1000)
	for i, unit := range units {
		value := float64(n) / divisor
		rounded := math.Round(value*10) / 10
		if rounded < 1000 || i == len(units)-1 {
			text := strconv.FormatFloat(rounded, 'f', 1, 64)
			text = strings.TrimSuffix(text, ".0")
			return text + unit
		}
		divisor *= 1000
	}
	return strconv.FormatInt(n, 10)
}

func displayKey(key string) string {
	runes := []rune(key)
	start := 0
	for start < len(runes) && unicode.IsSpace(runes[start]) {
		start++
	}
	end := len(runes)
	for end > start && unicode.IsSpace(runes[end-1]) {
		end--
	}
	var b strings.Builder
	for _, r := range runes[:start] {
		if unicode.IsSpace(r) {
			b.WriteRune('␠')
		} else {
			b.WriteRune(r)
		}
	}
	b.WriteString(string(runes[start:end]))
	for _, r := range runes[end:] {
		if unicode.IsSpace(r) {
			b.WriteRune('␠')
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}
