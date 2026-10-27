package doory

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const (
	oidcTestSite         = "https://doory.example.com"
	oidcTestIssuer       = "https://idp.example.com/application/o/doory/"
	oidcTestDiscovery    = "https://idp.example.com/application/o/doory/.well-known/openid-configuration"
	oidcTestClientID     = "doory-client"
	oidcTestClientSecret = "doory-secret"
	oidcTestCode         = "the-code"
	oidcTestNext         = "/dashboard/designs"
)

type fakeOIDCProvider struct {
	t             *testing.T
	key           *rsa.PrivateKey
	kid           string
	alg           string
	metadata      map[string]any
	claims        map[string]any
	userinfo      map[string]any
	authorization url.Values
	calls         []oidcHTTPRequest
}

var (
	testRSAOnce sync.Once
	testRSAKey  *rsa.PrivateKey
)

func testPrivateKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	testRSAOnce.Do(func() {
		key, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			panic(err)
		}
		testRSAKey = key
	})
	return testRSAKey
}

func newFakeOIDCProvider(t *testing.T) *fakeOIDCProvider {
	t.Helper()
	key := testPrivateKey(t)
	provider := &fakeOIDCProvider{
		t: t, key: key, kid: "key-1", alg: "RS256",
		metadata: map[string]any{
			"issuer":                                oidcTestIssuer,
			"authorization_endpoint":                "https://idp.example.com/authorize/",
			"token_endpoint":                        "https://idp.example.com/token/",
			"jwks_uri":                              "https://idp.example.com/jwks/",
			"userinfo_endpoint":                     "https://idp.example.com/userinfo/",
			"id_token_signing_alg_values_supported": []any{"RS256"},
		},
		claims: map[string]any{},
	}
	return provider
}

func (p *fakeOIDCProvider) install() {
	oldDo := oidcHTTPDo
	oldClear := oidcClearCache
	_ = oldClear
	oidcHTTPDo = p.do
	oidcClearCache()
	p.t.Cleanup(func() {
		oidcHTTPDo = oldDo
		oidcClearCache()
	})
}

func (p *fakeOIDCProvider) jwk() map[string]any {
	pub := p.key.PublicKey
	return map[string]any{
		"kty": "RSA", "use": "sig", "alg": "RS256", "kid": p.kid,
		"n": base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
		"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
	}
}

func (p *fakeOIDCProvider) idToken() string {
	now := time.Now().Unix()
	claims := map[string]any{
		"iss": oidcTestIssuer, "sub": "sub-1", "aud": oidcTestClientID,
		"exp": now + 300, "iat": now, "nonce": p.authorization.Get("nonce"),
		"email": "ada@example.com", "email_verified": true, "name": "Ada Lovelace",
	}
	for key, value := range p.claims {
		if value == nil {
			delete(claims, key)
		} else {
			claims[key] = value
		}
	}
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims(claims))
	token.Header["kid"] = p.kid
	signed, err := token.SignedString(p.key)
	if err != nil {
		p.t.Fatal(err)
	}
	return signed
}

func (p *fakeOIDCProvider) do(request oidcHTTPRequest) (oidcHTTPResponse, error) {
	p.calls = append(p.calls, request)
	jsonResponse := func(value any) (oidcHTTPResponse, error) {
		body, _ := json.Marshal(value)
		return oidcHTTPResponse{Status: 200, Body: body}, nil
	}
	switch request.URL {
	case oidcTestDiscovery:
		return jsonResponse(p.metadata)
	case p.metadata["jwks_uri"]:
		return jsonResponse(map[string]any{"keys": []any{p.jwk()}})
	case p.metadata["token_endpoint"]:
		return jsonResponse(map[string]any{
			"id_token": p.idToken(), "access_token": "access", "token_type": "Bearer",
		})
	case p.metadata["userinfo_endpoint"]:
		info := map[string]any{"sub": "sub-1"}
		for key, value := range p.userinfo {
			info[key] = value
		}
		return jsonResponse(info)
	default:
		return oidcHTTPResponse{Status: 404, Body: []byte(`{}`)}, nil
	}
}

func (p *fakeOIDCProvider) startQuery(t *testing.T, location string) url.Values {
	t.Helper()
	parsed, err := url.Parse(location)
	if err != nil {
		t.Fatal(err)
	}
	if got := parsed.Scheme + "://" + parsed.Host + parsed.Path; got != p.metadata["authorization_endpoint"] {
		t.Fatalf("authorization endpoint = %q", got)
	}
	p.authorization = parsed.Query()
	return p.authorization
}

func (p *fakeOIDCProvider) callbackURL(state string) string {
	return "/api/v1/auth/oidc/callback/?state=" + url.QueryEscape(state) + "&code=" + oidcTestCode
}

