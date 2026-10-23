// The accounts auth views, M2 slice.
//
// Ports backend/apps/accounts/views.py
// register, login, logout, me (GET/PATCH), password/change.
//
// Deferred within the family (PORT_MAP rows stay 🟡 with pending notes):
// password/reset + confirm, email/verify + resend (D9 email content needs a
// log-scraping or locmem assertion), google (fake-provider probe),
// me DELETE (confirm_identity + cascade semantics), avatar upload (Pillow
// parity, assets/core family).
package doory

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"
)

// maxJSONBody ports BoundedJSONParser's cap (core/parsers DEFAULT_MAX_BYTES);
// exceeding it answers 413 like PayloadTooLarge ("The request body is too
// large.") — unprobed path, recorded for the scenario backlog.
const maxJSONBody = 5 * 1024 * 1024

// jsonBody decodes a bounded JSON object.
func jsonBody(r *http.Request, dst any) error {
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxJSONBody+1))
	if err != nil {
		return err
	}
	if len(raw) > maxJSONBody {
		return errors.New("too large")
	}
	if len(raw) == 0 {
		return nil
	}
	// UseNumber: document validation needs integer/float distinction and
	// literal-preserving round trips (Python json semantics); typed struct
	// targets coerce numbers as before.
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	return dec.Decode(dst)
}

// enforceCSRFOr403 renders the probe-proven csrf_failed envelope.
func enforceCSRFOr403(w http.ResponseWriter, r *http.Request) bool {
	if err := verifyCSRF(r); err != nil {
		writeDRF(w, http.StatusForbidden, "csrf_failed", err.Error())
		return false
	}
	return true
}

// finishLogin ports django.contrib.auth.login for our session store:
// reuse when the same user + matching hash, else a fresh key (flush for a
// different user); ALWAYS rotate the csrf secret (rotate_token) and send
// both cookies (probe 0002 step03: register response carries a fresh
// csrftoken AND sessionid).
func (a *App) finishLogin(w http.ResponseWriter, r *http.Request, u *UserRow) {
	hash := sessionAuthHash(u.Password)
	key := newSessionKey()
	if cookie, err := r.Cookie(sessionCookieName); err == nil && cookie.Value != "" {
		if sess, _ := getSession(a.DB, cookie.Value); sess != nil {
			if sess.UserID == u.ID && sess.AuthHash == hash {
				key = sess.Key // same authenticated user: keep the key (probe0002e)
				_ = putSession(a.DB, key, u.ID, hash)
				setSessionCookie(w, key, a.Settings.SessionCookieSecure())
				a.rotateCSRFCookie(w, r)
				return
			}
			_ = flushSession(a.DB, sess.Key) // different user / stale hash
		}
	}
	_ = putSession(a.DB, key, u.ID, hash)
	setSessionCookie(w, key, a.Settings.SessionCookieSecure())
	a.rotateCSRFCookie(w, r)
}

// rotateCSRFCookie issues a NEW csrf secret (get_token -> rotate_token on
// login) and renews the cookie.
func (a *App) rotateCSRFCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     csrfCookieName,
		Value:    newCSRFSecret(),
		Path:     "/",
		MaxAge:   csrfCookieAge,
		Expires:  timeNowUTC().Add(csrfCookieAge * time.Second),
		SameSite: http.SameSiteLaxMode,
		Secure:   a.Settings.SessionCookieSecure(),
	})
}

// ---- register ----

type registerBody struct {
	Email       string `json:"email"`
	Password    string `json:"password"`
	DisplayName string `json:"display_name"`
}

