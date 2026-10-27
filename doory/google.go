// Google Identity Services ID-token verification and account linking
// (apps/accounts/google.py + the GoogleLoginView branch of accounts/views.py
// plus services.sign_in_with_google).
// Ports backend/apps/accounts/google.py

package doory

import (
	"crypto"
	"database/sql"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const (
	googleCertsURL       = "https://www.googleapis.com/oauth2/v3/certs"
	googleCredentialMax  = 8192
	googleAllowedIssuer1 = "accounts.google.com"
	googleAllowedIssuer2 = "https://accounts.google.com"
)

// GoogleSignInError is the credential failure whose message reaches the
// client (invalid_google_token).
type GoogleSignInError struct{ Message string }

func (e *GoogleSignInError) Error() string { return e.Message }

// GoogleIdentity is a verified Google account.
type GoogleIdentity struct {
	Sub   string
	Email string
	Name  string
}

func googleInvalidCredential() *GoogleSignInError {
	return &GoogleSignInError{Message: "The Google credential is invalid or expired."}
}

func googleVerifyCredential(credential, clientID string) (GoogleIdentity, error) {
	unverified, _, err := jwt.NewParser().ParseUnverified(credential, jwt.MapClaims{})
	if err != nil {
		return GoogleIdentity{}, googleInvalidCredential()
	}
	if alg, _ := unverified.Header["alg"].(string); alg != "RS256" {
		return GoogleIdentity{}, googleInvalidCredential()
	}
	kid, _ := unverified.Header["kid"].(string)
	keys, err := oidcJWKS(googleCertsURL, false)
	if err != nil {
		return GoogleIdentity{}, googleInvalidCredential()
	}
	candidates := oidcCandidates(keys, kid, "RS256")
	if len(candidates) != 1 && oidcCacheAdd(oidcCacheKey("jwks-refresh", googleCertsURL), 1, oidcJWKSRefreshSeconds) {
		keys, err = oidcJWKS(googleCertsURL, true)
		if err == nil {
			candidates = oidcCandidates(keys, kid, "RS256")
		}
	}
	if len(candidates) != 1 {
		return GoogleIdentity{}, googleInvalidCredential()
	}
	var key crypto.PublicKey
	key, err = oidcParseJWK(candidates[0], "RS256")
	if err != nil {
		return GoogleIdentity{}, googleInvalidCredential()
	}
	parser := jwt.NewParser(
		jwt.WithValidMethods([]string{"RS256"}),
		jwt.WithExpirationRequired(),
		jwt.WithIssuedAt(),
		jwt.WithLeeway(oidcClockSkewSeconds*time.Second),
	)
	claims := jwt.MapClaims{}
	parsed, err := parser.ParseWithClaims(credential, claims, func(*jwt.Token) (any, error) {
		return key, nil
	})
	if err != nil || !parsed.Valid {
		return GoogleIdentity{}, googleInvalidCredential()
	}
	issuer, _ := claims["iss"].(string)
	if issuer != googleAllowedIssuer1 && issuer != googleAllowedIssuer2 {
		return GoogleIdentity{}, googleInvalidCredential()
	}
	audiences := oidcAudienceValues(claims["aud"])
	audienceOK := clientID == ""
	for _, audience := range audiences {
		if audience == clientID {
			audienceOK = true
			break
		}
	}
	if !audienceOK {
		return GoogleIdentity{}, googleInvalidCredential()
	}
	sub, _ := claims["sub"].(string)
	email, _ := claims["email"].(string)
	email = strings.ToLower(strings.TrimSpace(email))
	if sub == "" || email == "" {
		return GoogleIdentity{}, &GoogleSignInError{Message: "The Google account did not share an email address."}
	}
	if !oidcTruthy(claims["email_verified"]) {
		return GoogleIdentity{}, &GoogleSignInError{
			Message: "The Google account's email address is not verified."}
	}
	name, _ := claims["name"].(string)
	return GoogleIdentity{Sub: sub, Email: email, Name: name}, nil
}

func (a *App) googleLogin(w http.ResponseWriter, r *http.Request) {
	if a.Settings.GoogleClientID == "" {
		writeDRF(w, http.StatusBadRequest, "google_not_configured",
			"Google sign-in is not configured on this server.")
		return
	}
	var body struct {
		Credential *string `json:"credential"`
	}
	if err := jsonBody(r, &body); err != nil {
		writeDRF(w, http.StatusBadRequest, "parse_error", "Malformed request body.")
		return
	}
	if body.Credential == nil {
		writeDRFValidation(w, []FieldError{SimpleField("credential", "This field is required.", "required")})
		return
	}
	credential := strings.TrimSpace(*body.Credential)
	if credential == "" {
		writeDRFValidation(w, []FieldError{SimpleField("credential", "This field may not be blank.", "blank")})
		return
	}
	if len(credential) > googleCredentialMax {
		writeDRFValidation(w, []FieldError{SimpleField("credential",
			fmt.Sprintf("Ensure this field has no more than %d characters.", googleCredentialMax), "max_length")})
		return
	}
	identity, err := googleVerifyCredential(credential, a.Settings.GoogleClientID)
	if err != nil {
		message := "The Google credential is invalid or expired."
		if gerr, ok := err.(*GoogleSignInError); ok {
			message = gerr.Message
		}
		writeDRF(w, http.StatusBadRequest, "invalid_google_token", message)
		return
	}
	user, created, signInErr := a.signInWithGoogle(identity)
	if signInErr != nil {
		writeDRF(w, http.StatusBadRequest, signInErr.Code, signInErr.Message)
		return
	}
	a.finishLogin(w, r, user)
	payload := struct {
		User    userEnvelope `json:"user"`
		Created bool         `json:"created"`
	}{User: user.envelope(), Created: created}
	b := encodeCompactJSON(payload)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", itoa(len(b)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(b)
}

type socialSignInError struct {
	Code    string
	Message string
}

func (e *socialSignInError) Error() string { return e.Message }

// signInWithGoogle ports services.sign_in_with_google, retrying a lost
// uniqueness race once.
func (a *App) signInWithGoogle(identity GoogleIdentity) (*UserRow, bool, *socialSignInError) {
	user, created, err := a.linkOrCreateGoogle(identity)
	if isUniqueViolation(err) {
		user, created, err = a.linkOrCreateGoogle(identity)
	}
	if err != nil {
		if socialErr, ok := err.(*socialSignInError); ok {
			return nil, false, socialErr
		}
		return nil, false, &socialSignInError{Code: "server_error", Message: err.Error()}
	}
	return user, created, nil
}

func (a *App) linkOrCreateGoogle(identity GoogleIdentity) (*UserRow, bool, error) {
	tx, err := a.DB.Begin()
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()
	user, err := getUserByGoogleSubQ(tx, identity.Sub)
	if err != nil {
		return nil, false, err
	}
	if user == nil {
		user, err = getUserByEmailCIQ(tx, identity.Email)
		if err != nil {
			return nil, false, err
		}
		if user != nil && user.GoogleSub.Valid && user.GoogleSub.String != "" && user.GoogleSub.String != identity.Sub {
			return nil, false, &socialSignInError{Code: "google_account_mismatch",
				Message: "This email address is already linked to another Google account."}
		}
	}
	if user != nil {
		if !user.IsActive {
			return nil, false, &socialSignInError{Code: "account_disabled", Message: "This account has been disabled."}
		}
		if !user.GoogleSub.Valid || user.GoogleSub.String == "" {
			user.GoogleSub = sql.NullString{String: identity.Sub, Valid: true}
		}
		if !user.IsEmailVerified {
			if err := forgetUnprovenSignIns(tx, user); err != nil {
				return nil, false, err
			}
		}
		if _, err := tx.Exec(`UPDATE accounts_user SET google_sub = ?, updated_at = ? WHERE id = ?`,
			identity.Sub, formatDjangoDateTime(time.Now()), user.ID); err != nil {
			return nil, false, err
		}
		if err := tx.Commit(); err != nil {
			return nil, false, err
		}
		reloaded, err := getUserByIDQ(a.DB, user.ID)
		if err != nil {
			return nil, false, err
		}
		if reloaded == nil {
			return nil, false, &socialSignInError{Code: "server_error", Message: "account disappeared"}
		}
		return reloaded, false, nil
	}
	sub := identity.Sub
	user, err = createSocialUser(tx, identity.Email,
		truncateRunes(cleanDisplayName(identity.Name), 80), true, &sub)
	if err != nil {
		return nil, false, err
	}
	if err := tx.Commit(); err != nil {
		return nil, false, err
	}
	return user, true, nil
}