func newOIDCEnv(t *testing.T, provider *fakeOIDCProvider) *adminEnv {
	t.Helper()
	env := newAdminEnv(t)
	env.settings.SiteURL = oidcTestSite
	env.settings.OIDCIssuer = oidcTestIssuer
	env.settings.OIDCClientID = oidcTestClientID
	env.settings.OIDCClientSecret = oidcTestClientSecret
	env.settings.OIDCScopes = "openid email profile"
	env.settings.OIDCTrustEmail = false
	provider.install()
	return env
}

func (e *adminEnv) doJSON(path, body string) (*http.Response, string) {
	e.t.Helper()
	req, err := http.NewRequest(http.MethodPost, e.url(path), strings.NewReader(body))
	if err != nil {
		e.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CSRFToken", e.csrf)
	res, err := e.client.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	data, _ := io.ReadAll(res.Body)
	_ = res.Body.Close()
	return res, string(data)
}

func (e *adminEnv) loginSession(user *UserRow) {
	e.t.Helper()
	key := newSessionKey()
	if err := putSession(e.db, key, user.ID, sessionAuthHash(user.Password)); err != nil {
		e.t.Fatal(err)
	}
	e.setCookie(&http.Cookie{Name: sessionCookieName, Value: key, Path: "/"})
}

func (e *adminEnv) refreshCSRF() {
	e.t.Helper()
	parsed, _ := url.Parse(e.server.URL)
	for _, cookie := range e.client.Jar.Cookies(parsed) {
		if cookie.Name == csrfCookieName {
			e.csrf = cookie.Value
			return
		}
	}
	e.t.Fatal("no csrf cookie in jar")
}

func (e *adminEnv) setVerified(user *UserRow) {
	e.t.Helper()
	if _, err := e.db.Exec(`UPDATE accounts_user SET is_email_verified = TRUE WHERE id = ?`, user.ID); err != nil {
		e.t.Fatal(err)
	}
	user.IsEmailVerified = true
}

func TestOIDCSignInCreatesAndReusesAccount(t *testing.T) {
	provider := newFakeOIDCProvider(t)
	env := newOIDCEnv(t, provider)

	res, _ := env.get("/api/v1/auth/oidc/start/?next=" + url.QueryEscape(oidcTestNext))
	if res.StatusCode != http.StatusFound {
		t.Fatalf("start status = %d", res.StatusCode)
	}
	query := provider.startQuery(t, res.Header.Get("Location"))
	if query.Get("response_type") != "code" || query.Get("client_id") != oidcTestClientID ||
		query.Get("redirect_uri") != oidcTestSite+oidcCallbackPath ||
		query.Get("code_challenge_method") != "S256" || query.Get("state") == "" {
		t.Fatalf("authorization query = %v", query)
	}

	res, _ = env.get(provider.callbackURL(query.Get("state")))
	if res.StatusCode != http.StatusFound || res.Header.Get("Location") != oidcTestSite+oidcTestNext {
		t.Fatalf("callback = %d %q", res.StatusCode, res.Header.Get("Location"))
	}
	var email, password string
	if err := env.db.QueryRow(`SELECT email, password FROM accounts_user`).Scan(&email, &password); err != nil {
		t.Fatal(err)
	}
	if email != "ada@example.com" || hasUsablePassword(password) {
		t.Fatalf("created account = %q password usable=%v", email, hasUsablePassword(password))
	}
	var identities int
	if err := env.db.QueryRow(`SELECT COUNT(*) FROM accounts_oidcidentity WHERE issuer = ? AND subject = ?`,
		oidcTestIssuer, "sub-1").Scan(&identities); err != nil || identities != 1 {
		t.Fatalf("identity count = %d (err %v)", identities, err)
	}

	// Returning user: the subject wins even when the provider's email changed.
	provider.claims = map[string]any{"email": "renamed@example.com"}
	res, _ = env.get("/api/v1/auth/oidc/start/?next=" + url.QueryEscape(oidcTestNext))
	query = provider.startQuery(t, res.Header.Get("Location"))
	res, _ = env.get(provider.callbackURL(query.Get("state")))
	if res.StatusCode != http.StatusFound || res.Header.Get("Location") != oidcTestSite+oidcTestNext {
		t.Fatalf("returning callback = %d %q", res.StatusCode, res.Header.Get("Location"))
	}
	var users int
	if err := env.db.QueryRow(`SELECT COUNT(*) FROM accounts_user`).Scan(&users); err != nil || users != 1 {
		t.Fatalf("user count = %d (err %v)", users, err)
	}
}

func TestOIDCExistingAccountNeedsTrustEmail(t *testing.T) {
	provider := newFakeOIDCProvider(t)
	env := newOIDCEnv(t, provider)
	existing := env.makeUser("ada@example.com", false, false)
	env.setVerified(existing)

	res, _ := env.get("/api/v1/auth/oidc/start/?next=" + url.QueryEscape(oidcTestNext))
	query := provider.startQuery(t, res.Header.Get("Location"))
	res, _ = env.get(provider.callbackURL(query.Get("state")))
	if res.StatusCode != http.StatusFound || !strings.Contains(res.Header.Get("Location"), "sso_error=oidc_account_exists") {
		t.Fatalf("untrusted existing account = %d %q", res.StatusCode, res.Header.Get("Location"))
	}
	var identities int
	_ = env.db.QueryRow(`SELECT COUNT(*) FROM accounts_oidcidentity`).Scan(&identities)
	if identities != 0 {
		t.Fatalf("identity created without trust: %d", identities)
	}

	env.settings.OIDCTrustEmail = true
	res, _ = env.get("/api/v1/auth/oidc/start/?next=" + url.QueryEscape(oidcTestNext))
	query = provider.startQuery(t, res.Header.Get("Location"))
	res, _ = env.get(provider.callbackURL(query.Get("state")))
	if res.StatusCode != http.StatusFound || res.Header.Get("Location") != oidcTestSite+oidcTestNext {
		t.Fatalf("trusted existing account = %d %q", res.StatusCode, res.Header.Get("Location"))
	}
	if err := env.db.QueryRow(`SELECT COUNT(*) FROM accounts_oidcidentity`).Scan(&identities); err != nil || identities != 1 {
		t.Fatalf("trusted identity count = %d (err %v)", identities, err)
	}
}

func TestOIDCConnectAndDisconnect(t *testing.T) {
	provider := newFakeOIDCProvider(t)
	env := newOIDCEnv(t, provider)
	user := env.makeUser("ada@example.com", false, false)
	env.loginSession(user)
	provider.claims = map[string]any{"email": "ada@work.example.com"}

	res, body := env.doJSON("/api/v1/auth/oidc/connection/", `{"password":"`+adminTestPassword+`"}`)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("connect status = %d body=%s", res.StatusCode, body)
	}
	var response struct {
		RedirectURL string `json:"redirect_url"`
	}
	if err := json.Unmarshal([]byte(body), &response); err != nil {
		t.Fatal(err)
	}
	query := provider.startQuery(t, response.RedirectURL)
	res, _ = env.get(provider.callbackURL(query.Get("state")))
	if res.StatusCode != http.StatusFound || !strings.Contains(res.Header.Get("Location"), "sso=connected") {
		t.Fatalf("connect callback = %d %q", res.StatusCode, res.Header.Get("Location"))
	}
	reloaded, _ := getUserByID(env.db, user.ID)
	if reloaded == nil || !reloaded.OidcLinked {
		t.Fatal("account is not linked")
	}

	req, _ := http.NewRequest(http.MethodDelete, env.url("/api/v1/auth/oidc/connection/"), nil)
	req.Header.Set("X-CSRFToken", env.csrf)
	res, err := env.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(res.Body)
	_ = res.Body.Close()
	if res.StatusCode != http.StatusOK || !strings.Contains(string(data), `"oidc_linked":false`) {
		t.Fatalf("disconnect = %d %s", res.StatusCode, data)
	}
}

