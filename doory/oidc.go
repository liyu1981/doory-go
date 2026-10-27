// OpenID Connect sign-in: the authorization-code flow with PKCE, run by the
// server (apps/accounts/oidc.py + the OIDC views in accounts/views.py).
//
// begin() keeps state, nonce and the PKCE verifier in the session (single
// use, ten minutes); the provider sends the browser back to
// /api/v1/auth/oidc/callback/, where takeFlow() and complete() check the
// answer, redeem the code and verify the ID token. Provider documents are
// fetched with timeouts, no redirects, a size cap and address checks.
// Ports backend/apps/accounts/oidc.py

package doory

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	oidcCallbackPath = "/api/v1/auth/oidc/callback/"
	oidcLoginPath    = "/login"     // views.LOGIN_PATH
	oidcSettingsPath = "/settings"  // views.SETTINGS_PATH
	oidcDefaultNext  = "/dashboard" // views.DEFAULT_NEXT

	oidcUnavailable = "oidc_unavailable"
	oidcFailed      = "oidc_failed"
	oidcCancelled   = "oidc_cancelled"
	oidcExpired     = "oidc_expired"

	oidcFlowMaxAgeSeconds   = 10 * 60
	oidcMaxPendingFlows     = 5
	oidcMaxEmailLength      = 254
	oidcMaxNextPathLength   = 2000
	oidcRandomStateBytes    = 32
	oidcRandomNonceBytes    = 32
	oidcRandomVerifierBytes = 64
)

// OidcError carries the code the user sees; provider details only go to the
// log (never into the redirect).
type OidcError struct {
	Code   string
	Detail string
}

func (e *OidcError) Error() string {
	if e.Detail != "" {
		return e.Detail
	}
	return e.Code
}

// OidcClaims is a verified provider identity.
type OidcClaims struct {
	Issuer        string
	Subject       string
	Email         string // "" when the provider sent none (or not usable)
	EmailVerified bool
	Name          string
}

// oidcFlow is one pending sign-in/connect flow stored in the session
// (Django's session["oidc_flows"][state]).
type oidcFlow struct {
	Nonce    string  `json:"nonce"`
	Verifier string  `json:"verifier"`
	Next     string  `json:"next"`
	Mode     string  `json:"mode"`
	User     string  `json:"user,omitempty"`
	At       float64 `json:"at"`
}

