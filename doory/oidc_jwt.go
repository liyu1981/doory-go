// Provider documents, JWKS and ID-token verification for OIDC and Google
// sign-in. The network layer never follows redirects, caps every document,
// and refuses link-local/cloud-metadata addresses (a self-hosted provider on
// a private network is still allowed).
// Ports backend/apps/accounts/oidc.py

package doory

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const (
	oidcUserAgent            = "Doory-oidc/1.0"
	oidcConnectTimeout       = 5 * time.Second
	oidcReadTimeout          = 10 * time.Second
	oidcMaxSeconds           = 20 * time.Second
	oidcMaxDocumentBytes     = 256 * 1024
	oidcMetadataCacheSeconds = time.Hour
	oidcJWKSRefreshSeconds   = time.Minute
	oidcClockSkewSeconds     = 60
)

type oidcHTTPRequest struct {
	Method string
	URL    string
	Header http.Header
	Body   []byte
}

type oidcHTTPResponse struct {
	Status int
	Body   []byte
}

// oidcHTTPDo is replaceable in tests (the fake provider serves discovery,
// keys, token and userinfo without a real network).
var oidcHTTPDo = oidcHTTPDoReal

var oidcAlgorithmKeyType = map[string]string{
	"RS256": "RSA", "RS384": "RSA", "RS512": "RSA",
	"PS256": "RSA", "PS384": "RSA", "PS512": "RSA",
	"ES256": "EC", "ES384": "EC", "ES512": "EC",
	"EdDSA": "OKP",
}

type oidcCacheEntry struct {
	value any
	at    time.Time
}

var (
	oidcCacheMu sync.Mutex
	oidcCache   = map[string]oidcCacheEntry{}
)

func oidcCacheGet(key string, maxAge time.Duration) (any, bool) {
	oidcCacheMu.Lock()
	defer oidcCacheMu.Unlock()
	entry, ok := oidcCache[key]
	if !ok || time.Since(entry.at) > maxAge {
		return nil, false
	}
	return entry.value, true
}

func oidcCacheSet(key string, value any) {
	oidcCacheMu.Lock()
	oidcCache[key] = oidcCacheEntry{value: value, at: time.Now()}
	oidcCacheMu.Unlock()
}

// oidcCacheAdd sets key only when it is absent (the JWKS refresh throttle).
func oidcCacheAdd(key string, value any, maxAge time.Duration) bool {
	oidcCacheMu.Lock()
	defer oidcCacheMu.Unlock()
	if entry, ok := oidcCache[key]; ok && time.Since(entry.at) <= maxAge {
		return false
	}
	oidcCache[key] = oidcCacheEntry{value: value, at: time.Now()}
	return true
}

func oidcClearCache() {
	oidcCacheMu.Lock()
	oidcCache = map[string]oidcCacheEntry{}
	oidcCacheMu.Unlock()
}

func oidcCacheKey(kind, rawURL string) string {
	sum := sha256.Sum256([]byte(rawURL))
	return "oidc:" + kind + ":" + base64.RawURLEncoding.EncodeToString(sum[:])
}

func oidcFetchJSON(method, rawURL, errorCode string, headers http.Header, body []byte) (map[string]any, error) {
	if headers == nil {
		headers = http.Header{}
	}
	if headers.Get("Accept") == "" {
		headers.Set("Accept", "application/json")
	}
	headers.Set("User-Agent", oidcUserAgent)
	response, err := oidcHTTPDo(oidcHTTPRequest{Method: method, URL: rawURL, Header: headers, Body: body})
	if err != nil {
		return nil, &OidcError{Code: oidcUnavailable, Detail: method + " " + oidcHost(rawURL) + " failed: " + err.Error()}
	}
	if len(response.Body) > oidcMaxDocumentBytes {
		return nil, &OidcError{Code: oidcUnavailable,
			Detail: fmt.Sprintf("%s answered over %d bytes.", oidcHost(rawURL), oidcMaxDocumentBytes)}
	}
	var data map[string]any
	jsonErr := json.Unmarshal(response.Body, &data)
	if response.Status != http.StatusOK || jsonErr != nil || data == nil {
		detail := ""
		if jsonErr == nil && data != nil {
			if value, ok := data["error"]; ok {
				detail = fmt.Sprintf(": error=%v %v", pyRepr(value), pyRepr(data["error_description"]))
			}
		}
		return nil, &OidcError{Code: errorCode,
			Detail: fmt.Sprintf("%s %s answered HTTP %d%s", method, oidcHost(rawURL), response.Status, detail)}
	}
	return data, nil
}

