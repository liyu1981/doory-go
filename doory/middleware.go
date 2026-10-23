// The middleware stack, mirroring Django's MIDDLEWARE list order from
// backend/doory/settings/base.py. Django builds the entire response in
// memory and the WSGI server sends it after the middleware chain returns —
// buffer.go gives this port the same model, so late layers (cors's Vary
// merge) and setdefault semantics behave exactly like the oracle.
//
// Chain (outermost first):
//
//	#1  SecurityMiddleware: nosniff, Referrer-Policy, COOP
//	#3  cors: Vary: origin merged AFTER the inner chain runs; preflight is
//	    answered inside the router, skipping #9..#12 — which is why oracle
//	    preflight responses lack Permissions-Policy / X-Frame-Options /
//	    Cache-Control (probe 0001 step 07)
//	#9  XFrameOptionsMiddleware: X-Frame-Options: DENY (setdefault)
//	#11 PermissionsPolicyMiddleware: Permissions-Policy (setdefault)
//	#12 PrivateApiCacheMiddleware: /api/ without Cache-Control -> no-store
//	  — handler (innermost)
//
// Commit order = inner-first: Cache-Control (#12), Permissions-Policy (#11),
// X-Frame-Options (#9), then cors merges Vary (#3), then security (#1) —
// exactly probe 0001's header order.
package doory

import (
	"bufio"
	"net"
	"net/http"
	"strings"
)

const defaultPermissionsPolicy = "accelerometer=(), bluetooth=(), browsing-topics=(), camera=(), geolocation=(), gyroscope=(), magnetometer=(), microphone=(), payment=(), serial=(), usb=()"

// securityHeaders ports SecurityMiddleware: SECURE_CONTENT_TYPE_NOSNIFF
// (default True), SECURE_REFERRER_POLICY (default
// "strict-origin-when-cross-origin") and
// SECURE_CROSS_ORIGIN_OPENER_POLICY (default same-origin). HSTS and the SSL
// redirect are prod/TLS concerns behind a proxy (D1).
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		next.ServeHTTP(w, r)
	})
}

// patchVary ports django's patch_vary_headers: append a token to an existing
// Vary (comma-joined, insertion order) or set it fresh; no duplicates.
// corsheaders passes lowercase 'origin'; session/CSRF middleware 'Cookie'.
func patchVary(w http.ResponseWriter, token string) {
	existing := w.Header().Get("Vary")
	if existing == "" {
		w.Header().Set("Vary", token)
		return
	}
	for _, part := range splitComma(existing) {
		if part == token {
			return
		}
	}
	w.Header().Set("Vary", existing+", "+token)
}

func splitComma(s string) []string {
	var out []string
	start := 0
	for i := 0; i <= len(s); i++ {
		if i == len(s) || s[i] == ',' {
			out = append(out, trimSpace(s[start:i]))
			start = i + 1
		}
	}
	return out
}

func trimSpace(s string) string {
	for len(s) > 0 && (s[0] == ' ' || s[0] == '\t') {
		s = s[1:]
	}
	for len(s) > 0 && (s[len(s)-1] == ' ' || s[len(s)-1] == '\t') {
		s = s[:len(s)-1]
	}
	return s
}

// setdefaultWriter adds a header when the response commits unless the
// handler produced one already (Django's response.headers.setdefault).
type setdefaultWriter struct {
	http.ResponseWriter
	key, value string
	called     bool
}

func (s *setdefaultWriter) WriteHeader(code int) {
	if !s.called && s.Header().Get(s.key) == "" {
		s.Header().Set(s.key, s.value)
	}
	s.called = true
	s.ResponseWriter.WriteHeader(code)
}

func (s *setdefaultWriter) Write(b []byte) (int, error) {
	if !s.called {
		s.WriteHeader(http.StatusOK)
	}
	return s.ResponseWriter.Write(b)
}

// Hijack/Flush delegate so the WebSocket upgrade sees an unbuffered writer
// (the wrapper would otherwise hide http.Hijacker — probe0012).
func (s *setdefaultWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if h, ok := s.ResponseWriter.(http.Hijacker); ok {
		return h.Hijack()
	}
	return nil, nil, http.ErrNotSupported
}

func (s *setdefaultWriter) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// corsLayer ports django-cors-headers: preflights are answered HERE,
// short-circuiting #9-#12 (their process_response never runs — probe 0001
// step 07), and every response merges Vary: origin after the inner chain.
func corsLayer(s *Settings) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodOptions && r.Header.Get("Origin") != "" &&
				r.Header.Get("Access-Control-Request-Method") != "" {
				servePreflight(w, r, s.AllowedOrigins)
				return
			}
			next.ServeHTTP(w, r)
			patchVary(w, "origin")
		})
	}
}

// servePreflight ports the django-cors-headers preflight path for the
// default (disallowed-origin) configuration: empty 200, Content-Type
// text/html, only the SecurityMiddleware headers + Vary: origin.
func servePreflight(w http.ResponseWriter, r *http.Request, allowed []string) {
	origin := r.Header.Get("Origin")
	allowedHere := false
	for _, a := range allowed {
		if a == origin {
			allowedHere = true
		}
	}
	if allowedHere {
		// Allowed-origin responses need an oracle probe with
		// CORS_ALLOWED_ORIGINS configured first (PLAN S4): deliberately
		// unimplemented rather than guessed.
		writeDRF(w, http.StatusInternalServerError, "server_error",
			"allowed-origin preflight not yet ported")
		return
	}
	patchVary(w, "origin")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Length", "0")
	w.WriteHeader(http.StatusOK)
}

