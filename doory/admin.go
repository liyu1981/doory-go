// The Django-admin-compatible site (D2, G27): route dispatch, staff gate,
// per-model permission checks, the descriptor registry, the changelist query
// engine and the shared form/messages plumbing.
//
// The oracle is django.contrib.admin + unfold at the pin. Markup is
// deliberately not Unfold's (D2 records the difference); paths, statuses,
// redirects, form field names, DB writes and file side effects are matched.
package doory

import (
	"context"
	"database/sql"
	"fmt"
	"html"
	"html/template"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	adminLoginURL      = "/admin/login/"
	adminIndexURL      = "/admin/"
	adminSiteTitle     = "Django site admin"
	adminSiteHeader    = "Django administration"
	adminIndexTitle    = "Site administration"
	adminPasswordDone  = "/admin/password_change/done/"
	adminListPerPage   = 100
	adminMaxPerPage    = 500
	adminMessagesName  = "messages"
	adminMessageMaxAge = 0 // Django's cookie storage sets no Max-Age
)

// adminMessage is one django.contrib.messages Message.
type adminMessage struct {
	Level int // 10 debug, 20 info, 25 success, 30 warning, 40 error
	Text  string
}

func adminSuccess(text string) adminMessage { return adminMessage{25, text} }
func adminError(text string) adminMessage   { return adminMessage{40, text} }
func adminWarning(text string) adminMessage { return adminMessage{30, text} }
func adminInfo(text string) adminMessage    { return adminMessage{20, text} }

func (m adminMessage) Tag() string {
	switch m.Level {
	case 25:
		return "success"
	case 30:
		return "warning"
	case 40:
		return "error"
	case 10:
		return "debug"
	default:
		return "info"
	}
}

// adminCell is one rendered table cell.
type adminCell struct {
	Text string
	Raw  template.HTML
}

func adminTextCell(text string) adminCell { return adminCell{Text: text} }
func adminHTMLCell(html template.HTML) adminCell {
	return adminCell{Raw: html}
}

// Render is the HTML-safe form the templates print.
func (c adminCell) Render() template.HTML {
	if c.Raw != "" {
		return c.Raw
	}
	return template.HTML(djangoEscape(c.Text))
}

// String returns the cell for attribute contexts.
func (c adminCell) String() string {
	if c.Raw != "" {
		return string(c.Raw)
	}
	return c.Text
}

// Plain is the escaped text (titles/attributes).
func (c adminCell) Plain() string {
	if c.Raw != "" {
		return html.UnescapeString(string(c.Raw))
	}
	return c.Text
}

// adminListRow is one changelist row.
type adminListRow struct {
	ID       string
	Cells    map[string]adminCell
	Editable map[string]string
}

// adminColumn is one list_display entry.
type adminColumn struct {
	Name  string // key in adminListRow.Cells
	Label string
	Sort  string // SQL expression when sortable (empty: not sortable)
	Link  bool   // cell links to the change page
}

// adminFilterChoice is one rendered filter link.
type adminFilterChoice struct {
	Value    string
	Label    string
	URL      string
	Selected bool
}

// adminFilter renders one list_filter sidebar block.
type adminFilter struct {
	Label   string
	Param   string // query parameter, e.g. "is_active__exact" or "trashed"
	Kind    string // bool, choice, fk, date, custom
	Choices []adminFilterChoice
	Column  string // SQL column for exact/fk/date filters
	// Options are the declared lookups for choice/fk filters (OptionsFn
	// resolves DB-backed ones at request time).
	Options   []adminFilterChoice
	OptionsFn func(a *App) []adminFilterChoice
	// Custom builds a clause (without WHERE); url.Values holds the request's
	// filter parameters. Empty clause means "no filter".
	Custom func(values url.Values) (string, []any)
}

// adminSearchField is one search_fields entry: a SQL expression taking one
// LIKE argument (already lower-cased and wrapped in %…% by the engine).
type adminSearchField struct {
	SQL string
}

