// The accounts_user row: lookup, envelope serialization, and the exact
//
// Ports backend/apps/accounts/models.py
// UserSerializer shape (apps/accounts/serializers.py):
// id, email, display_name, name, avatar_url, is_email_verified,
// has_password, google_linked, created_at — probed in
// probes/0002-accounts (register 201 body).
package doory

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"net/http"
	"strings"
	"time"
)

// UserRow maps accounts_user.
type UserRow struct {
	ID              string // 32-hex (uuid4, no dashes in storage)
	Email           string
	DisplayName     string
	Avatar          string
	GoogleSub       sql.NullString
	IsEmailVerified bool
	IsActive        bool
	IsStaff         bool
	IsSuperuser     bool
	Password        string
	OidcLinked      bool
	LastLogin       sql.NullString
	CreatedAt       string // Django sqlite datetime storage
	UpdatedAt       string
}

// getUserByEmail: case-insensitive on stored lowercase rows
// (UserManager.get_by_natural_key: email__iexact).
func getUserByEmail(db *sql.DB, email string) (*UserRow, error) {
	return scanUser(db.QueryRow(
		`SELECT id, email, display_name, avatar, google_sub, is_email_verified, is_active,
		        is_staff, is_superuser, password,
		        EXISTS(SELECT 1 FROM accounts_oidcidentity WHERE user_id = accounts_user.id),
		        last_login, created_at, updated_at
		 FROM accounts_user WHERE email = ?`, strings.ToLower(strings.TrimSpace(email))))
}

func getUserByID(db *sql.DB, id string) (*UserRow, error) {
	return scanUser(db.QueryRow(
		`SELECT id, email, display_name, avatar, google_sub, is_email_verified, is_active,
		        is_staff, is_superuser, password,
		        EXISTS(SELECT 1 FROM accounts_oidcidentity WHERE user_id = accounts_user.id),
		        last_login, created_at, updated_at
		 FROM accounts_user WHERE id = ?`, id))
}

func scanUser(row *sql.Row) (*UserRow, error) {
	var u UserRow
	err := row.Scan(&u.ID, &u.Email, &u.DisplayName, &u.Avatar, &u.GoogleSub,
		&u.IsEmailVerified, &u.IsActive, &u.IsStaff, &u.IsSuperuser, &u.Password, &u.OidcLinked,
		&u.LastLogin, &u.CreatedAt, &u.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &u, nil
}

// sessionUser resolves the request's sessionid cookie to a live user.
// dead=true means the cookie pointed at a session that EXISTS but no longer
// authenticates (stale auth hash, deleted/inactive user): Django flushes
// such sessions and the response then CLEARS the cookie — probed: oracle
// step 20 (me with the second jar after the password change) returns
// {"user":null} AND Set-Cookie sessionid="". A cookie whose row is simply
// absent/expired is not cleared (pending probe, accounts backlog).
func (a *App) sessionUser(r *http.Request) (*UserRow, bool) {
	cookie, err := r.Cookie(sessionCookieName)
	if err != nil || cookie.Value == "" {
		return nil, false
	}
	sess, err := getSession(a.DB, cookie.Value)
	if err != nil || sess == nil {
		return nil, false
	}
	u, err := getUserByID(a.DB, sess.UserID)
	if err != nil || u == nil || !u.IsActive {
		_ = deleteSession(a.DB, sess.Key)
		return nil, true
	}
	if sess.AuthHash != sessionAuthHash(u.Password) {
		_ = deleteSession(a.DB, sess.Key)
		return nil, true
	}
	return u, false
}

// createUser ports UserManager.create_user: lowercase email, hashed
// password, uuid4 pk, is_staff/is_superuser false.
func (a *App) createUser(email, password, displayName string) (*UserRow, error) {
	u := &UserRow{
		ID:          newUUID4Hex(),
		Email:       strings.ToLower(strings.TrimSpace(email)),
		DisplayName: displayName,
		Password:    hashPassword(password),
		IsActive:    true,
	}
	now := formatDjangoDateTime(time.Now())
	_, err := a.DB.Exec(
		`INSERT INTO accounts_user
		 (password, last_login, is_superuser, id, created_at, updated_at, email,
		  display_name, avatar, google_sub, is_email_verified, is_active, is_staff)
		 VALUES (?, NULL, FALSE, ?, ?, ?, ?, ?, '', NULL, FALSE, TRUE, FALSE)`,
		u.Password, u.ID, now, now, u.Email, u.DisplayName)
	if err != nil {
		return nil, err
	}
	u.CreatedAt, u.UpdatedAt = now, now
	return u, nil
}

// newUUID4Hex: RFC 4122 v4 in the storage format (32 lowercase hex, no
// dashes — Django UUIDField on sqlite stores uuid.hex... actually Django
// stores the canonical str WITHOUT dashes? probed rows: id column char(32);
// the SERIALIZER renders WITH dashes — see dashedUUID).
func newUUID4Hex() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return hex.EncodeToString(b[:])
}

