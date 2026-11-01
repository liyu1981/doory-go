// DRF-compatible rate throttles (G22).
//
// Upstream uses DRF's default AnonRateThrottle/UserRateThrottle on every API
// view, plus a handful of per-user scoped throttles. The port keeps the same
// fixed-window-ish counters in-process (D1: one process); identities are the
// user id for authenticated scopes and the client address for anonymous
// scopes, with X-Forwarded-For only trusted when NUM_PROXIES is configured.
package doory

import (
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// defaultThrottleRates mirrors REST_FRAMEWORK["DEFAULT_THROTTLE_RATES"].
func defaultThrottleRates() map[string]string {
	return map[string]string{
		"anon":             "240/min",
		"user":             "1200/min",
		"auth":             "20/min",
		"oidc":             "30/min",
		"upload":           "60/min",
		"stock":            "60/min",
		"video_upload":     "20/min",
		"stock_preview":    "300/min",
		"bulk_fill":        "120/min",
		"bulk_fill_images": "300/min",
		"export":           "30/min",
		"optimize":         "60/min",
		"optimize_offsite": "10/min",
		"ai":               "30/min",
	}
}

type throttleWindow struct {
	start time.Time
	count int
}

type throttleGuard struct {
	mu      sync.Mutex
	buckets map[string]*throttleWindow
}

func newThrottleGuard() *throttleGuard {
	return &throttleGuard{buckets: map[string]*throttleWindow{}}
}

// parseThrottleRate parses DRF's "<n>/<period>" form ("60/min", "200/h").
func parseThrottleRate(rate string) (limit int, duration time.Duration, ok bool) {
	rate = strings.TrimSpace(rate)
	parts := strings.SplitN(rate, "/", 2)
	if len(parts) != 2 {
		return 0, 0, false
	}
	limit, err := strconv.Atoi(strings.TrimSpace(parts[0]))
	if err != nil || limit < 0 {
		return 0, 0, false
	}
	switch strings.ToLower(strings.TrimSpace(parts[1])) {
	case "s", "sec", "second":
		duration = time.Second
	case "m", "min", "minute":
		duration = time.Minute
	case "h", "hour":
		duration = time.Hour
	case "d", "day":
		duration = 24 * time.Hour
	default:
		return 0, 0, false
	}
	return limit, duration, true
}

func (a *App) throttleRate(scope string) string {
	if a.Settings != nil && a.Settings.ThrottleRates != nil {
		if rate, ok := a.Settings.ThrottleRates[scope]; ok {
			return rate
		}
	}
	return defaultThrottleRates()[scope]
}

// allowThrottle charges one request for scope/identity. It returns the wait
// until the window resets when the limit is reached.
func (a *App) allowThrottle(scope, ident string) (bool, float64) {
	if ident == "" {
		return true, 0
	}
	limit, duration, ok := parseThrottleRate(a.throttleRate(scope))
	if !ok {
		return true, 0
	}
	if a.throttle == nil {
		a.throttle = newThrottleGuard()
	}
	a.throttle.mu.Lock()
	defer a.throttle.mu.Unlock()
	key := scope + ":" + ident
	now := time.Now()
	window := a.throttle.buckets[key]
	if window == nil || now.Sub(window.start) >= duration {
		a.throttle.buckets[key] = &throttleWindow{start: now, count: 1}
		return limit > 0, waitSeconds(duration, now)
	}
	if window.count >= limit {
		return false, waitSeconds(duration, window.start)
	}
	window.count++
	return true, 0
}

func waitSeconds(duration time.Duration, start time.Time) float64 {
	wait := time.Until(start.Add(duration)).Seconds()
	if wait < 0 {
		return 0
	}
	return wait
}

// clientIP is DRF's BaseThrottle.get_ident with NUM_PROXIES's opt-in trust.
func (a *App) clientIP(r *http.Request) string {
	if a.Settings != nil && a.Settings.NumProxies >= 0 {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			parts := strings.Split(xff, ",")
			if len(parts) > 0 {
				return strings.TrimSpace(parts[len(parts)-1])
			}
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil {
		return host
	}
	return r.RemoteAddr
}

func (a *App) throttleIdentity(r *http.Request, user *UserRow, scope string) string {
	switch scope {
	case "user":
		if user == nil {
			return ""
		}
		return user.ID
	case "anon":
		if user != nil {
			return ""
		}
		return a.clientIP(r)
	default:
		if user != nil {
			return user.ID
		}
		return a.clientIP(r)
	}
}

// allowRouteThrottles applies the default anon/user class plus the route's
// scoped classes. It writes the DRF Throttled envelope and returns false when
// any limit is reached.
func (a *App) allowRouteThrottles(w http.ResponseWriter, r *http.Request, route Route, user *UserRow) bool {
	method := strings.ToUpper(r.Method)
	noDefault := route.NoDefaultThrottle || (route.NoDefaultThrottleMethods != nil &&
		route.NoDefaultThrottleMethods[method])
	if !noDefault {
		scope := "anon"
		if user != nil {
			scope = "user"
		}
		if ok, wait := a.allowThrottle(scope, a.throttleIdentity(r, user, scope)); !ok {
			writeDRFThrottledDetail(w, wait, throttledDefault)
			return false
		}
	}
	scopes := append([]string(nil), route.ThrottleScopes...)
	scopes = append(scopes, route.MethodThrottleScopes[method]...)
	for _, scope := range scopes {
		if ok, wait := a.allowThrottle(scope, a.throttleIdentity(r, user, scope)); !ok {
			writeDRFThrottledDetail(w, wait, throttledDefault)
			return false
		}
	}
	return true
}

// chargeThrottle ports designs.fill.charge for calls made after a request has
// already passed its view throttles (one row/image, MCP tools).
func (a *App) chargeThrottle(user *UserRow, scope string) (bool, float64) {
	if user == nil {
		return true, 0
	}
	return a.allowThrottle(scope, user.ID)
}