// adminListQuery is the parsed changelist state.
type adminListQuery struct {
	Search string
	Page   int
	Order  int
	Values url.Values
	// Filters is the rendered sidebar state (filled by the engine).
	Filters []adminFilter
}

type adminFieldKind int

const (
	adminText adminFieldKind = iota
	adminTextarea
	adminEmail
	adminBool
	adminSelect
	adminNumber
	adminDecimal
	adminDateTime
	adminPassword
	adminFile
	adminJSON
	adminReadonly
	adminHidden
	adminMultiSelect
	adminCheckboxMulti
)

// adminField is one form field (or read-only display) in a fieldset.
type adminField struct {
	Name     string
	Label    string
	Kind     adminFieldKind
	Choices  []adminFilterChoice
	Required bool
	Help     string
	Wide     bool
	// Display renders a read-only field. The object's Display map wins when
	// present; this is the fallback.
	Display func(obj *adminObject) template.HTML
}

// adminFieldset groups fields as in ModelAdmin.fieldsets.
type adminFieldset struct {
	Title    string
	Fields   []adminField
	Collapse bool
}

// adminInlineColumn is one column of an inline formset.
type adminInlineColumn struct {
	Name    string
	Label   string
	Kind    adminFieldKind
	Choices []adminFilterChoice
	Wide    bool
}

// adminInlineRow is one existing (or extra) inline row.
type adminInlineRow struct {
	Index   int
	ID      string
	Values  map[string]string
	Display map[string]template.HTML
	Delete  bool
}

// adminInline is a tabular inline (FontVariant, ProviderModel).
type adminInline struct {
	Prefix    string
	Title     string
	Columns   []adminInlineColumn
	Rows      []adminInlineRow
	ExtraRows []adminInlineRow
	Extra     int
	CanDelete bool
	// HideAdd hides the add-another row (Django's extra=0 + no add).
	HideAdd bool
}

// adminObject is a model instance rendered by the change view.
type adminObject struct {
	ID      string
	Repr    string
	Values  map[string]string
	Display map[string]template.HTML
	Raw     map[string]any
	Inlines []adminInline
}

// adminAction is one bulk changelist action.
type adminAction struct {
	Name  string
	Label string
	Run   func(a *App, actor *UserRow, ids []string) ([]adminMessage, error)
}

// adminDetailAction is one object-level action (always a link/GET upstream
// except where the custom view accepts POST itself).
type adminDetailAction struct {
	Name  string // path segment
	Label string
	// Perm is the permission verb checked in addition to change: "change" or
	// "add"; empty means view.
	Perm string
	Run  func(a *App, w http.ResponseWriter, r *http.Request, actor *UserRow, obj *adminObject)
}

// adminExtraRoute is a model-scoped custom page.
type adminExtraRoute struct {
	Suffix  string // appended after /admin/app/model/<id>/
	Methods []string
	Perm    string // verb on this model; empty means change
	Run     func(a *App, w http.ResponseWriter, r *http.Request, actor *UserRow, id string)
	NoID    bool // suffix is registered without an object id
}

