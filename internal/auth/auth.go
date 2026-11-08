package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// TokenPrefix 让项目 token 在日志和代码里容易识别。
const TokenPrefix = "kv_"

// GenerateToken 生成一个新的项目访问 token。
func GenerateToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return TokenPrefix + base64.RawURLEncoding.EncodeToString(b), nil
}

// HashToken 以 SHA-256 保存 token 摘要，数据库中不出现明文 token。
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// VerifyToken 使用恒定时间比较，避免时序侧信道。
func VerifyToken(token, hash string) bool {
	return subtle.ConstantTimeCompare([]byte(HashToken(token)), []byte(hash)) == 1
}

// HashPassword 生成 bcrypt 摘要，成本使用库的默认值。
func HashPassword(plain string) (string, error) {
	b, err := bcrypt.GenerateFromPassword([]byte(plain), bcrypt.DefaultCost)
	return string(b), err
}

// CheckPassword 校验管理员密码。
func CheckPassword(hash, plain string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(plain)) == nil
}

// ValidatePasswordHash 检查配置里的 hash 是否是合法 bcrypt 摘要。
func ValidatePasswordHash(hash string) error {
	_, err := bcrypt.Cost([]byte(hash))
	if err != nil {
		return errors.New("管理员密码摘要不是合法的 bcrypt 值")
	}
	return nil
}

// Session 是写入 cookie 的管理员会话内容。
type Session struct {
	ExpiresAt int64 `json:"exp"`
	Epoch     int64 `json:"epoch"`
}

// Valid 判断会话是否仍然有效。
func (s Session) Valid() bool {
	return time.Now().Unix() < s.ExpiresAt
}

// NewSession 创建一个会话。跨站请求由 SameSite=Strict + Origin/Referer 校验防护。
func NewSession(ttl time.Duration) Session {
	return Session{ExpiresAt: time.Now().Add(ttl).Unix()}
}

// NewSessionWithEpoch 创建带凭据版本的会话。
func NewSessionWithEpoch(ttl time.Duration, epoch int64) Session {
	return Session{ExpiresAt: time.Now().Add(ttl).Unix(), Epoch: epoch}
}

// Flash 是一次性提示，用于在重定向后显示 token 或错误信息。
type Flash struct {
	ExpiresAt int64  `json:"exp"`
	Message   string `json:"message,omitempty"`
	Error     string `json:"error,omitempty"`
	Token     string `json:"token,omitempty"`
}

// Valid 判断提示是否过期。
func (f Flash) Valid() bool {
	return time.Now().Unix() < f.ExpiresAt
}

// NewFlash 构造一个十分钟后过期的提示。
func NewFlash() Flash {
	return Flash{ExpiresAt: time.Now().Add(10 * time.Minute).Unix()}
}

// CookieSigner 用 HMAC-SHA256 对 cookie 值做签名。
type CookieSigner struct {
	secret []byte
}

// NewCookieSigner 创建签名器。
func NewCookieSigner(secret []byte) *CookieSigner {
	return &CookieSigner{secret: secret}
}

// Sign 序列化并签名任意值。
func (s *CookieSigner) Sign(v any) (string, error) {
	payload, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return s.sign(payload), nil
}

// Unsign 校验签名并反序列化。
func (s *CookieSigner) Unsign(value string, dst any) error {
	payload, err := s.unsign(value)
	if err != nil {
		return err
	}
	return json.Unmarshal(payload, dst)
}

func (s *CookieSigner) sign(payload []byte) string {
	mac := hmac.New(sha256.New, s.secret)
	mac.Write(payload)
	return base64.RawURLEncoding.EncodeToString(payload) + "." +
		base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (s *CookieSigner) unsign(value string) ([]byte, error) {
	payloadB64, sigB64, ok := strings.Cut(value, ".")
	if !ok {
		return nil, errors.New("cookie 格式错误")
	}
	payload, err := base64.RawURLEncoding.DecodeString(payloadB64)
	if err != nil {
		return nil, errors.New("cookie 负载错误")
	}
	sig, err := base64.RawURLEncoding.DecodeString(sigB64)
	if err != nil {
		return nil, errors.New("cookie 签名错误")
	}
	mac := hmac.New(sha256.New, s.secret)
	mac.Write(payload)
	if !hmac.Equal(sig, mac.Sum(nil)) {
		return nil, errors.New("cookie 签名不匹配")
	}
	return payload, nil
}
