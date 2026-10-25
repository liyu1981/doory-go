// Email verification and password reset endpoints (accounts/views.py
// PasswordResetView, PasswordResetConfirmView, EmailVerifyView,
// EmailResendView) plus the email cooldown the resend guard uses.
package doory

import (
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

// emailCooldown ports lockout.claim_email_slot: a per-address, per-purpose
// cooldown held in process (Django's locmem cache upstream).
type emailCooldown struct {
	mu    sync.Mutex
	slots map[string]time.Time
}

var accountEmailCooldown = &emailCooldown{slots: map[string]time.Time{}}

func (c *emailCooldown) claim(email, purpose string, cooldown time.Duration) bool {
	key := "auth:cooldown:" + purpose + ":" + fingerprint(email, purpose)
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	if expiry, ok := c.slots[key]; ok && now.Before(expiry) {
		return false
	}
	c.slots[key] = now.Add(cooldown)
	return true
}

// passwordResetRequest ports PasswordResetView: always 204 so the endpoint
// cannot be used to discover which emails have accounts.
func (a *App) passwordResetRequest(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Email *string `json:"email"`
	}
	if err := jsonBody(r, &body); err != nil {
		writeDRF(w, http.StatusBadRequest, "parse_error", "Malformed request body.")
		return
	}
	if body.Email == nil {
		writeDRFValidation(w, []FieldError{SimpleField("email", "This field is required.", "required")})
		return
	}
	email := strings.ToLower(strings.TrimSpace(*body.Email))
	if len([]rune(email)) > 254 {
		writeDRFValidation(w, []FieldError{SimpleField("email",
			"Ensure this field has no more than 254 characters.", "max_length")})
		return
	}
	if !validEmail(email) {
		writeDRFValidation(w, []FieldError{SimpleField("email",
			"Enter a valid email address.", "invalid")})
		return
	}
	cooldown := time.Duration(a.Settings.AuthEmailCooldownSeconds) * time.Second
	if accountEmailCooldown.claim(email, "reset", cooldown) {
		user, err := getUserByEmail(a.DB, email)
		if err != nil {
			logerr(err, "passwordResetRequest.lookup")
		} else if user != nil && user.IsActive {
			a.sendPasswordResetEmail(user)
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// linkFields validates the uid/token pair shared by the two confirm views.
func linkFields(w http.ResponseWriter, uid, token *string) bool {
	var fields []FieldError
	if uid == nil {
		fields = append(fields, SimpleField("uid", "This field is required.", "required"))
	} else if len([]rune(*uid)) > 128 {
		fields = append(fields, SimpleField("uid",
			"Ensure this field has no more than 128 characters.", "max_length"))
	}
	if token == nil {
		fields = append(fields, SimpleField("token", "This field is required.", "required"))
	} else if len([]rune(*token)) > 128 {
		fields = append(fields, SimpleField("token",
			"Ensure this field has no more than 128 characters.", "max_length"))
	}
	if len(fields) > 0 {
		writeDRFValidation(w, fields)
		return false
	}
	return true
}

// passwordResetConfirm ports PasswordResetConfirmView: the link proves control
// of the mailbox, so a successful reset also verifies the address, ends the
// account lockout and revokes every API token.
func (a *App) passwordResetConfirm(w http.ResponseWriter, r *http.Request) {
	var body struct {
		UID         *string `json:"uid"`
		Token       *string `json:"token"`
		NewPassword *string `json:"new_password"`
	}
	if err := jsonBody(r, &body); err != nil {
		writeDRF(w, http.StatusBadRequest, "parse_error", "Malformed request body.")
		return
	}
	if !linkFields(w, body.UID, body.Token) {
		return
	}
	var fields []FieldError
	if body.NewPassword == nil {
		fields = append(fields, SimpleField("new_password", "This field is required.", "required"))
	} else if len([]rune(*body.NewPassword)) > passwordMaxLength {
		fields = append(fields, SimpleField("new_password",
			fmt.Sprintf("Ensure this field has no more than %d characters.", passwordMaxLength), "max_length"))
	}
	if len(fields) > 0 {
		writeDRFValidation(w, fields)
		return
	}
	u := a.decodeUID(*body.UID)
	if u == nil || !a.Settings.checkPasswordResetToken(u, *body.Token) {
		writeDRF(w, http.StatusBadRequest, "invalid_token", "This link is invalid or has expired.")
		return
	}
	var passwordErrors []FieldError
	for _, problem := range validateNewPassword(*body.NewPassword, u.Email) {
		passwordErrors = append(passwordErrors, SimpleField("new_password", problem.Message, problem.Code))
	}
	if len(passwordErrors) > 0 {
		writeDRFValidation(w, passwordErrors)
		return
	}
	now := formatDjangoDateTime(time.Now().UTC())
	if _, err := a.DB.Exec(`DELETE FROM accounts_oidcidentity WHERE user_id = ?`, u.ID); err != nil {
		logerr(err, "passwordResetConfirm.oidc")
		writeDRF(w, http.StatusInternalServerError, "server_error", "Something went wrong on our side.")
		return
	}
	if _, err := a.DB.Exec(`UPDATE accounts_user SET password = ?, is_email_verified = TRUE, updated_at = ?
		WHERE id = ?`, hashPassword(*body.NewPassword), now, u.ID); err != nil {
		logerr(err, "passwordResetConfirm.update")
		writeDRF(w, http.StatusInternalServerError, "server_error", "Something went wrong on our side.")
		return
	}
	if _, err := a.DB.Exec(`UPDATE apitokens_apitoken SET revoked_at = ?, updated_at = ?
		WHERE owner_id = ? AND revoked_at IS NULL`, now, now, u.ID); err != nil {
		logerr(err, "passwordResetConfirm.tokens")
	}
	a.lockout.reset(u.Email)
	w.WriteHeader(http.StatusNoContent)
}

// emailVerify ports EmailVerifyView: flip is_email_verified when the link's
// token covers the current state.
func (a *App) emailVerify(w http.ResponseWriter, r *http.Request) {
	var body struct {
		UID   *string `json:"uid"`
		Token *string `json:"token"`
	}
	if err := jsonBody(r, &body); err != nil {
		writeDRF(w, http.StatusBadRequest, "parse_error", "Malformed request body.")
		return
	}
	if !linkFields(w, body.UID, body.Token) {
		return
	}
	u := a.decodeUID(*body.UID)
	if u == nil || !a.Settings.checkEmailVerificationToken(u, *body.Token) {
		writeDRF(w, http.StatusBadRequest, "invalid_token", "This link is invalid or has expired.")
		return
	}
	if _, err := a.DB.Exec(`UPDATE accounts_user SET is_email_verified = TRUE, updated_at = ? WHERE id = ?`,
		formatDjangoDateTime(time.Now().UTC()), u.ID); err != nil {
		logerr(err, "emailVerify.update")
		writeDRF(w, http.StatusInternalServerError, "server_error", "Something went wrong on our side.")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// emailResend ports EmailResendView: already-verified accounts answer 204
// without sending; a send within the cooldown is a 429 with the wait.
func (a *App) emailResend(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	if !u.IsEmailVerified {
		cooldown := time.Duration(a.Settings.AuthEmailCooldownSeconds) * time.Second
		if !accountEmailCooldown.claim(u.Email, "verify", cooldown) {
			writeDRFThrottledDetail(w, cooldown.Seconds(), "A verification email was just sent.")
			return
		}
		a.sendVerificationEmail(u)
	}
	w.WriteHeader(http.StatusNoContent)
}