func oidcHost(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Hostname() == "" {
		return "?"
	}
	return parsed.Hostname()
}

// oidcHTTPDoReal is the production fetcher: address checks plus a pinned dial
// so a second DNS answer cannot swap in a private/metadata address.
func oidcHTTPDoReal(request oidcHTTPRequest) (oidcHTTPResponse, error) {
	parsed, err := url.Parse(request.URL)
	if err != nil {
		return oidcHTTPResponse{}, fmt.Errorf("invalid URL")
	}
	port := parsed.Port()
	if port == "" {
		if parsed.Scheme == "http" {
			port = "80"
		} else {
			port = "443"
		}
	}
	addresses, err := oidcResolveAllowed(parsed.Hostname())
	if err != nil {
		return oidcHTTPResponse{}, err
	}
	dialer := &net.Dialer{Timeout: oidcConnectTimeout}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			var last error
			for _, address := range addresses {
				conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(address, port))
				if err == nil {
					return conn, nil
				}
				last = err
			}
			if last == nil {
				last = fmt.Errorf("no address")
			}
			return nil, last
		},
		TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12},
	}
	client := &http.Client{
		Transport: transport,
		Timeout:   oidcMaxSeconds,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	var body io.Reader
	if request.Body != nil {
		body = strings.NewReader(string(request.Body))
	}
	httpRequest, err := http.NewRequest(request.Method, request.URL, body)
	if err != nil {
		return oidcHTTPResponse{}, err
	}
	for key, values := range request.Header {
		for _, value := range values {
			httpRequest.Header.Add(key, value)
		}
	}
	if request.Body != nil && httpRequest.Header.Get("Content-Type") == "" {
		httpRequest.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	response, err := client.Do(httpRequest)
	if err != nil {
		return oidcHTTPResponse{}, err
	}
	defer response.Body.Close()
	limited := io.LimitReader(response.Body, oidcMaxDocumentBytes+1)
	payload, err := io.ReadAll(limited)
	if err != nil {
		return oidcHTTPResponse{}, err
	}
	if len(payload) > oidcMaxDocumentBytes {
		return oidcHTTPResponse{Status: response.StatusCode}, fmt.Errorf("document too large")
	}
	return oidcHTTPResponse{Status: response.StatusCode, Body: payload}, nil
}

func oidcResolveAllowed(host string) ([]string, error) {
	host = strings.Trim(host, "[]")
	if ip := net.ParseIP(host); ip != nil {
		if oidcNeverFetched(ip) {
			return nil, fmt.Errorf("%s resolves to an address that is not allowed", host)
		}
		return []string{ip.String()}, nil
	}
	ips, err := net.LookupIP(host)
	if err != nil || len(ips) == 0 {
		return nil, fmt.Errorf("the host name cannot be found")
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(ips))
	for _, ip := range ips {
		if oidcNeverFetched(ip) {
			return nil, fmt.Errorf("%s resolves to an address that is not allowed", host)
		}
		text := ip.String()
		if !seen[text] {
			seen[text] = true
			out = append(out, text)
		}
	}
	return out, nil
}

func oidcNeverFetched(ip net.IP) bool {
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	if ip.Equal(net.ParseIP("100.100.100.200")) || ip.Equal(net.ParseIP("fd00:ec2::254")) {
		return true
	}
	return ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsUnspecified() || ip.IsMulticast()
}

func oidcStringList(value any) []string {
	switch list := value.(type) {
	case []string:
		return list
	case []any:
		out := make([]string, 0, len(list))
		for _, item := range list {
			if text, ok := item.(string); ok {
				out = append(out, text)
			}
		}
		return out
	default:
		return nil
	}
}

func (a *App) providerMetadata() (map[string]any, error) {
	configured := a.Settings.OIDCIssuer
	cacheKey := oidcCacheKey("metadata", configured)
	if cached, ok := oidcCacheGet(cacheKey, oidcMetadataCacheSeconds); ok {
		return cached.(map[string]any), nil
	}
	metadata, err := oidcFetchJSON("GET",
		strings.TrimRight(configured, "/")+"/.well-known/openid-configuration", oidcUnavailable, nil, nil)
	if err != nil {
		return nil, err
	}
	issuer, _ := metadata["issuer"].(string)
	if strings.TrimRight(issuer, "/") != strings.TrimRight(configured, "/") {
		return nil, &OidcError{Code: oidcUnavailable,
			Detail: fmt.Sprintf("Discovery names issuer %v, not %v.", pyRepr(issuer), pyRepr(configured))}
	}
	for _, name := range []string{"authorization_endpoint", "token_endpoint", "jwks_uri"} {
		endpoint, _ := metadata[name].(string)
		if !a.Settings.oidcAllowedURL(endpoint, true) {
			return nil, &OidcError{Code: oidcUnavailable, Detail: "Discovery has no usable " + name + "."}
		}
	}
	if endpoint, _ := metadata["userinfo_endpoint"].(string); !a.Settings.oidcAllowedURL(endpoint, true) {
		delete(metadata, "userinfo_endpoint")
	}
	oidcCacheSet(cacheKey, metadata)
	return metadata, nil
}

func oidcJWKS(rawURL string, refresh bool) ([]map[string]any, error) {
	cacheKey := oidcCacheKey("jwks", rawURL)
	if !refresh {
		if cached, ok := oidcCacheGet(cacheKey, oidcMetadataCacheSeconds); ok {
			return cached.([]map[string]any), nil
		}
	}
	document, err := oidcFetchJSON("GET", rawURL, oidcUnavailable, nil, nil)
	if err != nil {
		return nil, err
	}
	var keys []map[string]any
	if listed, ok := document["keys"].([]any); ok {
		for _, item := range listed {
			if key, ok := item.(map[string]any); ok {
				keys = append(keys, key)
			}
		}
	}
	oidcCacheSet(cacheKey, keys)
	return keys, nil
}

func oidcCandidates(keys []map[string]any, kid, alg string) []map[string]any {
	keyType := oidcAlgorithmKeyType[alg]
	var out []map[string]any
	for _, key := range keys {
		if key["kty"] != keyType {
			continue
		}
		if _, private := key["d"]; private {
			continue
		}
		if rawUse, ok := key["use"]; ok {
			use, isString := rawUse.(string)
			if !isString || use != "sig" {
				continue
			}
		}
		if rawAlg, ok := key["alg"]; ok {
			keyAlg, isString := rawAlg.(string)
			if !isString || keyAlg != alg {
				continue
			}
		}
		if kid != "" && key["kid"] != kid {
			continue
		}
		out = append(out, key)
	}
	return out
}

func (a *App) oidcSigningKey(metadata map[string]any, kid, alg string) (crypto.PublicKey, error) {
	rawURL, _ := metadata["jwks_uri"].(string)
	keys, err := oidcJWKS(rawURL, false)
	if err != nil {
		return nil, err
	}
	candidates := oidcCandidates(keys, kid, alg)
	if len(candidates) == 0 {
		refreshKey := oidcCacheKey("jwks-refresh", rawURL)
		if oidcCacheAdd(refreshKey, 1, oidcJWKSRefreshSeconds) {
			keys, err = oidcJWKS(rawURL, true)
			if err != nil {
				return nil, err
			}
			candidates = oidcCandidates(keys, kid, alg)
		}
	}
	if len(candidates) != 1 {
		return nil, &OidcError{Code: oidcFailed,
			Detail: fmt.Sprintf("No single %s key for kid=%v (%d found).", alg, pyRepr(kid), len(candidates))}
	}
	key, err := oidcParseJWK(candidates[0], alg)
	if err != nil {
		return nil, &OidcError{Code: oidcFailed,
			Detail: fmt.Sprintf("Unusable key kid=%v: %v", pyRepr(kid), err)}
	}
	return key, nil
}

func oidcParseJWK(key map[string]any, alg string) (crypto.PublicKey, error) {
	decode := func(name string) ([]byte, error) {
		text, ok := key[name].(string)
		if !ok || text == "" {
			return nil, fmt.Errorf("missing %s", name)
		}
		return base64.RawURLEncoding.DecodeString(text)
	}
	switch oidcAlgorithmKeyType[alg] {
	case "RSA":
		nBytes, err := decode("n")
		if err != nil {
			return nil, err
		}
		eBytes, err := decode("e")
		if err != nil {
			return nil, err
		}
		e := 0
		for _, b := range eBytes {
			e = e<<8 | int(b)
		}
		if e == 0 {
			return nil, fmt.Errorf("invalid exponent")
		}
		return &rsa.PublicKey{N: new(big.Int).SetBytes(nBytes), E: e}, nil
	case "EC":
		curveName, _ := key["crv"].(string)
		var curve elliptic.Curve
		switch curveName {
		case "P-256":
			curve = elliptic.P256()
		case "P-384":
			curve = elliptic.P384()
		case "P-521":
			curve = elliptic.P521()
		default:
			return nil, fmt.Errorf("unsupported curve %q", curveName)
		}
		xBytes, err := decode("x")
		if err != nil {
			return nil, err
		}
		yBytes, err := decode("y")
		if err != nil {
			return nil, err
		}
		x, y := new(big.Int).SetBytes(xBytes), new(big.Int).SetBytes(yBytes)
		if !curve.IsOnCurve(x, y) {
			return nil, fmt.Errorf("point is not on the curve")
		}
		return &ecdsa.PublicKey{Curve: curve, X: x, Y: y}, nil
	case "OKP":
		xBytes, err := decode("x")
		if err != nil {
			return nil, err
		}
		if len(xBytes) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("invalid Ed25519 key")
		}
		return ed25519.PublicKey(xBytes), nil
	default:
		return nil, fmt.Errorf("unsupported algorithm %q", alg)
	}
}