// safeNextPath ports accounts/views.safe_next_path: `value` when it is an
// in-app path (no scheme, host or control characters), else the dashboard.
func safeNextPath(value string) string {
	if utf8.RuneCountInString(value) > oidcMaxNextPathLength {
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

func oidcRedirectToProvider(w http.ResponseWriter, target string) {
	w.Header().Set("Location", target)
	w.WriteHeader(http.StatusFound)
}

func oidcLogFailure(stage string, err *OidcError) {
	// Cancelled/expired is routine; everything else is for the admin.
	logerr(err, "OIDC "+stage+" failed ("+err.Code+")")
}

// oidcSession reads the request's live session, or starts a new anonymous
// one (the response cookie is only sent once the caller saves it).
func (a *App) oidcSession(r *http.Request) *Session {
	if cookie, err := r.Cookie(sessionCookieName); err == nil && cookie.Value != "" {
		if existing, err := getSession(a.DB, cookie.Value); err == nil && existing != nil {
			return existing
		}
	}
	return &Session{Key: newSessionKey()}
}

func (a *App) persistOIDCSession(session *Session) {
	_ = putSessionWithFlows(a.DB, session.Key, session.UserID, session.AuthHash, session.Flows)
}

func (a *App) saveOIDCSession(w http.ResponseWriter, r *http.Request, session *Session) {
	a.persistOIDCSession(session)
	setSessionCookie(w, session.Key, a.Settings.SessionCookieSecure())
}

func oidcRandomToken(bytes int) string {
	buf := make([]byte, bytes)
	if _, err := rand.Read(buf); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(buf)
}

func oidcFresh(final float64) bool {
	return time.Since(time.Unix(int64(final), 0)) < oidcFlowMaxAgeSeconds*time.Second
}

func oidcLiveFlows(flows map[string]oidcFlow) map[string]oidcFlow {
	out := map[string]oidcFlow{}
	for state, flow := range flows {
		if oidcFresh(flow.At) {
			out[state] = flow
		}
	}
	return out
}

// oidcBegin ports oidc.begin: store a fresh flow and return the provider URL.
func (a *App) oidcBegin(session *Session, nextPath, mode, userID string) (string, error) {
	metadata, err := a.providerMetadata()
	if err != nil {
		return "", err
	}
	state := oidcRandomToken(oidcRandomStateBytes)
	nonce := oidcRandomToken(oidcRandomNonceBytes)
	verifier := oidcRandomToken(oidcRandomVerifierBytes)
	flows := oidcLiveFlows(session.Flows)
	flows[state] = oidcFlow{
		Nonce: nonce, Verifier: verifier, Next: nextPath, Mode: mode,
		User: userID, At: float64(time.Now().Unix()),
	}
	for len(flows) > oidcMaxPendingFlows {
		states := sortOIDCFlows(flows)
		delete(flows, states[0])
	}
	session.Flows = flows
	challenge := base64.RawURLEncoding.EncodeToString(func() []byte {
		sum := sha256.Sum256([]byte(verifier))
		return sum[:]
	}())
	values := [][2]string{
		{"response_type", "code"},
		{"client_id", a.Settings.OIDCClientID},
		{"redirect_uri", a.oidcRedirectURI()},
		{"scope", a.Settings.OIDCScopes},
		{"state", state},
		{"nonce", nonce},
		{"code_challenge", challenge},
		{"code_challenge_method", "S256"},
	}
	var pairs []string
	for _, pair := range values {
		pairs = append(pairs, url.QueryEscape(pair[0])+"="+url.QueryEscape(pair[1]))
	}
	endpoint, _ := metadata["authorization_endpoint"].(string)
	separator := "?"
	if strings.Contains(endpoint, "?") {
		separator = "&"
	}
	return endpoint + separator + strings.Join(pairs, "&"), nil
}

// oidcTakeFlow removes (and returns) the flow for `state`; the session is
// written back even when nothing matched (Django's take_flow does too, which
// creates an anonymous session on a cookie-less callback).
func oidcTakeFlow(session *Session, state string) *oidcFlow {
	flows := oidcLiveFlows(session.Flows)
	flow, ok := flows[state]
	if state != "" {
		delete(flows, state)
	}
	session.Flows = flows
	if !ok {
		return nil
	}
	return &flow
}

func (a *App) oidcRedirectURI() string {
	return strings.TrimRight(a.Settings.SiteURL, "/") + oidcCallbackPath
}

// oidcComplete ports oidc.complete: redeem the code and verify the identity.
func (a *App) oidcComplete(flow oidcFlow, code, iss string, issPresent bool) (OidcClaims, error) {
	metadata, err := a.providerMetadata()
	if err != nil {
		return OidcClaims{}, err
	}
	issuer, _ := metadata["issuer"].(string)
	if issPresent && iss != issuer {
		return OidcClaims{}, &OidcError{Code: oidcFailed,
			Detail: fmt.Sprintf("The callback came from issuer %v, not %v.", pyRepr(iss), pyRepr(issuer))}
	}
	if !issPresent && metadata["authorization_response_iss_parameter_supported"] == true {
		return OidcClaims{}, &OidcError{Code: oidcFailed,
			Detail: "The callback has no iss parameter although the provider sends it."}
	}
	tokens, err := a.oidcRedeem(metadata, code, flow.Verifier)
	if err != nil {
		return OidcClaims{}, err
	}
	idToken, _ := tokens["id_token"].(string)
	if idToken == "" {
		return OidcClaims{}, &OidcError{Code: oidcFailed, Detail: "The token response has no ID token."}
	}
	claims, err := a.oidcVerifyIDToken(idToken, metadata, flow.Nonce)
	if err != nil {
		return OidcClaims{}, err
	}
	accessToken, _ := tokens["access_token"].(string)
	email, _ := claims["email"].(string)
	if email == "" && accessToken != "" {
		if endpoint, _ := metadata["userinfo_endpoint"].(string); endpoint != "" {
			info, err := a.oidcUserinfo(endpoint, accessToken, claimString(claims["sub"]))
			if err != nil {
				return OidcClaims{}, err
			}
			for key, value := range info {
				if oidcFalsy(claims[key]) {
					claims[key] = value
				}
			}
		}
	}
	return oidcToClaims(issuer, claims), nil
}

func (a *App) oidcRedeem(metadata map[string]any, code, verifier string) (map[string]any, error) {
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", a.oidcRedirectURI())
	form.Set("code_verifier", verifier)
	methods := oidcStringList(metadata["token_endpoint_auth_methods_supported"])
	if len(methods) == 0 {
		// Python's `[] or ["client_secret_basic"]`: an empty list also
		// falls back to the RFC default.
		methods = []string{"client_secret_basic"}
	}
	headers := http.Header{}
	headers.Set("Content-Type", "application/x-www-form-urlencoded")
	if containsString(methods, "client_secret_basic") {
		auth := url.QueryEscape(a.Settings.OIDCClientID) + ":" + url.QueryEscape(a.Settings.OIDCClientSecret)
		headers.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(auth)))
	} else if containsString(methods, "client_secret_post") {
		form.Set("client_id", a.Settings.OIDCClientID)
		form.Set("client_secret", a.Settings.OIDCClientSecret)
	} else {
		return nil, &OidcError{Code: oidcUnavailable,
			Detail: fmt.Sprintf("The token endpoint takes none of our client auth methods: %v", methods)}
	}
	endpoint, _ := metadata["token_endpoint"].(string)
	return oidcFetchJSON("POST", endpoint, oidcFailed, headers, []byte(form.Encode()))
}

