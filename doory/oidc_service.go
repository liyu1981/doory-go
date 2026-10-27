// Account linking for OIDC sign-in and connecting an existing account
// (apps/accounts/services.py).
// Ports backend/apps/accounts/services.py

package doory

import (
	"database/sql"
	"fmt"
	"strings"
	"time"
)

type rowQuerier interface {
	QueryRow(query string, args ...any) *sql.Row
}

const oidcUserColumns = `SELECT id, email, display_name, avatar, google_sub, is_email_verified, is_active,
	        is_staff, is_superuser, password,
	        EXISTS(SELECT 1 FROM accounts_oidcidentity WHERE user_id = accounts_user.id),
	        last_login, created_at, updated_at
	 FROM accounts_user`

func getUserByIDQ(q rowQuerier, id string) (*UserRow, error) {
	return scanUser(q.QueryRow(oidcUserColumns+` WHERE id = ?`, id))
}

func getUserByEmailCIQ(q rowQuerier, email string) (*UserRow, error) {
	return scanUser(q.QueryRow(oidcUserColumns+` WHERE LOWER(email) = LOWER(?)`,
		strings.TrimSpace(email)))
}

func getUserByGoogleSubQ(q rowQuerier, sub string) (*UserRow, error) {
	return scanUser(q.QueryRow(oidcUserColumns+` WHERE google_sub = ?`, sub))
}

func oidcIdentityUserID(q rowQuerier, issuer, subject string) (string, error) {
	var userID string
	err := q.QueryRow(`SELECT user_id FROM accounts_oidcidentity WHERE issuer = ? AND subject = ?`,
		issuer, subject).Scan(&userID)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return userID, err
}

func userHasOIDCIssuer(q rowQuerier, userID, issuer string) (bool, error) {
	var exists bool
	err := q.QueryRow(`SELECT EXISTS(SELECT 1 FROM accounts_oidcidentity WHERE user_id = ? AND issuer = ?)`,
		userID, issuer).Scan(&exists)
	return exists, err
}

func createOIDCIdentity(tx *sql.Tx, userID, issuer, subject string) error {
	now := formatDjangoDateTime(time.Now())
	_, err := tx.Exec(`INSERT INTO accounts_oidcidentity
		(id, created_at, updated_at, issuer, subject, user_id) VALUES (?, ?, ?, ?, ?, ?)`,
		newUUID4Hex(), now, now, issuer, subject, userID)
	return err
}

func deleteOIDCIdentities(q interface {
	Exec(query string, args ...any) (sql.Result, error)
}, userID string) error {
	_, err := q.Exec(`DELETE FROM accounts_oidcidentity WHERE user_id = ?`, userID)
	return err
}

func revokeAllTokens(q interface {
	Exec(query string, args ...any) (sql.Result, error)
}, userID string) error {
	now := formatDjangoDateTime(time.Now())
	_, err := q.Exec(`UPDATE apitokens_apitoken SET revoked_at = ?, updated_at = ?
		WHERE owner_id = ? AND revoked_at IS NULL`, now, now, userID)
	return err
}

