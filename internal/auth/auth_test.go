package auth

import (
	"strings"
	"testing"
	"time"
)

func TestHashAndVerify(t *testing.T) {
	h, err := HashPassword("correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(h, "pbkdf2-sha256$600000$") {
		t.Fatalf("unexpected encoding %q", h)
	}
	if !VerifyPassword(h, "correct horse battery") {
		t.Fatal("correct password rejected")
	}
	if VerifyPassword(h, "correct horse batterY") {
		t.Fatal("wrong password accepted")
	}
	h2, _ := HashPassword("correct horse battery")
	if h == h2 {
		t.Fatal("hashes must be salted")
	}
	for _, bad := range []string{"", "plain", "pbkdf2-sha256$x$y$z", "md5$1$aa$bb", "pbkdf2-sha256$99999999$AA$AA"} {
		if VerifyPassword(bad, "whatever") {
			t.Fatalf("malformed hash %q verified", bad)
		}
	}
}

func TestValidateUsername(t *testing.T) {
	for _, ok := range []string{"admin", "dr00py", "a.b_c-d", strings.Repeat("x", 32)} {
		if err := ValidateUsername(ok); err != nil {
			t.Errorf("%q rejected: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "ab", strings.Repeat("x", 33), "admin' OR '1'='1", "<script>", "a b", "admin;--", "émile", "a\x00b", "adm\nin"} {
		if ValidateUsername(bad) == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestValidatePassword(t *testing.T) {
	if ValidatePassword("short") == nil {
		t.Error("short password accepted")
	}
	if ValidatePassword(strings.Repeat("a", MaxPasswordLen+1)) == nil {
		t.Error("overlong password accepted")
	}
	if ValidatePassword("pass\x00word1") == nil {
		t.Error("control char accepted")
	}
	if ValidatePassword("\xff\xfe\xfdabcdefgh") == nil {
		t.Error("invalid utf-8 accepted")
	}
	if err := ValidatePassword("pässwörd with spaces"); err != nil {
		t.Errorf("valid password rejected: %v", err)
	}
}

func TestSessionToken(t *testing.T) {
	tok, digest, err := NewSessionToken()
	if err != nil {
		t.Fatal(err)
	}
	if len(tok) != 43 {
		t.Fatalf("token length %d, want 43", len(tok))
	}
	if HashToken(tok) != digest || digest == tok {
		t.Fatal("digest mismatch")
	}
}

func TestLimiter(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	l := NewLimiter(3, time.Minute)
	l.now = func() time.Time { return now }
	for i := 0; i < 3; i++ {
		if ok, _ := l.Allowed("k"); !ok {
			t.Fatalf("locked after %d failures", i)
		}
		l.Fail("k")
	}
	if ok, _ := l.Allowed("k"); ok {
		t.Fatal("not locked after max failures")
	}
	if ok, _ := l.Allowed("other"); !ok {
		t.Fatal("unrelated key locked")
	}
	now = now.Add(61 * time.Second)
	if ok, _ := l.Allowed("k"); !ok {
		t.Fatal("still locked after window")
	}
	l.Fail("k")
	l.Reset("k")
	if ok, _ := l.Allowed("k"); !ok {
		t.Fatal("reset did not clear")
	}
}
