// Signed, expiring, single-use tokens for password reset and email
// verification (apps/accounts/tokens.py): Django PasswordResetTokenGenerators
// — an HMAC over user state, no database table, expiring after
// PASSWORD_RESET_TIMEOUT and dying when the covered state changes.
package doory

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"time"
)

const (
	passwordResetKeySalt = "django.contrib.auth.tokens.PasswordResetTokenGenerator"
	emailVerifyKeySalt   = "apps.accounts.tokens.EmailVerificationTokenGenerator"
	passwordResetTimeout = 24 * 60 * 60 // PASSWORD_RESET_TIMEOUT
)

// tokenEpoch is Django's timestamp origin: 2001-01-01, in local time
// (`datetime.now()` is naive local time upstream).
var tokenEpoch = time.Date(2001, 1, 1, 0, 0, 0, 0, time.Local)

// base36Encode ports django.utils.http.int_to_base36.
func base36Encode(i int64) string {
	const chars = "0123456789abcdefghijklmnopqrstuvwxyz"
	if i < 36 {
		return string(chars[i])
	}
	var parts []byte
	for i != 0 {
		n := i % 36
		i /= 36
		parts = append(parts, chars[n])
	}
	for l, r := 0, len(parts)-1; l < r; l, r = l+1, r-1 {
		parts[l], parts[r] = parts[r], parts[l]
	}
	return string(parts)
}

// base36Decode ports django.utils.http.base36_to_int (13-digit guard).
func base36Decode(s string) (int64, bool) {
	if len(s) > 13 || s == "" {
		return 0, false
	}
	value, err := strconv.ParseInt(s, 36, 64)
	if err != nil {
		return 0, false
	}
	return value, true
}

// saltedHMAC ports django.utils.crypto.salted_hmac with algorithm sha256:
// key = sha256(key_salt + secret), HMAC-SHA256(key, value).
func saltedHMAC(keySalt, value, secret string) string {
	key := sha256.Sum256([]byte(keySalt + secret))
	mac := hmac.New(sha256.New, key[:])
	mac.Write([]byte(value))
	return hex.EncodeToString(mac.Sum(nil))
}

// tokenSeconds is PasswordResetTokenGenerator._num_seconds.
func tokenSeconds(t time.Time) int64 { return int64(t.Sub(tokenEpoch).Seconds()) }

// resetTokenUserState is PasswordResetTokenGenerator._make_hash_value for the
// password reset generator: pk + password + last_login (microseconds
// truncated, naive UTC) + timestamp + email.
func resetTokenUserState(u *UserRow, timestamp int64) string {
	login := ""
	if u.LastLogin.Valid && u.LastLogin.String != "" {
		login = u.LastLogin.String
		if t, err := parseDjangoDateTime(login); err == nil {
			login = t.UTC().Format("2006-01-02 15:04:05")
		}
	}
	return fmt.Sprintf("%s%s%s%d%s", dashedUUID(u.ID), u.Password, login, timestamp, u.Email)
}

// emailVerifyUserState is EmailVerificationTokenGenerator._make_hash_value:
// pk + email + is_email_verified (Python's True/False) + timestamp.
func emailVerifyUserState(u *UserRow, timestamp int64) string {
	verified := "False"
	if u.IsEmailVerified {
		verified = "True"
	}
	return fmt.Sprintf("%s%s%s%d", dashedUUID(u.ID), u.Email, verified, timestamp)
}

// makeToken is _make_token_with_timestamp: "<base36 ts>-<every other hex char
// of the salted HMAC>".
func makeToken(keySalt, state string, timestamp int64, secret string) string {
	digest := saltedHMAC(keySalt, state, secret)
	short := make([]byte, 0, len(digest)/2)
	for i := 0; i < len(digest); i += 2 {
		short = append(short, digest[i])
	}
	return base36Encode(timestamp) + "-" + string(short)
}

// makePasswordResetToken ports password_reset_token_generator.make_token.
func (s *Settings) makePasswordResetToken(u *UserRow) string {
	ts := tokenSeconds(time.Now())
	return makeToken(passwordResetKeySalt, resetTokenUserState(u, ts), ts, s.SecretKey)
}

// makeEmailVerificationToken ports
// email_verification_token_generator.make_token.
func (s *Settings) makeEmailVerificationToken(u *UserRow) string {
	ts := tokenSeconds(time.Now())
	return makeToken(emailVerifyKeySalt, emailVerifyUserState(u, ts), ts, s.SecretKey)
}

// checkToken ports PasswordResetTokenGenerator.check_token (timestamp window
// + constant-time compare against the secret and its fallbacks).
func (s *Settings) checkToken(keySalt string, stateFunc func(int64) string, token string) bool {
	if token == "" {
		return false
	}
	parts := strings.SplitN(token, "-", 2)
	if len(parts) != 2 {
		return false
	}
	ts, ok := base36Decode(parts[0])
	if !ok {
		return false
	}
	if tokenSeconds(time.Now())-ts > passwordResetTimeout {
		return false
	}
	state := stateFunc(ts)
	secrets := append([]string{s.SecretKey}, s.SecretKeyFallbacks...)
	for _, secret := range secrets {
		candidate := makeToken(keySalt, state, ts, secret)
		if subtle.ConstantTimeCompare([]byte(candidate), []byte(token)) == 1 {
			return true
		}
	}
	return false
}

// checkPasswordResetToken ports password_reset_token_generator.check_token.
func (s *Settings) checkPasswordResetToken(u *UserRow, token string) bool {
	return s.checkToken(passwordResetKeySalt, func(ts int64) string {
		return resetTokenUserState(u, ts)
	}, token)
}

// checkEmailVerificationToken ports
// email_verification_token_generator.check_token.
func (s *Settings) checkEmailVerificationToken(u *UserRow, token string) bool {
	return s.checkToken(emailVerifyKeySalt, func(ts int64) string {
		return emailVerifyUserState(u, ts)
	}, token)
}

// encodeUID ports tokens.encode_uid (urlsafe base64 of the dashed uuid, with
// Django's padding kept).
func encodeUID(u *UserRow) string {
	return base64.URLEncoding.EncodeToString([]byte(dashedUUID(u.ID)))
}

// decodeUID ports tokens.decode_uid: an active user from an encoded uid, or
// nil for anything malformed or unknown.
func (a *App) decodeUID(uid string) *UserRow {
	raw, err := base64.URLEncoding.DecodeString(uid)
	if err != nil {
		if raw, err = base64.RawURLEncoding.DecodeString(uid); err != nil {
			return nil
		}
	}
	id := strings.ToLower(string(raw))
	if !uuidShape.MatchString(id) {
		return nil
	}
	u, err := getUserByID(a.DB, hexUUID(id))
	if err != nil || u == nil || !u.IsActive {
		return nil
	}
	return u
}
