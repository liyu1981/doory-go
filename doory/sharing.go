// Sharing: owner-side share-link and comment endpoints
// (apps/sharing/{views,serializers,services,models}.py owner half).
//
// Probe-first: probes/0008-sharing (37 transcripts): link envelope with
// SITE_URL-based url + urlsafe token, role/expiry validation messages,
// comment author order (name, avatar_url, id), x/y pairing rules, owner
// comment CRUD.
//
// Ports backend/apps/sharing/views.py (owner endpoints)
// Ports backend/apps/sharing/serializers.py
// Ports backend/apps/sharing/services.py
// Ports backend/apps/sharing/models.py
package doory

import (
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	maxActiveLinksPerDesign = 20
	maxCommentsPerDesign    = 2000
	shareTokenBytes         = 24 // token_urlsafe(24) -> 32 urlsafe chars
	commentBodyMax          = 2000
	sharePasswordMin        = 4
	sharePasswordMax        = 128
)

var shareRoles = []string{"view", "comment", "edit"}

// shareLinkRow maps sharing_sharelink.
type shareLinkRow struct {
	ID           string
	DesignID     string
	Token        string
	Role         string
	IsActive     bool
	ExpiresAt    sql.NullString
	PasswordHash string
	CreatedBy    sql.NullString
	LastAccessed sql.NullString
	AccessCount  int64
	CreatedAt    string
	UpdatedAt    string
}

const shareLinkCols = `id, design_id, token, role, is_active, expires_at, password_hash,
	created_by_id, last_accessed_at, access_count, created_at, updated_at`

// shareLinkJoinCols qualifies the same columns for joined queries (id is
// ambiguous across sharelink/design/user otherwise).
const shareLinkJoinCols = `l.id, l.design_id, l.token, l.role, l.is_active, l.expires_at,
	l.password_hash, l.created_by_id, l.last_accessed_at, l.access_count, l.created_at, l.updated_at`

func scanShareLink(s scanner) (shareLinkRow, error) {
	var l shareLinkRow
	err := s.Scan(&l.ID, &l.DesignID, &l.Token, &l.Role, &l.IsActive, &l.ExpiresAt,
		&l.PasswordHash, &l.CreatedBy, &l.LastAccessed, &l.AccessCount, &l.CreatedAt, &l.UpdatedAt)
	return l, err
}

// shareLinkEnvelope mirrors ShareLinkSerializer field order.
type shareLinkEnvelope struct {
	ID          string  `json:"id"`
	Token       string  `json:"token"`
	Role        string  `json:"role"`
	URL         string  `json:"url"`
	IsActive    bool    `json:"is_active"`
	ExpiresAt   *string `json:"expires_at"`
	HasPassword bool    `json:"has_password"`
	CreatedAt   string  `json:"created_at"`
}

func (l *shareLinkRow) envelope(siteURL string) shareLinkEnvelope {
	return shareLinkEnvelope{
		ID: dashedUUID(l.ID), Token: l.Token, Role: l.Role,
		URL:         strings.TrimRight(siteURL, "/") + "/s/" + l.Token,
		IsActive:    l.IsActive,
		ExpiresAt:   nullableDatetime(l.ExpiresAt),
		HasPassword: l.PasswordHash != "",
		CreatedAt:   renderDRFDatetime(l.CreatedAt),
	}
}

// newShareToken ports generate_share_token: token_urlsafe(24) = 32 urlsafe
// chars (probe0008: PKuHIg5PDzZVbUaltgkiO7I31myjrdtw).
func newShareToken() string {
	buf := make([]byte, shareTokenBytes)
	if _, err := rand.Read(buf); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(buf)
}

// activeLinkWhere mirrors services.active_links: not expired, on, design
// not trashed, owner enabled.
const activeLinkWhere = `l.is_active = 1
	AND (l.expires_at IS NULL OR l.expires_at > ?)
	AND d.trashed_at IS NULL
	AND u.is_active = 1`

// getOwnDesignForOwner ports OwnerAPIView.get_design: owner-scoped,404
// otherwise (document deferred — not needed for link/comment lists).
func (a *App) getOwnDesignForOwner(w http.ResponseWriter, r *http.Request) (*designRow, bool) {
	u := currentUser(r)
	id := hexUUID(routeParam(r, "design_pk"))
	d, err := a.getDesignByID(id)
	if err != nil {
		logerr(err, "getOwnDesignForOwner")
		writeDRF(w, http.StatusInternalServerError, "server_error", "Something went wrong on our side.")
		return nil, false
	}
	if d == nil || d.OwnerID != u.ID {
		writeDRF(w, http.StatusNotFound, "not_found", "Not found.")
		return nil, false
	}
	return d, true
}

