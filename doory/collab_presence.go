// Presence and access re-checks (apps/collab/store.py, presence section):
// admit/release/heartbeat rows, the live-room test behind the409
// use_realtime guard, and check_access — why each connection must close now.

package doory

import (
	"database/sql"
	"time"
)

// CollabAdmission is store.Admission.
type CollabAdmission struct {
	OK           bool
	ConnectionID string
	Reason       string
}

// CollabAdmit ports store.admit: register a connection unless the per-design
// or per-user limit is reached. The row is inserted first and the counts
// checked afterwards, so concurrent connects cannot both slip under a limit.
func (a *App) CollabAdmit(designID, userID, role, linkID, instance string) CollabAdmission {
	now := formatDjangoDateTime(time.Now())
	id := newUUID4HexDashed()
	createdAt := now
	if _, err := a.DB.Exec(
		`INSERT INTO collab_roomconnection (id, created_at, updated_at, design_id, user_id,
		 instance, role, share_link_id, heartbeat_at) VALUES (?,?,?,?,?,?,?,?,?)`,
		id, createdAt, now, designID, userID, instance, role, nullStr(linkID), now); err != nil {
		return CollabAdmission{OK: false, Reason: "design_full"}
	}
	cutoff := collabStaleCutoff()
	var perDesign, perUser int
	_ = a.DB.QueryRow(`SELECT COUNT(*) FROM collab_roomconnection
	      WHERE design_id = ? AND heartbeat_at > ? AND created_at <= ?`,
		designID, cutoff, createdAt).Scan(&perDesign)
	_ = a.DB.QueryRow(`SELECT COUNT(*) FROM collab_roomconnection
	      WHERE user_id = ? AND heartbeat_at > ? AND created_at <= ?`,
		userID, cutoff, createdAt).Scan(&perUser)
	maxDesign := envInt("REALTIME_MAX_CONNECTIONS_PER_DESIGN", 25)
	maxUser := envInt("REALTIME_MAX_CONNECTIONS_PER_USER", 10)
	if perDesign > maxDesign {
		_, _ = a.DB.Exec(`DELETE FROM collab_roomconnection WHERE id = ?`, id)
		return CollabAdmission{OK: false, Reason: "design_full"}
	}
	if perUser > maxUser {
		_, _ = a.DB.Exec(`DELETE FROM collab_roomconnection WHERE id = ?`, id)
		return CollabAdmission{OK: false, Reason: "too_many_connections"}
	}
	return CollabAdmission{OK: true, ConnectionID: id}
}

func nullStr(v string) any {
	if v == "" {
		return nil
	}
	return v
}

// CollabOtherInstanceServes ports store.other_instance_serves.
func (a *App) CollabOtherInstanceServes(designID, instance string) []string {
	rows, err := a.DB.Query(`SELECT DISTINCT instance FROM collab_roomconnection
	      WHERE design_id = ? AND heartbeat_at > ? AND instance <> ?`,
		designID, collabStaleCutoff(), instance)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil
		}
		out = append(out, s)
	}
	return out
}

// CollabRelease ports store.release.
func (a *App) CollabRelease(connectionID string) {
	_, _ = a.DB.Exec(`DELETE FROM collab_roomconnection WHERE id = ?`, connectionID)
}

