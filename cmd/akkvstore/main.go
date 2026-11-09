package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"akkvstore/internal/admin"
	"akkvstore/internal/config"
	"akkvstore/internal/httpapi"
	"akkvstore/internal/store"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "AKKV Store 启动失败:", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	st, err := store.Open(store.Options{
		SystemDBPath:    cfg.SystemDBPath,
		DataDir:         cfg.DataDir,
		InlineThreshold: cfg.InlineThreshold,
		MaxValueBytes:   cfg.MaxValueBytes,
	})
	if err != nil {
		return err
	}
	defer st.Close()

	secret := cfg.SessionSecret
	if len(secret) == 0 {
		b := make([]byte, 32)
		if _, err := rand.Read(b); err != nil {
			return fmt.Errorf("生成会话密钥: %w", err)
		}
		secret = []byte(base64.RawURLEncoding.EncodeToString(b))
		logger.Warn("AKKV_SESSION_SECRET 未设置，已生成临时密钥；服务重启后管理员需要重新登录")
	}

	adminHandler, err := admin.New(st, secret, cfg.SessionTTL, cfg.CookieSecure, logger)
	if err != nil {
		return err
	}
	apiHandler := httpapi.New(st, cfg.MaxValueBytes, cfg.CORSOrigins, logger)

	root := http.NewServeMux()
	root.Handle("/v1/", http.StripPrefix("/v1", apiHandler.Handler()))
	root.Handle("/admin/", http.StripPrefix("/admin", adminHandler.Handler()))
	root.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := st.Ping(ctx); err != nil {
			http.Error(w, "database unavailable", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, "ok\n")
	})
	// 只匹配根路径，避免 "/" 子树通配与 "/v1/"、"/admin/" 产生 ServeMux 冲突。
	root.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/admin/", http.StatusFound)
	})

	handler := recoverMiddleware(logger, loggingMiddleware(logger, securityHeaders(rejectUncleanPaths(root))))
	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}

	errCh := make(chan error, 1)
	go func() {
		logger.Info("AKKV Store 启动",
			"listen", cfg.Listen,
			"data_dir", cfg.DataDir,
			"system_db", cfg.SystemDBPath,
			"max_value_bytes", cfg.MaxValueBytes,
			"inline_threshold", cfg.InlineThreshold,
		)
		errCh <- srv.ListenAndServe()
	}()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	select {
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		logger.Info("收到退出信号，开始关闭")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("关闭 HTTP 服务: %w", err)
	}
	logger.Info("已退出")
	return nil
}

// rejectUncleanPaths 在 ServeMux 做路径清理/重定向之前拒绝 //、.、.. 路径段。
// 否则 PUT /kv//name 会被 307 成 /kv/name，实际写到另一个 key 上。
func rejectUncleanPaths(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hasUncleanPath(r.URL.Path) || hasUncleanPath(r.URL.EscapedPath()) {
			if strings.HasPrefix(r.URL.Path, "/v1/") {
				httpapi.WriteError(w, http.StatusBadRequest, "invalid_path", "路径不能包含连续斜杠或 . / .. 段")
				return
			}
			http.Error(w, "路径不合法", http.StatusBadRequest)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func hasUncleanPath(p string) bool {
	if strings.Contains(p, "//") {
		return true
	}
	for _, segment := range strings.Split(p, "/") {
		if segment == "." || segment == ".." {
			return true
		}
	}
	return false
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "same-origin")
		next.ServeHTTP(w, r)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (r *statusRecorder) WriteHeader(code int) {
	if r.status == 0 {
		r.status = code
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(p []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	n, err := r.ResponseWriter.Write(p)
	r.bytes += int64(n)
	return n, err
}

func (r *statusRecorder) Unwrap() http.ResponseWriter {
	return r.ResponseWriter
}

func loggingMiddleware(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)
		if r.URL.Path == "/healthz" {
			return
		}
		duration := time.Since(start)
		status := rec.status
		if status == 0 {
			status = http.StatusOK
		}
		// 高频 KV 请求不逐条写 stdout；只保留错误和慢请求。
		if status < http.StatusBadRequest && duration < 250*time.Millisecond {
			return
		}
		log.Info("request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", status,
			"bytes", rec.bytes,
			"duration_ms", duration.Milliseconds(),
			"remote", r.RemoteAddr,
		)
	})
}

func recoverMiddleware(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				log.Error("panic",
					"value", v,
					"method", r.Method,
					"path", r.URL.Path,
					"stack", string(debug.Stack()),
				)
				http.Error(w, "internal server error", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}
