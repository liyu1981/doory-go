// Accounts-family unit evidence (M2): probe-anchored assertions whose
// failures prove specific ported semantics. Each test names the mutation
// that would break it — mutation-proven, per the methodology (test file
// dispositions in ledger/test-mapping-v*.json reference these).
package doory

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// newTestRequest builds a request carrying one csrf cookie and/or header.
func newTestRequest(cookie, header string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login/", nil)
	if cookie != "" {
		r.AddCookie(&http.Cookie{Name: csrfCookieName, Value: cookie})
	}
	if header != "" {
		r.Header.Set("X-CSRFToken", header)
	}
	return r
}

// PROBE-ANCHORED cross-implementation proof: this is a REAL password hash
// read from the pinned oracle's database after a probed register
// (probes/0002-accounts/password-hashes.txt, alice, plaintext
// "Tr0ub4dor&3xkcd9"). Mutation: change pbkdf2Iterations, the block
// function, the encoded format, or hasUsablePassword — this test fails
// while a Go-only round trip could still pass. It proves Go verifies
// Django-created users (D5 groundwork).
func TestVerifyProbedDjangoHash(t *testing.T) {
	const probed = "pbkdf2_sha256$1500000$GKwsGdcksunGaaJ8Kt9v3c$v9phHAHaV1uMPi6XIJnwq0FdV8pQJr+7DkHSRqjHxxI="
	if !verifyPassword("Tr0ub4dor&3xkcd9", probed) {
		t.Fatal("probed Django hash rejected — Go password verification does not match the oracle")
	}
	if verifyPassword("wrongpass123", probed) {
		t.Fatal("wrong password accepted")
	}
	if !hasUsablePassword(probed) {
		t.Fatal("pbkdf2 hash must be usable")
	}
	if hasUsablePassword("!random-unusable") {
		t.Fatal("unusable marker must be rejected")
	}
}

// Password validator messages/codes in settings order — the exact strings
// probes/0002-accounts steps 05-07 asserted from the oracle. Mutation:
// reorder validators, drop one, or alter a message -> the slices mismatch.
func TestPasswordValidatorMessages(t *testing.T) {
	cases := []struct {
		password string
		email    string
		want     []PasswordError
	}{
		{"short", "nope2@example.com", []PasswordError{
			{"This password is too short. It must contain at least 8 characters.", "password_too_short"},
		}},
		{"12345678", "nope3@example.com", []PasswordError{
			{"This password is too common.", "password_too_common"},
			{"This password is entirely numeric.", "password_entirely_numeric"},
		}},
		{"password", "erin@example.com", []PasswordError{
			{"This password is too common.", "password_too_common"},
		}},
		// Similarity: the full email as password gives quick_ratio 1.0
		// against the whole-attribute part (>= 0.7).
		{"carol@example.com", "carol@example.com", []PasswordError{
			{"The password is too similar to the email.", "password_too_similar"},
		}},
		// The probed happy password passes every validator.
		{"Tr0ub4dor&3xkcd9", "alice@example.com", nil},
	}
	for _, c := range cases {
		got := validateNewPassword(c.password, c.email)
		if len(got) != len(c.want) {
			t.Errorf("%q: got %d errors %+v, want %d %+v", c.password, len(got), got, len(c.want), c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("%q[%d]: got %+v, want %+v", c.password, i, got[i], c.want[i])
			}
		}
	}
}

// The four probe-proven csrf rejection messages (microprobes +
// probes/0002-accounts steps 02/10/12/15). Mutation: change message
// ordering/wording or the length rule -> exact-string comparison fails.
func TestVerifyCSRFMessages(t *testing.T) {
	mustRequest := func(cookie, header string) error {
		r := newTestRequest(cookie, header)
		return verifyCSRF(r)
	}
	if err := mustRequest("", "anything123"); err != errCSRFCookieNotSet {
		t.Errorf("no cookie: got %v", err)
	}
	if err := mustRequest("abc123", ""); err != errCSRFTokenMissing {
		t.Errorf("no header: got %v", err)
	}
	if err := mustRequest("abc123", "tooshort"); err != errCSRFTokenLength {
		t.Errorf("bad length: got %v", err)
	}
	if err := mustRequest("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa!", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"); err != errCSRFTokenValue {
		t.Errorf("mismatch: got %v", err)
	}
	if err := mustRequest("abcdefghijklmnopqrstuvwxyz012345", "abcdefghijklmnopqrstuvwxyz012345"); err != nil {
		t.Errorf("bare secret match must pass: %v", err)
	}
	// Masked form: mask+"cipher" decoding to the cookie secret
	secret := "abcdefghijklmnopqrstuvwxyz012345"
	masked := maskCSRFSecret(secret)
	if err := mustRequest(secret, masked); err != nil {
		t.Errorf("masked token must pass: %v (masked=%q)", err, masked)
	}
}

// maskCSRFSecret encodes like Django _mask_cipher_secret: cipher[i] =
// chars[(secretIdx + maskIdx) % 62].
func maskCSRFSecret(secret string) string {
	mask := "0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"
	out := make([]byte, len(secret))
	for i := 0; i < len(secret); i++ {
		s := stringsIndexByte(csrfAllowedChars, secret[i])
		m := stringsIndexByte(csrfAllowedChars, mask[i%len(mask)])
		out[i] = csrfAllowedChars[(s+m)%len(csrfAllowedChars)]
	}
	return mask[:len(secret)] + string(out)
}

// Lockout threshold: exactly 10 failures pass the check, the 11th is
// throttled with a wait sentence (probe run B: attempts 1-10 => 400,
// attempt 11 => 429). Mutation: off-by-one in the >= comparison or the
// window -> this fails.
func TestLockoutThreshold(t *testing.T) {
	g := newLockoutGuard()
	for i := 0; i < lockoutMaxAttempts; i++ {
		if throttled, _ := g.check("x@example.com"); throttled {
			t.Fatalf("throttled after only %d failures", i)
		}
		g.recordFailure("x@example.com")
	}
	throttled, wait := g.check("x@example.com")
	if !throttled {
		t.Fatal("not throttled after max attempts")
	}
	if wait < 1 || wait > lockoutWindowSeconds {
		t.Errorf("wait out of window: %v", wait)
	}
	msg := throttledMessage(wait)
	if want := "Too many failed attempts. Try again later. Expected available in "; !hasPrefixStr(msg, want) {
		t.Errorf("message %q missing prefix %q", msg, want)
	}
	// A successful check resets (probe: guard.reset on correct password).
	g.reset("x@example.com")
	if throttled, _ := g.check("x@example.com"); throttled {
		t.Fatal("still throttled after reset")
	}
	// Different accounts are independent (per-account guard).
	if throttled, _ := g.check("other@example.com"); throttled {
		t.Fatal("unrelated account throttled")
	}
}

func hasPrefixStr(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}