// CollabHeartbeat ports store.heartbeat: refresh the rows of the connections
// this process really holds; others (leaked ones) go stale.
func (a *App) CollabHeartbeat(instance string, sharedLayer bool, counters map[string]any, connectionIDs []string) {
	now := formatDjangoDateTime(time.Now())
	for _, id := range connectionIDs {
		_, _ = a.DB.Exec(`UPDATE collab_roomconnection SET heartbeat_at = ? WHERE instance = ? AND id = ?`,
			now, instance, id)
	}
	countersJSON := collabJSON(counters)
	var n int
	_ = a.DB.QueryRow(`SELECT COUNT(*) FROM collab_collabinstance WHERE instance = ?`,
		instance).Scan(&n)
	if n == 0 {
		_, _ = a.DB.Exec(`INSERT INTO collab_collabinstance (id, created_at, updated_at, instance,
		      shared_layer, heartbeat_at, counters) VALUES (?,?,?,?,?,?,?)`,
			newUUID4HexDashed(), now, now, instance, sharedLayer, now, countersJSON)
	} else {
		_, _ = a.DB.Exec(`UPDATE collab_collabinstance SET heartbeat_at=?, shared_layer=?,
		      counters=?, updated_at=? WHERE instance=?`, now, sharedLayer, countersJSON, now, instance)
	}
	stale := formatDjangoDateTime(time.Now().Add(-heartbeatsStale * 4))
	_, _ = a.DB.Exec(`DELETE FROM collab_roomconnection WHERE heartbeat_at < ?`, stale)
	_, _ = a.DB.Exec(`DELETE FROM collab_collabinstance WHERE heartbeat_at < ?`, stale)
}

// CollabForgetInstance ports store.forget_instance.
func (a *App) CollabForgetInstance(instance string) {
	_, _ = a.DB.Exec(`DELETE FROM collab_roomconnection WHERE instance = ?`, instance)
	_, _ = a.DB.Exec(`DELETE FROM collab_collabinstance WHERE instance = ?`, instance)
}

// CollabRoomIsLive ports store.room_is_live: true while some process holds a
// connection to the design (REST document saves answer409 use_realtime).
func (a *App) CollabRoomIsLive(designID string) bool {
	var n int
	_ = a.DB.QueryRow(`SELECT COUNT(*) FROM collab_roomconnection WHERE design_id = ? AND heartbeat_at > ?`,
		designID, collabStaleCutoff()).Scan(&n)
	return n > 0
}

// --------------------------------------------------------- access recheck ---

// CollabMemberCheck is store.MemberCheck.
type CollabMemberCheck struct {
	MemberID   string
	UserID     string
	Role       string
	LinkID     string
	SessionKey string
}

// CollabAccessReport is store.AccessReport: why (if at all) each connection
// must be closed now. Keys of Closed are member ids.
type CollabAccessReport struct {
	Closed   map[string]string
	Title    string
	Revision int64
}

// collabCloseCodes is room.CLOSE_FOR_REASON (shared with the room/consumer).
var collabCloseCodes = map[string]int{
	"link_revoked":    4403,
	"link_expired":    4403,
	"role_changed":    4403,
	"account_disabled": 4403,
	"session_ended":   4401,
	"design_trashed":  4404,
	"design_deleted":  4404,
}

// CollabCheckAccess ports store.check_access.
func (a *App) CollabCheckAccess(designID string, members []CollabMemberCheck) CollabAccessReport {
	closed := map[string]string{}
	var (
		title     string
		revision  int64
		ownerID   sql.NullString
		trashed   sql.NullString
	)
	err := a.DB.QueryRow(`SELECT title, revision, owner_id, trashed_at FROM designs_design WHERE id = ?`,
		designID).Scan(&title, &revision, &ownerID, &trashed)
	if err == sql.ErrNoRows {
		for _, m := range members {
			closed[m.MemberID] = "design_deleted"
		}
		return CollabAccessReport{Closed: closed}
	}
	if err != nil {
		return CollabAccessReport{Closed: closed}
	}
	if trashed.Valid {
		for _, m := range members {
			closed[m.MemberID] = "design_trashed"
		}
		return CollabAccessReport{Closed: closed, Title: title, Revision: revision}
	}

	// Active users by id.
	users := map[string]*UserRow{}
	for _, m := range members {
		if _, seen := users[m.UserID]; seen {
			continue
		}
		if u, uerr := getUserByID(a.DB, m.UserID); uerr == nil && u != nil && u.IsActive {
			users[m.UserID] = u
		}
	}
	// Valid (active, unexpired) links by id.
	validLinks := map[string]shareLinkRow{}
	linkIDs := []string{}
	for _, m := range members {
		if m.LinkID != "" && !containsStr(linkIDs, m.LinkID) {
			linkIDs = append(linkIDs, m.LinkID)
		}
	}
	for _, id := range linkIDs {
		if l, ok := a.collabActiveLink(id); ok {
			validLinks[id] = l
		}
	}
	for _, m := range members {
		u := users[m.UserID]
		if u == nil {
			closed[m.MemberID] = "account_disabled"
			continue
		}
		if !a.collabSessionValid(m.SessionKey, u) {
			closed[m.MemberID] = "session_ended"
			continue
		}
		if m.LinkID != "" {
			if problem := a.collabLinkProblem(m, validLinks); problem != "" {
				closed[m.MemberID] = problem
			}
			continue
		}
		if m.Role != "owner" || !ownerID.Valid || ownerID.String != m.UserID {
			closed[m.MemberID] = "role_changed"
		}
	}
	return CollabAccessReport{Closed: closed, Title: title, Revision: revision}
}

