// Site-level admin views: index, app index, search, login/logout, password
// changes, autocomplete, the i18n catalog and the view-on-site shortcut.
package doory

import (
	"database/sql"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// adminPageRoute is one site-level page.
type adminPageRoute struct {
	name   string
	public bool
	run    func(w http.ResponseWriter, r *http.Request, u *UserRow)
}

func (a *App) adminRoute(path, method string) (adminPageRoute, bool) {
	switch path {
	case "":
		if method != http.MethodGet && method != http.MethodHead {
			return adminPageRoute{}, false
		}
		return adminPageRoute{name: "index", run: a.adminIndex}, true
	case "search/":
		return adminPageRoute{name: "search", run: a.adminSearch}, true
	case "login/":
		return adminPageRoute{name: "login", public: true, run: a.adminLogin}, true
	case "logout/":
		return adminPageRoute{name: "logout", public: true, run: a.adminLogout}, true
	case "password_change/":
		return adminPageRoute{name: "password_change", run: a.adminPasswordChange}, true
	case "password_change/done/":
		return adminPageRoute{name: "password_change_done", run: a.adminPasswordChangeDone}, true
	case "autocomplete/":
		return adminPageRoute{name: "autocomplete", run: a.adminAutocomplete}, true
	case "jsi18n/":
		return adminPageRoute{name: "jsi18n", public: true, run: a.adminJsi18n}, true
	}
	if strings.HasPrefix(path, "r/") {
		return adminPageRoute{name: "view_on_site", run: a.adminViewOnSite}, true
	}
	return adminPageRoute{}, false
}

// verifyAdminCSRF accepts the header token or the form field, like
// CsrfViewMiddleware._check_token.
func verifyAdminCSRF(r *http.Request) error {
	cookie, err := r.Cookie(csrfCookieName)
	if err != nil || cookie.Value == "" {
		return errCSRFCookieNotSet
	}
	token := r.Header.Get("X-CSRFToken")
	if token == "" {
		token = r.Header.Get("X-CSRFTOKEN")
	}
	if token == "" {
		_ = r.ParseMultipartForm(32 << 20)
		token = r.FormValue("csrfmiddlewaretoken")
	}
	if token == "" {
		return errCSRFTokenMissing
	}
	switch len(token) {
	case csrfSecretLen:
		if constantTimeEqualString(token, cookie.Value) {
			return nil
		}
		return errCSRFTokenValue
	case csrfSecretLen * 2:
		if constantTimeEqualString(unmaskCSRF(token), cookie.Value) {
			return nil
		}
		return errCSRFTokenValue
	default:
		return errCSRFTokenLength
	}
}

func constantTimeEqualString(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	var v byte
	for i := 0; i < len(a); i++ {
		v |= a[i] ^ b[i]
	}
	return v == 0
}

func (a *App) adminBasePage(r *http.Request, u *UserRow) adminBase {
	b := adminBase{
		SiteTitle:     adminSiteTitle,
		SiteHeader:    adminSiteHeader,
		IndexTitle:    adminIndexTitle,
		User:          u,
		HasPermission: adminIsStaff(u),
		Messages:      a.readAdminMessages(r),
		CSRFToken:     a.ensureAdminCSRFValue(r),
	}
	if u != nil {
		b.UserLabel = u.Email
	}
	return b
}

// ensureAdminCSRFValue ports get_token: reuse a valid cookie secret (the
// response renews its expiry) or mint one; either way process_response sends
// the cookie for a page that rendered a form token.
func (a *App) ensureAdminCSRFValue(r *http.Request) string {
	st := adminRequestStateOf(r)
	secret := ""
	if c, err := r.Cookie(csrfCookieName); err == nil && validCSRFSecret(c.Value) {
		secret = c.Value
	}
	if st != nil {
		st.csrfNeeded = true
		if secret == "" {
			if st.csrfSecret == "" {
				st.csrfSecret = newCSRFSecret()
			}
			return st.csrfSecret
		}
		st.csrfSecret = secret
	}
	return secret
}

// clearAdminMessages deletes the flash cookie once it has been rendered.
func (a *App) clearAdminMessages(w http.ResponseWriter) {
	a.setAdminMessages(w, nil)
}

// ---- index / app index ----

var adminAppLabels = map[string]string{
	"accounts":  "Accounts",
	"ai":        "Ai",
	"apitokens": "Apitokens",
	"assets":    "Assets",
	"auth":      "Authentication and Authorization",
	"brand":     "Brand",
	"designs":   "Designs",
	"elements":  "Elements",
	"fonts":     "Fonts",
	"gallery":   "Gallery",
	"presets":   "Presets",
	"sharing":   "Sharing",
}

func adminAppLabel(app string) string {
	if label, ok := adminAppLabels[app]; ok {
		return label
	}
	if app == "" {
		return app
	}
	return strings.ToUpper(app[:1]) + app[1:]
}

func (a *App) adminVisibleModels(u *UserRow, app string) []adminLink {
	var links []adminLink
	for _, m := range a.admin().models {
		if app != "" && m.AppLabel != app {
			continue
		}
		if !a.adminCanView(u, m) && !a.adminCanAdd(u, m) {
			continue
		}
		links = append(links, adminLink{
			Label: m.Plural,
			URL:   "/admin/" + m.AppLabel + "/" + m.Model + "/",
		})
	}
	return links
}

func (a *App) adminIndexApps(u *UserRow) []adminAppBlock {
	seen := map[string]int{}
	var blocks []adminAppBlock
	for _, m := range a.admin().models {
		if !a.adminCanView(u, m) && !a.adminCanAdd(u, m) {
			continue
		}
		if idx, ok := seen[m.AppLabel]; ok {
			blocks[idx].Models = append(blocks[idx].Models, adminLink{
				Label: m.Plural,
				URL:   "/admin/" + m.AppLabel + "/" + m.Model + "/",
			})
			continue
		}
		seen[m.AppLabel] = len(blocks)
		blocks = append(blocks, adminAppBlock{
			Name:  m.AppLabel,
			Label: adminAppLabel(m.AppLabel),
			Models: []adminLink{{
				Label: m.Plural,
				URL:   "/admin/" + m.AppLabel + "/" + m.Model + "/",
			}},
		})
	}
	return blocks
}

func (a *App) adminIndex(w http.ResponseWriter, r *http.Request, u *UserRow) {
	if !adminIsStaff(u) {
		adminRedirect(w, r, adminLoginRedirect(r))
		return
	}
	base := a.adminBasePage(r, u)
	base.Title = adminIndexTitle
	a.admin().render(w, r, http.StatusOK, "index", &adminIndexPage{
		adminBase: base,
		Apps:      a.adminIndexApps(u),
	})
}

func (a *App) adminAppIndex(w http.ResponseWriter, r *http.Request, u *UserRow, app string) {
	links := a.adminVisibleModels(u, app)
	if len(links) == 0 {
		a.admin().render404(w, r, u)
		return
	}
	base := a.adminBasePage(r, u)
	base.Title = adminAppLabel(app)
	base.Breadcrumbs = []adminCrumb{{Label: adminAppLabel(app)}}
	a.admin().render(w, r, http.StatusOK, "app_index", &adminIndexPage{
		adminBase: base,
		Apps: []adminAppBlock{{
			Name:   app,
			Label:  adminAppLabel(app),
			Models: links,
		}},
	})
}

// ---- login / logout ----

const adminLoginError = "Please enter the correct email and password for a staff account. Note that both fields may be case-sensitive."

func adminNextURL(r *http.Request, fallback string) string {
	next := r.FormValue("next")
	if next == "" {
		next = r.URL.Query().Get("next")
	}
	if next == "" || !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") {
		return fallback
	}
	return next
}

