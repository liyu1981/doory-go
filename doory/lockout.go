// Per-account password-attempt guard: after
// AUTH_LOCKOUT_MAX_ATTEMPTS (10, base.py) failed password checks for one
// account inside AUTH_LOCKOUT_WINDOW_SECONDS (900), refuse with HTTP 429
// until the window passes. State is process-local memory here where
// upstream uses the cache (CACHE_URL locmem by default): the observable
// contract — counts, threshold, window, 429 envelope — is scenario-proven;
// the storage is not (D6).
//
// The 429 message combines the detail with DRF's wait sentence (probed:
// "Too many failed attempts. Try again later. Expected available in 894
// seconds." + retry_after 894).
// Ports backend/apps/accounts/lockout.py
package doory

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"
)

const (
	lockoutMaxAttempts   = 10      // base.py AUTH_LOCKOUT_MAX_ATTEMPTS
	lockoutWindowSeconds = 15 * 60 // base.py AUTH_LOCKOUT_WINDOW_SECONDS
	lockoutDetail        = "Too many failed attempts. Try again later."
)

type lockoutState struct {
	count     int
	expiresAt time.Time
}

// lockoutGuard is the process-local PasswordAttemptGuard.
type lockoutGuard struct {
	mu     sync.Mutex
	states map[string]*lockoutState
}

func newLockoutGuard() *lockoutGuard {
	return &lockoutGuard{states: map[string]*lockoutState{}}
}

// fingerprint ports email_fingerprint: a keyed, non-reversible cache-key
// component over the lowercased trimmed email (Django salted_hmac; the
// exact HMAC algorithm is internal storage — D6 — what matters is
// stability per email and non-reversibility).
func fingerprint(email, purpose string) string {
	mac := hmac.New(sha256.New, []byte("doory.accounts."+purpose))
	mac.Write([]byte(strings.ToLower(strings.TrimSpace(email))))
	return hex.EncodeToString(mac.Sum(nil))
}

// live returns the current window state or nil (none / expired).
func (g *lockoutGuard) live(key string) *lockoutState {
	g.mu.Lock()
	defer g.mu.Unlock()
	s, ok := g.states[key]
	if !ok || !time.Now().Before(s.expiresAt) {
		if ok {
			delete(g.states, key)
		}
		return nil
	}
	return s
}

// check reports the remaining wait (0 = pass) like PasswordAttemptGuard.check.
func (g *lockoutGuard) check(email string) (throttled bool, waitSeconds float64) {
	s := g.live(fingerprint(email, "lockout"))
	if s != nil && s.count >= lockoutMaxAttempts {
		wait := s.expiresAt.Sub(time.Now()).Seconds()
		if wait < 1 {
			wait = 1
		}
		return true, wait
	}
	return false, 0
}

// recordFailure bumps the window count.
func (g *lockoutGuard) recordFailure(email string) {
	key := fingerprint(email, "lockout")
	g.mu.Lock()
	defer g.mu.Unlock()
	now := time.Now()
	s, ok := g.states[key]
	if !ok || !now.Before(s.expiresAt) {
		s = &lockoutState{expiresAt: now.Add(lockoutWindowSeconds * time.Second)}
		g.states[key] = s
	}
	s.count++
}

// reset clears the window (successful password check).
func (g *lockoutGuard) reset(email string) {
	key := fingerprint(email, "lockout")
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.states, key)
}

// throttledMessage renders the probed 429 message: detail + DRF's wait
// sentence with a ceil'd integer (probes/0002-accounts step 35).
func throttledMessage(waitSeconds float64) string {
	return throttledMessageDetail(lockoutDetail, waitSeconds)
}

// throttledMessageDetail: "<detail> Expected available in <ceil> seconds."
// (DRF: detail + get_wait_message).
func throttledMessageDetail(detail string, waitSeconds float64) string {
	return fmt.Sprintf("%s Expected available in %d seconds.", detail, int(math.Ceil(waitSeconds)))
}