// adminModel is one registered model page.
type adminModel struct {
	AppLabel string
	Model    string // lowercase model name
	Verbose  string // singular, lower-case, used in permission/messages
	Plural   string // plural label, used in headings
	Name     string // verbose_name used in "Can add <name>"
	Table    string
	IntPK    bool

	// list page
	Columns       []adminColumn
	LinkName      string // which column links (normally also Link=true)
	SearchFields  []adminSearchField
	Filters       []adminFilter
	DateHierarchy string
	Ordering      []string // default ORDER BY expressions
	PerPage       int
	ListEditable  []string
	// SelectSQL and FromSQL drive the generic list query. FromSQL may join.
	SelectSQL string
	FromSQL   string
	ScanList  func(a *App, rows *sql.Rows) (adminListRow, error)
	// BaseWhere is always ANDed (e.g. provider owner IS NULL).
	BaseWhere string
	BaseArgs  []any

	// form pages
	Fieldsets    []adminFieldset
	AddFieldsets []adminFieldset
	// FormFields lists every editable field, in addition to Fieldsets
	// (inlines use fields that are not in a fieldset).
	FormFields []adminField
	Inlines    []adminInline
	// Choices resolves DB-backed select options per field name.
	Choices map[string]func(a *App) []adminFilterChoice
	// AutocompleteFields maps a form field name to the model key its FK
	// points at (Django's autocomplete_fields); the autocomplete view
	// resolves the TARGET admin from it.
	AutocompleteFields map[string]string

	Actions       []adminAction
	DetailActions []adminDetailAction
	ExtraRoutes   []adminExtraRoute

	// capability gates
	AllowAdd    bool
	AllowChange bool
	AllowDelete bool
	// ReadOnlyModel means the change page exists but has no editable fields.
	ReadOnlyModel bool
	// NoChangePermission makes has_change_permission always false (Django
	// overrides in DesignVersion/Comment/UserProvider/AICall) while the view
	// permission still lets the page render.
	NoChangePermission bool
	// PlainAdmin marks a model registered with django.contrib.admin.ModelAdmin
	// rather than unfold's: it lacks the SAMEORIGIN decorator (auth.Group).
	PlainAdmin bool

	// persistence hooks
	Get    func(a *App, id string) (*adminObject, error)
	New    func(a *App) *adminObject
	Add    func(a *App, actor *UserRow, form *adminForm) (id string, errs adminErrors, err error)
	Change func(a *App, actor *UserRow, id string, form *adminForm) (errs adminErrors, err error)
	Delete func(a *App, actor *UserRow, id string) error
	Repr   func(a *App, id string) string
}

// adminErrors maps field name -> messages (Django's form.errors).
type adminErrors map[string][]string

func (e adminErrors) add(field, msg string) {
	e[field] = append(e[field], msg)
}

func (e adminErrors) any() bool { return len(e) > 0 }

// adminSite is the registry + template set.
type adminSite struct {
	app       *App
	models    []*adminModel
	byKey     map[string]*adminModel
	byPath    map[string]*adminModel
	templates map[string]*template.Template
}

func newAdminSite(app *App) *adminSite {
	s := &adminSite{
		app:       app,
		byKey:     map[string]*adminModel{},
		byPath:    map[string]*adminModel{},
		templates: loadAdminTemplates(),
	}
	for _, m := range adminRegistry() {
		if m.PerPage == 0 {
			m.PerPage = adminListPerPage
		}
		if m.Plural == "" {
			m.Plural = m.Verbose + "s"
		}
		if m.Name == "" {
			m.Name = m.Verbose
		}
		s.models = append(s.models, m)
		s.byKey[m.AppLabel+"."+m.Model] = m
		s.byPath["/admin/"+m.AppLabel+"/"+m.Model] = m
	}
	return s
}

func (a *App) admin() *adminSite {
	a.adminOnce.Do(func() { a.adminSite = newAdminSite(a) })
	return a.adminSite
}

// ---- permissions ----

// adminHasPerm ports User.has_perm: superusers pass, otherwise the direct
// and group permissions of the auth tables are checked.
func (a *App) adminHasPerm(u *UserRow, perm string) bool {
	if u == nil || !u.IsActive {
		return false
	}
	if u.IsSuperuser {
		return true
	}
	var count int
	err := a.DB.QueryRow(
		`SELECT COUNT(*) FROM (
		   SELECT p.id FROM auth_permission p
		   JOIN django_content_type ct ON ct.id = p.content_type_id
		   WHERE ct.app_label || '.' || p.codename = ?
		     AND p.id IN (
		       SELECT permission_id FROM accounts_user_user_permissions WHERE user_id = ?
		       UNION
		       SELECT gp.permission_id FROM auth_group_permissions gp
		       JOIN accounts_user_groups ug ON ug.group_id = gp.group_id
		       WHERE ug.user_id = ?
		     )
		 ) checked`, perm, u.ID, u.ID).Scan(&count)
	if err != nil {
		logerr(err, "adminHasPerm")
		return false
	}
	return count > 0
}

