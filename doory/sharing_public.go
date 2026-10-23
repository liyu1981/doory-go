// Public (token) sharing endpoints + the password gate
// (apps/sharing/views.py Shared* half + services password windows +
// apps/fonts/usage.py for the shared font payload).
//
// Probe-first (probes/0008-sharing): never-cache string + Vary order
// ("Cookie, X-Share-Password, origin"), shared design payload bytes
// (owner pseudonym, document passthrough, fonts []), password_required401,
// the lock at exactly the 10th wrong password with the DEFAULT DRF detail
// ("Request was throttled. Expected available in N seconds."), the shared
// revision-conflict message (differs from the owner path!), role and
// comments-disabled errors.
//
// Ports backend/apps/sharing/views.py (Shared* views)
// Ports backend/apps/sharing/services.py (resolve/password/save)
// Ports backend/apps/fonts/usage.py
package doory

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"math"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
)

const neverCache = "max-age=0, no-cache, no-store, must-revalidate, private"

const (
	gateHeader         = "X-Share-Password"
	passwordWindowSec  = 15 * 60
	pwLimitPair        = 10  // per token + ip
	pwLimitIP          = 30  // per ip
	pwLimitToken       = 100 // per token
	throttledDefault   = "Request was throttled."
	sharedCommentRoles = "comment,edit" // roles whose links see comments
	maxFontNamesInDoc  = 64
	maxFontDocDepth    = 32
	maxFontDocNodes    = 200_000
)