func createSocialUser(q interface {
	Exec(query string, args ...any) (sql.Result, error)
}, email, displayName string, verified bool, googleSub *string) (*UserRow, error) {
	now := formatDjangoDateTime(time.Now())
	user := &UserRow{
		ID:              newUUID4Hex(),
		Email:           strings.ToLower(strings.TrimSpace(email)),
		DisplayName:     displayName,
		Password:        "!",
		IsEmailVerified: verified,
		IsActive:        true,
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	var sub any
	if googleSub != nil {
		user.GoogleSub = sql.NullString{String: *googleSub, Valid: true}
		sub = *googleSub
	}
	_, err := q.Exec(`INSERT INTO accounts_user
		(password, last_login, is_superuser, id, created_at, updated_at, email,
		 display_name, avatar, google_sub, is_email_verified, is_active, is_staff)
		VALUES (?, NULL, FALSE, ?, ?, ?, ?, ?, '', ?, ?, TRUE, FALSE)`,
		user.Password, user.ID, now, now, user.Email, user.DisplayName, sub, verified)
	if err != nil {
		return nil, err
	}
	return user, nil
}

// forgetUnprovenSignIns drops whatever let someone into an address that was
// never proven, now that the provider proved it (pre-hijack defence).
func forgetUnprovenSignIns(tx *sql.Tx, user *UserRow) error {
	now := formatDjangoDateTime(time.Now())
	if _, err := tx.Exec(`UPDATE accounts_user SET password = '!', is_email_verified = TRUE, updated_at = ?
		WHERE id = ?`, now, user.ID); err != nil {
		return err
	}
	if err := deleteOIDCIdentities(tx, user.ID); err != nil {
		return err
	}
	if err := revokeAllTokens(tx, user.ID); err != nil {
		return err
	}
	user.Password = "!"
	user.IsEmailVerified = true
	user.OidcLinked = false
	return nil
}

func oidcServiceError(code, detail string) *OidcError {
	return &OidcError{Code: code, Detail: detail}
}

func oidcAsError(err error) *OidcError {
	if err == nil {
		return nil
	}
	if oidcErr, ok := err.(*OidcError); ok {
		return oidcErr
	}
	return oidcServiceError(oidcFailed, err.Error())
}

// signInWithOIDC ports services.sign_in_with_oidc: find, link or create the
// account for a verified identity, retrying a lost uniqueness race once.
func (a *App) signInWithOIDC(claims OidcClaims) (*UserRow, bool, *OidcError) {
	user, created, err := a.oidcFindLinkOrCreate(claims)
	if isUniqueViolation(err) {
		user, created, err = a.oidcFindLinkOrCreate(claims)
		if isUniqueViolation(err) {
			return nil, false, oidcServiceError(oidcFailed,
				"OIDC sign-in raced another one for the same account twice.")
		}
	}
	if err != nil {
		return nil, false, oidcAsError(err)
	}
	return user, created, nil
}

func (a *App) oidcFindLinkOrCreate(claims OidcClaims) (*UserRow, bool, error) {
	tx, err := a.DB.Begin()
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()

	if userID, err := oidcIdentityUserID(tx, claims.Issuer, claims.Subject); err != nil {
		return nil, false, err
	} else if userID != "" {
		user, err := getUserByIDQ(tx, userID)
		if err != nil {
			return nil, false, err
		}
		if user == nil {
			return nil, false, oidcServiceError(oidcFailed, "OIDC identity points at a missing account.")
		}
		if !user.IsActive {
			return nil, false, oidcServiceError("account_disabled", "OIDC sign-in into a disabled account.")
		}
		if err := tx.Commit(); err != nil {
			return nil, false, err
		}
		return user, false, nil
	}

	if claims.Email == "" {
		return nil, false, oidcServiceError("oidc_no_email", "The provider sent no usable email address.")
	}
	user, err := getUserByEmailCIQ(tx, claims.Email)
	if err != nil {
		return nil, false, err
	}
	if user != nil {
		if !a.Settings.OIDCTrustEmail || user.IsStaff || user.IsSuperuser {
			return nil, false, oidcServiceError("oidc_account_exists",
				"An account has this email; connect it from Settings.")
		}
		if !claims.EmailVerified {
			return nil, false, oidcServiceError("oidc_email_unverified",
				"The provider has not verified this email address.")
		}
		if !user.IsActive {
			return nil, false, oidcServiceError("account_disabled", "OIDC sign-in into a disabled account.")
		}
		if !user.IsEmailVerified {
			if err := forgetUnprovenSignIns(tx, user); err != nil {
				return nil, false, err
			}
		} else if hasIssuer, err := userHasOIDCIssuer(tx, user.ID, claims.Issuer); err != nil {
			return nil, false, err
		} else if hasIssuer {
			return nil, false, oidcServiceError("oidc_account_mismatch",
				"Connected to another account at the provider.")
		}
		if _, err := tx.Exec(`UPDATE accounts_user SET updated_at = ? WHERE id = ?`,
			formatDjangoDateTime(time.Now()), user.ID); err != nil {
			return nil, false, err
		}
		if err := createOIDCIdentity(tx, user.ID, claims.Issuer, claims.Subject); err != nil {
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
			return nil, false, oidcServiceError(oidcFailed, "account disappeared during sign-in")
		}
		return reloaded, false, nil
	}

	user, err = createSocialUser(tx, claims.Email, truncateRunes(cleanDisplayName(claims.Name), 80),
		claims.EmailVerified && a.Settings.OIDCTrustEmail, nil)
	if err != nil {
		return nil, false, err
	}
	if err := createOIDCIdentity(tx, user.ID, claims.Issuer, claims.Subject); err != nil {
		return nil, false, err
	}
	if err := tx.Commit(); err != nil {
		return nil, false, err
	}
	user.OidcLinked = true
	return user, true, nil
}

// connectOIDC ports services.connect_oidc, including the lost-race retry.
func (a *App) connectOIDC(user *UserRow, claims OidcClaims) *OidcError {
	if err := a.connectOIDCOnce(user, claims); err != nil {
		if isUniqueViolation(err) {
			if err := a.connectOIDCOnce(user, claims); err != nil {
				if isUniqueViolation(err) {
					return oidcServiceError("oidc_identity_in_use", "Connecting raced another connection twice.")
				}
				return oidcAsError(err)
			}
			return nil
		}
		return oidcAsError(err)
	}
	return nil
}

func (a *App) connectOIDCOnce(user *UserRow, claims OidcClaims) error {
	tx, err := a.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	existingUserID, err := oidcIdentityUserID(tx, claims.Issuer, claims.Subject)
	if err != nil {
		return err
	}
	if existingUserID != "" {
		if existingUserID != user.ID {
			return oidcServiceError("oidc_identity_in_use", "That provider account belongs to someone else.")
		}
		return tx.Commit()
	}
	hasIssuer, err := userHasOIDCIssuer(tx, user.ID, claims.Issuer)
	if err != nil {
		return err
	}
	if hasIssuer {
		return oidcServiceError("oidc_account_mismatch", "Connected to another account at the provider.")
	}
	if err := createOIDCIdentity(tx, user.ID, claims.Issuer, claims.Subject); err != nil {
		return err
	}
	return tx.Commit()
}

// disconnectOIDC removes every connected provider identity, refusing when
// that would leave the account no way to sign in.
func (a *App) disconnectOIDC(user *UserRow) *OidcError {
	if !user.HasPassword() && !(user.GoogleSub.Valid && user.GoogleSub.String != "") {
		return oidcServiceError("last_sign_in_method",
			"Set a password first, or you won't be able to sign in.")
	}
	if err := deleteOIDCIdentities(a.DB, user.ID); err != nil {
		return oidcServiceError("server_error", fmt.Sprintf("delete OIDC identity: %v", err))
	}
	return nil
}