func (a *App) adminHasModulePerm(u *UserRow, appLabel string) bool {
	if u == nil || !u.IsActive {
		return false
	}
	if u.IsSuperuser {
		return true
	}
	var count int
	err := a.DB.QueryRow(
		`SELECT COUNT(*) FROM auth_permission p
		 JOIN django_content_type ct ON ct.id = p.content_type_id
		 WHERE ct.app_label = ?
		   AND p.id IN (
		     SELECT permission_id FROM accounts_user_user_permissions WHERE user_id = ?
		     UNION
		     SELECT gp.permission_id FROM auth_group_permissions gp
		     JOIN accounts_user_groups ug ON ug.group_id = gp.group_id
		     WHERE ug.user_id = ?
		   )`, appLabel, u.ID, u.ID).Scan(&count)
	if err != nil {
		logerr(err, "adminHasModulePerm")
		return false
	}
	return count > 0
}

func (m *adminModel) perm(verb string) string {
	return m.AppLabel + "." + verb + "_" + m.Model
}

func (a *App) adminCanView(u *UserRow, m *adminModel) bool {
	// Django's has_view_or_change_permission; add-only staff can also reach
	// the changelist and the add page.
	return a.adminHasPerm(u, m.perm("view")) || a.adminCanChange(u, m) || a.adminCanAdd(u, m)
}

func (a *App) adminCanAdd(u *UserRow, m *adminModel) bool {
	return m.AllowAdd && a.adminHasPerm(u, m.perm("add"))
}

func (a *App) adminCanChange(u *UserRow, m *adminModel) bool {
	if m.NoChangePermission {
		return false
	}
	return m.AllowChange && a.adminHasPerm(u, m.perm("change"))
}

func (a *App) adminCanDelete(u *UserRow, m *adminModel) bool {
	return m.AllowDelete && a.adminHasPerm(u, m.perm("delete"))
}

func (a *App) adminPasswordPerm(u *UserRow) bool {
	return a.adminHasPerm(u, "auth.change_user") || a.adminHasPerm(u, "accounts.change_user")
}

// adminIsStaff ports AdminSite.has_permission.
func adminIsStaff(u *UserRow) bool { return u != nil && u.IsActive && u.IsStaff }

// adminURLID renders the object id the way Django's URL reverses it: UUIDs
// dashed, integer PKs plain.
func adminURLID(m *adminModel, id string) string {
	if m.IntPK {
		return id
	}
	return dashedUUID(id)
}

// ---- responses ----

func adminNeverCache(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", neverCache)
	// add_never_cache_headers stamps Expires with the wall clock (D10: the
	// comparator ignores it; kept for header-shape parity).
	w.Header().Set("Expires", time.Now().UTC().Format(http.TimeFormat))
	patchVary(w, "Cookie")
}