func (a *App) registerView(w http.ResponseWriter, r *http.Request) {
	var body registerBody
	if err := jsonBody(r, &body); err != nil {
		writeDRF(w, http.StatusBadRequest, "bad_request", "Malformed request body.")
		return
	}
	fields, cleanedName := a.validateRegister(&body)
	if len(fields) > 0 {
		writeDRFValidation(w, fields)
		return
	}
	u, err := a.createUser(body.Email, body.Password, cleanedName)
	if err != nil {
		// Lost race with a concurrent registration (IntegrityError path).
		if isUniqueViolation(err) {
			writeDRFValidation(w, []FieldError{{
				Name:     "email",
				Messages: []string{"An account with this email already exists."},
				Codes:    []string{"email_taken"},
			}})
			return
		}
		writeDRF(w, http.StatusInternalServerError, "server_error", "Something went wrong on our side.")
		return
	}
	// send_verification_email(user): the oracle's console backend prints to
	// its log; email CONTENT parity is D9 (pending) and not response-visible.
	a.finishLogin(w, r, u)
	b := encodeCompactJSON(map[string]any{"user": u.envelope()})
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", itoa(len(b)))
	w.WriteHeader(http.StatusCreated)
	_, _ = w.Write(b)
}

// validateRegister mirrors RegisterSerializer field-by-field, in
// declaration order (email, password, display_name), then the object-level
// password validators. Returns nil when valid, else the ordered errors.
func (a *App) validateRegister(body *registerBody) ([]FieldError, string) {
	var fields []FieldError

	// email: EmailField(max_length=254) -> validate_email (strip+lower,
	// uniqueness) in the same loop pass.
	var emailErr Messages
	if body.Email == "" {
		emailErr.add("This field is required.", "required")
	} else if len(body.Email) > 254 {
		emailErr.add("Ensure this field has no more than 254 characters.", "max_length")
	} else if !validEmail(body.Email) {
		emailErr.add("Enter a valid email address.", "invalid")
	} else {
		normalized := strings.ToLower(strings.TrimSpace(body.Email))
		if existing, _ := getUserByEmail(a.DB, normalized); existing != nil {
			emailErr.add("An account with this email already exists.", "email_taken")
		} else {
			body.Email = normalized
		}
	}
	if len(emailErr.Messages) > 0 {
		fields = append(fields, FieldError{Name: "email", Messages: emailErr.Messages, Codes: emailErr.Codes})
	}

	// password: write-only CharField(max_length=128, trim_whitespace=False)
	// — no validators at field level; object-level check_new_password below.
	var passErr Messages
	if body.Password == "" {
		passErr.add("This field is required.", "required")
	} else if len(body.Password) > passwordMaxLength {
		passErr.add("Ensure this field has no more than 128 characters.", "max_length")
	}
	if len(passErr.Messages) > 0 {
		fields = append(fields, FieldError{Name: "password", Messages: passErr.Messages, Codes: passErr.Codes})
	}

	// display_name: optional, allow_blank, max_length=80, cleaned.
	if len(body.DisplayName) > 80 {
		fields = append(fields, FieldError{Name: "display_name",
			Messages: []string{"Ensure this field has no more than 80 characters."},
			Codes:    []string{"max_length"}})
	}
	body.DisplayName = cleanDisplayName(body.DisplayName)

	// Object-level: only when field-level email+password are clean
	// (DRF skips validate() once to_internal_value collected errors).
	if len(fields) == 0 {
		for _, perr := range validateNewPassword(body.Password, body.Email) {
			passErr.add(perr.Message, perr.Code)
		}
		if len(passErr.Messages) > 0 {
			fields = append(fields, FieldError{Name: "password", Messages: passErr.Messages, Codes: passErr.Codes})
		}
	}

	// validated_data semantics: return the cleaned display name with the
	// (possibly empty) error list.
	return fields, cleanDisplayName(body.DisplayName)
}

// Messages accumulates a field's messages + codes in lockstep.
type Messages struct {
	Messages []string
	Codes    []string
}

func (m *Messages) add(msg, code string) {
	m.Messages = append(m.Messages, msg)
	m.Codes = append(m.Codes, code)
}

// cleanDisplayName ports serializers.clean_display_name: collapse
// whitespace, drop control characters.
func cleanDisplayName(value string) string {
	fields := strings.Fields(value)
	for i, f := range fields {
		var b strings.Builder
		for _, r := range f {
			if r >= 0x20 && r != 0x7f {
				b.WriteRune(r)
			}
		}
		fields[i] = b.String()
	}
	return strings.Join(fields, " ")
}