func (a *App) oidcUserinfo(endpoint, accessToken, subject string) (map[string]any, error) {
	headers := http.Header{}
	headers.Set("Authorization", "Bearer "+accessToken)
	info, err := oidcFetchJSON("GET", endpoint, oidcFailed, headers, nil)
	if err != nil {
		return nil, err
	}
	if claimString(info["sub"]) != subject {
		return nil, &OidcError{Code: oidcFailed, Detail: "Userinfo belongs to another subject."}
	}
	out := map[string]any{}
	for _, key := range []string{"email", "email_verified", "name", "preferred_username"} {
		if value, ok := info[key]; ok {
			out[key] = value
		}
	}
	return out, nil
}

// oidcTruthy mirrors Python's `value in (True, "true")`, where 1 also
// compares equal to True.
func oidcTruthy(value any) bool {
	switch v := value.(type) {
	case bool:
		return v
	case string:
		return v == "true"
	case float64:
		return v == 1
	case int:
		return v == 1
	default:
		return false
	}
}

// oidcFalsy mirrors Python truthiness for the userinfo merge
// (`if not claims.get(key)`).
func oidcFalsy(value any) bool {
	switch v := value.(type) {
	case nil:
		return true
	case bool:
		return !v
	case string:
		return v == ""
	case float64:
		return v == 0
	case int:
		return v == 0
	case []any:
		return len(v) == 0
	default:
		return false
	}
}

func claimString(value any) string {
	text, _ := value.(string)
	return text
}

func oidcToClaims(issuer string, claims map[string]any) OidcClaims {
	email := strings.ToLower(strings.TrimSpace(claimString(claims["email"])))
	if email != "" && (!validEmail(email) || len(email) > oidcMaxEmailLength) {
		email = ""
	}
	names := []string{claimString(claims["name"]), claimString(claims["preferred_username"])}
	name := ""
	for _, candidate := range names {
		if strings.TrimSpace(candidate) != "" {
			name = candidate
			break
		}
	}
	verified := oidcTruthy(claims["email_verified"])
	return OidcClaims{
		Issuer: issuer, Subject: claimString(claims["sub"]), Email: email,
		EmailVerified: verified, Name: name,
	}
}

// ---- views -----------------------------------------------------------------

// oidcStart ports OidcStartView.get: send the browser to the provider, or
// back to the sign-in page with the error.
func (a *App) oidcStart(w http.ResponseWriter, r *http.Request) {
	nextPath := safeNextPath(r.URL.Query().Get("next"))
	if !a.Settings.oidcEnabled() {
		a.oidcLoginFailed(w, r, oidcUnavailable, nextPath)
		return
	}
	session := a.oidcSession(r)
	target, err := a.oidcBegin(session, nextPath, "login", "")
	if err != nil {
		oidcErr := oidcAsError(err)
		oidcLogFailure("start", oidcErr)
		a.oidcLoginFailed(w, r, oidcErr.Code, nextPath)
		return
	}
	a.saveOIDCSession(w, r, session)
	oidcRedirectToProvider(w, target)
}