func (s *adminSite) render(w http.ResponseWriter, r *http.Request, status int, name string, page any) {
	tmpl := s.templates[name]
	if tmpl == nil {
		logerr(fmt.Errorf("missing admin template %q", name), "admin.render")
		http.Error(w, "admin template missing", http.StatusInternalServerError)
		return
	}
	var buf strings.Builder
	if err := tmpl.Execute(&buf, page); err != nil {
		logerr(err, "admin.render."+name)
		http.Error(w, "admin template error", http.StatusInternalServerError)
		return
	}
	// process_response side effects: get_token() re-sends the CSRF cookie,
	// and an iterated message storage deletes the flash cookie with the page
	// that showed it (error pages do neither).
	if st := adminRequestStateOf(r); st != nil && s.app != nil {
		if st.csrfNeeded {
			s.app.setCSRFCookie(w, st.csrfSecret)
		}
		if st.messagesUsed {
			s.app.clearAdminMessages(w)
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(buf.Len()))
	w.WriteHeader(status)
	_, _ = w.Write([]byte(buf.String()))
}

func (s *adminSite) render403(w http.ResponseWriter, r *http.Request, u *UserRow, message string) {
	// PermissionDenied is raised inside the view: the never_cache and
	// SAMEORIGIN decorators never see a response, so the error page carries
	// only the session middleware's Vary (probe 0018). The dispatch set the
	// never_cache headers on the way in; strip them here.
	adminDropCacheHeaders(w)
	s.render(w, r, http.StatusForbidden, "error", &adminErrorPage{
		adminBase: adminBase{Title: "Permission denied", User: u, HasPermission: true},
		Message:   message,
	})
}

func (s *adminSite) render404(w http.ResponseWriter, r *http.Request, u *UserRow) {
	adminDropCacheHeaders(w)
	s.render(w, r, http.StatusNotFound, "error", &adminErrorPage{
		adminBase: adminBase{Title: "Not found", User: u},
		Message:   "The requested admin page does not exist.",
	})
}

// adminDropCacheHeaders undoes the dispatch's never_cache headers for an
// exception page, keeping the session middleware's Vary: Cookie.
func adminDropCacheHeaders(w http.ResponseWriter) {
	w.Header().Del("Cache-Control")
	w.Header().Del("Expires")
	patchVary(w, "Cookie")
}

// adminBase is embedded by every page model.
type adminBase struct {
	Title         string
	Subtitle      string
	SiteTitle     string
	SiteHeader    string
	IndexTitle    string
	User          *UserRow
	HasPermission bool
	Breadcrumbs   []adminCrumb
	Messages      []adminMessage
	CSRFToken     string
	// UserLabel is the header's username (User.get_username -> email).
	UserLabel string
}

type adminCrumb struct {
	Label string
	URL   string
}

type adminErrorPage struct {
	adminBase
	Message string
}

// adminRedirect renders HttpResponseRedirect (302): Django 6 sends Location
// with an empty body (Content-Length: 0) and the HTML content type, unlike
// net/http's <a href>…</a> page.
func adminRedirect(w http.ResponseWriter, r *http.Request, target string) {
	adminRedirectStatus(w, target, http.StatusFound)
}

// adminRedirectStatus is the shared concrete redirect: bodyless, Location +
// Django's text/html content type (also used by APPEND_SLASH's 301).
func adminRedirectStatus(w http.ResponseWriter, target string, status int) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Location", target)
	w.WriteHeader(status)
}

// ---- request state ----

// adminRequestState carries the per-request bits Django keeps in
// request.META: the CSRF token get_token() prepared for the response and
// whether the message storage was iterated (the cookie is deleted then).
type adminRequestState struct {
	csrfNeeded   bool
	csrfSecret   string
	messagesUsed bool
}

type adminRequestStateKey struct{}

func adminRequestStateOf(r *http.Request) *adminRequestState {
	st, _ := r.Context().Value(adminRequestStateKey{}).(*adminRequestState)
	return st
}

// ---- dispatch ----

// isAdminPath reports the paths the admin site owns. AdminSite.catch_all_view
// is mounted at /admin/<path>, and /admin itself APPEND_SLASHes to /admin/.
func isAdminPath(path string) bool {
	return path == "/admin" || strings.HasPrefix(path, "/admin/")
}

// adminCacheableView reports the site views AdminSite.get_urls mounts with
// wrap(..., cacheable=True): they skip the never_cache headers but still
// vary on the session cookie (probe 0018).
func adminCacheableView(path string) bool {
	switch path {
	case "jsi18n/", "password_change/", "password_change/done/":
		return true
	}
	return false
}

