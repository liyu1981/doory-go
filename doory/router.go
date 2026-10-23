// The kernel router: route registration, DRF-compatible Allow computation,
// error flows, and the CSRF/session/permission dispatch that mirrors
// DRF + CsrfProtectedMixin ordering (probed in probes/0002-accounts):
//
//	csrf "always" mode (CsrfProtectedMixin): CSRF runs BEFORE session
//	  consultation — csrf-failed responses carry no Vary: Cookie
//	  (probe steps 02/15: "Vary: origin" only).
//	csrf "when-authed" mode (SessionAuthentication): the session is read
//	  first (Vary: Cookie), then the token is enforced for authenticated
//	  unsafe requests only.
//	401 (not_authenticated) comes after both — an anonymous logout with a
//	  VALID token answers 401 (microprobe2).
//
// The Allow rule (source-verified against the pinned DRF/Django + probed):
//   - DRF responses ALWAYS carry Allow = ', '.join(allowed_methods)
//     (APIView.default_response_headers, rest_framework/views.py:159-161)
//   - @api_view(METHODS): http_method_names = METHODS∪{options} only
//     (rest_framework/decorators.py:46-47) => Allow = METHODS + OPTIONS
//   - APIView/ViewSet subclasses: DRF http_method_names order
//     [get post put patch delete head options trace], filtered by handler
//     existence, HEAD auto-aliases GET (django/views/generic/base.py:128-129)
package doory

import (
	"context"
	"net/http"
	"regexp"
	"strings"
)

// AllowStyle says how a route computes its Allow header.
type AllowStyle int

const (
	AllowAPIView AllowStyle = iota // @api_view(methods): METHODS + OPTIONS
	AllowClass                     // APIView/ViewSet: DRF order + head alias
)

// CsrfMode: when a route enforces cookie+header on unsafe methods.
type CsrfMode int

const (
	CSRFNone CsrfMode = iota
	// CSRFAlways: CsrfProtectedMixin — anonymous callers included.
	CSRFAlways
	// CSRFWhenAuthed: SessionAuthentication.enforce_csrf — authed only.
	CSRFWhenAuthed
)

// SessionMode: whether the session cookie is consulted (Vary: Cookie +
// lookup). Django's default authentication classes read the session on
// every request of such routes; authentication_classes=[] routes do not.
type SessionMode int

const (
	SessionNone SessionMode = iota
	SessionConsult
)

// RequireMode: when a session must exist (IsAuthenticated).
type RequireMode int

const (
	RequireNever RequireMode = iota
	RequireAlways
	RequireUnsafe // safe methods (GET/HEAD/OPTIONS) may be anonymous
)

var slugShape = regexp.MustCompile(`^[-a-zA-Z0-9_]+$`)

var uuidShape = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

type contextKey int

const userContextKey contextKey = 0
const routeParamsKey contextKey = 1

// routeParam returns a path-template parameter captured by dispatch.
func routeParam(r *http.Request, name string) string {
	m, _ := r.Context().Value(routeParamsKey).(map[string]string)
	return m[name]
}

// Route is one registered path. Path may be a template with {uuid}
// placeholders: a segment must match the placeholder's format (dashed
// lowercase uuid) or the route does NOT match and the request falls to
// handler404's spaced-json dialect — Django's resolver behaves the same
// (a non-uuid in <uuid:pk> never reaches the view). Extracted params land
// in the request context (routeParam).
type Route struct {
	Path    string
	Methods []string
	Style   AllowStyle
	// AllowNone marks a NON-DRF (Django) view: no Allow on success
	// (Django only sets Allow on 405) — e.g. /sitemap.xml.
	AllowNone bool
	// SharedHeaders applies the shared-view finalize headers (never-cache +
	// Vary Cookie/X-Share-Password) to EVERY response of this route —
	// DRF's finalize_response runs even for permission401s (probe0008
	// steps17/18/32).
	SharedHeaders bool
	Csrf          CsrfMode
	Session       SessionMode
	Require       RequireMode
	Handler       http.HandlerFunc
}

// currentUser returns the session-resolved user stashed by dispatch (nil
// for anonymous).
func currentUser(r *http.Request) *UserRow {
	u, _ := r.Context().Value(userContextKey).(*UserRow)
	return u
}

// AllowHeader computes this route's Allow value (evidence: probe 0001 +
// 0002: health "GET, OPTIONS", csrf "GET, HEAD, OPTIONS", me "GET, PATCH,
// DELETE, HEAD, OPTIONS").
func (r Route) AllowHeader() string {
	var methods []string
	switch r.Style {
	case AllowAPIView:
		for _, m := range r.Methods {
			methods = append(methods, strings.ToUpper(m))
		}
		methods = append(methods, "OPTIONS")
	case AllowClass:
		have := map[string]bool{}
		for _, m := range r.Methods {
			have[strings.ToLower(m)] = true
		}
		if have["get"] {
			have["head"] = true // View.setup: head = get
		}
		have["options"] = true // View.options always exists
		for _, m := range []string{"get", "post", "put", "patch", "delete", "head", "options", "trace"} {
			if have[m] {
				methods = append(methods, strings.ToUpper(m))
			}
		}
	}
	return strings.Join(methods, ", ")
}

// Router dispatches to registered routes.
type Router struct {
	app     *App
	routes  []Route
	allowed []string // CORS_ALLOWED_ORIGINS (prod env read; default: none)
}

func newRouter(a *App) *Router {
	return &Router{app: a, allowed: a.Settings.AllowedOrigins}
}

func (rt *Router) Register(routes ...Route) {
	rt.routes = append(rt.routes, routes...)
}

