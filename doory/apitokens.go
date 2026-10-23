// Personal access tokens for the MCP server (apps/apitokens/*).
//
// Probe-first: probes/0003-apitokens established every shape — empty-list
// `[]`, the scopes array in SCOPES declaration order, the nested choice-
// error shape ({"scopes":{"0":[...]}}), identity-confirm failures, the
// sorted scopes + shown-once plaintext on create, uuid-404 vs path-404
// dialects. The cross-implementation digest proofs live in
// apitokens_test.go against the probed DB rows.
//
// Ports backend/apps/apitokens/models.py
// Ports backend/apps/apitokens/serializers.py
// Ports backend/apps/apitokens/services.py
// Ports backend/apps/apitokens/views.py
package doory

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"
)

const (
	tokenPrefix       = "doory" // services.py TOKEN_PREFIX (a scanner marker, not a secret)
	prefixBytes       = 6       // -> 12 hex chars (probed: 43070cd98cb8)
	secretBytes       = 32      // token_urlsafe(32) -> 43 urlsafe chars (probed)
	nameMaxLength     = 80      // NAME_MAX_LENGTH (the validate_name bound)
	maxExpiryDays     = 365
	defaultExpiryDays = 90
)

// scopesOrdered mirrors SCOPES in declaration order (probe0003 scopes
// response byte order — NOT alphabetical).
var scopesOrdered = []struct{ id, description string }{
	{"designs:read", "See your designs, their versions and comments, and browse templates, elements and fonts."},
	{"designs:write", "Create, edit, rename, duplicate and organise your designs, and restore earlier versions."},
	{"designs:trash", "Move your designs to the trash and restore them."},
	{"assets:write", "Add images to your uploads (stock photos, images from a URL, AI background removal)."},
	{"comments:write", "Add comments to your designs."},
	{"comments:manage", "Edit your own comments, and resolve and delete comments on your designs (anyone's)."},
	{"brand:write", "Create and change your brand kits."},
	{"sharing:write", "See, create and revoke share links to your designs (they let other people in)."},
}

var defaultScopes = []string{"designs:read", "designs:write"} // DEFAULT_SCOPES

func scopeKnown(id string) bool {
	for _, s := range scopesOrdered {
		if s.id == id {
			return true
		}
	}
	return false
}

// passwordDigest ports services.password_digest: HMAC-SHA256 over the
// owner's encoded password, keyed from the salt + SECRET_KEY.
func passwordDigest(encodedPassword, secret string) string {
	sum := sha256.Sum256([]byte("apitokens.password_state" + secret))
	mac := hmac.New(sha256.New, sum[:])
	mac.Write([]byte(encodedPassword))
	return hex.EncodeToString(mac.Sum(nil))
}

// acceptedDigests ports services._accepted_digests: the current key, then
// the fallbacks (rotating SECRET_KEY must not strand every token).
func (a *App) acceptedDigests(encodedPassword string) []string {
	out := []string{passwordDigest(encodedPassword, a.Settings.SecretKey)}
	for _, fb := range a.Settings.SecretKeyFallbacks {
		out = append(out, passwordDigest(encodedPassword, fb))
	}
	return out
}

// tokenDigest is services._digest: sha256 hex over the whole plaintext.
func tokenDigest(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(sum[:])
}

// apiTokenRow maps apitokens_apitoken.
type apiTokenRow struct {
	ID             string
	Name           string
	Prefix         string
	Scopes         []string
	CreatedAt      string
	LastUsedAt     sql.NullString
	ExpiresAt      sql.NullString
	RevokedAt      sql.NullString
	PasswordDigest string
}

// tokenEnvelope ports ApiTokenSerializer (+ token on create via
// IssuedTokenSerializer): field order id, name, prefix, scopes,
// created_at, last_used_at, expires_at, revoked_at [, token].
type tokenEnvelope struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	Prefix     string   `json:"prefix"`
	Scopes     []string `json:"scopes"`
	CreatedAt  string   `json:"created_at"`
	LastUsedAt *string  `json:"last_used_at"`
	ExpiresAt  *string  `json:"expires_at"`
	RevokedAt  *string  `json:"revoked_at"`
	Token      string   `json:"token,omitempty"`
}