// ---- login ----

type loginBody struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

func (a *App) loginView(w http.ResponseWriter, r *http.Request) {
	var body loginBody
	if err := jsonBody(r, &body); err != nil {
		writeDRF(w, http.StatusBadRequest, "bad_request", "Malformed request body.")
		return
	}
	var fields []FieldError
	var f Messages
	if body.Email == "" {
		f.add("This field is required.", "required")
	} else if len(body.Email) > 254 {
		f.add("Ensure this field has no more than 254 characters.", "max_length")
	} else if !validEmail(body.Email) {
		f.add("Enter a valid email address.", "invalid")
	}
	if len(f.Messages) > 0 {
		fields = append(fields, FieldError{Name: "email", Messages: f.Messages, Codes: f.Codes})
	}
	var pf Messages
	if body.Password == "" {
		pf.add("This field is required.", "required")
	} else if len(body.Password) > passwordMaxLength {
		pf.add("Ensure this field has no more than 128 characters.", "max_length")
	}
	if len(pf.Messages) > 0 {
		fields = append(fields, FieldError{Name: "password", Messages: pf.Messages, Codes: pf.Codes})
	}
	if len(fields) > 0 {
		writeDRFValidation(w, fields)
		return
	}

	email := strings.ToLower(strings.TrimSpace(body.Email))
	// PasswordAttemptGuard(email).check(): locked accounts answer 429, not
	// "wrong password".
	if throttled, wait := a.lockout.check(email); throttled {
		writeDRFThrottled(w, wait)
		return
	}
	// Unknown emails, wrong passwords and disabled accounts are
	// indistinguishable; each counts towards the lockout.
	user, _ := getUserByEmail(a.DB, email)
	ok := user != nil && user.IsActive && verifyPassword(body.Password, user.Password)
	if !ok {
		a.lockout.recordFailure(email)
		writeDRF(w, http.StatusBadRequest, "invalid_credentials", "Incorrect email or password.")
		return
	}
	a.lockout.reset(email)
	a.finishLogin(w, r, user)
	b := encodeCompactJSON(map[string]any{"user": user.envelope()})
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", itoa(len(b)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(b)
}

// writeDRFThrottled renders the probed lockout envelope (429, code
// throttled, retry_after) — detail + DRF's wait sentence.
func writeDRFThrottled(w http.ResponseWriter, waitSeconds float64) {
	writeDRFThrottledDetail(w, waitSeconds, lockoutDetail)
}

// writeDRFThrottledDetail is the generic DRF Throttled envelope; the detail
// differs per caller (lockout custom detail; sharing uses the default
// "Request was throttled." — probe0008 step29).
func writeDRFThrottledDetail(w http.ResponseWriter, waitSeconds float64, detail string) {
	retry := int(ceil(waitSeconds)) // DRF Throttled ceils wait at construction
	body := encodeCompactJSON(ErrorEnvelope{Error: ErrorBody{
		Code:       "throttled",
		Message:    throttledMessageDetail(detail, waitSeconds),
		RetryAfter: &retry,
	}})
	w.Header().Set("Content-Type", "application/json")
	// DRF exception_handler: headers['Retry-After'] = '%d' % exc.wait
	w.Header().Set("Retry-After", itoa(retry))
	w.Header().Set("Content-Length", itoa(len(body)))
	w.WriteHeader(http.StatusTooManyRequests)
	_, _ = w.Write(body)
}

// ---- logout ----

func (a *App) logoutView(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(sessionCookieName); err == nil && cookie.Value != "" {
		_ = deleteSession(a.DB, cookie.Value)
	}
	clearSessionCookie(w, a.Settings.SessionCookieSecure())
	w.Header().Set("Content-Length", "0")
	w.WriteHeader(http.StatusNoContent)
}

// ---- me ----