func (rt *Router) find(path string) (Route, bool) {
	for _, r := range rt.routes {
		if !strings.Contains(r.Path, "{") && r.Path == path {
			return r, true
		}
	}
	for _, r := range rt.routes {
		if strings.Contains(r.Path, "{") {
			if _, ok := matchTemplate(r.Path, path); ok {
				return r, true
			}
		}
	}
	return Route{}, false
}

// matchTemplate matches "/a/{uuid}/b" style paths, validating {uuid}
// segments as dashed lowercase uuids (Django's <uuid:...> converter).
func matchTemplate(pattern, path string) (map[string]string, bool) {
	// Segment split WITHOUT trimming trailing slashes: Django's
	// `<converter>/` patterns only match paths that END in "/" — a path
	// without the slash misses the route and CommonMiddleware's
	// APPEND_SLASH answers301 instead of the view (oracle probe0008
	// step18:301 vs a direct match). Split both sides identically: a
	// trailing "/" yields a final empty segment which must pair with the
	// pattern's own.
	pp := splitSegments(pattern)
	sp := splitSegments(path)
	if len(pp) != len(sp) {
		return nil, false
	}
	params := map[string]string{}
	for i := range pp {
		if strings.HasPrefix(pp[i], "{") && strings.HasSuffix(pp[i], "}") {
			name := pp[i][1 : len(pp[i])-1]
			// Django's slug converter [-a-zA-Z0-9_]+: other segments never
			// match and fall to handler404's dialect.
			if name == "slug" && !slugShape.MatchString(sp[i]) {
				return nil, false
			}
			// Django <uuid:...> converters: "uuid" params and every *_pk
			// param (design_pk, version_pk) reject non-uuid segments, so the
			// route does not match and handler404's dialect answers instead.
			if (name == "uuid" || strings.HasSuffix(name, "_pk")) && !uuidShape.MatchString(sp[i]) {
				return nil, false
			}
			params[name] = sp[i]
			continue
		}
		if pp[i] != sp[i] {
			return nil, false
		}
	}
	return params, true
}

// matchParams extracts template params for the matched route (called from
// dispatch after find; empty for exact routes).
// splitSegments splits a path or pattern into segments, keeping exactly the
// trailing empty element a trailing slash produces (one leading "/" is
// consumed, never interior slashes).
func splitSegments(p string) []string {
	p = strings.TrimPrefix(p, "/")
	segs := strings.Split(p, "/")
	if len(segs) > 1 && segs[len(segs)-1] == "" {
		// keep the empty tail: it represents the trailing slash
		// (Django pattern-vs-path fidelity)
		return segs
	}
	return segs
}

func matchParams(rt *Router, path string) map[string]string {
	for _, r := range rt.routes {
		if strings.Contains(r.Path, "{") {
			if params, ok := matchTemplate(r.Path, path); ok {
				return params
			}
		}
	}
	return nil
}

func (rt *Router) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	route, ok := rt.find(r.URL.Path)
	if !ok {
		rt.app.notFound(w, r)
		return
	}

	// DRF default_response_headers: Allow rides on every response of a DRF
	// route, including the error branches below. Django views (AllowNone)
	// never set it on success.
	if !route.AllowNone {
		w.Header().Set("Allow", route.AllowHeader())
	}
	if route.SharedHeaders {
		sharedHeaders(w)
	}

	if !route.hasMethod(r.Method) {
		writeDRF(w, http.StatusMethodNotAllowed, "method_not_allowed",
			`Method "`+strings.ToUpper(r.Method)+`" not allowed.`)
		return
	}

	if params := matchParams(rt, r.URL.Path); len(params) > 0 {
		r = r.WithContext(context.WithValue(r.Context(), routeParamsKey, params))
	}

	unsafe := r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodOptions

	// CSRF, always-mode first (CsrfProtectedMixin runs before everything in
	// initial()): a failure does NOT consult the session (no Vary: Cookie).
	if route.Csrf == CSRFAlways && unsafe {
		if err := verifyCSRF(r); err != nil {
			writeDRF(w, http.StatusForbidden, "csrf_failed", err.Error())
			return
		}
	}

	// Session consultation (default authentication classes read it).
	var user *UserRow
	if route.Session == SessionConsult {
		patchVary(w, "Cookie")
		var dead bool
		user, dead = rt.app.sessionUser(r)
		if dead {
			// Django flushes a stale session AND the response clears the
			// cookie (probe0002-parity step 20).
			clearSessionCookie(w, rt.app.Settings.SessionCookieSecure())
		}
	}
	if user != nil {
		r = r.WithContext(context.WithValue(r.Context(), userContextKey, user))
	}

	// CSRF, when-authed mode (SessionAuthentication.enforce_csrf).
	if route.Csrf == CSRFWhenAuthed && unsafe && user != nil {
		if err := verifyCSRF(r); err != nil {
			writeDRF(w, http.StatusForbidden, "csrf_failed", err.Error())
			return
		}
	}

	// IsAuthenticated.
	if user == nil &&
		(route.Require == RequireAlways || (route.Require == RequireUnsafe && unsafe)) {
		writeDRF(w, http.StatusUnauthorized, "not_authenticated",
			"Authentication credentials were not provided.")
		return
	}

	route.Handler(w, r)
}

func (r Route) hasMethod(m string) bool {
	for _, want := range r.Methods {
		if strings.EqualFold(want, m) {
			return true
		}
	}
	// View.options answers a bare OPTIONS on every route (CORS preflights
	// are intercepted by the cors layer before the router).
	return m == http.MethodOptions
}