func (a *App) adminLogin(w http.ResponseWriter, r *http.Request, u *UserRow) {
	// AdminSite.login: redirect an authenticated staff member to the target.
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		if adminIsStaff(u) {
			adminRedirect(w, r, adminNextURL(r, adminIndexURL))
			return
		}
	} else if r.Method == http.MethodPost {
		_ = r.ParseMultipartForm(32 << 20)
		username := strings.TrimSpace(r.FormValue("username"))
		password := r.FormValue("password")
		next := adminNextURL(r, adminIndexURL)
		user, _ := getUserByEmail(a.DB, strings.ToLower(username))
		if user != nil && user.IsActive && verifyPassword(password, user.Password) && user.IsStaff {
			a.finishLogin(w, r, user)
			adminRedirect(w, r, next)
			return
		}
		base := a.adminBasePage(r, u)
		base.Title = "Log in"
		a.admin().render(w, r, http.StatusOK, "login", &adminLoginPage{
			adminBase: base,
			Next:      next,
			Error:     adminLoginError,
			Username:  username,
			AppPath:   r.URL.RequestURI(),
		})
		return
	} else {
		w.Header().Set("Allow", "GET, POST, HEAD, OPTIONS")
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	base := a.adminBasePage(r, u)
	base.Title = "Log in"
	a.admin().render(w, r, http.StatusOK, "login", &adminLoginPage{
		adminBase: base,
		Next:      adminNextURL(r, adminIndexURL),
		AppPath:   r.URL.RequestURI(),
	})
}

