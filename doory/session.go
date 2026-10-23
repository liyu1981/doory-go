// Sessions: django_session-backed auth with Django's exact lifecycle rules,
// probed in probes/0002-accounts and verified against the pinned
// django.contrib.auth source:
//
//   - login(): same user + matching auth-hash in the existing session ->
//     REUSE the session key; different user or stale hash -> flush (new
//     empty session); anonymous session -> cycle_key (new key). Always
//     rotate_token (csrf secret) — probed: register/login responses carry a
//     fresh csrftoken AND the sessionid.
//   - update_session_auth_hash (password change): cycle_key + store the new
//     hash — probed: the change response sets a NEW sessionid, this session
//     survives, every other session of the user dies (auth-hash mismatch
//     on lookup -> anonymous).
//   - logout(): delete the row, clear the cookie WITHOUT HttpOnly
//     (probed: sessionid=""; Expires epoch; Max-Age=0; Path=/; SameSite=Lax).
//
// Cookie: sessionid, 32 chars [a-z0-9] (Django get_random_string(32,
// hex_lowercase-ish alphabet)), Max-Age=2592000 (base.py
// SESSION_COOKIE_AGE=30 days), Path=/, HttpOnly, SameSite=Lax, Secure only
// under prod SECURE_COOKIES (probe config: off).
//
// Session DATA format is Go-native JSON (not Django's signed serializer):
// interoperability with Django-written sessions stays open as divergence D5.
package doory

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
	"time"
)

const (
	sessionCookieName   = "sessionid"
	sessionKeyLen       = 32
	sessionCookieMaxAge = 2592000 // base.py SESSION_COOKIE_AGE (30 days)
	sessionCookiePath   = "/"
	sessionHashAlgo     = "sha256hex-of-encoded-password" // Go session data marker (D5)
)

// Session is one authenticated session row.
type Session struct {
	Key       string
	UserID    string
	AuthHash  string
	ExpiresAt time.Time
}

// sessionAuthHash mirrors AbstractBaseUser.get_session_auth_hash: sha256 hex
// over the ENCODED password — opaque to clients; observable only through
// change-persistence (same value after change) vs other-session death.
func sessionAuthHash(encodedPassword string) string {
	sum := sha256.Sum256([]byte(encodedPassword))
	return hex.EncodeToString(sum[:])
}

// newSessionKey: 32 chars from a-z0-9 (Django _get_new_session_key uses
// get_random_string(32, ...) with the lowercase alnum alphabet — probed
// sessionid values match: 5ov9afw4pj1w9c5od1mrebnyi063t7dh).
func newSessionKey() string {
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	rnd := make([]byte, sessionKeyLen)
	if _, err := rand.Read(rnd); err != nil {
		panic(err)
	}
	out := make([]byte, sessionKeyLen)
	for i, b := range rnd {
		out[i] = alphabet[int(b)%len(alphabet)]
	}
	return string(out)
}

// getSession reads a live session (row present + not expired).
func getSession(db *sql.DB, key string) (*Session, error) {
	row := db.QueryRow(
		`SELECT session_data, expire_date FROM django_session WHERE session_key = ?`, key)
	var data, expire string
	if err := row.Scan(&data, &expire); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	expires, err := parseDjangoDateTime(expire)
	if err != nil {
		return nil, nil // unparsable -> dead, like an expired row
	}
	if !time.Now().UTC().Before(expires) {
		return nil, nil
	}
	// Go-native payload: "<userID>|<authHash>|go-session-v1"
	parts := strings.SplitN(data, "|", 3)
	if len(parts) != 3 || parts[2] != "go-session-v1" {
		return nil, nil // a Django-written session is not readable (D5 open)
	}
	return &Session{Key: key, UserID: parts[0], AuthHash: parts[1], ExpiresAt: expires}, nil
}

func putSession(db *sql.DB, key, userID, authHash string) error {
	expires := time.Now().UTC().Add(sessionCookieMaxAge * time.Second)
	payload := userID + "|" + authHash + "|go-session-v1"
	_, err := db.Exec(`INSERT INTO django_session (session_key, session_data, expire_date)
		VALUES (?, ?, ?)
		ON CONFLICT(session_key) DO UPDATE SET session_data = excluded.session_data, expire_date = excluded.expire_date`,
		key, payload, formatDjangoDateTime(expires))
	return err
}

func deleteSession(db *sql.DB, key string) error {
	_, err := db.Exec(`DELETE FROM django_session WHERE session_key = ?`, key)
	return err
}

// cycleSessionKey mints a new key for the same content (cycle_key).
func cycleSessionKey(db *sql.DB, s *Session) (string, error) {
	newKey := newSessionKey()
	if err := deleteSession(db, s.Key); err != nil {
		return "", err
	}
	if err := putSession(db, newKey, s.UserID, s.AuthHash); err != nil {
		return "", err
	}
	return newKey, nil
}

// flushSession empties the session (different-user login): delete the row so
// the next save creates a fresh one.
func flushSession(db *sql.DB, key string) error { return deleteSession(db, key) }

// setSessionCookie sends the sessionid cookie exactly like Django's
// set_cookie(SESSION_COOKIE_NAME, ...) under the probed configuration
// (Secure=false when SECURE_COOKIES is off — settings.Features follow the
// caller: secure is passed in).
func setSessionCookie(w http.ResponseWriter, key string, secure bool) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    key,
		Path:     sessionCookiePath,
		MaxAge:   sessionCookieMaxAge,
		Expires:  time.Now().UTC().Add(sessionCookieMaxAge * time.Second),
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   secure,
	})
}

// clearSessionCookie ports Django's delete_cookie for logout: empty value,
// epoch expiry, Max-Age=0, SameSite kept, HttpOnly NOT set (probed).
func clearSessionCookie(w http.ResponseWriter, secure bool) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     sessionCookiePath,
		MaxAge:   -1, // net/http renders Max-Age=0 for negative, like Django
		Expires:  time.Unix(0, 0).UTC(),
		HttpOnly: false,
		SameSite: http.SameSiteLaxMode,
		Secure:   secure,
	})
}

// formatDjangoDateTime renders sqlite datetime storage: naive UTC,
// "2006-01-02 15:04:05.000000" (Django USE_TZ stores naive UTC on sqlite).
func formatDjangoDateTime(t time.Time) string {
	return t.UTC().Format("2006-01-02 15:04:05.000000")
}

// parseDjangoDateTime accepts Django's sqlite datetime forms (space or T
// separator, optional microseconds, optional Z/offset).
func parseDjangoDateTime(s string) (time.Time, error) {
	layouts := []string{
		"2006-01-02 15:04:05.000000",
		"2006-01-02 15:04:05.999999",
		"2006-01-02 15:04:05",
		"2006-01-02T15:04:05.000000Z",
		"2006-01-02T15:04:05Z",
	}
	for _, layout := range layouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("unparsable datetime %q", s)
}