func (a *App) oidcVerifyIDToken(token string, metadata map[string]any, nonce string) (map[string]any, error) {
	unverified, _, err := jwt.NewParser().ParseUnverified(token, jwt.MapClaims{})
	if err != nil {
		return nil, &OidcError{Code: oidcFailed, Detail: "Malformed ID token: " + err.Error()}
	}
	alg, _ := unverified.Header["alg"].(string)
	if _, ok := oidcAlgorithmKeyType[alg]; !ok {
		return nil, &OidcError{Code: oidcFailed,
			Detail: fmt.Sprintf("ID token signed with %v, which is not accepted.", pyRepr(alg))}
	}
	if supported := oidcStringList(metadata["id_token_signing_alg_values_supported"]); supported != nil && !containsString(supported, alg) {
		return nil, &OidcError{Code: oidcFailed,
			Detail: fmt.Sprintf("ID token signed with %v, which is not accepted.", pyRepr(alg))}
	}
	kid, _ := unverified.Header["kid"].(string)
	verifyingKey, err := a.oidcSigningKey(metadata, kid, alg)
	if err != nil {
		return nil, err
	}
	issuer, _ := metadata["issuer"].(string)
	clientID := a.Settings.OIDCClientID
	parser := jwt.NewParser(
		jwt.WithValidMethods([]string{alg}),
		jwt.WithAudience(clientID),
		jwt.WithIssuer(issuer),
		jwt.WithLeeway(oidcClockSkewSeconds*time.Second),
		jwt.WithExpirationRequired(),
		jwt.WithIssuedAt(),
	)
	claims := jwt.MapClaims{}
	parsed, err := parser.ParseWithClaims(token, claims, func(*jwt.Token) (any, error) {
		return verifyingKey, nil
	})
	if err != nil || parsed == nil || !parsed.Valid {
		detail := "ID token rejected."
		if err != nil {
			detail = "ID token rejected: " + err.Error()
		}
		return nil, &OidcError{Code: oidcFailed, Detail: detail}
	}
	for _, name := range []string{"iss", "sub", "aud", "exp", "iat"} {
		if _, ok := claims[name]; !ok {
			return nil, &OidcError{Code: oidcFailed, Detail: "ID token has no " + name + " claim."}
		}
	}
	tokenNonce, _ := claims["nonce"].(string)
	if !constantTimeEqual(tokenNonce, nonce) {
		return nil, &OidcError{Code: oidcFailed, Detail: "ID token nonce does not match."}
	}
	audiences := oidcAudienceValues(claims["aud"])
	azp, hasAZP := claims["azp"]
	if (hasAZP || len(audiences) > 1) && azp != clientID {
		return nil, &OidcError{Code: oidcFailed, Detail: "ID token was issued to another client (azp)."}
	}
	sub, _ := claims["sub"].(string)
	if sub == "" || len(sub) > 255 {
		return nil, &OidcError{Code: oidcFailed, Detail: "ID token has no usable sub."}
	}
	return claims, nil
}

func oidcAudienceValues(value any) []string {
	switch v := value.(type) {
	case string:
		return []string{v}
	case []any:
		var out []string
		for _, item := range v {
			if text, ok := item.(string); ok {
				out = append(out, text)
			}
		}
		return out
	case []string:
		return v
	default:
		return nil
	}
}

func constantTimeEqual(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	return subtleStringEqual(a, b)
}

func subtleStringEqual(a, b string) bool {
	var diff byte
	for i := 0; i < len(a); i++ {
		diff |= a[i] ^ b[i]
	}
	return diff == 0
}

func sortOIDCFlows(flows map[string]oidcFlow) []string {
	type pair struct {
		state string
		at    float64
	}
	pairs := make([]pair, 0, len(flows))
	for state, flow := range flows {
		pairs = append(pairs, pair{state, flow.At})
	}
	sort.Slice(pairs, func(i, j int) bool { return pairs[i].at < pairs[j].at })
	states := make([]string, len(pairs))
	for i, item := range pairs {
		states[i] = item.state
	}
	return states
}