// adminDispatch is the site's front controller. It replaces router dispatch
// for /admin* (the paths are not in the API route ledger), and owns session
// consultation, CSRF, the staff gate and the staff cache headers.
func (a *App) adminDispatch(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/admin" {
		target := "/admin/"
		if r.URL.RawQuery != "" {
			target += "?" + r.URL.RawQuery
		}
		adminRedirectStatus(w, target, http.StatusMovedPermanently)
		return
	}

	// Session middleware: read the cookie on every admin request.
	r = r.WithContext(context.WithValue(r.Context(), adminRequestStateKey{}, &adminRequestState{}))
	u, dead := a.sessionUser(r)
	if dead {
		clearSessionCookie(w, a.Settings.SessionCookieSecure())
	}

	// CsrfViewMiddleware: unsafe requests need the cookie + header/form token.
	// A rejection happens before the admin view, so it carries neither
	// never_cache nor Vary: Cookie (probed 0018).
	if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodOptions {
		if err := verifyAdminCSRF(r); err != nil {
			a.admin().render(w, r, http.StatusForbidden, "error", &adminErrorPage{
				adminBase: adminBase{Title: "Forbidden", User: u, HasPermission: adminIsStaff(u)},
				Message:   adminCSRFMessage(err),
			})
			return
		}
	}

	// Everything below is AdminSite.admin_view: @never_cache unless the site
	// view is mounted cacheable=True (jsi18n, password_change*).
	path := strings.TrimPrefix(r.URL.Path, "/admin/")
	if adminCacheableView(path) {
		// SessionMiddleware still marks the response cookie-dependent.
		patchVary(w, "Cookie")
	} else {
		adminNeverCache(w)
	}
	// Login/logout/password pages are site-level.
	if page, ok := a.adminRoute(path, r.Method); ok {
		if page.public {
			page.run(w, r, u)
			return
		}
		if !adminIsStaff(u) {
			// AdminSite.admin_view: logout redirects to the index when the
			// caller is not staff; everything else goes to login?next=…
			if page.name == "logout" {
				adminRedirect(w, r, adminIndexURL)
				return
			}
			adminRedirect(w, r, adminLoginRedirect(r))
			return
		}
		page.run(w, r, u)
		return
	}

	// Model pages.
	if m, rest, ok := a.adminModelRoute(r.URL.Path); ok {
		if !adminIsStaff(u) {
			adminRedirect(w, r, adminLoginRedirect(r))
			return
		}
		if !a.adminHasModulePerm(u, m.AppLabel) || !a.adminCanView(u, m) {
			a.admin().render403(w, r, u, "You do not have permission to view this object.")
			return
		}
		a.adminModelDispatch(w, r, u, m, rest)
		return
	}

	// App index /search/ etc. are handled above; remaining paths are 404.
	if strings.Count(r.URL.Path, "/") == 3 && strings.HasSuffix(r.URL.Path, "/") {
		app := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/admin/"), "/")
		if a.adminAppExists(app) {
			if !adminIsStaff(u) {
				adminRedirect(w, r, adminLoginRedirect(r))
				return
			}
			a.adminAppIndex(w, r, u, app)
			return
		}
	}

	// Unmatched admin paths still pass through AdminSite.catch_all_view, which
	// is wrapped by admin_view: the staff gate runs before APPEND_SLASH, so an
	// anonymous /admin/<path> is a login redirect, not a 301 (probe 0018).
	if !adminIsStaff(u) {
		adminRedirect(w, r, adminLoginRedirect(r))
		return
	}

	// CommonMiddleware APPEND_SLASH: an admin page without its trailing slash
	// gets a bodyless 301 when the slashed form exists.
	if (r.Method == http.MethodGet || r.Method == http.MethodHead) && !strings.HasSuffix(r.URL.Path, "/") {
		if a.adminPathExists(r.URL.Path+"/", r.Method) {
			target := r.URL.Path + "/"
			if r.URL.RawQuery != "" {
				target += "?" + r.URL.RawQuery
			}
			adminRedirectStatus(w, target, http.StatusMovedPermanently)
			return
		}
	}
	a.admin().render404(w, r, u)
}