func (a *App) adminLogout(w http.ResponseWriter, r *http.Request, u *UserRow) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST, OPTIONS")
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if cookie, err := r.Cookie(sessionCookieName); err == nil && cookie.Value != "" {
		_ = deleteSession(a.DB, cookie.Value)
	}
	clearSessionCookie(w, a.Settings.SessionCookieSecure())
	base := a.adminBasePage(r, nil)
	base.Title = "Logged out"
	a.admin().render(w, r, http.StatusOK, "logged_out", base)
}

// ---- password changes ----

func (a *App) adminPasswordChange(w http.ResponseWriter, r *http.Request, u *UserRow) {
	if !adminIsStaff(u) {
		adminRedirect(w, r, adminLoginRedirect(r))
		return
	}
	base := a.adminBasePage(r, u)
	base.Title = "Change password"
	page := &adminPasswordChangePage{adminBase: base, Self: true, Errors: adminErrors{}}
	if r.Method == http.MethodPost {
		_ = r.ParseMultipartForm(32 << 20)
		old := r.FormValue("old_password")
		p1 := r.FormValue("new_password1")
		p2 := r.FormValue("new_password2")
		if old == "" {
			page.Errors.add("old_password", "This field is required.")
		} else if !verifyPassword(old, u.Password) {
			page.Errors.add("old_password", "Your old password was entered incorrectly. Please enter it again.")
		}
		if p1 == "" {
			page.Errors.add("new_password1", "This field is required.")
		}
		if p2 == "" {
			page.Errors.add("new_password2", "This field is required.")
		} else if p1 != p2 {
			page.Errors.add("new_password2", "The two password fields didn’t match.")
		}
		if p1 != "" && p2 != "" && p1 == p2 {
			for _, perr := range validateNewPassword(p1, u.Email) {
				page.Errors.add("new_password1", perr.Message)
			}
		}
		if !page.Errors.any() {
			encoded := hashPassword(p1)
			if _, err := a.DB.Exec(`UPDATE accounts_user SET password = ?, updated_at = ? WHERE id = ?`,
				encoded, formatDjangoDateTime(timeNowUTC()), u.ID); err != nil {
				logerr(err, "admin.passwordChange")
			} else {
				// update_session_auth_hash: cycle the key with the new hash.
				if cookie, err := r.Cookie(sessionCookieName); err == nil && cookie.Value != "" {
					if sess, _ := getSession(a.DB, cookie.Value); sess != nil {
						newKey := newSessionKey()
						_ = deleteSession(a.DB, sess.Key)
						_ = putSession(a.DB, newKey, u.ID, sessionAuthHash(encoded))
						setSessionCookie(w, newKey, a.Settings.SessionCookieSecure())
					}
				}
				adminRedirect(w, r, adminPasswordDone)
				return
			}
		}
	} else if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, POST, HEAD, OPTIONS")
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	a.admin().render(w, r, http.StatusOK, "password_change", page)
}

func (a *App) adminPasswordChangeDone(w http.ResponseWriter, r *http.Request, u *UserRow) {
	if !adminIsStaff(u) {
		adminRedirect(w, r, adminLoginRedirect(r))
		return
	}
	base := a.adminBasePage(r, u)
	base.Title = "Password change successful"
	a.admin().render(w, r, http.StatusOK, "password_change_done", base)
}

// ---- per-user password page ----