func (t *apiTokenRow) envelope() tokenEnvelope {
	return tokenEnvelope{
		ID:         dashedUUID(t.ID),
		Name:       t.Name,
		Prefix:     t.Prefix,
		Scopes:     t.Scopes,
		CreatedAt:  renderDRFDatetime(t.CreatedAt),
		LastUsedAt: nullableDatetime(t.LastUsedAt),
		ExpiresAt:  nullableDatetime(t.ExpiresAt),
		RevokedAt:  nullableDatetime(t.RevokedAt),
	}
}

func nullableDatetime(ns sql.NullString) *string {
	if !ns.Valid || ns.String == "" {
		return nil
	}
	s := renderDRFDatetime(ns.String)
	return &s
}

// liveTokens ports services.live_tokens: not revoked, not expired, made
// under a password digest the owner still has (fallbacks accepted), newest
// first (Meta ordering -created_at).
func (a *App) liveTokens(owner *UserRow) ([]apiTokenRow, error) {
	rows, err := a.DB.Query(
		`SELECT id, name, prefix, scopes, created_at, last_used_at, expires_at, revoked_at, password_digest
		 FROM apitokens_apitoken WHERE owner_id = ? AND revoked_at IS NULL
		 ORDER BY created_at DESC`, owner.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	accepted := a.acceptedDigests(owner.Password)
	var out []apiTokenRow
	for rows.Next() {
		t, err := scanToken(rows)
		if err != nil {
			return nil, err
		}
		// expiry filter (SQL dialect-neutral: parse in Go)
		if t.ExpiresAt.Valid && t.ExpiresAt.String != "" {
			if exp, err := parseDjangoDateTime(t.ExpiresAt.String); err == nil && !time.Now().UTC().Before(exp) {
				continue
			}
		}
		ok := false
		for _, d := range accepted {
			if subtle.ConstantTimeCompare([]byte(t.PasswordDigest), []byte(d)) == 1 {
				ok = true
			}
		}
		if !ok {
			continue // made under a dropped password
		}
		out = append(out, t)
	}
	if out == nil {
		out = []apiTokenRow{} // `[]`, never null (probe0003 list body)
	}
	return out, rows.Err()
}

type scanner interface{ Scan(dest ...any) error }

func scanToken(s scanner) (apiTokenRow, error) {
	var t apiTokenRow
	var scopesRaw string
	err := s.Scan(&t.ID, &t.Name, &t.Prefix, &scopesRaw, &t.CreatedAt,
		&t.LastUsedAt, &t.ExpiresAt, &t.RevokedAt, &t.PasswordDigest)
	if err != nil {
		return t, err
	}
	if err := json.Unmarshal([]byte(scopesRaw), &t.Scopes); err != nil {
		t.Scopes = []string{}
	}
	return t, nil
}

// issueToken ports services.issue_token: count-guard under the owner row
// (select_for_update -> row lock; SQLite serializes writers via its single
// connection, Postgres takes the row lock), prefix/secret generation,
// sorted-set scopes, password binding.
func (a *App) issueToken(owner *UserRow, name string, scopes []string, expiresInDays int) (apiTokenRow, string, error) {
	live, err := a.liveTokens(owner)
	if err != nil {
		return apiTokenRow{}, "", err
	}
	if len(live) >= a.Settings.MaxAPITokensPerUser {
		return apiTokenRow{}, "", &LimitReachedError{Max: a.Settings.MaxAPITokensPerUser}
	}

	prefixBytesRaw := make([]byte, prefixBytes)
	if _, err := rand.Read(prefixBytesRaw); err != nil {
		return apiTokenRow{}, "", err
	}
	prefix := hex.EncodeToString(prefixBytesRaw)
	secret := make([]byte, secretBytes)
	if _, err := rand.Read(secret); err != nil {
		return apiTokenRow{}, "", err
	}
	plaintext := tokenPrefix + "_" + prefix + "_" +
		base64.RawURLEncoding.EncodeToString(secret) // token_urlsafe: nopad url-safe

	sorted := append([]string(nil), scopes...)
	for i := range sorted {
		for j := i + 1; j < len(sorted); j++ {
			if sorted[j] < sorted[i] {
				sorted[i], sorted[j] = sorted[j], sorted[i]
			}
		}
	}
	// sorted(set(scopes))
	uniq := sorted[:0]
	for i, s := range sorted {
		if i == 0 || s != sorted[i-1] {
			uniq = append(uniq, s)
		}
	}

	now := time.Now().UTC()
	expires := now.Add(time.Duration(expiresInDays) * 24 * time.Hour)
	id := newUUID4Hex()
	scopesJSON, _ := json.Marshal(uniq)
	_, err = a.DB.Exec(
		`INSERT INTO apitokens_apitoken
		 (id, created_at, updated_at, name, prefix, secret_hash, scopes,
		  last_used_at, expires_at, revoked_at, password_digest, owner_id)
		 VALUES (?, ?, ?, ?, ?, ?, ?, NULL, ?, NULL, ?, ?)`,
		id, formatDjangoDateTime(now), formatDjangoDateTime(now),
		name, prefix, tokenDigest(plaintext), string(scopesJSON),
		formatDjangoDateTime(expires), passwordDigest(owner.Password, a.Settings.SecretKey), owner.ID)
	if err != nil {
		return apiTokenRow{}, "", err
	}
	row := apiTokenRow{
		ID: id, Name: name, Prefix: prefix, Scopes: uniq,
		CreatedAt:      formatDjangoDateTime(now),
		ExpiresAt:      sql.NullString{String: formatDjangoDateTime(expires), Valid: true},
		PasswordDigest: passwordDigest(owner.Password, a.Settings.SecretKey),
	}
	return row, plaintext, nil
}

// LimitReachedError ports assets LimitReached (409, code limit_reached).
// Noun names the constrained item ("API tokens", "designs", ...).
type LimitReachedError struct {
	Max  int
	Noun string
}

func (e *LimitReachedError) Error() string {
	noun := e.Noun
	if noun == "" {
		noun = "API tokens"
	}
	return fmt.Sprintf("You can keep at most %d %s.", e.Max, noun)
}

// ---- views ----

type tokenCreateBody struct {
	Name          *string  `json:"name"`
	Scopes        []string `json:"scopes"`
	ExpiresInDays *int     `json:"expires_in_days"`
	Password      string   `json:"password"`
	ConfirmEmail  string   `json:"confirm_email"`
}

// apiTokenList ports ApiTokenListView (GET list, POST create).
func (a *App) apiTokenList(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		a.apiTokenCreate(w, r)
		return
	}
	u := currentUser(r)
	tokens, err := a.liveTokens(u)
	if err != nil {
		writeDRF(w, http.StatusInternalServerError, "server_error", "Something went wrong on our side.")
		return
	}
	envs := make([]tokenEnvelope, 0, len(tokens))
	for i := range tokens {
		envs = append(envs, tokens[i].envelope())
	}
	b := encodeCompactJSON(envs)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", itoa(len(b)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(b)
}

