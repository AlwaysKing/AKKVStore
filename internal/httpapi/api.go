package httpapi

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"akkvstore/internal/store"
)

// API 提供 /v1 下的 JSON-RPC 控制面和二进制数据通道。
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
	for _, origin := range origins {
		if origin == "*" {
			a.allowAll = true
			continue
		}
		a.allowed[origin] = struct{}{}
	}
	return a
}

// Handler 返回挂载在 /v1 前缀下的处理器（由调用方 StripPrefix）。
func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/time", a.handleTime)
	mux.HandleFunc("/api", a.handleRPC)
	mux.HandleFunc("/upload", a.handleUpload)
	mux.HandleFunc("/download", a.handleDownload)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "not_found", "接口不存在")
	})
	return a.cors(mux)
}

type tableItem struct {
	Name      string `json:"name"`
	KeyCount  int64  `json:"keyCount"`
	UpdatedAt int64  `json:"updatedAt"`
}

type keyItem struct {
	Table        string          `json:"table,omitempty"`
	Key          string          `json:"key"`
	ContentType  store.ValueType `json:"contentType"`
	Size         int64           `json:"size"`
	UpdatedAt    int64           `json:"updatedAt"`
	Deleted      bool            `json:"deleted"`
	Value        *string         `json:"value,omitempty"`
	ValueOmitted bool            `json:"valueOmitted,omitempty"`
}

func (a *API) authenticate(
	w http.ResponseWriter,
	r *http.Request,
	project string,
) (store.Project, store.ProjectToken, bool) {
	token, ok := bearerToken(r)
	if !ok {
		w.Header().Set("WWW-Authenticate", `Bearer realm="akkv"`)
		writeError(w, http.StatusUnauthorized, "unauthorized", "缺少 Bearer token")
		return store.Project{}, store.ProjectToken{}, false
	}
	p, projectToken, err := a.store.VerifyProjectToken(project, token)
	if err != nil {
		if errors.Is(err, store.ErrUnauthorized) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="akkv"`)
			writeError(w, http.StatusUnauthorized, "unauthorized", "project 或 token 不正确")
			return store.Project{}, store.ProjectToken{}, false
		}
		a.log.Error("verify token", "project", project, "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "服务内部错误")
		return store.Project{}, store.ProjectToken{}, false
	}
	return p, projectToken, true
}

func bearerToken(r *http.Request) (string, bool) {
	value := r.Header.Get("Authorization")
	scheme, token, ok := strings.Cut(value, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	token = strings.TrimSpace(token)
	return token, token != ""
}

func (a *API) recordAccess(token store.ProjectToken, read bool) {
	if err := a.store.RecordTokenAccess(token.ProjectID, token.ID, read); err != nil {
		a.log.Error("record token access", "token_id", token.ID, "err", err)
	}
}

func (a *API) recordWrites(token store.ProjectToken, count int64) {
	if err := a.store.RecordTokenWrites(token.ID, count); err != nil {
		a.log.Error("record token writes", "token_id", token.ID, "err", err)
	}
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]string{
			"code":    code,
			"message": message,
		},
	})
}

func etag(hash string) string {
	return `"` + hash + `"`
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
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers",
				"Authorization, Content-Type, AKKV-Index-Project, AKKV-Index-Table, "+
					"AKKV-Index-Key, AKKV-Data-Updated-At, AKKV-Data-Condition")
			w.Header().Set("Access-Control-Expose-Headers", "ETag, Content-Length, Last-Modified")
			w.Header().Set("Access-Control-Max-Age", "86400")
		}
		if r.Method == http.MethodOptions {
			w.Header().Set("Allow", "GET, POST, PUT, OPTIONS")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// WriteError 输出与其他 API 错误一致的 JSON 结构，供外层路由拒绝非法路径时复用。
func WriteError(w http.ResponseWriter, status int, code, message string) {
	writeError(w, status, code, message)
}