func (a *App) adminUserPassword(w http.ResponseWriter, r *http.Request, u *UserRow, m *adminModel, id string) {
	if !a.adminHasPerm(u, "auth.change_user") && !a.adminHasPerm(u, "accounts.change_user") {
		a.admin().render403(w, r, u, "You do not have permission to change this user's password.")
		return
	}
	obj, err := m.Get(a, id)
	if err != nil || obj == nil {
		a.admin().render404(w, r, u)
		return
	}
	base := a.adminBasePage(r, u)
	base.Title = "Change password: " + obj.Repr
	base.Breadcrumbs = []adminCrumb{
		{Label: m.Plural, URL: "/admin/" + m.AppLabel + "/" + m.Model + "/"},
		{Label: obj.Repr, URL: "/admin/" + m.AppLabel + "/" + m.Model + "/" + adminURLID(m, id) + "/change/"},
		{Label: "Password"},
	}
	page := &adminPasswordChangePage{adminBase: base, ObjectID: id, Target: obj, Errors: adminErrors{}}
	if r.Method == http.MethodPost {
		_ = r.ParseMultipartForm(32 << 20)
		p1 := r.FormValue("password1")
		p2 := r.FormValue("password2")
		if p1 == "" {
			page.Errors.add("password1", "This field is required.")
		}
		if p2 == "" {
			page.Errors.add("password2", "This field is required.")
		} else if p1 != p2 {
			page.Errors.add("password2", "The two password fields didn’t match.")
		}
		if p1 != "" && p1 == p2 {
			for _, perr := range validateNewPassword(p1, obj.Values["email"]) {
				page.Errors.add("password1", perr.Message)
			}
		}
		if !page.Errors.any() {
			encoded := hashPassword(p1)
			if _, err := a.DB.Exec(`UPDATE accounts_user SET password = ?, updated_at = ? WHERE id = ?`,
				encoded, formatDjangoDateTime(timeNowUTC()), id); err != nil {
				logerr(err, "admin.userPassword")
			}
			a.adminLog(u, "accounts.user", id, obj.Repr, 2, `[{"changed": {"fields": ["password"]}}]`)
			a.flashAdminMessages(w, r, adminSuccess("Password changed successfully."))
			adminRedirect(w, r, "/admin/"+m.AppLabel+"/"+m.Model+"/"+adminURLID(m, id)+"/change/")
			return
		}
	}
	a.admin().render(w, r, http.StatusOK, "password_change", page)
}

// ---- search ----

func (a *App) adminSearch(w http.ResponseWriter, r *http.Request, u *UserRow) {
	if !adminIsStaff(u) {
		adminRedirect(w, r, adminLoginRedirect(r))
		return
	}
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	var hits []adminSearchHit
	if query != "" {
		for _, m := range a.admin().models {
			if !a.adminCanView(u, m) || len(m.SearchFields) == 0 {
				continue
			}
			rows, err := a.adminSearchRows(m, query, 20)
			if err != nil {
				logerr(err, "admin.search."+m.Model)
				continue
			}
			for _, row := range rows {
				title := row.Cells[m.LinkName].Plain()
				if title == "" {
					title = row.ID
				}
				hits = append(hits, adminSearchHit{
					Model: m.Plural,
					Label: m.Verbose,
					Title: title,
					URL:   "/admin/" + m.AppLabel + "/" + m.Model + "/" + adminURLID(m, row.ID) + "/change/",
				})
			}
		}
	}
	base := a.adminBasePage(r, u)
	base.Title = "Search"
	base.Breadcrumbs = []adminCrumb{{Label: "Search"}}
	a.admin().render(w, r, http.StatusOK, "search", &adminSearchPage{
		adminBase: base,
		Query:     query,
		Results:   hits,
	})
}

// ---- autocomplete ----

type adminAutocompleteResult struct {
	ID   string
	Text string
}

