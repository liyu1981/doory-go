// OpenID Connect endpoints (0.2.0, apps/accounts/oidc.py + accounts/views.py).
//
// The port supports the observable behaviour of a server WITHOUT a provider
// configured — the default, and what /config/ advertises (features.oidc_login
// = false): the two public GETs redirect back to the sign-in page with
// `sso_error=<code>` (as the oracle's `_login_failed` does), the connect
// endpoint answers `oidc_not_configured`, and disconnecting removes the
// stored identity. The provider flows themselves (discovery, PKCE, JWKS,
// token exchange, session flows, sign-in/connect services) are pending;
// with a provider configured the port answers a loud notPorted error instead
// of pretending. PORT_MAP: backend/apps/accounts/oidc.py 🟡,
// backend/apps/accounts/views.py pending list.
package doory

import (
	"net/http"
	"net/url"
	"strings"
	"unicode/utf8"
)

const (
	oidcLoginPath    = "/login"     // views.LOGIN_PATH
	oidcSettingsPath = "/settings"  // views.SETTINGS_PATH
	oidcDefaultNext  = "/dashboard" // views.DEFAULT_NEXT

	oidcUnavailable = "oidc_unavailable" // oidc.UNAVAILABLE
	oidcExpired     = "oidc_expired"     // oidc.EXPIRED

	maxNextPathLength = 2000
)

// safeNextPath ports accounts/views.safe_next_path: `value` when it is an
// in-app path (no scheme, host or control characters), else the dashboard.
func safeNextPath(value string) string {
	if utf8.RuneCountInString(value) > maxNextPathLength {
		return oidcDefaultNext
	}
	if !strings.HasPrefix(value, "/") || strings.HasPrefix(value, "//") || strings.HasPrefix(value, "/\\") {
		return oidcDefaultNext
	}
	for _, r := range value {
		if r < 32 || r == 127 {
			return oidcDefaultNext
		}
	}
	return value
}

// oidcAppRedirect ports views._app_redirect: SITE_URL (without a trailing
// slash) + the in-app path + the query, in the caller's parameter order.
func (a *App) oidcAppRedirect(w http.ResponseWriter, r *http.Request, path string, params [][2]string) {
	target := strings.TrimRight(a.Settings.SiteURL, "/") + path
	if len(params) > 0 {
		var pairs []string
		for _, param := range params {
			pairs = append(pairs, url.QueryEscape(param[0])+"="+url.QueryEscape(param[1]))
		}
		target += "?" + strings.Join(pairs, "&")
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Location", target)
	w.WriteHeader(http.StatusFound)
}

// oidcLoginFailed ports views._login_failed: back to the sign-in page with
// the error, still knowing where the user was going.
func (a *App) oidcLoginFailed(w http.ResponseWriter, r *http.Request, code, nextPath string) {
	params := [][2]string{{"sso_error", code}}
	if nextPath != oidcDefaultNext {
		params = append(params, [2]string{"next", nextPath})
	}
	a.oidcAppRedirect(w, r, oidcLoginPath, params)
}

// oidcStart ports OidcStartView.get for a server without a provider: the
// sign-in cannot start, so the caller lands back on /login.
func (a *App) oidcStart(w http.ResponseWriter, r *http.Request) {
	nextPath := safeNextPath(r.URL.Query().Get("next"))
	if !a.Settings.oidcEnabled() {
		a.oidcLoginFailed(w, r, oidcUnavailable, nextPath)
		return
	}
	// A provider is configured: begin() (discovery + PKCE + session flow) is
	// unported. Say so instead of redirecting somewhere that cannot work.
	writeDRF(w, http.StatusInternalServerError, "server_error",
		"route registered but not ported: oidc.begin (accounts/oidc.py pending)")
}

// oidcCallback ports OidcCallbackView.get for a server without a provider:
// no flow was ever stored, so `take_flow` finds nothing and the oracle
// answers `oidc_expired` (its message goes to the log only).
func (a *App) oidcCallback(w http.ResponseWriter, r *http.Request) {
	if !a.Settings.oidcEnabled() {
		// take_flow(session, state) always stores the (here empty) flow map
		// back, so Django's session is modified and the middleware re-sends
		// the cookie — creating an anonymous session when the request has
		// none (a logged-out browser). The flows themselves are unported.
		a.touchSessionForFlow(w, r)
		a.oidcLoginFailed(w, r, oidcExpired, oidcDefaultNext)
		return
	}
	writeDRF(w, http.StatusInternalServerError, "server_error",
		"route registered but not ported: oidc.complete (accounts/oidc.py pending)")
}

// touchSessionForFlow reproduces the session side effect of
// oidc.take_flow(): the session is written, so the response carries the
// sessionid cookie (the same key for a live session; a fresh anonymous one
// otherwise).
func (a *App) touchSessionForFlow(w http.ResponseWriter, r *http.Request) {
	key := ""
	if cookie, err := r.Cookie("sessionid"); err == nil {
		key = cookie.Value
	}
	if key != "" {
		if existing, err := getSession(a.DB, key); err == nil && existing != nil {
			setSessionCookie(w, existing.Key, a.Settings.SecureCookies)
			return
		}
	}
	key = newSessionKey()
	// The flow map is the only payload: no user, no auth hash (anonymous
	// session, like Django's session_data for an empty dict).
	_ = putSession(a.DB, key, "", "")
	setSessionCookie(w, key, a.Settings.SecureCookies)
}

// oidcConnection ports OidcConnectionView: POST starts connecting an OIDC
// account (a provider is needed), DELETE disconnects it.
func (a *App) oidcConnection(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	switch r.Method {
	case http.MethodPost:
		if !a.Settings.oidcEnabled() {
			writeDRF(w, http.StatusBadRequest, "oidc_not_configured",
				"Single sign-on is not configured on this server.")
			return
		}
		// confirm_identity + oidc.begin are unported (pending with the flows).
		writeDRF(w, http.StatusInternalServerError, "server_error",
			"route registered but not ported: oidc.begin (accounts/oidc.py pending)")
	case http.MethodDelete:
		// disconnect_oidc: refused when that would leave the account no way in.
		if !hasUsablePassword(u.Password) && !(u.GoogleSub.Valid && u.GoogleSub.String != "") {
			writeDRF(w, http.StatusBadRequest, "last_sign_in_method",
				"Set a password first, or you won't be able to sign in.")
			return
		}
		if _, err := a.DB.Exec(`DELETE FROM accounts_oidcidentity WHERE user_id = ?`, u.ID); err != nil {
			logerr(err, "oidcConnection.delete")
			writeDRF(w, http.StatusInternalServerError, "server_error", "Something went wrong on our side.")
			return
		}
		reloaded, err := getUserByID(a.DB, u.ID)
		if err != nil || reloaded == nil {
			writeDRF(w, http.StatusInternalServerError, "server_error", "Something went wrong on our side.")
			return
		}
		writeJSON(w, http.StatusOK, struct {
			User userEnvelope `json:"user"`
		}{User: reloaded.envelope()})
	default:
		writeDRF(w, http.StatusMethodNotAllowed, "method_not_allowed",
			`Method "`+strings.ToUpper(r.Method)+`" not allowed.`)
	}
}