// adminLoginRedirect ports redirect_to_login(request.get_full_path(),
// admin:login): the next parameter is query-encoded with "/" left safe.
func adminLoginRedirect(r *http.Request) string {
	next := r.URL.Path
	if r.URL.RawQuery != "" {
		next += "?" + r.URL.RawQuery
	}
	q := url.Values{}
	q.Set("next", next)
	return adminLoginURL + "?" + strings.ReplaceAll(q.Encode(), "%2F", "/")
}

// adminAppExists reports whether a registered model belongs to the app.
func (a *App) adminAppExists(app string) bool {
	for _, m := range a.admin().models {
		if m.AppLabel == app {
			return true
		}
	}
	return false
}

func (a *App) adminPathExists(path, method string) bool {
	p := strings.TrimPrefix(path, "/admin/")
	if _, ok := a.adminRoute(p, method); ok {
		return true
	}
	if _, _, ok := a.adminModelRoute(path); ok {
		return true
	}
	if strings.Count(path, "/") == 3 && strings.HasSuffix(path, "/") {
		return a.adminAppExists(strings.TrimSuffix(strings.TrimPrefix(path, "/admin/"), "/"))
	}
	return false
}

// adminModelRoute matches /admin/<app>/<model>/<rest…>. Like Django's
// ModelAdmin urls, every model pattern ends in a slash; a missing slash is
// handled by APPEND_SLASH.
func (a *App) adminModelRoute(path string) (*adminModel, string, bool) {
	if !strings.HasSuffix(path, "/") {
		return nil, "", false
	}
	parts := strings.Split(strings.TrimPrefix(path, "/admin/"), "/")
	if len(parts) < 3 {
		return nil, "", false
	}
	m := a.admin().byPath["/admin/"+parts[0]+"/"+parts[1]]
	if m == nil {
		return nil, "", false
	}
	return m, strings.TrimSuffix(strings.Join(parts[2:], "/"), "/"), true
}

// adminModelDispatch handles one model page's remainder (no trailing slash).
func (a *App) adminModelDispatch(w http.ResponseWriter, r *http.Request, u *UserRow, m *adminModel, rest string) {
	s := a.admin()
	// ProviderAdmin.get_urls registers env/<id>/test/ before the model pages;
	// the environment providers are not rows, so there is no object id.
	if m.AppLabel == "ai" && m.Model == "provider" && strings.HasPrefix(rest, "env/") {
		segs := strings.Split(rest, "/")
		if len(segs) == 3 && segs[2] == "test" {
			a.adminAIEnvTest(w, r, u, segs[1])
			return
		}
		s.render404(w, r, u)
		return
	}
	switch {
	case rest == "":
		a.adminChangelist(w, r, u, m)
	case rest == "add":
		a.adminAdd(w, r, u, m)
	case !strings.Contains(rest, "/"):
		// django.views.generic.base.RedirectView: always 302 to the change
		// page, echoing the raw object id (even for a nonexistent id, probe
		// 0018), so no lookup happens here.
		adminRedirect(w, r, "/admin/"+m.AppLabel+"/"+m.Model+"/"+rest+"/change/")
	default:
		segs := strings.Split(rest, "/")
		id := adminQueryID(m, segs[0])
		switch segs[1] {
		case "change":
			a.adminChange(w, r, u, m, id)
		case "delete":
			a.adminDelete(w, r, u, m, id)
		case "history":
			a.adminHistory(w, r, u, m, id)
		case "password":
			if m.AppLabel == "accounts" && m.Model == "user" {
				a.adminUserPassword(w, r, u, m, id)
				return
			}
			s.render404(w, r, u)
		case "upload":
			if m.AppLabel == "elements" && m.Model == "elementpack" {
				a.adminPackUpload(w, r, u, m, id)
				return
			}
			s.render404(w, r, u)
		default:
			// Unfold's actions_detail: <object_id>/<action path>/, where the
			// path is the method name (upload_svgs sets url_path=upload-svgs).
			for _, action := range m.DetailActions {
				if action.Name != segs[1] {
					continue
				}
				verb := action.Perm
				if verb == "" {
					verb = "change"
				}
				if !a.adminHasPerm(u, m.perm(verb)) {
					s.render403(w, r, u, "You do not have permission to perform this action.")
					return
				}
				obj, err := m.Get(a, id)
				if err != nil || obj == nil {
					s.render404(w, r, u)
					return
				}
				action.Run(a, w, r, u, obj)
				return
			}
			for _, extra := range m.ExtraRoutes {
				if extra.NoID {
					continue
				}
				if extra.Suffix == segs[1] {
					a.adminExtra(w, r, u, m, id, extra)
					return
				}
			}
			s.render404(w, r, u)
		}
	}
}