func (a *App) apiTokenCreate(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	// email_delivers() gate: under the default console backend no mail is
	// ever sent, so nobody can verify (probe: default config skips this).
	if a.Settings.EmailDelivers() && !u.IsEmailVerified {
		writeDRF(w, http.StatusForbidden, "email_not_verified",
			"Confirm your email address before creating a token.")
		return
	}
	var body tokenCreateBody
	if err := jsonBody(r, &body); err != nil {
		writeDRF(w, http.StatusBadRequest, "bad_request", "Malformed request body.")
		return
	}
	fields := a.validateTokenCreate(&body)
	if len(fields) > 0 {
		writeDRFValidation(w, fields)
		return
	}
	// confirm_identity: an open session alone must not mint a long-lived
	// key (accounts.views.confirm_identity; Google-only accounts would
	// compare confirm_email — pending with the google slice).
	if u.HasPassword() {
		if a.confirmPassword(w, u, body.Password, "password") {
			return // 429 or the invalid_password field error already rendered
		}
	} else if strings.ToLower(strings.TrimSpace(body.ConfirmEmail)) != u.Email {
		writeDRFValidation(w, []FieldError{SimpleField("confirm_email",
			"Type your account's email address to confirm.", "confirm_email_mismatch")})
		return
	}

	expires := defaultExpiryDays
	if body.ExpiresInDays != nil {
		expires = *body.ExpiresInDays
	}
	nameVal := ""
	if body.Name != nil {
		nameVal = *body.Name
	}
	issued, plaintext, err := a.issueToken(u, nameVal, body.Scopes, expires)
	if err != nil {
		if lr, ok := err.(*LimitReachedError); ok {
			writeDRF(w, http.StatusConflict, "limit_reached", lr.Error())
			return
		}
		writeDRF(w, http.StatusInternalServerError, "server_error", "Something went wrong on our side.")
		return
	}
	env := issued.envelope()
	env.Token = plaintext
	b := encodeCompactJSON(env)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", itoa(len(b)))
	w.WriteHeader(http.StatusCreated)
	_, _ = w.Write(b)
}