// meView: GET never fails for anonymous callers (the SPA bootstraps);
// PATCH/DELETE need a session (dispatch's RequireUnsafe).
func (a *App) meView(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		var payload any
		if u := currentUser(r); u != nil {
			payload = map[string]any{"user": u.envelope()}
		} else {
			payload = map[string]any{"user": nil}
		}
		b := encodeCompactJSON(payload)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", itoa(len(b)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(b)
	case http.MethodPatch:
		a.mePatch(w, r)
	default:
		writeDRF(w, http.StatusMethodNotAllowed, "method_not_allowed",
			`Method "`+strings.ToUpper(r.Method)+`" not allowed.`)
	}
}

type profileBody struct {
	DisplayName  *string `json:"display_name"`
	RemoveAvatar bool    `json:"remove_avatar"`
}

func (a *App) mePatch(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	var body profileBody
	if err := jsonBody(r, &body); err != nil {
		writeDRF(w, http.StatusBadRequest, "bad_request", "Malformed request body.")
		return
	}
	var fields []FieldError
	if body.DisplayName != nil && len(*body.DisplayName) > 80 {
		fields = append(fields, FieldError{Name: "display_name",
			Messages: []string{"Ensure this field has no more than 80 characters."},
			Codes:    []string{"max_length"}})
	}
	// avatar upload (build_avatar/replace_avatar, Pillow) is deferred to the
	// assets/core slice; remove_avatar on an empty avatar is a no-op that
	// still saves — probed behavior pending; PATCH scope here is
	// display_name only (PORT_MAP note).
	if len(fields) > 0 {
		writeDRFValidation(w, fields)
		return
	}
	if body.DisplayName != nil {
		cleaned := cleanDisplayName(*body.DisplayName)
		if _, err := a.DB.Exec(
			`UPDATE accounts_user SET display_name = ?, updated_at = ? WHERE id = ?`,
			cleaned, formatDjangoDateTime(timeNowUTC()), u.ID); err != nil {
			writeDRF(w, http.StatusInternalServerError, "server_error", "Something went wrong on our side.")
			return
		}
		u.DisplayName = cleaned
	}
	b := encodeCompactJSON(map[string]any{"user": u.envelope()})
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", itoa(len(b)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(b)
}

// ---- password change ----

type passwordChangeBody struct {
	OldPassword string `json:"old_password"`
	NewPassword string `json:"new_password"`
}