// tokenPattern mirrors services.TOKEN_PATTERN.
var tokenPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{16,64}$`)

// ---- password gate (three independent fixed windows, in-process like the
// lockout guard — observable behavior scenario-proven, storage D6-style) ----

type gateState struct {
	count     int
	startedAt time.Time
}

type passwordGate struct {
	mu     sync.Mutex
	states map[string]*gateState
}

func newPasswordGate() *passwordGate {
	return &passwordGate{states: map[string]*gateState{}}
}

func gateDigest(parts ...string) string {
	h := sha256.New()
	h.Write([]byte(strings.Join(parts, "\x1f")))
	return hex.EncodeToString(h.Sum(nil))[:40]
}

// record adds a failure to one bucket (fixed window from first failure).
func (g *passwordGate) record(key string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := time.Now()
	s, ok := g.states[key]
	if !ok || now.Sub(s.startedAt) >= passwordWindowSec*time.Second {
		s = &gateState{startedAt: now}
		g.states[key] = s
	}
	s.count++
}

func (g *passwordGate) count(key string) (int, time.Duration) {
	g.mu.Lock()
	defer g.mu.Unlock()
	s, ok := g.states[key]
	if !ok || time.Since(s.startedAt) >= passwordWindowSec*time.Second {
		delete(g.states, key)
		return 0, 0
	}
	return s.count, time.Until(s.startedAt.Add(passwordWindowSec * time.Second))
}

func (g *passwordGate) clear(key string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.states, key)
}

// enforceLinkPassword ports services.enforce_link_password: no password ->
// pass; missing/blank header -> PasswordRequired (NOT counted); locked ->
// PasswordLocked EVEN for the right password; wrong -> count all three
// windows then PasswordRequired; right -> clear the pair window.
func (a *App) enforceLinkPassword(link *shareLinkRow, supplied string, ip string) error {
	if link.PasswordHash == "" {
		return nil
	}
	supplied = strings.TrimSpace(supplied)
	if supplied == "" {
		return errPasswordRequired
	}
	pairKey := gateDigest("pair", link.Token, ip)
	ipKey := gateDigest("ip", ip)
	tokenKey := gateDigest("token", link.Token)
	// ensure_not_locked: max retry over exhausted buckets
	var maxWait time.Duration
	locked := false
	for _, key := range []string{pairKey, ipKey, tokenKey} {
		var limit int
		switch key {
		case pairKey:
			limit = pwLimitPair
		case ipKey:
			limit = pwLimitIP
		default:
			limit = pwLimitToken
		}
		count, wait := a.gate.count(key)
		if count >= limit {
			locked = true
			if wait > maxWait {
				maxWait = wait
			}
		}
	}
	if locked {
		return &passwordLockedError{retryAfter: math.Max(1, math.Ceil(maxWait.Seconds()))}
	}
	if verifyPassword(supplied, link.PasswordHash) {
		a.gate.clear(pairKey)
		return nil
	}
	a.gate.record(pairKey)
	a.gate.record(ipKey)
	a.gate.record(tokenKey)
	return errPasswordRequired
}

var errPasswordRequired = &gateError{code: "password_required",
	message: "A password is required to open this link."}

type gateError struct {
	code, message string
}

func (e *gateError) Error() string { return e.message }

type passwordLockedError struct{ retryAfter float64 }

func (e *passwordLockedError) Error() string { return "locked" }

// clientIP strips the port (Django REMOTE_ADDR has none; both sides see
// "127.0.0.1" in the harness — bucket keys stay aligned).
func clientIP(r *http.Request) string {
	addr := r.RemoteAddr
	if idx := strings.LastIndex(addr, ":"); idx >= 0 {
		return addr[:idx]
	}
	return addr
}

// ---- link resolution ----

// resolveLink ports services.resolve_link: token pattern, active filter
// (not expired/on/design not trashed/owner enabled) with the design joined.
func (a *App) resolveLink(token string) (*shareLinkRow, *sharedDesignRow, bool) {
	if !tokenPattern.MatchString(token) {
		return nil, nil, false
	}
	now := formatDjangoDateTime(time.Now())
	row := a.DB.QueryRow(
		`SELECT `+shareLinkJoinCols+`, d.id, d.title, d.width, d.height, d.revision,
		        d.document, d.owner_id, d.trashed_at
		 FROM sharing_sharelink l
		 JOIN designs_design d ON d.id = l.design_id
		 JOIN accounts_user u ON u.id = d.owner_id
		 WHERE l.token = ? AND `+activeLinkWhere, token, now)
	l, d, err := scanLinkJoin(row)
	if err == sql.ErrNoRows {
		return nil, nil, false
	}
	if err != nil {
		logerr(err, "resolveLink")
		return nil, nil, false
	}
	return l, d, true
}

func scanLinkJoin(row *sql.Row) (*shareLinkRow, *sharedDesignRow, error) {
	var (
		l   shareLinkRow
		d   sharedDesignRow
		err error
	)
	err = row.Scan(&l.ID, &l.DesignID, &l.Token, &l.Role, &l.IsActive, &l.ExpiresAt,
		&l.PasswordHash, &l.CreatedBy, &l.LastAccessed, &l.AccessCount, &l.CreatedAt, &l.UpdatedAt,
		&d.ID, &d.Title, &d.Width, &d.Height, &d.Revision, &d.Document, &d.OwnerID, &d.TrashedAt)
	if err != nil {
		return nil, nil, err
	}
	return &l, &d, nil
}

// sharedDesignRow carries the joined design fields the public payload uses.
type sharedDesignRow struct {
	ID        string
	Title     string
	Width     int64
	Height    int64
	Revision  int64
	Document  string
	OwnerID   string
	TrashedAt sql.NullString
}

// ---- public views ----

// sharedDesignGet ports SharedDesignView.get: never-cache + Vary, password
// gate, payload with the owner pseudonym + document + uploaded fonts.
func (a *App) sharedDesignGet(w http.ResponseWriter, r *http.Request) {
	link, design, ok := a.resolveLink(routeParam(r, "token"))
	if !ok {
		writeDRF(w, http.StatusNotFound, "not_found", "Not found.")
		return
	}
	if err := a.enforceLinkPassword(link, r.Header.Get(gateHeader), clientIP(r)); err != nil {
		a.writeGateError(w, err)
		return
	}
	sharedHeaders(w)
	owner, _ := getUserByID(a.DB, design.OwnerID)
	name := "Doory user"
	var avatar *string
	if owner != nil {
		name = publicName(owner)
	}
	doc := compactStoredJSON([]byte(design.Document))
	fonts := a.fontsUsedIn(design.Document, design.OwnerID)
	writeJSON(w, http.StatusOK, sharedDesignPayload{
		DesignID: dashedUUID(design.ID),
		Role:     link.Role,
		Title:    design.Title,
		Width:    design.Width,
		Height:   design.Height,
		Revision: design.Revision,
		Owner:    sharedOwner{Name: name, AvatarURL: avatar},
		Document: rawCompacted(doc),
		Fonts:    fonts,
	})
	// record_access: one UPDATE (access_count/last_accessed) — never part of
	// any response; kept for row fidelity with a comment (observable only
	// through admin surfaces, divergence D2).
	_, _ = a.DB.Exec(
		`UPDATE sharing_sharelink SET access_count = access_count + 1, last_accessed_at = ?
		 WHERE id = ?`, formatDjangoDateTime(time.Now()), link.ID)
}

type sharedOwner struct {
	Name      string  `json:"name"`
	AvatarURL *string `json:"avatar_url"`
}

type sharedDesignPayload struct {
	DesignID string       `json:"design_id"`
	Role     string       `json:"role"`
	Title    string       `json:"title"`
	Width    int64        `json:"width"`
	Height   int64        `json:"height"`
	Revision int64        `json:"revision"`
	Owner    sharedOwner  `json:"owner"`
	Document rawCompacted `json:"document"`
	Fonts    []any        `json:"fonts"`
}

// sharedHeaders ports SharedAPIView.finalize_response: never-cache + Vary
// Cookie and the password gate header (cors appends origin afterwards).
// Called from dispatch for every response of a shared route — including
// permission401s and handler404s (probe0008 steps17/18/32).
func sharedHeaders(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", neverCache)
	// add_never_cache_headers also stamps Expires with the current time
	// (wall clock — comparator-denied like Date, D10).
	w.Header().Set("Expires", time.Now().UTC().Format(http.TimeFormat))
	patchVary(w, "Cookie")
	patchVary(w, gateHeader)
}

// writeGateError maps the password gate to its probe-proven envelopes.
func (a *App) writeGateError(w http.ResponseWriter, err error) {
	if locked, ok := err.(*passwordLockedError); ok {
		writeDRFThrottledDetail(w, locked.retryAfter, throttledDefault)
		return
	}
	writeDRF(w, http.StatusUnauthorized, "password_required",
		"A password is required to open this link.")
}

// sharedDesignPatch ports SharedDesignView.patch (edit-role optimistic save).
func (a *App) sharedDesignPatch(w http.ResponseWriter, r *http.Request) {
	limit := int64(maxDesignDocumentBytes + 64*1024)
	if r.ContentLength > limit {
		writeDRF(w, http.StatusRequestEntityTooLarge, "payload_too_large",
			"The request body is too large.")
		return
	}
	link, design, ok := a.resolveLink(routeParam(r, "token"))
	if !ok {
		writeDRF(w, http.StatusNotFound, "not_found", "Not found.")
		return
	}
	if err := a.enforceLinkPassword(link, r.Header.Get(gateHeader), clientIP(r)); err != nil {
		a.writeGateError(w, err)
		return
	}
	sharedHeaders(w)
	if link.Role != "edit" {
		// ShareRoleInsufficient: PermissionDenied — the handler turns it
		// into401 for ANON callers (RequireUnsafe already answered401) and
		// keeps403 for authenticated ones (probe0008 step33).
		writeDRF(w, http.StatusForbidden, "share_role_insufficient",
			"This link is read-only.")
		return
	}
	var body struct {
		// json.RawMessage for INPUT (unmarshal); rawCompacted only renders.
		Document     json.RawMessage `json:"document"`
		BaseRevision *int64          `json:"base_revision"`
	}
	if err := jsonBody(r, &body); err != nil {
		writeDRF(w, http.StatusBadRequest, "parse_error", "Malformed request body.")
		return
	}
	var fields []FieldError
	if body.Document == nil {
		fields = append(fields, SimpleField("document", "This field is required.", "required"))
	}
	if body.BaseRevision == nil {
		fields = append(fields, SimpleField("base_revision", "This field is required.", "required"))
	} else if *body.BaseRevision < 1 {
		fields = append(fields, SimpleField("base_revision",
			"Ensure this value is greater than or equal to 1.", "min_value"))
	}
	if len(fields) > 0 {
		writeDRFValidation(w, fields)
		return
	}
	doc, err := parseJSONValue(body.Document)
	if err != nil {
		writeDRFValidation(w, []FieldError{SimpleField("document", "Not a valid JSON object.", "invalid")})
		return
	}
	if verr := validateDocument(doc, maxDesignDocumentBytes); verr != nil {
		writeDRF(w, verr.Status, verr.Code, verr.Message)
		return
	}
	// save_shared_document: re-read live (trashed -> NotFound), conflict,
	// validate (done), store + revision bump + auto version + CAS write.
	live, err := a.getDesignByID(design.ID)
	if err != nil || live == nil || live.TrashedAt.Valid {
		writeDRF(w, http.StatusNotFound, "not_found", "Not found.")
		return
	}
	if live.Revision != *body.BaseRevision {
		writeConflict(w, live.Revision, live.UpdatedAt,
			"The design was changed by someone else.")
		return
	}
	info, _ := describeDocument(doc)
	now := formatDjangoDateTime(time.Now())
	res, err := a.DB.Exec(
		`UPDATE designs_design SET document = ?, width = ?, height = ?, page_count = ?,
		 preset = ?, revision = ?, updated_at = ?
		 WHERE id = ? AND revision = ? AND trashed_at IS NULL`,
		compactStoredJSON(body.Document), info.Width, info.Height, info.PageCount,
		info.Preset, live.Revision+1, now, live.ID, *body.BaseRevision)
	if err != nil {
		logerr(err, "sharedDesignPatch")
		writeDRF(w, http.StatusInternalServerError, "server_error", "Something went wrong on our side.")
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		writeDRF(w, http.StatusNotFound, "not_found", "Not found.")
		return
	}
	// record_auto_version_if_due attributed to the actor (designs port).
	if fresh, err := a.getDesignByID(live.ID); err == nil && fresh != nil {
		a.maybeRecordAutoVersion(fresh, compactStoredJSON(body.Document), currentUser(r))
	}
	writeJSON(w, http.StatusOK, sharedSaveResult{
		Revision:  live.Revision + 1,
		UpdatedAt: renderDRFDatetime(now),
	})
}

type sharedSaveResult struct {
	Revision  int64  `json:"revision"`
	UpdatedAt string `json:"updated_at"`
}

// ---- shared comments ----

// loadCommentLink resolves + password gate + role check (comments role).
func (a *App) loadCommentLink(w http.ResponseWriter, r *http.Request) (*shareLinkRow, bool) {
	link, _, ok := a.resolveLink(routeParam(r, "token"))
	if !ok {
		writeDRF(w, http.StatusNotFound, "not_found", "Not found.")
		return nil, false
	}
	if err := a.enforceLinkPassword(link, r.Header.Get(gateHeader), clientIP(r)); err != nil {
		a.writeGateError(w, err)
		return nil, false
	}
	if link.Role != "comment" && link.Role != "edit" {
		// CommentsDisabled is a NotFound SUBCLASS carrying its own code
		// (probe0008 step34: code comments_disabled, NOT not_found).
		writeDRF(w, http.StatusNotFound, "comments_disabled",
			"This link does not allow comments.")
		return nil, false
	}
	return link, true
}

// sharedCommentsList ports SharedCommentListCreateView.get.
func (a *App) sharedCommentsList(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		a.sharedCommentsCreate(w, r)
		return
	}
	link, ok := a.loadCommentLink(w, r)
	if !ok {
		return
	}
	sharedHeaders(w)
	rows, err := a.DB.Query(
		"SELECT "+commentCols+" FROM sharing_comment WHERE design_id = ?"+
			" ORDER BY created_at, id", link.DesignID)
	if err != nil {
		logerr(err, "sharedCommentsList")
		writeDRF(w, http.StatusInternalServerError, "server_error", "Something went wrong on our side.")
		return
	}
	defer rows.Close()
	comments := []commentRow{}
	for rows.Next() {
		c, err := scanComment(rows)
		if err != nil {
			logerr(err, "sharedCommentsList.scan")
			writeDRF(w, http.StatusInternalServerError, "server_error", "Something went wrong on our side.")
			return
		}
		comments = append(comments, c)
	}
	page, ferr := windowSlice(r, len(comments), 100)
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

// sharedCommentsCreate: auth first (non-GET public_methods), then gate+role.
func (a *App) sharedCommentsCreate(w http.ResponseWriter, r *http.Request) {
	link, ok := a.loadCommentLink(w, r)
	if !ok {
		return
	}
	sharedHeaders(w)
	a.createCommentFor(w, r, link.DesignID)
}

// sharedCommentDetail PATCH (author pin move) / DELETE (author removes own).
func (a *App) sharedCommentDetail(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPatch:
		a.sharedCommentPatch(w, r)
	case http.MethodDelete:
		a.sharedCommentDelete(w, r)
	}
}

func (a *App) loadOwnSharedComment(w http.ResponseWriter, r *http.Request) (*commentRow, *shareLinkRow, bool) {
	link, ok := a.loadCommentLink(w, r) // PATCH: role gate after auth (dispatch); DELETE: any valid link
	if !ok {
		return nil, nil, false
	}
	u := currentUser(r)
	if u == nil {
		writeDRF(w, http.StatusUnauthorized, "not_authenticated",
			"Authentication credentials were not provided.")
		return nil, nil, false
	}
	c, err := a.getCommentByID(hexUUID(routeParam(r, "uuid")))
	if err != nil || c == nil || c.DesignID != link.DesignID || c.AuthorID != u.ID {
		writeDRF(w, http.StatusNotFound, "not_found", "Not found.")
		return nil, nil, false
	}
	return c, link, true
}

// sharedCommentPatch ports SharedCommentDetailView.patch: CommentMoveMember
// rules — anchor required, replies not movable, author-scoped.
func (a *App) sharedCommentPatch(w http.ResponseWriter, r *http.Request) {
	c, link, ok := a.loadOwnSharedComment(w, r)
	if !ok {
		return
	}
	sharedHeaders(w)
	var body struct {
		PageID *string  `json:"page_id"`
		X      *float64 `json:"x"`
		Y      *float64 `json:"y"`
	}
	if err := jsonBody(r, &body); err != nil {
		writeDRF(w, http.StatusBadRequest, "parse_error", "Malformed request body.")
		return
	}
	var fields []FieldError
	anchorPresent := body.PageID != nil || body.X != nil || body.Y != nil
	if !anchorPresent {
		fields = append(fields, SimpleField("x",
			"Give the new position of the comment.", "invalid"))
	}
	if c.ParentID.Valid && c.ParentID.String != "" {
		fields = append(fields, SimpleField("x",
			"Only top-level comments can be moved.", "invalid"))
	}
	if (body.X == nil) != (body.Y == nil) {
		fields = append(fields, SimpleField("x", "x and y must be given together.", "invalid"))
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
	a.moveCommentAnchor(c, body, link)
	fresh, _ := a.getCommentByID(c.ID)
	writeJSON(w, http.StatusOK, fresh.envelope(a))
}

// moveCommentAnchor applies the pin to the comment and its replies
// (services.move: replies always follow the thread).
func (a *App) moveCommentAnchor(c *commentRow, body struct {
	PageID *string  `json:"page_id"`
	X      *float64 `json:"x"`
	Y      *float64 `json:"y"`
}, link *shareLinkRow) {
	updates := []string{}
	args := []any{}
	if body.PageID != nil {
		v := *body.PageID
		if v == "" {
			args = append(args, "")
		} else {
			args = append(args, v)
		}
		updates = append(updates, "page_id = ?")
	}
	if body.X != nil && body.Y != nil {
		updates = append(updates, "x = ?", "y = ?")
		args = append(args, *body.X, *body.Y)
	}
	if len(updates) == 0 {
		return
	}
	updates = append(updates, "updated_at = ?")
	args = append(args, formatDjangoDateTime(time.Now()))
	args = append(args, c.ID)
	if _, err := a.DB.Exec(
		"UPDATE sharing_comment SET "+strings.Join(updates, ", ")+" WHERE id = ?", args...); err != nil {
		logerr(err, "moveCommentAnchor")
		return
	}
	setters := make([]string, 0, len(updates))
	pargs := []any{}
	i := 0
	for ; i < len(updates)-1; i++ {
		setters = append(setters, updates[i])
		pargs = append(pargs, args[i])
	}
	setters = append(setters, "updated_at = ?")
	pargs = append(pargs, args[i], c.ID)
	_, _ = a.DB.Exec(
		"UPDATE sharing_comment SET "+strings.Join(setters, ", ")+" WHERE parent_id = ?", pargs...)
}

// sharedCommentDelete: author removes own comment through any valid link.
func (a *App) sharedCommentDelete(w http.ResponseWriter, r *http.Request) {
	c, _, ok := a.loadOwnSharedComment(w, r)
	if !ok {
		return
	}
	sharedHeaders(w)
	if _, err := a.DB.Exec(`DELETE FROM sharing_comment WHERE id = ?`, c.ID); err != nil {
		logerr(err, "sharedCommentDelete")
		writeDRF(w, http.StatusInternalServerError, "server_error", "Something went wrong on our side.")
		return
	}
	w.Header().Set("Content-Length", "0")
	w.WriteHeader(http.StatusNoContent)
}

// ---- fonts used by a shared document (apps/fonts/usage.py) ----

// fontsUsedIn ports uploaded_fonts_used_by: distinct family names the
// document's text uses (casefolded keys, first-seen order, cap 64), then
// the owner's uploaded families matching, ordered family,id.
func (a *App) fontsUsedIn(document string, ownerID string) []any {
	doc, err := parseJSONValue([]byte(document))
	if err != nil {
		return []any{}
	}
	wanted := fontNamesInDocument(doc)
	if len(wanted) == 0 {
		return []any{}
	}
	rows, err := a.DB.Query(
		`SELECT `+familyCols+` FROM fonts_fontfamily WHERE owner_id = ?
		 ORDER BY family, id`, ownerID)
	if err != nil {
		return []any{}
	}
	defer rows.Close()
	matched := map[string]bool{}
	for _, w := range wanted {
		matched[strings.ToLower(w)] = true
	}
	out := []any{}
	for rows.Next() {
		f, err := scanFamily(rows)
		if err != nil {
			return out
		}
		if matched[strings.ToLower(f.Family)] {
			variants, _ := a.variantsOf([]string{f.ID})
			out = append(out, f.envelope(variants[f.ID]))
		}
	}
	return out
}

// fontNamesInDocument walks the document iteratively (depth/node budgets
// like usage.py: object stacks + per-character styles).
func fontNamesInDocument(doc any) []string {
	names := map[string]string{} // casefold -> original
	order := []string{}
	add := func(stack any) {
		name := primaryFontFamily(stack)
		if name == "" || len([]rune(name)) > 120 || len(names) >= maxFontNamesInDoc {
			return
		}
		key := strings.ToLower(name)
		if _, ok := names[key]; !ok {
			names[key] = name
			order = append(order, key)
		}
	}
	obj, ok := doc.(map[string]any)
	if !ok {
		return nil
	}
	pages, _ := obj["pages"].([]any)
	type node struct {
		value any
		depth int
	}
	var stack []node
	for _, raw := range pages {
		page, _ := raw.(map[string]any)
		if page == nil {
			continue
		}
		objects, _ := page["objects"].([]any)
		for i := len(objects) - 1; i >= 0; i-- {
			stack = append(stack, node{objects[i], 0})
		}
	}
	visited := 0
	for len(stack) > 0 && visited < maxFontDocNodes {
		n := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		o, ok := n.value.(map[string]any)
		if !ok {
			continue
		}
		visited++
		add(o["fontFamily"])
		if styles, ok := o["styles"].([]any); ok {
			for _, entry := range styles {
				if style, ok := entry.(map[string]any); ok {
					if s, ok := style["style"].(map[string]any); ok {
						add(s["fontFamily"])
					}
				}
			}
		}
		if n.depth < maxFontDocDepth {
			if children, ok := o["objects"].([]any); ok {
				for i := len(children) - 1; i >= 0; i-- {
					stack = append(stack, node{children[i], n.depth + 1})
				}
			}
		}
	}
	out := make([]string, len(order))
	for i, k := range order {
		out[i] = names[k]
	}
	return out
}

// primaryFontFamily ports usage.primary_family: first family of a CSS
// stack, unquoted.
func primaryFontFamily(stack any) string {
	s, ok := stack.(string)
	if !ok || s == "" {
		return ""
	}
	first := s
	if idx := strings.IndexByte(s, ','); idx >= 0 {
		first = s[:idx]
	}
	return strings.Trim(first, " \t\"'")
}