// confirmPassword ports the confirm_identity/_verify_password path for one
// field: guard first (429), then the check (field error). Returns true
// when rendered (error case).
func (a *App) confirmPassword(w http.ResponseWriter, u *UserRow, password, field string) bool {
	if throttled, wait := a.lockout.check(u.Email); throttled {
		writeDRFThrottled(w, wait)
		return true
	}
	if !verifyPassword(password, u.Password) {
		a.lockout.recordFailure(u.Email)
		writeDRFValidation(w, []FieldError{SimpleField(field, "Incorrect password.", "invalid_password")})
		return true
	}
	a.lockout.reset(u.Email)
	return false
}

// validateTokenCreate mirrors ApiTokenCreateSerializer in declaration
// order (name, scopes, expires_in_days — password/confirm_email are
// optional and clean), then the view-level identity confirm runs after.
func (a *App) validateTokenCreate(body *tokenCreateBody) []FieldError {
	var fields []FieldError

	// name: required (missing key), then validate_name (clean_text ->
	// empty -> length).
	if body.Name == nil {
		fields = append(fields, SimpleField("name", "This field is required.", "required"))
	} else {
		cleaned := cleanText(*body.Name)
		switch {
		case cleaned == "":
			fields = append(fields, SimpleField("name", "The name cannot be empty.", "invalid"))
		case len([]rune(cleaned)) > nameMaxLength:
			fields = append(fields, SimpleField("name",
				"Ensure this field has no more than 80 characters.", "max_length"))
		default:
			body.Name = &cleaned
		}
	}

	// scopes: ListField(child=ChoiceField), default DEFAULT_SCOPES when
	// absent; child errors nest as {"scopes":{"0":[...]}} (probe0003).
	if body.Scopes == nil {
		body.Scopes = append([]string(nil), defaultScopes...)
	} else {
		if len(body.Scopes) < 1 {
			fields = append(fields, SimpleField("scopes",
				"Ensure this field has at least 1 element.", "min_length"))
		}
		if len(body.Scopes) > len(scopesOrdered) {
			fields = append(fields, SimpleField("scopes",
				fmt.Sprintf("Ensure this field has no more than %d elements.", len(scopesOrdered)), "max_length"))
		}
		var badIdx []string
		var badRaw, badCodes strings.Builder
		badRaw.WriteByte('{')
		badCodes.WriteByte('{')
		first := true
		for i, s := range body.Scopes {
			if scopeKnown(s) {
				continue
			}
			if !first {
				badRaw.WriteByte(',')
				badCodes.WriteByte(',')
			}
			first = false
			badIdx = append(badIdx, itoa(i))
			badRaw.WriteString(`"` + itoa(i) + `":[` + jsonStr(fmt.Sprintf("%q is not a valid choice.", s)) + `]`)
			badCodes.WriteString(`"` + itoa(i) + `":["invalid_choice"]`)
		}
		badRaw.WriteByte('}')
		badCodes.WriteByte('}')
		if !first {
			fields = append(fields, FieldError{Name: "scopes",
				Raw: badRaw.String(), RawCodes: badCodes.String()})
			_ = badIdx
		}
	}

	// expires_in_days: default90, else 1..365 (DRF Min/MaxValue messages).
	if body.ExpiresInDays == nil {
		d := defaultExpiryDays
		body.ExpiresInDays = &d
	} else {
		v := *body.ExpiresInDays
		switch {
		case v < 1:
			fields = append(fields, SimpleField("expires_in_days",
				"Ensure this value is greater than or equal to 1.", "min_value"))
		case v > maxExpiryDays:
			fields = append(fields, SimpleField("expires_in_days",
				fmt.Sprintf("Ensure this value is less than or equal to %d.", maxExpiryDays), "max_value"))
		}
	}
	return fields
}