// hexUUID converts a canonical (dashed) uuid param to storage form
// (32 lowercase hex — Django stores uuid.hex).
func hexUUID(dashed string) string {
	dashed = strings.ToLower(dashed)
	if len(dashed) == 36 {
		return strings.ReplaceAll(dashed, "-", "")
	}
	return dashed
}

// dashedUUID renders the canonical form the serializer emits.
func dashedUUID(hex32 string) string {
	if len(hex32) != 32 {
		return hex32
	}
	return hex32[0:8] + "-" + hex32[8:12] + "-" + hex32[12:16] + "-" +
		hex32[16:20] + "-" + hex32[20:32]
}

// userEnvelope is the UserSerializer output, values pre-formatted so JSON
// key order matches the serializer's field tuple exactly.
type userEnvelope struct {
	ID              string  `json:"id"`
	Email           string  `json:"email"`
	DisplayName     string  `json:"display_name"`
	Name            string  `json:"name"`
	AvatarURL       *string `json:"avatar_url"`
	IsEmailVerified bool    `json:"is_email_verified"`
	HasPassword     bool    `json:"has_password"`
	GoogleLinked    bool    `json:"google_linked"`
	OidcLinked      bool    `json:"oidc_linked"`
	CreatedAt       string  `json:"created_at"`
}

// envelope builds the serializer shape: name = display_name or the email's
// local part (User.name property); avatar_url null while avatar uploads
// are unported (assets family); created_at rendered like DRF's
// DateTimeField (ISO-8601, microseconds, Z).
func (u *UserRow) envelope() userEnvelope {
	name := u.DisplayName
	if name == "" {
		if at := strings.IndexByte(u.Email, '@'); at > 0 {
			name = u.Email[:at]
		}
	}
	return userEnvelope{
		ID:              dashedUUID(u.ID),
		Email:           u.Email,
		DisplayName:     u.DisplayName,
		Name:            name,
		AvatarURL:       nil,
		IsEmailVerified: u.IsEmailVerified,
		HasPassword:     hasUsablePassword(u.Password),
		GoogleLinked:    u.GoogleSub.Valid && u.GoogleSub.String != "",
		OidcLinked:      u.OidcLinked,
		CreatedAt:       renderDRFDatetime(u.CreatedAt),
	}
}

// renderDRFDatetime converts Django sqlite storage to DRF's isoformat:
// "2006-01-02T15:04:05.000000Z" (UTC, always six fractional digits —
// probed: 2026-10-21T22:15:46.477809Z).
func renderDRFDatetime(stored string) string {
	t, err := parseDjangoDateTime(stored)
	if err != nil {
		return stored
	}
	return t.UTC().Format("2006-01-02T15:04:05.000000Z")
}

// validEmail ports Django's EmailValidator for the probed cases
// (probe0002 step09: "not-an-email" -> "Enter a valid email address." with
// code invalid). The dot-atom user part and domain grammar mirror
// django/core/validators.py at the pin; exotic internationalized forms are
// covered by later probes (backlog note in the accounts PORT_MAP row).
func validEmail(value string) bool {
	at := strings.LastIndexByte(value, '@')
	if at <= 0 || at == len(value)-1 {
		return false
	}
	return validEmailUser(value[:at]) && validEmailDomain(value[at+1:])
}
