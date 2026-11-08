package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Config 是服务运行所需的全部配置，全部来自环境变量。
type Config struct {
	Listen          string
	DataDir         string
	SystemDBPath    string
	SessionSecret   []byte
	SessionTTL      time.Duration
	CookieSecure    bool
	MaxValueBytes   int64
	InlineThreshold int64
	CORSOrigins     []string
}

// Load 读取环境变量并校验。默认值以单机 Docker 部署为基准。
func Load() (Config, error) {
	c := Config{
		Listen:          getenv("AKKV_LISTEN", ":8080"),
		DataDir:         getenv("AKKV_DATA_DIR", "./data"),
		SessionTTL:      12 * time.Hour,
		CookieSecure:    true,
		MaxValueBytes:   100 << 20,
		InlineThreshold: 256 << 10,
		CORSOrigins:     []string{"*"},
	}

	c.SystemDBPath = getenv("AKKV_SYSTEM_DB_PATH", filepath.Join(c.DataDir, "system.db"))

	if v := os.Getenv("AKKV_SESSION_SECRET"); v != "" {
		if len(v) < 16 {
			return c, errors.New("AKKV_SESSION_SECRET 至少需要 16 个字符")
		}
		c.SessionSecret = []byte(v)
	}

	if v := os.Getenv("AKKV_SESSION_TTL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return c, fmt.Errorf("AKKV_SESSION_TTL 格式错误: %w", err)
		}
		if d < time.Minute {
			return c, errors.New("AKKV_SESSION_TTL 不能小于 1m")
		}
		c.SessionTTL = d
	}

	if v := os.Getenv("AKKV_COOKIE_SECURE"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return c, fmt.Errorf("AKKV_COOKIE_SECURE 格式错误: %w", err)
		}
		c.CookieSecure = b
	}

	if v := os.Getenv("AKKV_MAX_VALUE_BYTES"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			return c, fmt.Errorf("AKKV_MAX_VALUE_BYTES 必须是非负整数: %q", v)
		}
		c.MaxValueBytes = n
	}

	if v := os.Getenv("AKKV_INLINE_THRESHOLD"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n <= 0 {
			return c, fmt.Errorf("AKKV_INLINE_THRESHOLD 必须是正整数: %q", v)
		}
		c.InlineThreshold = n
	}

	if v, ok := os.LookupEnv("AKKV_CORS_ORIGINS"); ok {
		// 显式设置为空字符串表示关闭 CORS；未设置时才使用默认的 "*"。
		c.CORSOrigins = splitTrim(v)
	}

	return c, nil
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func splitTrim(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
