// MCP bearer auth + transport edge: apps/mcp/auth.py (TokenAuthMiddleware)
// and the /mcp mount surface behind it (server, tools/resources/prompts).
//
// Endpoints (Streamable HTTP, stateless JSON responses):
//   GET  /mcp/health  -> DB health probe (no auth; 200/503 {"status":...})
//   POST /mcp         -> JSON-RPC 2.0 over HTTP (bearer PAT required)
//
// Off-switch (MCP_ENABLED): anon -> 404 {"error":"not_found",
// "error_description":"The MCP server is turned off."}.

package doory

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

// mcpNotFoundBody is the off-switch refusal shape (auth.py).
type mcpNotFoundBody struct {
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

// mcpInvalidTokenBody is the bearer refusal shape (auth.py).
type mcpInvalidTokenBody struct {
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

// mcpHealth answers the container/health probe: DB connectivity only.
func (a *App) mcpHealth(w http.ResponseWriter, r *http.Request) {
	if !a.dbReachable() {
		writeDjangoJSON(w, http.StatusServiceUnavailable, "unavailable", `{"status":"unavailable"}`)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	b, _ := json.Marshal(map[string]string{"status": "ok"})
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(b)
}

// mcpHandler is the Streamable HTTP app: bearer auth -> JSON-RPC dispatch.
func (a *App) mcpHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST, OPTIONS")
		writeDjangoJSON(w, http.StatusMethodNotAllowed, "method_not_allowed", "Method not allowed.")
		return
	}
	if !a.Settings.MCPEnabled {
		writeMcpNotFound(w)
		return
	}
	if _, err := a.authenticateMcpToken(r); err != nil {
		writeMcpInvalidToken(w)
		return
	}
	a.mcpDispatch(w, r)
}

// mcpNotFound answers the off-switch / disabled shape (auth.py).
func writeMcpNotFound(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNotFound)
	b, _ := json.Marshal(mcpNotFoundBody{Error: "not_found", ErrorDescription: "The MCP server is turned off."})
	_, _ = w.Write(b)
}

// writeMcpInvalidToken answers the bearer refusal (auth.py): 401 with the
// WWW-Authenticate challenge + JSON error body.
func writeMcpInvalidToken(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="Doory", error="invalid_token"`)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusUnauthorized)
	b, _ := json.Marshal(mcpInvalidTokenBody{
		Error:            "invalid_token",
		ErrorDescription: "Send a Doory personal access token: Authorization: Bearer doory_…",
	})
	_, _ = w.Write(b)
}

// errMcpTokenSentinel marks the bearer-refusal path.
type errMcpTokenSentinel struct{}

func (errMcpTokenSentinel) Error() string { return "invalid token" }

// authenticateMcpToken ports apps/mcp/auth.py TokenAuthMiddleware ->
// apps/apitokens.services.authenticate: bearer plaintext -> token row ->
// owner user; nil/expired/revoked -> refused.
func (a *App) authenticateMcpToken(r *http.Request) (*McpCaller, error) {
	authz := r.Header.Get("Authorization")
	value, ok := bearerToken(authz)
	if !ok || value == "" {
		return nil, errMcpTokenSentinel{}
	}
	userID, tokenRow, ok2 := a.lookupMcpToken(value)
	if !ok2 || userID == "" || tokenRow == nil {
		return nil, errMcpTokenSentinel{}
	}
	u, uerr := getUserByID(a.DB, userID)
	if uerr != nil || u == nil {
		return nil, errMcpTokenSentinel{}
	}
	if tokenRow.RevokedAt.Valid || tokenRow.ExpiresAt.Valid {
		return nil, errMcpTokenSentinel{}
	}
	return &McpCaller{User: u, Token: value, Scopes: tokenRow.Scopes, Label: tokenRow.Name}, nil
}

// bearerToken splits "Bearer <token>" per auth.py (scheme lower-cased,
// value trimmed; "Bearer" alone or empty -> not a bearer credential).
func bearerToken(authz string) (string, bool) {
	s := trimStr(authz)
	s = trimStr(s)
	var scheme, value string
	if i := indexByte(s, ' '); i >= 0 {
		scheme = lowerStr(trimStr(s[:i]))
		value = trimStr(s[i+1:])
	} else {
		scheme = lowerStr(trimStr(s))
		value = ""
	}
	if scheme != "bearer" || value == "" {
		return "", false
	}
	return value, true
}

func indexByte(s string, b byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			return i
		}
	}
	return -1
}

func trimStr(s string) string {
	start := 0
	for start < len(s) && (s[start] == ' ' || s[start] == '\t' || s[start] == '\n' || s[start] == '\r') {
		start++
	}
	end := len(s)
	for end > start && (s[end-1] == ' ' || s[end-1] == '\t' || s[end-1] == '\n' || s[end-1] == '\r') {
		end--
	}
	return s[start:end]
}

func lowerStr(s string) string {
	b := []byte(s)
	for i := range b {
		if b[i] >= 'A' && b[i] <= 'Z' {
			b[i] += 'a' - 'A'
		}
	}
	return string(b)
}

// lookupMcpToken ports apps/apitokens/services.authenticate: bearer plaintext
// -> token row (prefix + whole-token sha256 digest + the owner's password
// digest) -> owner user id.
func (a *App) lookupMcpToken(plaintext string) (string, *apiTokenRow, bool) {
	plaintext = strings.TrimSpace(plaintext)
	parts := strings.SplitN(plaintext, "_", 3)
	if len(parts) != 3 || parts[0] != tokenPrefix || parts[1] == "" || parts[2] == "" {
		return "", nil, false
	}
	prefix := parts[1]
	var row apiTokenRow
	var scopesRaw, secretHash, ownerID string
	err := a.DB.QueryRow(
		`SELECT id, name, prefix, scopes, created_at, last_used_at, expires_at, revoked_at,
		        password_digest, secret_hash, owner_id
		 FROM apitokens_apitoken WHERE prefix = ?`, prefix).Scan(
		&row.ID, &row.Name, &row.Prefix, &scopesRaw, &row.CreatedAt,
		&row.LastUsedAt, &row.ExpiresAt, &row.RevokedAt, &row.PasswordDigest, &secretHash, &ownerID)
	if err != nil {
		return "", nil, false
	}
	if subtle.ConstantTimeCompare([]byte(tokenDigest(plaintext)), []byte(secretHash)) != 1 {
		return "", nil, false
	}
	if row.RevokedAt.Valid {
		return "", nil, false
	}
	if row.ExpiresAt.Valid && row.ExpiresAt.String != "" {
		if exp, perr := parseDjangoDateTime(row.ExpiresAt.String); perr == nil && !time.Now().UTC().Before(exp) {
			return "", nil, false
		}
	}
	owner, uerr := getUserByID(a.DB, ownerID)
	if uerr != nil || owner == nil || !owner.IsActive {
		return "", nil, false
	}
	matched := false
	for _, digest := range a.acceptedDigests(owner.Password) {
		if subtle.ConstantTimeCompare([]byte(row.PasswordDigest), []byte(digest)) == 1 {
			matched = true
			break
		}
	}
	if !matched {
		return "", nil, false // the password changed since the token was made
	}
	var scopes []string
	if jsonErr := json.Unmarshal([]byte(scopesRaw), &scopes); jsonErr != nil {
		scopes = []string{}
	}
	row.Scopes = scopes
	return ownerID, &row, true
}

// dbReachable is the MCP health DB probe (auth._database_ok).
func (a *App) dbReachable() bool {
	if a.DB == nil {
		return false
	}
	if err := a.DB.Ping(); err != nil {
		return false
	}
	return true
}
