// Package auth implements password hashing, session tokens and account input rules
// (contract docs/v2_contract.md section 2).
package auth

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	PBKDF2Iterations = 210000
	saltBytes        = 16
	keyBytes         = 32
	tokenBytes       = 32
	hashPrefix       = "pbkdf2_sha256"

	SessionLifetime = 30 * 24 * time.Hour
	CookieName      = "flylab_session"

	MinPasswordLen    = 8
	MaxPasswordLen    = 128
	MaxDisplayNameLen = 64
)

var (
	ErrInvalidUsername    = errors.New("username must be 3-32 characters of a-z, 0-9, '_', '.', '-'")
	ErrWeakPassword       = fmt.Errorf("password must be %d-%d characters", MinPasswordLen, MaxPasswordLen)
	ErrInvalidDisplayName = fmt.Errorf("display_name must be at most %d characters", MaxDisplayNameLen)

	usernameRe = regexp.MustCompile(`^[a-z0-9_.-]{3,32}$`)
)

// NormalizeUsername lowercases and trims the username and checks it against the rules.
func NormalizeUsername(raw string) (string, error) {
	u := strings.ToLower(strings.TrimSpace(raw))
	if !usernameRe.MatchString(u) {
		return "", ErrInvalidUsername
	}
	return u, nil
}

// ValidatePassword enforces the 8-128 character rule (counted in characters, not bytes).
func ValidatePassword(pw string) error {
	n := utf8.RuneCountInString(pw)
	if n < MinPasswordLen || n > MaxPasswordLen {
		return ErrWeakPassword
	}
	return nil
}

// NormalizeDisplayName trims the display name; empty means "use the username".
func NormalizeDisplayName(raw, username string) (string, error) {
	d := strings.TrimSpace(raw)
	if d == "" {
		return username, nil
	}
	if utf8.RuneCountInString(d) > MaxDisplayNameLen {
		return "", ErrInvalidDisplayName
	}
	return d, nil
}

// HashPassword returns pbkdf2_sha256$<iter>$<salt_b64>$<hash_b64>.
func HashPassword(pw string) (string, error) {
	salt := make([]byte, saltBytes)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("read random salt: %w", err)
	}
	key, err := pbkdf2.Key(sha256.New, pw, salt, PBKDF2Iterations, keyBytes)
	if err != nil {
		return "", fmt.Errorf("pbkdf2: %w", err)
	}
	return fmt.Sprintf("%s$%d$%s$%s", hashPrefix, PBKDF2Iterations,
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(key)), nil
}

// VerifyPassword compares pw with an encoded hash in constant time. A malformed stored hash
// is an error (not a plain "wrong password"), so a corrupt users row is visible.
func VerifyPassword(pw, encoded string) (bool, error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 4 || parts[0] != hashPrefix {
		return false, errors.New("stored password hash has an unknown format")
	}
	iter, err := strconv.Atoi(parts[1])
	if err != nil || iter < 1 || iter > 10_000_000 {
		return false, errors.New("stored password hash has an invalid iteration count")
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[2])
	if err != nil {
		return false, fmt.Errorf("stored password hash has an invalid salt: %w", err)
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[3])
	if err != nil || len(want) == 0 {
		return false, errors.New("stored password hash has an invalid key")
	}
	got, err := pbkdf2.Key(sha256.New, pw, salt, iter, len(want))
	if err != nil {
		return false, fmt.Errorf("pbkdf2: %w", err)
	}
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

// dummyHash is verified against when a username does not exist, so that login timing does
// not reveal which usernames are registered.
var dummyHash = func() string {
	h, err := HashPassword("flylab-dummy-password")
	if err != nil {
		panic(fmt.Sprintf("auth: cannot initialise dummy hash: %v", err))
	}
	return h
}()

// BurnPasswordCheck spends the same time as a real verification.
func BurnPasswordCheck(pw string) {
	_, _ = VerifyPassword(pw, dummyHash)
}

// NewToken returns a fresh 32-byte base64url session token.
func NewToken() (string, error) {
	b := make([]byte, tokenBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("read random token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// HashToken is the value stored in sessions.token_hash (hex SHA-256 of the token).
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