func (a *App) adminAutocomplete(w http.ResponseWriter, r *http.Request, u *UserRow) {
	values := r.URL.Query()
	appLabel := values.Get("app_label")
	modelName := values.Get("model_name")
	fieldName := values.Get("field_name")
	// AutocompleteJsonView.process_request: missing or unknown app/model/field
	// is a PermissionDenied, not an empty result page.
	source := a.admin().byKey[appLabel+"."+modelName]
	if source == nil || fieldName == "" {
		a.admin().render403(w, r, u, "You do not have permission to perform this action.")
		return
	}
	target := a.admin().byKey[source.AutocompleteFields[fieldName]]
	if target == nil {
		a.admin().render403(w, r, u, "You do not have permission to perform this action.")
		return
	}
	if len(target.SearchFields) == 0 {
		a.admin().render404(w, r, u)
		return
	}
	if !adminIsStaff(u) || !a.adminCanView(u, target) {
		a.admin().render403(w, r, u, "You do not have permission to perform this action.")
		return
	}
	term := values.Get("term")
	page, _ := strconv.Atoi(values.Get("page"))
	if page < 1 {
		page = 1
	}
	total, err := a.adminSearchCount(target, term)
	if err != nil {
		logerr(err, "admin.autocomplete.count."+target.Model)
	}
	pages := (total + 19) / 20
	if pages == 0 {
		pages = 1
	}
	if page > pages {
		// Paginator.get_page clamps an out-of-range page to the last one.
		page = pages
	}
	rows, err := a.adminSearchRowsPage(target, term, 20, (page-1)*20)
	if err != nil {
		logerr(err, "admin.autocomplete."+target.Model)
		rows = nil
	}
	var results []adminAutocompleteResult
	for _, row := range rows {
		text := row.Cells[target.LinkName].Plain()
		if text == "" {
			text = row.ID
		}
		results = append(results, adminAutocompleteResult{ID: adminURLID(target, row.ID), Text: text})
	}
	writeAdminAutocomplete(w, results, page < pages)
}

// writeAdminAutocomplete renders JsonResponse's body: json.dumps default
// separators (", " / ": ") and ensure_ascii, with results before pagination.
func writeAdminAutocomplete(w http.ResponseWriter, results []adminAutocompleteResult, more bool) {
	var b strings.Builder
	b.WriteString(`{"results": [`)
	for i, result := range results {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(`{"id": ` + pyJSONStringASCII(result.ID) +
			`, "text": ` + pyJSONStringASCII(result.Text) + `}`)
	}
	b.WriteString(`], "pagination": {"more": `)
	if more {
		b.WriteString("true")
	} else {
		b.WriteString("false")
	}
	b.WriteString("}}")
	body := []byte(b.String())
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// ---- i18n javascript ----

func (a *App) adminJsi18n(w http.ResponseWriter, r *http.Request, u *UserRow) {
	body := `/* gettext catalog (Go admin: D2) */
(function(){var d=document;function gettext(s){return s}
window.gettext=gettext;window.ngettext=function(s,p,n){return n===1?s:p};
window.interpolate=function(f,a){return f.replace(/%\((\w+)\)s/g,function(m,k){return a[k]})};
window.get_format=function(){return "text/javascript"};})();
`
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(body))
}

// ---- view on site ----

func (a *App) adminViewOnSite(w http.ResponseWriter, r *http.Request, u *UserRow) {
	// contenttypes.views.shortcut: every registered model lacks
	// get_absolute_url at the pin, so every resolvable object still 404s.
	a.admin().render404(w, r, u)
}

// ---- helpers ----

// adminLog writes a django_admin_log row (LogEntry.objects.log_action).
func (a *App) adminLog(u *UserRow, key, objectID, repr string, actionFlag int, changeMessage string) {
	if u == nil {
		return
	}
	var ctID sql.NullInt64
	parts := strings.SplitN(key, ".", 2)
	if len(parts) == 2 {
		_ = a.DB.QueryRow(`SELECT id FROM django_content_type WHERE app_label = ? AND model = ?`,
			parts[0], parts[1]).Scan(&ctID)
	}
	if len([]rune(repr)) > 200 {
		repr = string([]rune(repr)[:200])
	}
	if changeMessage == "" {
		changeMessage = "[]"
	}
	_, err := a.DB.Exec(`INSERT INTO django_admin_log
		(action_time, object_id, object_repr, action_flag, change_message, content_type_id, user_id)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		formatDjangoDateTime(timeNowUTC()), objectID, repr, actionFlag, changeMessage,
		nullableInt(ctID), u.ID)
	if err != nil {
		logerr(err, "admin.log")
	}
}

func nullableInt(v sql.NullInt64) any {
	if v.Valid {
		return v.Int64
	}
	return nil
}

// adminObjectURL is the change URL for an object.
func adminObjectURL(m *adminModel, id string) string {
	return "/admin/" + m.AppLabel + "/" + m.Model + "/" + adminURLID(m, id) + "/change/"
}

// adminQueryID normalises an object id parameter (dashed or 32-hex).
func adminQueryID(m *adminModel, raw string) string {
	raw = strings.TrimSpace(raw)
	if m.IntPK {
		return raw
	}
	return hexUUID(raw)
}

var _ = fmt.Sprintf
var _ = url.Values{}
