// CSRF verification for anonymous-unsafe endpoints:
//
// Ports backend/apps/core/csrf.py
// cookie + X-CSRFToken header), with Django's exact rejection messages —
// all four causes probed in probes/0002-accounts and microprobes:
//
//	cookie absent (any header)      -> "CSRF Failed: CSRF cookie not set."
//	header absent/empty, cookie set -> "CSRF Failed: CSRF token missing."
//	header len not 32/64            -> "CSRF Failed: CSRF token from the
//	                                    'X-Csrftoken' HTTP header has
//	                                    incorrect length."
//	header ok length, mismatch      -> "CSRF Failed: CSRF token from the
//	                                    'X-Csrftoken' HTTP header incorrect."
//
// Django accepts either the bare 32-char secret (the SPA's echo) or a
// 64-char masked token (mask + cipher halves, alphabet-index arithmetic
// mod 62 — django/middleware/csrf.py _unmask_cipher_token).
package doory

import (
	"crypto/subtle"
	"errors"
	"net/http"
)

// CSRF failure messages (exact strings; the envelope code is always
// csrf_failed — apps/core/exceptions.py _is_csrf_failure).
var (
	errCSRFCookieNotSet = errors.New("CSRF Failed: CSRF cookie not set.")
	errCSRFTokenMissing = errors.New("CSRF Failed: CSRF token missing.")
	errCSRFTokenLength  = errors.New("CSRF Failed: CSRF token from the 'X-Csrftoken' HTTP header has incorrect length.")
	errCSRFTokenValue   = errors.New("CSRF Failed: CSRF token from the 'X-Csrftoken' HTTP header incorrect.")
)

const csrfAllowedChars = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

// verifyCSRF ports SessionAuthentication.enforce_csrf for one request:
// nil when the header proves knowledge of the cookie's secret, the exact
// Django error otherwise (checked cookie-first, as Django does).
func verifyCSRF(r *http.Request) error {
	cookie, err := r.Cookie(csrfCookieName)
	if err != nil || cookie.Value == "" {
		return errCSRFCookieNotSet
	}
	header := r.Header.Get("X-CSRFToken")
	if header == "" {
		return errCSRFTokenMissing
	}
	switch len(header) {
	case csrfSecretLen:
		if subtle.ConstantTimeCompare([]byte(header), []byte(cookie.Value)) == 1 {
			return nil
		}
		return errCSRFTokenValue
	case csrfSecretLen * 2: // masked form: mask + cipher
		if subtle.ConstantTimeCompare([]byte(unmaskCSRF(header)), []byte(cookie.Value)) == 1 {
			return nil
		}
		return errCSRFTokenValue
	default:
		return errCSRFTokenLength
	}
}

// unmaskCSRF ports _unmask_cipher_token: split halves, per-index
// (cipher + mask) mod alphabet — wait, Django's mask path computes
// chars[(x + y) % len] where x=secret index, y=mask index for ENCODING;
// decoding is chars[(x - y) % len] on the cipher half (see
// _unmask_cipher_token: chars[x - y], negative allowed in Python).
// unmaskCSRF ports _unmask_cipher_token: the cipher half is decoded as
// chars[(cipherIdx - maskIdx) mod 62] per index (Django's note: negative
// values are fine — Python modulo; we wrap explicitly). Encoding is
// chars[(secretIdx + maskIdx) mod 62] (_mask_cipher_secret).
func unmaskCSRF(token string) string {
	mask := token[:csrfSecretLen]
	cipher := token[csrfSecretLen:]
	out := make([]byte, csrfSecretLen)
	for i := 0; i < csrfSecretLen; i++ {
		cipherIdx := stringsIndexByte(csrfAllowedChars, cipher[i])
		maskIdx := stringsIndexByte(csrfAllowedChars, mask[i])
		if cipherIdx < 0 || maskIdx < 0 {
			return "" // charset violation -> never matches
		}
		out[i] = csrfAllowedChars[(cipherIdx-maskIdx+len(csrfAllowedChars))%len(csrfAllowedChars)]
	}
	return string(out)
}

func stringsIndexByte(s string, c byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == c {
			return i
		}
	}
	return -1
}