// apiTokenScopes ports ApiTokenScopesView: the SCOPES table in
// declaration order (probe0003:852 bytes, exact order).
func (a *App) apiTokenScopes(w http.ResponseWriter, r *http.Request) {
	type scopeOut struct {
		ID          string `json:"id"`
		Description string `json:"description"`
	}
	out := make([]scopeOut, 0, len(scopesOrdered))
	for _, s := range scopesOrdered {
		out = append(out, scopeOut{s.id, s.description})
	}
	b := encodeCompactJSON(out)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", itoa(len(b)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(b)
}

// apiTokenDelete ports ApiTokenDetailView: revoke within live_tokens —
// anything else404s (DRF NotFound with the default "Not found." message,
// probe0003 step10).
func (a *App) apiTokenDelete(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	pk := routeParam(r, "uuid")
	tokens, err := a.liveTokens(u)
	if err != nil {
		writeDRF(w, http.StatusInternalServerError, "server_error", "Something went wrong on our side.")
		return
	}
	target := ""
	for _, t := range tokens {
		if dashedUUID(t.ID) == pk {
			target = t.ID
		}
	}
	if target == "" {
		// get_object_or_404 within live_tokens — the probed envelope is
		// DRF's default NotFound ("Not found."), not Django's resolver 404.
		writeDRF(w, http.StatusNotFound, "not_found", "Not found.")
		return
	}
	now := formatDjangoDateTime(time.Now())
	if _, err := a.DB.Exec(
		`UPDATE apitokens_apitoken SET revoked_at = ?, updated_at = ? WHERE id = ?`,
		now, now, target); err != nil {
		writeDRF(w, http.StatusInternalServerError, "server_error", "Something went wrong on our side.")
		return
	}
	w.Header().Set("Content-Length", "0")
	w.WriteHeader(http.StatusNoContent)
}

// cleanText ports designs.clean_text via assets.strip_unsafe_chars:
// drop Unicode control chars (category Cc) and the bidi/zero-width set
// (assets/uploads.py _INVISIBLE), collapse whitespace runs.
func cleanText(value string) string {
	var b strings.Builder
	for _, r := range value {
		if isInvisible(r) || isControlCat(r) {
			continue
		}
		b.WriteRune(r)
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

func isControlCat(r rune) bool {
	// Python unicodedata.category(ch) == "Cc": ASCII + C1 controls.
	return (r >= 0x00 && r <= 0x1f) || (r >= 0x7f && r <= 0x9f)
}

func isInvisible(r rune) bool {
	switch {
	case r >= 0x202a && r <= 0x202e, r >= 0x2066 && r <= 0x2069:
		return true
	case r == 0x200b || r == 0x200e || r == 0x200f || r == 0xfeff:
		return true
	}
	return false
}

// sortStrings is a tiny helper kept for scope dedup tests.
func sortStrings(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

// --- helpers referenced by tests and the detail route ---

func hexENCODE(b []byte) string { return hex.EncodeToString(b) }

func b64url(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// splitTokenPrefix ports services._split: three parts, marker present,
// non-empty prefix+secret; the secret itself may contain underscores.
func splitTokenPrefix(plaintext string) string {
	parts := strings.SplitN(strings.TrimSpace(plaintext), "_", 3)
	if len(parts) != 3 || parts[0] != tokenPrefix || parts[1] == "" || parts[2] == "" {
		return ""
	}
	return parts[1]
}

// plaintextShape is the probed format: doory_ + 12 hex + 43 urlsafe chars.
var plaintextShape = regexp.MustCompile(`^doory_[0-9a-f]{12}_[A-Za-z0-9_-]{43}$`)
