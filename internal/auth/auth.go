// Package auth provides password hashing, session tokens, credential
// validation and login throttling for the jellybird dashboard.
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
	"sync"
	"time"
	"unicode/utf8"
)

// PBKDF2-HMAC-SHA256 parameters (OWASP recommendation: 600k iterations).
const (
	hashIterations = 600_000
	saltBytes      = 16
	keyBytes       = 32
	hashScheme     = "pbkdf2-sha256"

	MinPasswordLen = 8
	// MaxPasswordLen caps input so an attacker can't make every login attempt
	// hash megabytes of data.
	MaxPasswordLen = 128
)

var usernameRE = regexp.MustCompile(`^[A-Za-z0-9._-]{3,32}$`)

// ValidateUsername accepts 3–32 characters from [A-Za-z0-9._-]. The strict
// allowlist means a username can never carry markup, quotes, whitespace or
// control characters into logs, templates or queries.
func ValidateUsername(u string) error {
	if !usernameRE.MatchString(u) {
		return errors.New("username must be 3–32 characters: letters, digits, '.', '_' or '-'")
	}
	return nil
}

// ValidatePassword enforces length bounds and valid UTF-8 with no control
// characters.
func ValidatePassword(p string) error {
	if !utf8.ValidString(p) {
		return errors.New("password contains invalid characters")
	}
	if utf8.RuneCountInString(p) < MinPasswordLen {
		return fmt.Errorf("password must be at least %d characters", MinPasswordLen)
	}
	if len(p) > MaxPasswordLen {
		return fmt.Errorf("password must be at most %d bytes", MaxPasswordLen)
	}
	for _, r := range p {
		if r < 0x20 || r == 0x7f {
			return errors.New("password contains control characters")
		}
	}
	return nil
}

// HashPassword returns an encoded "pbkdf2-sha256$iter$salt$key" string.
func HashPassword(password string) (string, error) {
	salt := make([]byte, saltBytes)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key, err := pbkdf2.Key(sha256.New, password, salt, hashIterations, keyBytes)
	if err != nil {
		return "", err
	}
	enc := base64.RawStdEncoding
	return strings.Join([]string{hashScheme, strconv.Itoa(hashIterations), enc.EncodeToString(salt), enc.EncodeToString(key)}, "$"), nil
}

// VerifyPassword reports whether password matches encoded, comparing the
// derived keys in constant time.
func VerifyPassword(encoded, password string) bool {
	if len(password) > MaxPasswordLen {
		return false
	}
	parts := strings.Split(encoded, "$")
	if len(parts) != 4 || parts[0] != hashScheme {
		return false
	}
	iter, err := strconv.Atoi(parts[1])
	if err != nil || iter < 1 || iter > 10_000_000 {
		return false
	}
	enc := base64.RawStdEncoding
	salt, err1 := enc.DecodeString(parts[2])
	want, err2 := enc.DecodeString(parts[3])
	if err1 != nil || err2 != nil || len(want) == 0 {
		return false
	}
	got, err := pbkdf2.Key(sha256.New, password, salt, iter, len(want))
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare(got, want) == 1
}

// dummyHash is verified against when a username doesn't exist, so a login
// for an unknown user costs the same time as one for a real user and can't
// be used to enumerate accounts.
var dummyHash = sync.OnceValue(func() string {
	h, err := HashPassword("jellybird-dummy-password")
	if err != nil {
		panic(err)
	}
	return h
})

// BurnVerify performs a throwaway verification to equalize timing.
func BurnVerify(password string) { _ = VerifyPassword(dummyHash(), password) }

// NewSessionToken returns a 256-bit random token (sent to the browser) and
// its SHA-256 hex digest (the only form stored in the database, so a leaked
// DB doesn't hand out live sessions).
func NewSessionToken() (token, digest string, err error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", "", err
	}
	token = base64.RawURLEncoding.EncodeToString(b)
	return token, HashToken(token), nil
}

// HashToken returns the stored digest for a session token.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// EqualSecret compares two secrets in constant time.
func EqualSecret(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// Limiter throttles failed logins per key (client IP, username). After
// maxFailures failures inside window the key is locked out for window.
type Limiter struct {
	maxFailures int
	window      time.Duration

	mu      sync.Mutex
	entries map[string]*limiterEntry
	now     func() time.Time
}

type limiterEntry struct {
	failures    int
	first       time.Time
	lockedUntil time.Time
}

// NewLimiter returns a limiter allowing maxFailures failures per window.
func NewLimiter(maxFailures int, window time.Duration) *Limiter {
	return &Limiter{maxFailures: maxFailures, window: window, entries: map[string]*limiterEntry{}, now: time.Now}
}

// Allowed reports whether key may attempt a login and, if not, how long
// until it may.
func (l *Limiter) Allowed(key string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	e := l.entries[key]
	if e == nil {
		return true, 0
	}
	if now := l.now(); now.Before(e.lockedUntil) {
		return false, e.lockedUntil.Sub(now)
	}
	return true, 0
}

// Fail records a failed attempt for key.
func (l *Limiter) Fail(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.gc(now)
	e := l.entries[key]
	if e == nil || now.Sub(e.first) > l.window {
		e = &limiterEntry{first: now}
		l.entries[key] = e
	}
	e.failures++
	if e.failures >= l.maxFailures {
		e.lockedUntil = now.Add(l.window)
	}
}

// Reset clears key after a successful login.
func (l *Limiter) Reset(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.entries, key)
}

// gc drops stale entries so the map can't grow without bound.
func (l *Limiter) gc(now time.Time) {
	if len(l.entries) < 1024 {
		return
	}
	for k, e := range l.entries {
		if now.Sub(e.first) > l.window && now.After(e.lockedUntil) {
			delete(l.entries, k)
		}
	}
}