func TestGoogleSignInCreatesLinksAndValidates(t *testing.T) {
	provider := newFakeOIDCProvider(t)
	env := newAdminEnv(t)
	env.settings.GoogleClientID = "google-client"
	provider.install()

	sign := func(sub, email string) string {
		now := time.Now().Unix()
		token := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
			"iss": "accounts.google.com", "sub": sub, "aud": "google-client",
			"exp": now + 300, "iat": now, "email": email,
			"email_verified": true, "name": "Ada Lovelace",
		})
		token.Header["kid"] = provider.kid
		signed, err := token.SignedString(provider.key)
		if err != nil {
			t.Fatal(err)
		}
		return signed
	}
	installGoogleCerts := func() {
		oldDo := oidcHTTPDo
		oidcHTTPDo = func(request oidcHTTPRequest) (oidcHTTPResponse, error) {
			if request.URL == googleCertsURL {
				body, _ := json.Marshal(map[string]any{"keys": []any{provider.jwk()}})
				return oidcHTTPResponse{Status: 200, Body: body}, nil
			}
			return oidcHTTPResponse{Status: 404, Body: []byte(`{}`)}, nil
		}
		t.Cleanup(func() { oidcHTTPDo = oldDo; oidcClearCache() })
		oidcClearCache()
	}
	installGoogleCerts()

	credential := sign("google-sub-1", "ada@example.com")
	res, body := env.doJSON("/api/v1/auth/google/", fmt.Sprintf(`{"credential":%q}`, credential))
	if res.StatusCode != http.StatusOK {
		t.Fatalf("google status = %d body=%s", res.StatusCode, body)
	}
	if !strings.Contains(body, `"created":true`) || !strings.Contains(body, `"google_linked":true`) ||
		!strings.Contains(body, `"has_password":false`) {
		t.Fatalf("google body = %s", body)
	}
	env.refreshCSRF()
	var email, password string
	var sub string
	if err := env.db.QueryRow(`SELECT email, password, google_sub FROM accounts_user`).Scan(&email, &password, &sub); err != nil {
		t.Fatal(err)
	}
	if email != "ada@example.com" || sub != "google-sub-1" || hasUsablePassword(password) {
		t.Fatalf("google user = %q/%q usable=%v", email, sub, hasUsablePassword(password))
	}

	// Same subject, changed email: the same account is returned.
	credential = sign("google-sub-1", "renamed@example.com")
	_, body = env.doJSON("/api/v1/auth/google/", fmt.Sprintf(`{"credential":%q}`, credential))
	if !strings.Contains(body, `"created":false`) {
		t.Fatalf("returning google = %s", body)
	}
	env.refreshCSRF()
	var users int
	_ = env.db.QueryRow(`SELECT COUNT(*) FROM accounts_user`).Scan(&users)
	if users != 1 {
		t.Fatalf("google user count = %d", users)
	}

	// A verified local account keeps its password when linked.
	existing := env.makeUser("grace@example.com", false, false)
	env.setVerified(existing)
	credential = sign("google-sub-2", "grace@example.com")
	res, body = env.doJSON("/api/v1/auth/google/", fmt.Sprintf(`{"credential":%q}`, credential))
	if res.StatusCode != http.StatusOK || !strings.Contains(body, `"created":false`) {
		t.Fatalf("google link = %d %s", res.StatusCode, body)
	}
	reloaded, _ := getUserByID(env.db, existing.ID)
	if reloaded == nil || !reloaded.GoogleSub.Valid || !verifyPassword(adminTestPassword, reloaded.Password) {
		t.Fatalf("google link damaged the account: %+v", reloaded)
	}
}