// shareLinkList ports ShareLinkListCreateView.get (paginated, newest first).
func (a *App) shareLinkList(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		a.shareLinkCreate(w, r)
		return
	}
	d, ok := a.getOwnDesignForOwner(w, r)
	if !ok {
		return
	}
	now := formatDjangoDateTime(time.Now())
	rows, err := a.DB.Query(
		`SELECT `+shareLinkJoinCols+` FROM sharing_sharelink l
		 JOIN designs_design d ON d.id = l.design_id
		 JOIN accounts_user u ON u.id = d.owner_id
		 WHERE l.design_id = ? AND `+activeLinkWhere+`
		 ORDER BY l.created_at DESC, l.id ASC`, d.ID, now)
	if err != nil {
		logerr(err, "shareLinkList")
		writeDRF(w, http.StatusInternalServerError, "server_error", "Something went wrong on our side.")
		return
	}
	defer rows.Close()
	links := []shareLinkRow{}
	for rows.Next() {
		l, err := scanShareLink(rows)
		if err != nil {
			logerr(err, "shareLinkList.scan")
			writeDRF(w, http.StatusInternalServerError, "server_error", "Something went wrong on our side.")
			return
		}
		links = append(links, l)
	}
	page, ferr := windowSlice(r, len(links), designPageSize)
	if ferr != nil {
		writeDRF(w, ferr.Status, ferr.Code, ferr.Message)
		return
	}
	from, to := clampWindow(len(links), page)
	envs := make([]shareLinkEnvelope, 0, to-from)
	for _, l := range links[from:to] {
		envs = append(envs, l.envelope(a.Settings.SiteURL))
	}
	resp, err := paginateSized(r, len(links), envs, designPageSize)
	if err != nil {
		if de, ok := err.(*DocumentError); ok {
			writeDRF(w, de.Status, de.Code, de.Message)
			return
		}
		writeDRF(w, http.StatusInternalServerError, "server_error", "Something went wrong on our side.")
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// shareLinkCreate ports ShareLinkListCreateView.post + create_link: trashed
// design -> bad_request envelope; active-link limit; validated role/expiry/
// password.
func (a *App) shareLinkCreate(w http.ResponseWriter, r *http.Request) {
	d, ok := a.getOwnDesignForOwner(w, r)
	if !ok {
		return
	}
	if d.TrashedAt.Valid {
		writeDRF(w, http.StatusBadRequest, "design_trashed", "Restore the design before sharing it.")
		return
	}
	var body struct {
		Role      string  `json:"role"`
		ExpiresAt *string `json:"expires_at"`
		Password  *string `json:"password"`
	}
	if err := jsonBody(r, &body); err != nil {
		writeDRF(w, http.StatusBadRequest, "parse_error", "Malformed request body.")
		return
	}
	fields, cleanPassword, expiry := a.validateShareWrite(&body, nil)
	if len(fields) > 0 {
		writeDRFValidation(w, fields)
		return
	}
	// create_link: active count under the design lock (SQLite serializes).
	count, err := a.countActiveLinks(d.ID)
	if err != nil {
		logerr(err, "shareLinkCreate.count")
		writeDRF(w, http.StatusInternalServerError, "server_error", "Something went wrong on our side.")
		return
	}
	if count >= maxActiveLinksPerDesign {
		writeDRF(w, http.StatusBadRequest, "share_link_limit",
			fmt.Sprintf("A design can have at most %d active share links.", maxActiveLinksPerDesign))
		return
	}
	u := currentUser(r)
	id := newUUID4Hex()
	now := formatDjangoDateTime(time.Now())
	pwHash := ""
	if cleanPassword != "" {
		pwHash = hashPassword(cleanPassword)
	}
	var expiryArg any
	if expiry != nil {
		expiryArg = *expiry
	}
	if _, err := a.DB.Exec(
		`INSERT INTO sharing_sharelink
		 (id, created_at, updated_at, design_id, token, role, is_active, expires_at,
		  password_hash, created_by_id, last_accessed_at, access_count)
		 VALUES (?, ?, ?, ?, ?, ?, 1, ?, ?, ?, NULL, 0)`,
		id, now, now, d.ID, newShareToken(), body.Role, expiryArg, pwHash, u.ID); err != nil {
		logerr(err, "shareLinkCreate.insert")
		writeDRF(w, http.StatusInternalServerError, "server_error", "Something went wrong on our side.")
		return
	}
	l, err := a.getShareLinkByID(id)
	if err != nil || l == nil {
		writeDRF(w, http.StatusInternalServerError, "server_error", "Something went wrong on our side.")
		return
	}
	a.notifyDesignRecheck(d.ID) // signals.link_changed (post_save)
	writeJSON(w, http.StatusCreated, l.envelope(a.Settings.SiteURL))
}

// validateShareWrite mirrors ShareLinkCreate/UpdateSerializer: role choice,
// expires_at future check, printable-ASCII password with length bounds.
func (a *App) validateShareWrite(body *struct {
	Role      string  `json:"role"`
	ExpiresAt *string `json:"expires_at"`
	Password  *string `json:"password"`
}, existing *shareLinkRow,
) (fields []FieldError, cleanPassword string, expiry *string) {
	if body.Role == "" && existing == nil {
		fields = append(fields, SimpleField("role", "This field is required.", "required"))
	} else if body.Role != "" && !choiceValid(shareRoles, body.Role) {
		fields = append(fields, SimpleField("role",
			fmt.Sprintf("%q is not a valid choice.", body.Role), "invalid_choice"))
	}
	if body.ExpiresAt != nil {
		t, err := parseISO8601(*body.ExpiresAt)
		if err != nil {
			fields = append(fields, SimpleField("expires_at",
				"Datetime has wrong format. Use one of these formats instead: YYYY-MM-DD HH:MM[:SS[.ffffff]][+ZZ[:ZZ]].",
				"invalid"))
		} else if t.Before(time.Now().UTC()) &&
			(existing == nil || existing.ExpiresAt.Valid == false) {
			// unchanged-vs-future: create path always checks
			fields = append(fields, SimpleField("expires_at",
				"expires_at must be in the future.", "invalid"))
		} else if err == nil {
			s := formatDjangoDateTime(t)
			expiry = &s
		}
	}
	if body.Password != nil && *body.Password != "" {
		pw := *body.Password
		if len(pw) < sharePasswordMin {
			fields = append(fields, SimpleField("password",
				fmt.Sprintf("Ensure this field has at least %d characters.", sharePasswordMin), "min_length"))
		} else if len(pw) > sharePasswordMax {
			fields = append(fields, SimpleField("password",
				fmt.Sprintf("Ensure this field has no more than %d characters.", sharePasswordMax), "max_length"))
		} else if !printableASCII(pw) {
			fields = append(fields, SimpleField("password",
				"Use only letters, digits, spaces and standard symbols (printable ASCII).", "ascii"))
		} else {
			cleanPassword = pw
		}
	}
	return fields, cleanPassword, expiry
}

func printableASCII(s string) bool {
	for _, r := range s {
		if r < 0x20 || r > 0x7e {
			return false
		}
	}
	return true
}

func parseISO8601(v string) (time.Time, error) {
	layouts := []string{
		time.RFC3339, "2006-01-02T15:04:05.999999", "2006-01-02T15:04:05",
		"2006-01-02 15:04:05.999999", "2006-01-02 15:04:05", "2006-01-02",
	}
	for _, l := range layouts {
		if t, err := time.Parse(l, v); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("bad datetime")
}

func (a *App) countActiveLinks(designID string) (int, error) {
	now := formatDjangoDateTime(time.Now())
	var n int
	err := a.DB.QueryRow(
		`SELECT COUNT(*) FROM sharing_sharelink l
		 JOIN designs_design d ON d.id = l.design_id
		 JOIN accounts_user u ON u.id = d.owner_id
		 WHERE l.design_id = ? AND `+activeLinkWhere, designID, now).Scan(&n)
	return n, err
}

func (a *App) getShareLinkByID(id string) (*shareLinkRow, error) {
	row := a.DB.QueryRow(
		"SELECT "+shareLinkCols+" FROM sharing_sharelink WHERE id = ?", id)
	l, err := scanShareLink(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &l, nil
}

// shareLinkDetail GET/PATCH/DELETE (owner-scoped via the design join).
func (a *App) shareLinkDetail(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		a.shareLinkRetrieve(w, r)
	case http.MethodPatch:
		a.shareLinkUpdate(w, r)
	case http.MethodDelete:
		a.shareLinkDestroy(w, r)
	}
}

func (a *App) loadOwnLink(w http.ResponseWriter, r *http.Request) (*shareLinkRow, *designRow, bool) {
	d, ok := a.getOwnDesignForOwner(w, r)
	if !ok {
		return nil, nil, false
	}
	pk := hexUUID(routeParam(r, "uuid"))
	row := a.DB.QueryRow(
		"SELECT "+shareLinkCols+" FROM sharing_sharelink WHERE id = ? AND design_id = ?", pk, d.ID)
	l, err := scanShareLink(row)
	if err == sql.ErrNoRows {
		writeDRF(w, http.StatusNotFound, "not_found", "Not found.")
		return nil, nil, false
	}
	if err != nil {
		logerr(err, "loadOwnLink")
		writeDRF(w, http.StatusInternalServerError, "server_error", "Something went wrong on our side.")
		return nil, nil, false
	}
	return &l, d, true
}

func (a *App) shareLinkRetrieve(w http.ResponseWriter, r *http.Request) {
	l, _, ok := a.loadOwnLink(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, l.envelope(a.Settings.SiteURL))
}

// shareLinkUpdate ports the detail PATCH + update_link (reactivating counts
// against the limit — was_active/now_active check).
func (a *App) shareLinkUpdate(w http.ResponseWriter, r *http.Request) {
	l, d, ok := a.loadOwnLink(w, r)
	if !ok {
		return
	}
	var body struct {
		Role      string  `json:"role"`
		ExpiresAt *string `json:"expires_at"`
		IsActive  *bool   `json:"is_active"`
		Password  *string `json:"password"`
	}
	if err := jsonBody(r, &body); err != nil {
		writeDRF(w, http.StatusBadRequest, "parse_error", "Malformed request body.")
		return
	}
	// share-write validation shares the create path (role optional here).
	writeBody := struct {
		Role      string  `json:"role"`
		ExpiresAt *string `json:"expires_at"`
		Password  *string `json:"password"`
	}{Role: body.Role, ExpiresAt: body.ExpiresAt, Password: body.Password}
	fields, cleanPassword, expiry := a.validateShareWrite(&writeBody, l)
	if len(fields) > 0 {
		writeDRFValidation(w, fields)
		return
	}

	wasActive, err := a.linkActive(l, time.Now().UTC())
	if err != nil {
		writeDRF(w, http.StatusInternalServerError, "server_error", "Something went wrong on our side.")
		return
	}
	updates := []string{}
	args := []any{}
	if body.Role != "" {
		if !choiceValid(shareRoles, body.Role) {
			writeDRFValidation(w, []FieldError{SimpleField("role",
				fmt.Sprintf("%q is not a valid choice.", body.Role), "invalid_choice")})
			return
		}
		updates = append(updates, "role = ?")
		args = append(args, body.Role)
	}
	if body.ExpiresAt != nil {
		updates = append(updates, "expires_at = ?")
		if expiry == nil {
			args = append(args, nil)
		} else {
			args = append(args, *expiry)
		}
	}
	if body.IsActive != nil {
		updates = append(updates, "is_active = ?")
		args = append(args, *body.IsActive)
	}
	if body.Password != nil {
		updates = append(updates, "password_hash = ?")
		if cleanPassword == "" {
			args = append(args, "") // empty removes protection
		} else {
			args = append(args, hashPassword(cleanPassword))
		}
	}
	if len(updates) == 0 {
		writeJSON(w, http.StatusOK, l.envelope(a.Settings.SiteURL))
		return
	}
	updates = append(updates, "updated_at = ?")
	args = append(args, formatDjangoDateTime(time.Now()))
	args = append(args, l.ID)
	if _, err := a.DB.Exec(
		"UPDATE sharing_sharelink SET "+strings.Join(updates, ", ")+" WHERE id = ?", args...); err != nil {
		logerr(err, "shareLinkUpdate")
		writeDRF(w, http.StatusInternalServerError, "server_error", "Something went wrong on our side.")
		return
	}
	fresh, _ := a.getShareLinkByID(l.ID)
	// reactivating counts against the active-link limit (update_link)
	nowActive, err := a.linkActive(fresh, time.Now().UTC())
	if err == nil && !wasActive && nowActive {
		count, cerr := a.countActiveLinks(d.ID)
		if cerr == nil && count > maxActiveLinksPerDesign {
			writeDRF(w, http.StatusBadRequest, "share_link_limit",
				fmt.Sprintf("A design can have at most %d active share links.", maxActiveLinksPerDesign))
			return
		}
	}
	a.notifyDesignRecheck(d.ID) // signals.link_changed (post_save)
	writeJSON(w, http.StatusOK, fresh.envelope(a.Settings.SiteURL))
}

func (a *App) linkActive(l *shareLinkRow, now time.Time) (bool, error) {
	if l == nil || !l.IsActive {
		return false, nil
	}
	if l.ExpiresAt.Valid && l.ExpiresAt.String != "" {
		exp, err := parseDjangoDateTime(l.ExpiresAt.String)
		if err == nil && !now.Before(exp) {
			return false, nil
		}
	}
	return true, nil
}

func (a *App) shareLinkDestroy(w http.ResponseWriter, r *http.Request) {
	l, _, ok := a.loadOwnLink(w, r)
	if !ok {
		return
	}
	if _, err := a.DB.Exec(`DELETE FROM sharing_sharelink WHERE id = ?`, l.ID); err != nil {
		logerr(err, "shareLinkDestroy")
		writeDRF(w, http.StatusInternalServerError, "server_error", "Something went wrong on our side.")
		return
	}
	a.notifyDesignRecheck(l.DesignID) // signals.link_changed (post_delete)
	w.Header().Set("Content-Length", "0")
	w.WriteHeader(http.StatusNoContent)
}

// ---- owner comments ----

// commentRow maps sharing_comment.
type commentRow struct {
	ID        string
	DesignID  string
	AuthorID  string
	Body      string
	PageID    string
	X         sql.NullFloat64
	Y         sql.NullFloat64
	ParentID  sql.NullString
	Resolved  bool
	CreatedAt string
	UpdatedAt string
}

const commentCols = `id, design_id, author_id, body, page_id, x, y, parent_id, resolved,
	created_at, updated_at`

func scanComment(s scanner) (commentRow, error) {
	var c commentRow
	err := s.Scan(&c.ID, &c.DesignID, &c.AuthorID, &c.Body, &c.PageID, &c.X, &c.Y,
		&c.ParentID, &c.Resolved, &c.CreatedAt, &c.UpdatedAt)
	return c, err
}

// pyFloat renders a Python float: integral values keep ".0" (probe0008:
// x:10.0) — Go would emit "10".
type pyFloat float64

func (f pyFloat) MarshalJSON() ([]byte, error) {
	v := float64(f)
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return []byte("null"), nil
	}
	s := strconv.FormatFloat(v, 'g', -1, 64)
	if !strings.ContainsAny(s, ".eEnN") {
		s += ".0"
	}
	return []byte(s), nil
}

// commentEnvelope mirrors CommentSerializer field order; author renders
// PublicUser fields FIRST then id (probe0008 step13 byte order).
type commentEnvelope struct {
	ID        string        `json:"id"`
	Author    commentAuthor `json:"author"`
	Body      string        `json:"body"`
	PageID    *string       `json:"page_id"`
	X         *pyFloat      `json:"x"`
	Y         *pyFloat      `json:"y"`
	Parent    *string       `json:"parent"`
	Resolved  bool          `json:"resolved"`
	CreatedAt string        `json:"created_at"`
}

type commentAuthor struct {
	Name      string  `json:"name"`
	AvatarURL *string `json:"avatar_url"`
	ID        string  `json:"id"`
}

func (c *commentRow) envelope(a *App) commentEnvelope {
	author, _ := getUserByID(a.DB, c.AuthorID)
	name := "Doory user" // serializers.ANONYMOUS_NAME
	var avatar *string
	var authorID string
	if author != nil {
		name = publicName(author)
		authorID = dashedUUID(author.ID)
	}
	var pageID *string
	if c.PageID != "" {
		pageID = &c.PageID
	}
	var x, y *pyFloat
	if c.X.Valid {
		v := pyFloat(c.X.Float64)
		x = &v
	}
	if c.Y.Valid {
		v := pyFloat(c.Y.Float64)
		y = &v
	}
	var parent *string
	if c.ParentID.Valid && c.ParentID.String != "" {
		p := dashedUUID(c.ParentID.String)
		parent = &p
	}
	return commentEnvelope{
		ID: dashedUUID(c.ID),
		Author: commentAuthor{
			Name: name, AvatarURL: avatar, ID: authorID,
		},
		Body: c.Body, PageID: pageID, X: x, Y: y, Parent: parent,
		Resolved: c.Resolved, CreatedAt: renderDRFDatetime(c.CreatedAt),
	}
}

// publicName ports serializers.public_name: display name or the shared
// pseudonym — never derived from the e-mail address.
func publicName(u *UserRow) string {
	if n := strings.TrimSpace(u.DisplayName); n != "" {
		return n
	}
	return "Doory user"
}

// commentsList ports DesignCommentListCreateView.get (paginated 100,
// filters resolved/page_id).
func (a *App) commentsList(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		a.commentsCreate(w, r)
		return
	}
	d, ok := a.getOwnDesignForOwner(w, r)
	if !ok {
		return
	}
	where := []string{"design_id = ?"}
	args := []any{d.ID}
	q := r.URL.Query()
	if v, present := q["resolved"]; present {
		b, err := parseDRFBool(v[0])
		if err != nil {
			writeDRFValidation(w, []FieldError{SimpleField("resolved",
				fmt.Sprintf("%q is not a valid boolean.", v[0]), "invalid")})
			return
		}
		if b {
			where = append(where, "resolved = 1")
		} else {
			where = append(where, "resolved = 0")
		}
	}
	if v := q.Get("page_id"); v != "" {
		where = append(where, "page_id = ?")
		args = append(args, v)
	}
	rows, err := a.DB.Query(
		"SELECT "+commentCols+" FROM sharing_comment WHERE "+strings.Join(where, " AND ")+
			" ORDER BY created_at, id", args...)
	if err != nil {
		logerr(err, "commentsList")
		writeDRF(w, http.StatusInternalServerError, "server_error", "Something went wrong on our side.")
		return
	}
	defer rows.Close()
	comments := []commentRow{}
	for rows.Next() {
		c, err := scanComment(rows)
		if err != nil {
			logerr(err, "commentsList.scan")
			writeDRF(w, http.StatusInternalServerError, "server_error", "Something went wrong on our side.")
			return
		}
		comments = append(comments, c)
	}
	page, ferr := windowSlice(r, len(comments), 100) // CommentPagination page_size
	if ferr != nil {
		writeDRF(w, ferr.Status, ferr.Code, ferr.Message)
		return
	}
	from, to := clampWindow(len(comments), page)
	envs := make([]commentEnvelope, 0, to-from)
	for _, c := range comments[from:to] {
		envs = append(envs, c.envelope(a))
	}
	resp, err := paginateSized(r, len(comments), envs, 100)
	if err != nil {
		if de, ok := err.(*DocumentError); ok {
			writeDRF(w, de.Status, de.Code, de.Message)
			return
		}
		writeDRF(w, http.StatusInternalServerError, "server_error", "Something went wrong on our side.")
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// commentsCreate ports DesignCommentListCreateView.post + create_comment
// (shared with the token path): trashed design, comment limit, anchor and
// parent rules from CommentCreateSerializer.
func (a *App) commentsCreate(w http.ResponseWriter, r *http.Request) {
	d, ok := a.getOwnDesignForOwner(w, r)
	if !ok {
		return
	}
	if d.TrashedAt.Valid {
		writeDRF(w, http.StatusBadRequest, "design_trashed",
			"Restore the design before commenting on it.")
		return
	}
	a.createCommentFor(w, r, d.ID)
}

// createCommentFor implements the shared create_comment flow.
func (a *App) createCommentFor(w http.ResponseWriter, r *http.Request, designID string) {
	u := currentUser(r)
	if u == nil {
		writeDRF(w, http.StatusUnauthorized, "not_authenticated",
			"Authentication credentials were not provided.")
		return
	}
	var body struct {
		Body   string   `json:"body"`
		PageID *string  `json:"page_id"`
		X      *float64 `json:"x"`
		Y      *float64 `json:"y"`
		Parent *string  `json:"parent"`
	}
	if err := jsonBody(r, &body); err != nil {
		writeDRF(w, http.StatusBadRequest, "parse_error", "Malformed request body.")
		return
	}
	var fields []FieldError
	if body.Body == "" {
		fields = append(fields, SimpleField("body", "This field is required.", "required"))
	} else if len([]rune(body.Body)) > commentBodyMax {
		fields = append(fields, SimpleField("body",
			fmt.Sprintf("Ensure this field has no more than %d characters.", commentBodyMax), "max_length"))
	}
	if body.PageID != nil && *body.PageID != "" && !validPageID(*body.PageID) {
		fields = append(fields, FieldError{Name: "page_id",
			Messages: []string{fmt.Sprintf(
				"Enter a value matching the pattern ^[A-Za-z0-9_.:-]{1,64}$")},
			Codes: []string{"invalid"}})
	}
	coordPaired := (body.X == nil) == (body.Y == nil)
	if !coordPaired {
		fields = append(fields, SimpleField("x", "x and y must be given together.", "invalid"))
	}
	// parent lookup: unknown -> {"parent": ["Unknown comment."]} (flat);
	// nested replies refused; the parent's anchor wins.
	var parentID any
	var parentPage, parentX, parentY any
	if body.Parent != nil && *body.Parent != "" {
		pk := hexUUID(*body.Parent)
		var foundPage string
		var foundX, foundY sql.NullFloat64
		var foundParent sql.NullString
		err := a.DB.QueryRow(
			`SELECT page_id, x, y, COALESCE(parent_id, '') FROM sharing_comment
			 WHERE id = ? AND design_id = ?`, pk, designID).
			Scan(&foundPage, &foundX, &foundY, &foundParent)
		if err == sql.ErrNoRows {
			fields = append(fields, SimpleField("parent", "Unknown comment.", "invalid"))
		} else if err == nil {
			if foundParent.String != "" {
				fields = append(fields, SimpleField("parent", "Replies cannot be nested.", "invalid"))
			} else {
				parentID = pk
				parentPage = foundPage
				if foundX.Valid {
					v := pyFloat(foundX.Float64)
					parentX = v
				}
				if foundY.Valid {
					v := pyFloat(foundY.Float64)
					parentY = v
				}
			}
		}
	}
	if len(fields) > 0 {
		writeDRFValidation(w, fields)
		return
	}

	var count int
	_ = a.DB.QueryRow(`SELECT COUNT(*) FROM sharing_comment WHERE design_id = ?`,
		designID).Scan(&count)
	if count >= maxCommentsPerDesign {
		writeDRF(w, http.StatusBadRequest, "comment_limit_reached",
			"This design has reached its comment limit.")
		return
	}

	page := ""
	if body.PageID != nil {
		page = *body.PageID
	}
	var xArg, yArg any
	switch {
	case parentID != nil:
		page, _ = parentPage.(string)
		if v, ok := parentX.(pyFloat); ok {
			xArg = float64(v)
		}
		if v, ok := parentY.(pyFloat); ok {
			yArg = float64(v)
		}
	case body.X != nil:
		xArg, yArg = *body.X, *body.Y
	default:
		if page == "" {
			page = ""
		}
	}
	id := newUUID4Hex()
	now := formatDjangoDateTime(time.Now())
	if _, err := a.DB.Exec(
		`INSERT INTO sharing_comment
		 (id, created_at, updated_at, design_id, author_id, body, page_id, x, y, parent_id, resolved)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0)`,
		id, now, now, designID, u.ID, body.Body, page, xArg, yArg, parentID); err != nil {
		logerr(err, "commentsCreate.insert")
		writeDRF(w, http.StatusInternalServerError, "server_error", "Something went wrong on our side.")
		return
	}
	c, err := a.getCommentByID(id)
	if err != nil || c == nil {
		writeDRF(w, http.StatusInternalServerError, "server_error", "Something went wrong on our side.")
		return
	}
	writeJSON(w, http.StatusCreated, c.envelope(a))
}

func (a *App) getCommentByID(id string) (*commentRow, error) {
	row := a.DB.QueryRow("SELECT "+commentCols+" FROM sharing_comment WHERE id = ?", id)
	c, err := scanComment(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// validPageID mirrors the _PAGE_ID pattern.
func validPageID(v string) bool {
	if len(v) < 1 || len(v) > 64 {
		return false
	}
	for _, r := range v {
		ok := r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9' ||
			r == '_' || r == '.' || r == ':' || r == '-'
		if !ok {
			return false
		}
	}
	return true
}

// commentDetail PATCH (owner rules from CommentUpdateSerializer) / DELETE
// (owner may delete any comment; others only their own — owner scope here).
func (a *App) commentDetail(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPatch:
		a.commentPatch(w, r)
	case http.MethodDelete:
		a.commentDelete(w, r)
	}
}

func (a *App) loadOwnComment(w http.ResponseWriter, r *http.Request) (*commentRow, *designRow, bool) {
	d, ok := a.getOwnDesignForOwner(w, r)
	if !ok {
		return nil, nil, false
	}
	pk := hexUUID(routeParam(r, "uuid"))
	c, err := a.getCommentByID(pk)
	if err != nil || c == nil || c.DesignID != d.ID {
		writeDRF(w, http.StatusNotFound, "not_found", "Not found.")
		return nil, nil, false
	}
	return c, d, true
}

func (a *App) commentPatch(w http.ResponseWriter, r *http.Request) {
	c, _, ok := a.loadOwnComment(w, r)
	if !ok {
		return
	}
	u := currentUser(r)
	var body struct {
		Resolved *bool    `json:"resolved"`
		Body     *string  `json:"body"`
		PageID   *string  `json:"page_id"`
		X        *float64 `json:"x"`
		Y        *float64 `json:"y"`
	}
	if err := jsonBody(r, &body); err != nil {
		writeDRF(w, http.StatusBadRequest, "parse_error", "Malformed request body.")
		return
	}
	var fields []FieldError
	if body.Body != nil && c.AuthorID != u.ID {
		fields = append(fields, SimpleField("body",
			"You can only change the text of your own comments.", "invalid"))
	}
	anchorPresent := body.PageID != nil || body.X != nil || body.Y != nil
	if c.ParentID.Valid && c.ParentID.String != "" {
		if body.Resolved != nil {
			fields = append(fields, SimpleField("resolved",
				"Only top-level comments can be resolved.", "invalid"))
		}
		if anchorPresent {
			fields = append(fields, SimpleField("x",
				"Only top-level comments can be moved.", "invalid"))
		}
	}
	if (body.X == nil) != (body.Y == nil) && anchorPresent {
		fields = append(fields, SimpleField("x", "x and y must be given together.", "invalid"))
	}
	if body.Body != nil && len([]rune(*body.Body)) > commentBodyMax {
		fields = append(fields, SimpleField("body",
			fmt.Sprintf("Ensure this field has no more than %d characters.", commentBodyMax), "max_length"))
	}
	if body.PageID != nil && *body.PageID != "" && !validPageID(*body.PageID) {
		fields = append(fields, FieldError{Name: "page_id",
			Messages: []string{"Enter a value matching the pattern ^[A-Za-z0-9_.:-]{1,64}$"},
			Codes:    []string{"invalid"}})
	}
	if len(fields) > 0 {
		writeDRFValidation(w, fields)
		return
	}
	updates := []string{}
	args := []any{}
	if body.Resolved != nil {
		updates = append(updates, "resolved = ?")
		args = append(args, *body.Resolved)
	}
	if body.Body != nil {
		updates = append(updates, "body = ?")
		args = append(args, cleanCommentBody(*body.Body))
	}
	anchor := map[string]*string{}
	if body.PageID != nil {
		v := *body.PageID
		anchor["page_id"] = &v
	}
	if body.X != nil && body.Y != nil {
		// top-level moves apply to replies too (services.move)
		if !c.ParentID.Valid || c.ParentID.String == "" {
			xs := formatFloat(*body.X)
			ys := formatFloat(*body.Y)
			anchor["x"] = &xs
			anchor["y"] = &ys
		}
	}
	for col, v := range anchor {
		updates = append(updates, col+" = ?")
		if v == nil {
			args = append(args, nil)
		} else {
			args = append(args, *v)
		}
	}
	if len(updates) == 0 {
		fresh, _ := a.getCommentByID(c.ID)
		writeJSON(w, http.StatusOK, fresh.envelope(a))
		return
	}
	updates = append(updates, "updated_at = ?")
	args = append(args, formatDjangoDateTime(time.Now()))
	args = append(args, c.ID)
	if _, err := a.DB.Exec(
		"UPDATE sharing_comment SET "+strings.Join(updates, ", ")+" WHERE id = ?", args...); err != nil {
		logerr(err, "commentPatch")
		writeDRF(w, http.StatusInternalServerError, "server_error", "Something went wrong on our side.")
		return
	}
	// replies follow the anchor (top-level moves)
	if len(anchor) > 0 && (!c.ParentID.Valid || c.ParentID.String == "") {
		setters := []string{}
		pargs := []any{}
		for col, v := range anchor {
			setters = append(setters, col+" = ?")
			if v == nil {
				pargs = append(pargs, nil)
			} else {
				pargs = append(pargs, *v)
			}
		}
		setters = append(setters, "updated_at = ?")
		pargs = append(pargs, formatDjangoDateTime(time.Now()))
		pargs = append(pargs, c.ID)
		_, _ = a.DB.Exec(
			"UPDATE sharing_comment SET "+strings.Join(setters, ", ")+
				" WHERE parent_id = ?", pargs...)
	}
	fresh, err := a.getCommentByID(c.ID)
	if err != nil || fresh == nil {
		writeDRF(w, http.StatusInternalServerError, "server_error", "Something went wrong on our side.")
		return
	}
	writeJSON(w, http.StatusOK, fresh.envelope(a))
}

func formatFloat(f float64) string {
	s := strconv.FormatFloat(f, 'g', -1, 64)
	return s
}

// cleanCommentBody trims like CharField (no internal change beyond str()).
func cleanCommentBody(v string) string { return v }

// commentDelete: owner may delete ANY comment of the design.
func (a *App) commentDelete(w http.ResponseWriter, r *http.Request) {
	c, _, ok := a.loadOwnComment(w, r)
	if !ok {
		return
	}
	if _, err := a.DB.Exec(`DELETE FROM sharing_comment WHERE id = ?`, c.ID); err != nil {
		logerr(err, "commentDelete")
		writeDRF(w, http.StatusInternalServerError, "server_error", "Something went wrong on our side.")
		return
	}
	w.Header().Set("Content-Length", "0")
	w.WriteHeader(http.StatusNoContent)
}