func (a *App) adminExtra(w http.ResponseWriter, r *http.Request, u *UserRow, m *adminModel, id string, extra adminExtraRoute) {
	allowed := false
	for _, method := range extra.Methods {
		if strings.EqualFold(method, r.Method) {
			allowed = true
		}
	}
	if !allowed {
		w.Header().Set("Allow", strings.Join(extra.Methods, ", "))
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	verb := extra.Perm
	if verb == "" {
		verb = "change"
	}
	if !a.adminHasPerm(u, m.perm(verb)) {
		a.admin().render403(w, r, u, "You do not have permission to perform this action.")
		return
	}
	extra.Run(a, w, r, u, id)
}

// adminCSRFMessage turns the header verifier's errors into Django's page text.
func adminCSRFMessage(err error) string {
	text := err.Error()
	switch err {
	case errCSRFCookieNotSet:
		return "CSRF cookie not set."
	case errCSRFTokenMissing:
		return "CSRF token missing."
	default:
		return "CSRF verification failed. Request aborted. " + text
	}
}

// ---- messages ----

// readAdminMessages consumes the messages cookie (CookieStorage._get).
func (a *App) readAdminMessages(r *http.Request) []adminMessage {
	c, err := r.Cookie(adminMessagesName)
	if err != nil || c.Value == "" {
		return nil
	}
	msgs := decodeAdminMessageCookie(c.Value, a.Settings.SecretKey)
	if len(msgs) > 0 {
		// {% if messages %} only asks for len(); the delete-cookie side effect
		// needs actual iteration, i.e. a message that is rendered.
		if st := adminRequestStateOf(r); st != nil {
			st.messagesUsed = true
		}
	}
	return msgs
}

// setAdminMessages writes (or deletes) the messages cookie.
func (a *App) setAdminMessages(w http.ResponseWriter, msgs []adminMessage) {
	if len(msgs) == 0 {
		http.SetCookie(w, &http.Cookie{
			Name:     adminMessagesName,
			Value:    "",
			Path:     "/",
			MaxAge:   -1,
			Expires:  time.Unix(0, 0).UTC(),
			HttpOnly: true,
			SameSite: http.SameSiteLaxMode,
			Secure:   a.Settings.SessionCookieSecure(),
		})
		return
	}
	value := encodeAdminMessageCookie(msgs, a.Settings.SecretKey)
	http.SetCookie(w, &http.Cookie{
		Name:     adminMessagesName,
		Value:    value,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   a.Settings.SessionCookieSecure(),
	})
}

// flashAdminMessages stores messages for the next request (Django's
// messages.add_message + response middleware).
func (a *App) flashAdminMessages(w http.ResponseWriter, r *http.Request, msgs ...adminMessage) {
	existing := a.readAdminMessages(r)
	a.setAdminMessages(w, append(existing, msgs...))
}

// ---- pagination ----

func adminParsePage(r *http.Request) int {
	raw := r.URL.Query().Get("p")
	if raw == "" {
		return 1
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 {
		return 1
	}
	return n
}

func adminParseOrder(r *http.Request) int {
	raw := r.URL.Query().Get("o")
	if raw == "" {
		return 0
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0
	}
	return n
}