func TestOIDCParseECJWK(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pad := func(value *big.Int) string {
		raw := value.Bytes()
		for len(raw) < 32 {
			raw = append([]byte{0}, raw...)
		}
		return base64.RawURLEncoding.EncodeToString(raw)
	}
	jwk := map[string]any{
		"kty": "EC", "crv": "P-256", "use": "sig", "alg": "ES256",
		"x": pad(key.X), "y": pad(key.Y),
	}
	parsed, err := oidcParseJWK(jwk, "ES256")
	if err != nil {
		t.Fatal(err)
	}
	signed, err := jwt.NewWithClaims(jwt.SigningMethodES256, jwt.MapClaims{"sub": "x"}).SignedString(key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := jwt.Parse(signed, func(*jwt.Token) (any, error) { return parsed, nil },
		jwt.WithValidMethods([]string{"ES256"})); err != nil {
		t.Fatalf("ES256 token did not verify with the parsed JWK: %v", err)
	}
}

func TestGoogleLoginConfigurationAndCredentialValidation(t *testing.T) {
	env := newAdminEnv(t)
	res, body := env.doJSON("/api/v1/auth/google/", `{"credential":"x"}`)
	if res.StatusCode != http.StatusBadRequest || !strings.Contains(body, `"code":"google_not_configured"`) {
		t.Fatalf("not configured = %d %s", res.StatusCode, body)
	}
	env.settings.GoogleClientID = "google-client"
	res, body = env.doJSON("/api/v1/auth/google/", `{}`)
	if res.StatusCode != http.StatusBadRequest || !strings.Contains(body, `"fields":{"credential"`) {
		t.Fatalf("missing credential = %d %s", res.StatusCode, body)
	}
	res, body = env.doJSON("/api/v1/auth/google/", `{"credential":"   "}`)
	if res.StatusCode != http.StatusBadRequest || !strings.Contains(body, `"field_codes":{"credential":["blank"]}`) {
		t.Fatalf("blank credential = %d %s", res.StatusCode, body)
	}
}

func TestOIDCStartWithoutProvider(t *testing.T) {
	env := newAdminEnv(t)
	env.settings.SiteURL = oidcTestSite
	res, _ := env.get("/api/v1/auth/oidc/start/?next=/design/1")
	if res.StatusCode != http.StatusFound ||
		res.Header.Get("Location") != oidcTestSite+"/login?sso_error=oidc_unavailable&next=%2Fdesign%2F1" {
		t.Fatalf("start without provider = %d %q", res.StatusCode, res.Header.Get("Location"))
	}
}
