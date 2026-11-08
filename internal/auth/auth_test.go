package auth

import (
	"strings"
	"testing"
	"time"
)

func TestTokenGenerateHashVerify(t *testing.T) {
	token, err := GenerateToken()
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}
	if !strings.HasPrefix(token, TokenPrefix) {
		t.Fatalf("token = %q", token)
	}
	hash := HashToken(token)
	if len(hash) != 64 {
		t.Fatalf("hash length = %d", len(hash))
	}
	if !VerifyToken(token, hash) {
		t.Fatal("VerifyToken should accept the matching token")
	}
	if VerifyToken(token+"x", hash) {
		t.Fatal("VerifyToken should reject a different token")
	}
	if VerifyToken("", hash) {
		t.Fatal("VerifyToken should reject an empty token")
	}
}

func TestPassword(t *testing.T) {
	hash, err := HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if err := ValidatePasswordHash(hash); err != nil {
		t.Fatalf("ValidatePasswordHash: %v", err)
	}
	if !CheckPassword(hash, "correct horse battery staple") {
		t.Fatal("CheckPassword should accept the password")
	}
	if CheckPassword(hash, "wrong") {
		t.Fatal("CheckPassword should reject a wrong password")
	}
	if err := ValidatePasswordHash("not-bcrypt"); err == nil {
		t.Fatal("ValidatePasswordHash should reject a non-bcrypt hash")
	}
}

func TestCookieSigner(t *testing.T) {
	signer := NewCookieSigner([]byte("0123456789abcdef0123456789abcdef"))
	in := Session{ExpiresAt: time.Now().Add(time.Hour).Unix()}
	value, err := signer.Sign(in)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}

	var out Session
	if err := signer.Unsign(value, &out); err != nil {
		t.Fatalf("Unsign: %v", err)
	}
	if out != in {
		t.Fatalf("out = %+v, want %+v", out, in)
	}

	parts := strings.SplitN(value, ".", 2)
	first := "A"
	if strings.HasPrefix(parts[0], "A") {
		first = "B"
	}
	tampered := first + parts[0][1:] + "." + parts[1]
	if err := signer.Unsign(tampered, &out); err == nil {
		t.Fatal("Unsign should reject a tampered value")
	}

	other := NewCookieSigner([]byte("abcdef0123456789abcdef0123456789"))
	if err := other.Unsign(value, &out); err == nil {
		t.Fatal("Unsign should reject a value signed with another secret")
	}
}

func TestSessionAndFlashExpiry(t *testing.T) {
	s := NewSession(time.Hour)
	if !s.Valid() {
		t.Fatalf("session = %+v", s)
	}

	expired := Session{ExpiresAt: time.Now().Add(-time.Second).Unix()}
	if expired.Valid() {
		t.Fatal("expired session should be invalid")
	}

	f := NewFlash()
	if !f.Valid() {
		t.Fatal("new flash should be valid")
	}
	old := Flash{ExpiresAt: time.Now().Add(-time.Second).Unix()}
	if old.Valid() {
		t.Fatal("expired flash should be invalid")
	}
}
