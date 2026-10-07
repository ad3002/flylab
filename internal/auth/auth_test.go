package auth_test

import (
	"strings"
	"testing"

	"github.com/ad3002/flylab/internal/auth"
)

func TestHashAndVerifyPassword(t *testing.T) {
	h, err := auth.HashPassword("correct horse battery")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	parts := strings.Split(h, "$")
	if len(parts) != 4 || parts[0] != "pbkdf2_sha256" || parts[1] != "210000" {
		t.Fatalf("unexpected hash format: %q", h)
	}
	if strings.Contains(h, "correct horse") {
		t.Fatalf("hash contains the plaintext password")
	}
	ok, err := auth.VerifyPassword("correct horse battery", h)
	if err != nil || !ok {
		t.Fatalf("expected the right password to verify, ok=%v err=%v", ok, err)
	}
	ok, err = auth.VerifyPassword("wrong password", h)
	if err != nil || ok {
		t.Fatalf("expected the wrong password to fail, ok=%v err=%v", ok, err)
	}
	h2, _ := auth.HashPassword("correct horse battery")
	if h2 == h {
		t.Fatalf("two hashes of the same password must differ (random salt)")
	}
}

func TestVerifyPasswordCorruptHashIsError(t *testing.T) {
	_, err := auth.VerifyPassword("whatever1", "md5$abc")
	if err == nil || !strings.Contains(err.Error(), "unknown format") {
		t.Fatalf("expected an unknown-format error, got %v", err)
	}
}

func TestNormalizeUsername(t *testing.T) {
	u, err := auth.NormalizeUsername("  Demo.User-1 ")
	if err != nil || u != "demo.user-1" {
		t.Fatalf("expected demo.user-1, got %q err=%v", u, err)
	}
	for _, bad := range []string{"ab", "has space", "юзер", strings.Repeat("a", 33), "semi;colon"} {
		if _, err := auth.NormalizeUsername(bad); err == nil {
			t.Fatalf("username %q should be rejected", bad)
		}
	}
}

func TestValidatePassword(t *testing.T) {
	if err := auth.ValidatePassword("short"); err == nil {
		t.Fatalf("7-char password must be rejected")
	}
	if err := auth.ValidatePassword(strings.Repeat("x", 129)); err == nil {
		t.Fatalf("129-char password must be rejected")
	}
	if err := auth.ValidatePassword("пароль12"); err != nil {
		t.Fatalf("8 Cyrillic characters must be accepted (count runes, not bytes): %v", err)
	}
}

func TestTokens(t *testing.T) {
	a, err := auth.NewToken()
	if err != nil {
		t.Fatalf("NewToken: %v", err)
	}
	b, _ := auth.NewToken()
	if a == b || len(a) != 43 {
		t.Fatalf("tokens must be unique 43-char base64url strings, got %q and %q", a, b)
	}
	if auth.HashToken(a) == a || len(auth.HashToken(a)) != 64 {
		t.Fatalf("HashToken must return 64 hex chars")
	}
}