func containsStr(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

// collabActiveLink is active_links for one id: not expired, on, design not
// trashed, owner enabled.
func (a *App) collabActiveLink(id string) (shareLinkRow, bool) {
	now := formatDjangoDateTime(time.Now())
	var l shareLinkRow
	err := a.DB.QueryRow(
		`SELECT l.id, l.design_id, l.token, l.role, l.is_active, l.expires_at,
		        l.password_hash, l.created_by_id, l.last_accessed_at, l.access_count,
		        l.created_at, l.updated_at
		 FROM sharing_sharelink l
		 JOIN designs_design d ON d.id = l.design_id
		 JOIN accounts_user u ON u.id = d.owner_id
		 WHERE l.id = ? AND l.is_active = 1
		   AND (l.expires_at IS NULL OR l.expires_at > ?)
		   AND d.trashed_at IS NULL AND u.is_active = 1`, id, now).
		Scan(&l.ID, &l.DesignID, &l.Token, &l.Role, &l.IsActive, &l.ExpiresAt,
			&l.PasswordHash, &l.CreatedBy, &l.LastAccessed, &l.AccessCount,
			&l.CreatedAt, &l.UpdatedAt)
	if err != nil {
		return shareLinkRow{}, false
	}
	return l, true
}

// collabLinkProblem port _link_problem: role_changed for a live link whose
// role moved, link_expired for an expired one, link_revoked otherwise.
func (a *App) collabLinkProblem(m CollabMemberCheck, valid map[string]shareLinkRow) string {
	if link, ok := valid[m.LinkID]; ok {
		if link.Role != m.Role {
			return "role_changed"
		}
		return ""
	}
	var (
		expires sql.NullString
		active  bool
	)
	if err := a.DB.QueryRow(`SELECT expires_at, is_active FROM sharing_sharelink WHERE id = ?`,
		m.LinkID).Scan(&expires, &active); err == nil {
		now := formatDjangoDateTime(time.Now())
		if expires.Valid && expires.String <= now {
			return "link_expired"
		}
	}
	return "link_revoked"
}

// collabSessionValid port _session_valid: the session exists (and has not
// expired) and records this user's current auth hash.
func (a *App) collabSessionValid(sessionKey string, u *UserRow) bool {
	if sessionKey == "" {
		return false
	}
	s, err := getSession(a.DB, sessionKey)
	if err != nil || s == nil {
		return false
	}
	if !time.Now().Before(s.ExpiresAt) {
		return false
	}
	// Current secret first, then fallbacks (django's accepted list). Our
	// session hash is secret-independent (sha256 over the encoded password),
	// so one comparison covers every accepted candidate.
	return s.AuthHash == sessionAuthHash(u.Password)
}