// privateAPICache ports PrivateApiCacheMiddleware (only /api/ paths).
func privateAPICache(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/") {
			next.ServeHTTP(w, r)
			return
		}
		sw := &setdefaultWriter{ResponseWriter: w, key: "Cache-Control", value: "no-store"}
		next.ServeHTTP(sw, r)
	})
}

// xFrameOptions ports XFrameOptionsMiddleware (X_FRAME_OPTIONS default DENY).
func xFrameOptions(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sw := &setdefaultWriter{ResponseWriter: w, key: "X-Frame-Options", value: "DENY"}
		next.ServeHTTP(sw, r)
	})
}

// permissionsPolicy ports PermissionsPolicyMiddleware (setdefault; the
// settings override PERMISSIONS_POLICY is not env-read upstream either).
func permissionsPolicy(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sw := &setdefaultWriter{ResponseWriter: w, key: "Permissions-Policy", value: defaultPermissionsPolicy}
		next.ServeHTTP(sw, r)
	})
}

// appendSlashRedirect ports CommonMiddleware's APPEND_SLASH: GET/HEAD on an
// unrouted path whose slashed form IS routed answers a bodyless301 —
// short-circuiting every layer below it (probe0008 step18: no
// Permissions-Policy/X-Frame-Options/Cache-Control on the redirect, unlike
// a router-level redirect).
func appendSlashRedirect(rt *Router) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if (r.Method == http.MethodGet || r.Method == http.MethodHead) &&
				!strings.HasSuffix(r.URL.Path, "/") {
				_, selfRouted := rt.find(r.URL.Path)
				if !selfRouted {
					if _, routed := rt.find(r.URL.Path + "/"); routed {
						target := r.URL.Path + "/"
						if r.URL.RawQuery != "" {
							target += "?" + r.URL.RawQuery
						}
						w.Header().Set("Location", target)
						w.Header().Set("Content-Type", "text/html; charset=utf-8")
						w.Header().Set("Content-Length", "0")
						w.WriteHeader(http.StatusMovedPermanently)
						return
					}
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}

// buildStack assembles the chain (outermost first: security, cors, xframe,
// permissions, cache; response commits inner-first).
// buildStack assembles the chain (outermost first: security, cors,
// append-slash (#5 — short-circuits #6..#12 like CommonMiddleware),
// xframe, permissions, cache; response commits inner-first). `rt` is also
// the final handler.
func buildStack(rt *Router, s *Settings) http.Handler {
	var final http.Handler = rt
	h := privateAPICache(final)    // #12 innermost
	h = permissionsPolicy(h)       // #11 wraps #12
	h = mediaSecurityHeaders(h)    // #10 wraps #11 (media paths only)
	h = xFrameOptions(h)           // #9 wraps #10
	h = appendSlashRedirect(rt)(h) // #5 (short-circuits #6..#12)
	h = corsLayer(s)(h)            // #3 wraps the rest
	h = securityHeaders(h)         // #1 outermost
	return h
}

// mediaSecurityHeaders ports apps/assets/middleware.
// MediaSecurityHeadersMiddleware: responses under /media/ get a hard nosniff
// (SET, not setdefault — Django overwrites) plus a sandboxing CSP; SVG
// responses add Content-Disposition: attachment. Sits at MIDDLEWARE #10:
// after Permissions-Policy (#11), before X-Frame-Options (#9) — probe0007
// step18 (media 404) shows the header set.
func mediaSecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sw := &mediaHeaderWriter{ResponseWriter: w, path: r.URL.Path}
		next.ServeHTTP(sw, r)
	})
}

type mediaHeaderWriter struct {
	http.ResponseWriter
	path string
	done bool
}

const mediaCSP = "sandbox; default-src 'none'; style-src 'unsafe-inline'"

func (m *mediaHeaderWriter) WriteHeader(code int) {
	if !m.done && strings.HasPrefix(m.path, "/media/") {
		h := m.Header()
		h.Set("X-Content-Type-Options", "nosniff") // overwrite semantics
		h.Set("Content-Security-Policy", mediaCSP)
		if strings.HasPrefix(h.Get("Content-Type"), "image/svg") {
			h.Set("Content-Disposition", "attachment")
		}
	}
	m.done = true
	m.ResponseWriter.WriteHeader(code)
}

func (m *mediaHeaderWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if h, ok := m.ResponseWriter.(http.Hijacker); ok {
		return h.Hijack()
	}
	return nil, nil, http.ErrNotSupported
}

func (m *mediaHeaderWriter) Flush() {
	if f, ok := m.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (m *mediaHeaderWriter) Write(b []byte) (int, error) {
	if !m.done {
		m.WriteHeader(http.StatusOK)
	}
	return m.ResponseWriter.Write(b)
}