// oidcCallback ports OidcCallbackView.get: finish sign-in or connect.
func (a *App) oidcCallback(w http.ResponseWriter, r *http.Request) {
	params := r.URL.Query()
	session := a.oidcSession(r)
	flow := oidcTakeFlow(session, params.Get("state"))
	// Persist the single-use flow removal now; the session cookie is sent by
	// whichever path finishes the request (finishLogin cycles it on success,
	// so it must not be sent twice here).
	a.persistOIDCSession(session)
	if flow == nil {
		oidcLogFailure("callback", &OidcError{Code: oidcExpired, Detail: "OIDC callback without a sign-in to finish."})
		setSessionCookie(w, session.Key, a.Settings.SessionCookieSecure())
		a.oidcLoginFailed(w, r, oidcExpired, oidcDefaultNext)
		return
	}
	connecting := flow.Mode == "connect"
	redirectFailure := func(err *OidcError) {
		oidcLogFailure("callback", err)
		setSessionCookie(w, session.Key, a.Settings.SessionCookieSecure())
		if connecting {
			a.oidcAppRedirect(w, r, oidcSettingsPath, [][2]string{{"sso_error", err.Code}})
			return
		}
		a.oidcLoginFailed(w, r, err.Code, flow.Next)
	}
	if !a.Settings.oidcEnabled() {
		redirectFailure(&OidcError{Code: oidcUnavailable, Detail: "OIDC sign-in was turned off."})
		return
	}
	if providerError, hasProviderError := params["error"]; hasProviderError {
		value := ""
		if len(providerError) > 0 {
			value = providerError[0]
		}
		code := oidcFailed
		if value == "access_denied" {
			code = oidcCancelled
		}
		redirectFailure(&OidcError{Code: code,
			Detail: fmt.Sprintf("The provider answered error=%v %v",
				pyRepr(value), pyRepr(params.Get("error_description")))})
		return
	}
	code := params.Get("code")
	if code == "" {
		redirectFailure(&OidcError{Code: oidcFailed, Detail: "The callback has no code."})
		return
	}
	var connUser *UserRow
	if connecting {
		connUser = currentUser(r)
		if connUser == nil || connUser.ID != flow.User {
			redirectFailure(&OidcError{Code: oidcExpired, Detail: "The session changed while connecting."})
			return
		}
	}
	_, issPresent := params["iss"]
	claims, err := a.oidcComplete(*flow, code, params.Get("iss"), issPresent)
	if err != nil {
		redirectFailure(oidcAsError(err))
		return
	}
	if connecting {
		if err := a.connectOIDC(connUser, claims); err != nil {
			redirectFailure(err)
			return
		}
		setSessionCookie(w, session.Key, a.Settings.SessionCookieSecure())
		a.oidcAppRedirect(w, r, oidcSettingsPath, [][2]string{{"sso", "connected"}})
		return
	}
	user, created, signInErr := a.signInWithOIDC(claims)
	if signInErr != nil {
		redirectFailure(signInErr)
		return
	}
	a.finishLogin(w, r, user)
	if created && !user.IsEmailVerified {
		a.sendVerificationEmail(user)
	}
	a.oidcAppRedirect(w, r, flow.Next, nil)
}

// oidcConnection ports OidcConnectionView: POST starts connecting, DELETE
// disconnects.
func (a *App) oidcConnection(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	switch r.Method {
	case http.MethodPost:
		if !a.Settings.oidcEnabled() {
			writeDRF(w, http.StatusBadRequest, "oidc_not_configured",
				"Single sign-on is not configured on this server.")
			return
		}
		var body struct {
			Password     *string `json:"password"`
			ConfirmEmail *string `json:"confirm_email"`
		}
		if err := jsonBody(r, &body); err != nil {
			writeDRF(w, http.StatusBadRequest, "bad_request", "Malformed request body.")
			return
		}
		password, confirmEmail := "", ""
		if body.Password != nil {
			password = *body.Password
		}
		if body.ConfirmEmail != nil {
			confirmEmail = *body.ConfirmEmail
		}
		if len(password) > passwordMaxLength {
			writeDRFValidation(w, []FieldError{{Name: "password",
				Messages: []string{fmt.Sprintf("Ensure this field has no more than %d characters.", passwordMaxLength)},
				Codes:    []string{"max_length"}}})
			return
		}
		if len(confirmEmail) > oidcMaxEmailLength {
			writeDRFValidation(w, []FieldError{{Name: "confirm_email",
				Messages: []string{fmt.Sprintf("Ensure this field has no more than %d characters.", oidcMaxEmailLength)},
				Codes:    []string{"max_length"}}})
			return
		}
		if u.HasPassword() {
			if a.confirmPassword(w, u, password, "password") {
				return
			}
		} else if strings.ToLower(strings.TrimSpace(confirmEmail)) != strings.ToLower(u.Email) {
			writeDRFValidation(w, []FieldError{SimpleField("confirm_email",
				"Type your account's email address to confirm.", "confirm_email_mismatch")})
			return
		}
		session := a.oidcSession(r)
		target, err := a.oidcBegin(session, oidcSettingsPath, "connect", u.ID)
		if err != nil {
			oidcErr := oidcAsError(err)
			oidcLogFailure("connect", oidcErr)
			writeDRF(w, http.StatusServiceUnavailable, oidcErr.Code,
				"The sign-in provider can't be reached right now.")
			return
		}
		a.saveOIDCSession(w, r, session)
		b := encodeCompactJSON(map[string]any{"redirect_url": target})
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", itoa(len(b)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(b)
	case http.MethodDelete:
		if err := a.disconnectOIDC(u); err != nil {
			if err.Code == "last_sign_in_method" {
				writeDRF(w, http.StatusBadRequest, err.Code, "Set a password first, or you won't be able to sign in.")
			} else {
				logerr(err, "oidcConnection.delete")
				writeDRF(w, http.StatusInternalServerError, "server_error", "Something went wrong on our side.")
			}
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