func (a *App) passwordChangeView(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	var body passwordChangeBody
	if err := jsonBody(r, &body); err != nil {
		writeDRF(w, http.StatusBadRequest, "bad_request", "Malformed request body.")
		return
	}

	// new_password: required, max128 (field level).
	var newErr Messages
	if body.NewPassword == "" {
		newErr.add("This field is required.", "required")
	} else if len(body.NewPassword) > passwordMaxLength {
		newErr.add("Ensure this field has no more than 128 characters.", "max_length")
	}
	if len(newErr.Messages) > 0 {
		writeDRFValidation(w, []FieldError{{Name: "new_password", Messages: newErr.Messages, Codes: newErr.Codes}})
		return
	}

	// old_password first? DRF validates fields in order old_password,
	// new_password (declaration order), then the view checks the old one.
	// Probe evidence: wrong old -> field error old_password; validator
	// failures on new land under new_password. Order when BOTH wrong: old
	// password check happens in the VIEW (after is_valid), so new_password
	// validator errors (object level) would come from is_valid FIRST.
	// check_new_password runs in the view AFTER _verify_password — see
	// PasswordChangeView.post: _verify_password BEFORE check_new_password.
	// So: old-password error wins over validator errors.
	if u.HasPassword() { // accounts without a password may set a first one
		if throttled, wait := a.lockout.check(u.Email); throttled {
			writeDRFThrottled(w, wait)
			return
		}
		if !verifyPassword(body.OldPassword, u.Password) {
			a.lockout.recordFailure(u.Email)
			writeDRFValidation(w, []FieldError{{
				Name: "old_password", Messages: []string{"Incorrect password."}, Codes: []string{"invalid_password"},
			}})
			return
		}
		a.lockout.reset(u.Email)
	}
	// check_new_password(new_password, user) — field new_password.
	if errs := validateNewPassword(body.NewPassword, u.Email); len(errs) > 0 {
		var fe FieldError
		fe.Name = "new_password"
		for _, e := range errs {
			fe.Messages = append(fe.Messages, e.Message)
			fe.Codes = append(fe.Codes, e.Code)
		}
		writeDRFValidation(w, []FieldError{fe})
		return
	}

	u.Password = hashPassword(body.NewPassword)
	if _, err := a.DB.Exec(
		`UPDATE accounts_user SET password = ?, updated_at = ? WHERE id = ?`,
		u.Password, formatDjangoDateTime(timeNowUTC()), u.ID); err != nil {
		writeDRF(w, http.StatusInternalServerError, "server_error", "Something went wrong on our side.")
		return
	}

	// update_session_auth_hash: cycle_key + store the NEW hash — this
	// session stays valid with a NEW sessionid cookie (probed 0002e), every
	// other session of the user dies on its next request (hash mismatch).
	if cookie, err := r.Cookie(sessionCookieName); err == nil && cookie.Value != "" {
		if sess, _ := getSession(a.DB, cookie.Value); sess != nil {
			_ = deleteSession(a.DB, sess.Key)
			newKey := newSessionKey()
			_ = putSession(a.DB, newKey, u.ID, sessionAuthHash(u.Password))
			setSessionCookie(w, newKey, a.Settings.SessionCookieSecure())
		}
	}

	// revoke_all: a token minted by whoever knew the old password must not
	// outlive it.
	if _, err := a.DB.Exec(
		`UPDATE apitokens_apitoken SET revoked_at = ?, updated_at = ?
		 WHERE owner_id = ? AND revoked_at IS NULL`,
		formatDjangoDateTime(timeNowUTC()), formatDjangoDateTime(timeNowUTC()), u.ID); err != nil &&
		!isMissingTable(err) {
		writeDRF(w, http.StatusInternalServerError, "server_error", "Something went wrong on our side.")
		return
	}

	w.Header().Set("Content-Length", "0")
	w.WriteHeader(http.StatusNoContent)
}

// HasPassword mirrors User.has_usable_password for the current row.
func (u *UserRow) HasPassword() bool { return hasUsablePassword(u.Password) }

// isUniqueViolation recognizes the email uniqueness race (IntegrityError
// -> field error email_taken).
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "UNIQUE") || strings.Contains(msg, "duplicate key") ||
		strings.Contains(msg, "Constraint failed")
}

func isMissingTable(err error) bool {
	return err != nil && strings.Contains(err.Error(), "no such table")
}

func validEmailUser(local string) bool {
	// ^[-!#$%&'*+/=?^_`{}|~0-9A-Z]+(\.[-!#$%&'*+/=?^_`{}|~0-9A-Z]+)*$
	charset := "-!#$%&'*+/=?^_`{}|~0-9A-Z"
	has := func(c byte) bool {
		for i := 0; i < len(charset); i++ {
			if charset[i] == c {
				return true
			}
		}
		return false
	}
	if local == "" {
		return false
	}
	for _, part := range strings.Split(local, ".") {
		if part == "" {
			return false
		}
		for i := 0; i < len(part); i++ {
			c := part[i]
			if (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || has(c) {
				continue
			}
			if c >= 'a' && c <= 'z' {
				continue // case-insensitive
			}
			return false
		}
	}
	return true
}

func validEmailDomain(domain string) bool {
	if domain == "" || !strings.Contains(domain, ".") {
		return false
	}
	labels := strings.Split(domain, ".")
	for _, label := range labels {
		if label == "" || len(label) > 63 {
			return false
		}
		if label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for i := 0; i < len(label); i++ {
			c := label[i]
			if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
				return false
			}
		}
	}
	last := labels[len(labels)-1]
	if last == "local" { // Django's placeholder TLD domain_regex requires a plausible TLD
		return false
	}
	return len(last) >= 2
}
