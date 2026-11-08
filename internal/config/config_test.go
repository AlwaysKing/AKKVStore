package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func clearEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"AKKV_LISTEN", "AKKV_DATA_DIR", "AKKV_SYSTEM_DB_PATH",
		"AKKV_SESSION_SECRET", "AKKV_SESSION_TTL", "AKKV_COOKIE_SECURE",
		"AKKV_MAX_VALUE_BYTES", "AKKV_INLINE_THRESHOLD", "AKKV_CORS_ORIGINS",
	} {
		old, existed := os.LookupEnv(k)
		if err := os.Unsetenv(k); err != nil {
			t.Fatalf("Unsetenv(%s): %v", k, err)
		}
		k := k
		t.Cleanup(func() {
			if existed {
				_ = os.Setenv(k, old)
			} else {
				_ = os.Unsetenv(k)
			}
		})
	}
}

func TestDefaults(t *testing.T) {
	clearEnv(t)

	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Listen != ":8080" {
		t.Fatalf("Listen = %q", c.Listen)
	}
	if c.DataDir != "./data" {
		t.Fatalf("DataDir = %q", c.DataDir)
	}
	if c.SystemDBPath != filepath.Join("./data", "system.db") {
		t.Fatalf("SystemDBPath = %q", c.SystemDBPath)
	}
	if c.SessionTTL != 12*time.Hour {
		t.Fatalf("SessionTTL = %v", c.SessionTTL)
	}
	if !c.CookieSecure {
		t.Fatal("CookieSecure should default to true")
	}
	if c.MaxValueBytes != 100<<20 {
		t.Fatalf("MaxValueBytes = %d", c.MaxValueBytes)
	}
	if c.InlineThreshold != 256<<10 {
		t.Fatalf("InlineThreshold = %d", c.InlineThreshold)
	}
	if len(c.CORSOrigins) != 1 || c.CORSOrigins[0] != "*" {
		t.Fatalf("CORSOrigins = %v", c.CORSOrigins)
	}
}

func TestCORSDisabledByEmptyValue(t *testing.T) {
	clearEnv(t)
	t.Setenv("AKKV_CORS_ORIGINS", "")

	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(c.CORSOrigins) != 0 {
		t.Fatalf("CORSOrigins = %v, want empty", c.CORSOrigins)
	}
}

func TestAllowsMissingBootstrapPassword(t *testing.T) {
	clearEnv(t)
	c, err := Load()
	if err != nil {
		t.Fatalf("Load without bootstrap password: %v", err)
	}
	_ = c
}

func TestOverrides(t *testing.T) {
	clearEnv(t)
	t.Setenv("AKKV_LISTEN", "127.0.0.1:9000")
	t.Setenv("AKKV_DATA_DIR", "/srv/akkv")
	t.Setenv("AKKV_SYSTEM_DB_PATH", "/srv/akkv/custom-system.db")
	t.Setenv("AKKV_SESSION_SECRET", "0123456789abcdef")
	t.Setenv("AKKV_SESSION_TTL", "2h30m")
	t.Setenv("AKKV_COOKIE_SECURE", "false")
	t.Setenv("AKKV_MAX_VALUE_BYTES", "0")
	t.Setenv("AKKV_INLINE_THRESHOLD", "1024")
	t.Setenv("AKKV_CORS_ORIGINS", "https://a.example.com, https://b.example.com")

	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Listen != "127.0.0.1:9000" || c.DataDir != "/srv/akkv" || c.SystemDBPath != "/srv/akkv/custom-system.db" {
		t.Fatalf("unexpected config: %+v", c)
	}
	if c.SessionTTL != 2*time.Hour+30*time.Minute || c.CookieSecure {
		t.Fatalf("unexpected session config: %+v", c)
	}
	if c.MaxValueBytes != 0 || c.InlineThreshold != 1024 {
		t.Fatalf("unexpected size config: %+v", c)
	}
	if strings.Join(c.CORSOrigins, ",") != "https://a.example.com,https://b.example.com" {
		t.Fatalf("CORSOrigins = %v", c.CORSOrigins)
	}
}

func TestValidationErrors(t *testing.T) {
	cases := []struct {
		name string
		set  func(t *testing.T)
		want string
	}{
		{
			name: "short secret",
			set: func(t *testing.T) {
				t.Setenv("AKKV_SESSION_SECRET", "short")
			},
			want: "至少需要 16",
		},
		{
			name: "bad ttl",
			set: func(t *testing.T) {
				t.Setenv("AKKV_SESSION_TTL", "nope")
			},
			want: "AKKV_SESSION_TTL",
		},
		{
			name: "negative max",
			set: func(t *testing.T) {
				t.Setenv("AKKV_MAX_VALUE_BYTES", "-1")
			},
			want: "AKKV_MAX_VALUE_BYTES",
		},
		{
			name: "zero threshold",
			set: func(t *testing.T) {
				t.Setenv("AKKV_INLINE_THRESHOLD", "0")
			},
			want: "AKKV_INLINE_THRESHOLD",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clearEnv(t)
			tc.set(t)
			_, err := Load()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want contains %q", err, tc.want)
			}
		})
	}
}
