// The changelist engine: filters, search, ordering, pagination, the bulk
// action POST and list_editable saves.
package doory

import (
	"database/sql"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

// adminChangelist serves /admin/<app>/<model>/ (GET list, POST actions).
func (a *App) adminChangelist(w http.ResponseWriter, r *http.Request, u *UserRow, m *adminModel) {
	if !a.adminCanView(u, m) && !a.adminCanAdd(u, m) {
		a.admin().render403(w, r, u, "You do not have permission to view this page.")
		return
	}
	// unfold's ModelAdmin.changelist_view @xframe_options_sameorigin: every
	// response of this view (including action redirects) is SAMEORIGIN, but a
	// PermissionDenied above never reaches the decorator.
	adminSameOriginModel(w, m)
	if r.Method == http.MethodPost {
		a.adminChangelistPost(w, r, u, m)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, POST, HEAD, OPTIONS")
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	a.renderAdminChangelist(w, r, u, m, adminErrors{}, "", nil)
}

// renderAdminChangelist builds and renders the page (also used after a form
// error or a list_editable save).
// adminSameOrigin is unfold's ModelAdmin.changelist_view /
// changeform_view @xframe_options_sameorigin decorator.
func adminSameOrigin(w http.ResponseWriter) {
	w.Header().Set("X-Frame-Options", "SAMEORIGIN")
}

// adminSameOriginModel applies the decorator only to Unfold admins: plain
// django.contrib.admin.ModelAdmin (auth.Group) keeps the stack's DENY.
func adminSameOriginModel(w http.ResponseWriter, m *adminModel) {
	if m != nil && m.PlainAdmin {
		return
	}
	adminSameOrigin(w)
}

func (a *App) renderAdminChangelist(w http.ResponseWriter, r *http.Request, u *UserRow,
	m *adminModel, errors adminErrors, message string, extraMsgs []adminMessage,
) {
	values := r.URL.Query()
	search := strings.TrimSpace(values.Get("q"))
	page := adminParsePage(r)
	order := adminParseOrder(r)

	filters, clauses, args := a.adminFilterState(m, values, r)

	where := []string{}
	whereArgs := []any{}
	if m.BaseWhere != "" {
		where = append(where, m.BaseWhere)
		whereArgs = append(whereArgs, m.BaseArgs...)
	}
	if len(clauses) > 0 {
		where = append(where, "("+strings.Join(clauses, " AND ")+")")
		whereArgs = append(whereArgs, args...)
	}
	if search != "" && len(m.SearchFields) > 0 {
		likes := make([]string, 0, len(m.SearchFields))
		pattern := "%" + strings.ToLower(search) + "%"
		for _, field := range m.SearchFields {
			likes = append(likes, "LOWER("+field.SQL+") LIKE ?")
			whereArgs = append(whereArgs, pattern)
		}
		where = append(where, "("+strings.Join(likes, " OR ")+")")
	}
	whereSQL := ""
	if len(where) > 0 {
		whereSQL = " WHERE " + strings.Join(where, " AND ")
	}

	total := 0
	countSQL := "SELECT COUNT(*) FROM " + m.FromSQL + whereSQL
	if err := a.DB.QueryRow(countSQL, whereArgs...).Scan(&total); err != nil {
		logerr(err, "admin.changelist.count."+m.Model)
		a.admin().render404(w, r, u)
		return
	}

	perPage := m.PerPage
	if perPage <= 0 {
		perPage = adminListPerPage
	}
	pages := (total + perPage - 1) / perPage
	if pages < 1 {
		pages = 1
	}
	if page > pages {
		page = pages
	}

	orderSQL := a.adminOrderSQL(m, order)
	listSQL := "SELECT " + m.SelectSQL + " FROM " + m.FromSQL + whereSQL
	if orderSQL != "" {
		listSQL += " ORDER BY " + orderSQL
	}
	listSQL += " LIMIT ? OFFSET ?"
	listArgs := append(append([]any{}, whereArgs...), perPage, (page-1)*perPage)
	dbRows, err := a.DB.Query(listSQL, listArgs...)
	if err != nil {
		logerr(err, "admin.changelist.list."+m.Model)
		a.admin().render404(w, r, u)
		return
	}
	var rows []adminListRow
	for dbRows.Next() {
		row, err := m.ScanList(a, dbRows)
		if err != nil {
			logerr(err, "admin.changelist.scan."+m.Model)
			continue
		}
		rows = append(rows, row)
	}
	_ = dbRows.Close()

	base := a.adminBasePage(r, u)
	base.Title = m.Plural
	base.Breadcrumbs = []adminCrumb{{Label: m.Plural}}
	if message != "" {
		base.Messages = append(base.Messages, adminError(message))
	}
	base.Messages = append(base.Messages, extraMsgs...)

	pageInfo := adminPagination{
		Show:  pages > 1,
		Page:  page,
		Pages: pages,
		Total: total,
		Start: (page-1)*perPage + 1,
		End:   page * perPage,
	}
	if pageInfo.End > total {
		pageInfo.End = total
	}
	if page > 1 {
		pageInfo.PrevURL = adminChangelistURL(r, map[string]string{"p": strconv.Itoa(page - 1)})
	}
	if page < pages {
		pageInfo.NextURL = adminChangelistURL(r, map[string]string{"p": strconv.Itoa(page + 1)})
	}

	changelist := &adminChangelistPage{
		adminBase:       base,
		Model:           m,
		PageTitle:       m.Plural,
		ObjectName:      m.Verbose,
		Columns:         m.Columns,
		Rows:            rows,
		Filters:         filters,
		SearchValue:     search,
		ShowSearch:      len(m.SearchFields) > 0,
		Actions:         m.Actions,
		ShowActions:     len(m.Actions) > 0,
		ShowAdd:         a.adminCanAdd(u, m),
		AddURL:          "/admin/" + m.AppLabel + "/" + m.Model + "/add/",
		ResultCount:     total,
		FullCount:       total,
		Pagination:      pageInfo,
		ShowResultCount: true,
		EditableFields: func() []adminField {
			var out []adminField
			for _, name := range m.ListEditable {
				for _, field := range m.formFields() {
					if field.Name == name {
						out = append(out, field)
					}
				}
			}
			return out
		}(),
	}
	changelist.ShowListForm = len(m.ListEditable) > 0 && a.adminCanChange(u, m)
	// Date hierarchy: render the latest years/months for quick drilling.
	changelist.DateLinks = a.adminDateLinks(m, values, whereSQL, whereArgs)
	if m.AppLabel == "ai" && m.Model == "provider" {
		changelist.EnvProviders, changelist.EnvErrors = a.adminEnvPanel()
	}
	a.admin().render(w, r, http.StatusOK, "changelist", changelist)
}

// adminChangelistURL copies the request query, applies changes, and returns
// the changelist path.
func adminChangelistURL(r *http.Request, changes map[string]string) string {
	q := r.URL.Query()
	for key, value := range changes {
		if value == "" {
			q.Del(key)
		} else {
			q.Set(key, value)
		}
	}
	path := r.URL.Path
	if path == "" {
		path = "/"
	}
	if encoded := q.Encode(); encoded != "" {
		return path + "?" + encoded
	}
	return path
}

// adminSortValue is the order key for a list_display column.
func (a *App) adminOrderSQL(m *adminModel, order int) string {
	if order != 0 {
		index := order
		direction := "ASC"
		if index < 0 {
			index = -index
			direction = "DESC"
		}
		index--
		if index >= 0 && index < len(m.Columns) && m.Columns[index].Sort != "" {
			return m.Columns[index].Sort + " " + direction
		}
	}
	if len(m.Ordering) > 0 {
		return strings.Join(m.Ordering, ", ")
	}
	return ""
}

// ---- filters ----

// adminFilterState renders the sidebar and returns the SQL clauses.
func (a *App) adminFilterState(m *adminModel, values url.Values, r *http.Request) ([]adminFilter, []string, []any) {
	var rendered []adminFilter
	var clauses []string
	var args []any
	for _, declared := range m.Filters {
		filter := declared
		filter.Choices = nil
		if filter.Param == "" {
			filter.Param = filter.Column + "__exact"
		}
		switch filter.Kind {
		case "bool":
			raw := values.Get(filter.Param)
			allURL := adminChangelistURL(r, map[string]string{filter.Param: ""})
			filter.Choices = []adminFilterChoice{
				{Value: "", Label: "All", URL: allURL, Selected: raw == ""},
				{Value: "1", Label: "Yes", URL: adminChangelistURL(r, map[string]string{filter.Param: "1"}), Selected: raw == "1"},
				{Value: "0", Label: "No", URL: adminChangelistURL(r, map[string]string{filter.Param: "0"}), Selected: raw == "0"},
			}
			if raw == "1" || raw == "0" {
				clauses = append(clauses, filter.Column+" = ?")
				args = append(args, raw)
			}
		case "choice", "fk":
			raw := values.Get(filter.Param)
			declared := filter.Options
			if filter.OptionsFn != nil {
				declared = filter.OptionsFn(a)
			}
			options := append([]adminFilterChoice{{Value: "", Label: "All", URL: adminChangelistURL(r, map[string]string{filter.Param: ""})}}, declared...)
			for i := range options {
				choice := &options[i]
				if choice.Value != "" {
					choice.URL = adminChangelistURL(r, map[string]string{filter.Param: choice.Value})
				}
				choice.Selected = raw == choice.Value
			}
			filter.Choices = options
			if raw != "" {
				clauses = append(clauses, filter.Column+" = ?")
				args = append(args, raw)
			}
		case "date":
			filter, extraClauses := a.adminDateFilter(r, filter, values)
			rendered = append(rendered, filter)
			for _, clause := range extraClauses {
				clauses = append(clauses, clause.sql)
				args = append(args, clause.args...)
			}
		case "custom":
			if filter.Custom != nil {
				clause, clauseArgs := filter.Custom(values)
				if clause != "" {
					clauses = append(clauses, clause)
					args = append(args, clauseArgs...)
				}
				// Lookups render as a 3-item All/In/Not list for the custom
				// filters that declare options.
				raw := values.Get(filter.Param)
				options := append([]adminFilterChoice{{Value: "", Label: "All", URL: adminChangelistURL(r, map[string]string{filter.Param: ""})}}, filter.Options...)
				for i := range options {
					choice := &options[i]
					if choice.Value != "" {
						choice.URL = adminChangelistURL(r, map[string]string{filter.Param: choice.Value})
					}
					choice.Selected = raw == choice.Value
				}
				filter.Choices = options
			}
		}
		rendered = append(rendered, filter)
	}
	return rendered, clauses, args
}

type adminDateClause struct {
	sql  string
	args []any
}

// adminDateFilter builds the selected year/month/day range and the lookup
// links (Django DateFieldListFilter's "Past 7 days" set is not needed; the
// hierarchy values are).
func (a *App) adminDateFilter(r *http.Request, filter adminFilter, values url.Values) (adminFilter, []adminDateClause) {
	year := values.Get(filter.Param + "__year")
	month := values.Get(filter.Param + "__month")
	day := values.Get(filter.Param + "__day")
	var clauses []adminDateClause
	build := func() (string, []any) {
		parts := []any{}
		expr := filter.Column
		if year != "" {
			y, _ := strconv.Atoi(year)
			if y > 0 {
				start := fmt.Sprintf("%04d-01-01 00:00:00", y)
				end := fmt.Sprintf("%04d-01-01 00:00:00", y+1)
				clauses = append(clauses, adminDateClause{expr + " >= ? AND " + expr + " < ?", []any{start, end}})
			}
		}
		if month != "" {
			mo, _ := strconv.Atoi(month)
			if mo >= 1 && mo <= 12 && year != "" {
				y, _ := strconv.Atoi(year)
				start := fmt.Sprintf("%04d-%02d-01 00:00:00", y, mo)
				nextY, nextM := y, mo+1
				if nextM > 12 {
					nextY, nextM = y+1, 1
				}
				end := fmt.Sprintf("%04d-%02d-01 00:00:00", nextY, nextM)
				clauses = append(clauses, adminDateClause{expr + " >= ? AND " + expr + " < ?", []any{start, end}})
			}
		}
		if day != "" && month != "" && year != "" {
			y, _ := strconv.Atoi(year)
			mo, _ := strconv.Atoi(month)
			d, _ := strconv.Atoi(day)
			if d >= 1 && d <= 31 {
				start := fmt.Sprintf("%04d-%02d-%02d 00:00:00", y, mo, d)
				end := fmt.Sprintf("%04d-%02d-%02d 00:00:00", y, mo, d+1)
				clauses = append(clauses, adminDateClause{expr + " >= ? AND " + expr + " < ?", []any{start, end}})
			}
		}
		return "", parts
	}
	build()
	filter.Choices = []adminFilterChoice{{Value: "", Label: "All", URL: adminChangelistURL(r, map[string]string{filter.Param + "__year": "", filter.Param + "__month": "", filter.Param + "__day": ""})}}
	return filter, clauses
}

// adminDateLinks renders the date-hierarchy row for a model.
func (a *App) adminDateLinks(m *adminModel, values url.Values, whereSQL string, whereArgs []any) []adminLink {
	if m.DateHierarchy == "" {
		return nil
	}
	column := m.DateHierarchy
	if !strings.Contains(column, ".") {
		column = "t." + column
	}
	rows, err := a.DB.Query("SELECT DISTINCT "+column+" FROM "+m.FromSQL+whereSQL+" LIMIT 2000", whereArgs...)
	if err != nil {
		return nil
	}
	defer rows.Close()
	years := map[string]bool{}
	for rows.Next() {
		var raw sql.NullString
		if err := rows.Scan(&raw); err == nil && raw.Valid && len(raw.String) >= 4 {
			years[raw.String[:4]] = true
		}
	}
	if len(years) < 1 {
		return nil
	}
	keys := make([]string, 0, len(years))
	for year := range years {
		keys = append(keys, year)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(keys)))
	var links []adminLink
	for _, year := range keys {
		links = append(links, adminLink{
			Label: year,
			URL:   adminChangelistURLView(m, map[string]string{columnParam(m) + "__year": year}),
		})
	}
	return links
}

// columnParam is the query-string base for the date hierarchy column.
func columnParam(m *adminModel) string {
	name := m.DateHierarchy
	if idx := strings.LastIndex(name, "."); idx >= 0 {
		name = name[idx+1:]
	}
	return name
}

// adminChangelistURLView builds a model changelist URL with extra parameters
// (no request in scope for precomputed links).
func adminChangelistURLView(m *adminModel, changes map[string]string) string {
	q := url.Values{}
	for key, value := range changes {
		q.Set(key, value)
	}
	return "/admin/" + m.AppLabel + "/" + m.Model + "/?" + q.Encode()
}

// adminSearchWhere builds the term filter used by the site search and the
// autocomplete endpoint (Django's get_search_results: no filter for "").
func adminSearchWhere(m *adminModel, term string) (string, []any) {
	var where []string
	var args []any
	if m.BaseWhere != "" {
		where = append(where, m.BaseWhere)
		args = append(args, m.BaseArgs...)
	}
	if term != "" && len(m.SearchFields) > 0 {
		pattern := "%" + strings.ToLower(term) + "%"
		likes := make([]string, 0, len(m.SearchFields))
		for _, field := range m.SearchFields {
			likes = append(likes, "LOWER("+field.SQL+") LIKE ?")
			args = append(args, pattern)
		}
		where = append(where, "("+strings.Join(likes, " OR ")+")")
	}
	if len(where) == 0 {
		return "", nil
	}
	return " WHERE " + strings.Join(where, " AND "), args
}

// adminSearchRows runs the changelist search for the site search and
// autocomplete endpoints.
func (a *App) adminSearchRows(m *adminModel, term string, limit int) ([]adminListRow, error) {
	return a.adminSearchRowsPage(m, term, limit, 0)
}

// adminSearchRowsPage is adminSearchRows with an offset (autocomplete paging).
func (a *App) adminSearchRowsPage(m *adminModel, term string, limit, offset int) ([]adminListRow, error) {
	whereSQL, args := adminSearchWhere(m, term)
	sqlText := "SELECT " + m.SelectSQL + " FROM " + m.FromSQL + whereSQL
	if len(m.Ordering) > 0 {
		sqlText += " ORDER BY " + strings.Join(m.Ordering, ", ")
	}
	sqlText += " LIMIT ?"
	args = append(args, limit)
	if offset > 0 {
		sqlText += " OFFSET ?"
		args = append(args, offset)
	}
	rows, err := a.DB.Query(sqlText, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []adminListRow
	for rows.Next() {
		row, err := m.ScanList(a, rows)
		if err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// adminSearchCount returns the number of search matches (autocomplete's
// paginator uses it to decide whether another page exists).
func (a *App) adminSearchCount(m *adminModel, term string) (int, error) {
	whereSQL, args := adminSearchWhere(m, term)
	var total int
	err := a.DB.QueryRow("SELECT COUNT(*) FROM "+m.FromSQL+whereSQL, args...).Scan(&total)
	return total, err
}

// ---- POST: actions / list_editable ----

func (a *App) adminChangelistPost(w http.ResponseWriter, r *http.Request, u *UserRow, m *adminModel) {
	if !a.adminCanChange(u, m) {
		a.admin().render403(w, r, u, "You do not have permission to change this object.")
		return
	}
	form, err := newAdminForm(r)
	if err != nil {
		a.adminChangelist(w, r, u, m)
		return
	}
	action := form.Value("action")
	if action == "" && len(m.ListEditable) > 0 {
		a.adminListEditable(w, r, u, m, form)
		return
	}
	ids := form.All("_selected_action")
	if action == "delete_selected" {
		a.adminDeleteSelected(w, r, u, m, ids)
		return
	}
	if action == "" {
		a.renderAdminChangelist(w, r, u, m, adminErrors{}, "", []adminMessage{
			adminWarning("Items must be selected in order to perform actions on them. No items have been changed."),
		})
		return
	}
	var chosen *adminAction
	for i := range m.Actions {
		if m.Actions[i].Name == action {
			chosen = &m.Actions[i]
			break
		}
	}
	if chosen == nil {
		a.admin().render(w, r, http.StatusBadRequest, "error", &adminErrorPage{
			adminBase: adminBase{Title: "Bad request", User: u, HasPermission: true},
			Message:   fmt.Sprintf("%q is not one of the available actions.", action),
		})
		return
	}
	if len(ids) == 0 {
		a.renderAdminChangelist(w, r, u, m, adminErrors{}, "", []adminMessage{
			adminWarning("Items must be selected in order to perform actions on them. No items have been changed."),
		})
		return
	}
	msgs, err := chosen.Run(a, u, ids)
	if err != nil {
		logerr(err, "admin.action."+action)
		a.renderAdminChangelist(w, r, u, m, adminErrors{}, "Something went wrong on our side.", nil)
		return
	}
	a.flashAdminMessages(w, r, msgs...)
	adminRedirect(w, r, adminChangelistURL(r, map[string]string{"p": "", "action": "", "select_across": "", "index": "", "_selected_action": ""}))
}

// adminListEditable saves inline-edited values named form-<pk>-<field>.
func (a *App) adminListEditable(w http.ResponseWriter, r *http.Request, u *UserRow, m *adminModel, form *adminForm) {
	changed := 0
	for _, field := range m.formFields() {
		isEditable := false
		for _, name := range m.ListEditable {
			if name == field.Name {
				isEditable = true
			}
		}
		if !isEditable {
			continue
		}
		for key, values := range form.Values {
			prefix := "form-"
			if !strings.HasPrefix(key, prefix) || !strings.HasSuffix(key, "-"+field.Name) {
				continue
			}
			id := strings.TrimSuffix(strings.TrimPrefix(key, prefix), "-"+field.Name)
			id = hexUUID(id)
			if id == "" || len(values) == 0 {
				continue
			}
			if _, err := a.DB.Exec("UPDATE "+m.Table+" SET "+field.Name+" = ? WHERE id = ?", values[0], id); err != nil {
				logerr(err, "admin.listEditable."+m.Model)
				continue
			}
			changed++
		}
	}
	if changed > 0 {
		a.flashAdminMessages(w, r, adminSuccess(fmt.Sprintf("The selected %s were changed successfully.", m.Plural)))
	}
	adminRedirect(w, r, adminChangelistURL(r, map[string]string{}))
}

// adminDeleteSelected ports django.contrib.admin.actions.delete_selected: a
// confirmation page for the selected objects, then the deletion itself.
func (a *App) adminDeleteSelected(w http.ResponseWriter, r *http.Request, u *UserRow, m *adminModel, rawIDs []string) {
	if len(rawIDs) == 0 {
		a.flashAdminMessages(w, r, adminWarning("Items must be selected in order to perform actions on them. No items have been changed."))
		adminRedirect(w, r, r.URL.RequestURI())
		return
	}
	if !a.adminCanDelete(u, m) {
		a.flashAdminMessages(w, r, adminWarning("No action selected."))
		adminRedirect(w, r, r.URL.RequestURI())
		return
	}
	// Resolve the selected objects (missing / out-of-scope ones are dropped,
	// matching Django's queryset.filter(pk__in=selected)).
	objs := make([]*adminObject, 0, len(rawIDs))
	for _, raw := range rawIDs {
		id := adminQueryID(m, raw)
		obj, err := m.Get(a, id)
		if err != nil || obj == nil {
			continue
		}
		objs = append(objs, obj)
	}
	form, err := newAdminForm(r)
	if err != nil {
		form = &adminForm{Values: map[string][]string{}}
	}
	if _, confirmed := form.Values["post"]; confirmed {
		for _, obj := range objs {
			if err := m.Delete(a, u, obj.ID); err != nil {
				logerr(err, "admin.deleteSelected."+m.Model)
				a.admin().render(w, r, http.StatusInternalServerError, "error", &adminErrorPage{
					adminBase: adminBase{Title: "Server error", User: u, HasPermission: true},
					Message:   "Something went wrong on our side.",
				})
				return
			}
			a.adminLog(u, m.AppLabel+"."+m.Model, obj.ID, obj.Repr, 3, "")
		}
		if n := len(objs); n > 0 {
			a.flashAdminMessages(w, r, adminSuccess(fmt.Sprintf("Successfully deleted %d %s.", n, adminModelNgettext(m, n))))
		}
		adminRedirect(w, r, r.URL.RequestURI())
		return
	}
	// Confirmation page.
	adminSameOriginModel(w, m)
	base := a.adminBasePage(r, u)
	base.Title = "Delete multiple objects"
	base.Breadcrumbs = []adminCrumb{
		{Label: m.Plural, URL: "/admin/" + m.AppLabel + "/" + m.Model + "/"},
		{Label: "Delete multiple objects"},
	}
	a.admin().render(w, r, http.StatusOK, "delete_selected_confirmation", &adminDeleteSelectedPage{
		adminBase:  base,
		Model:      m,
		Objects:    objs,
		ObjectName: adminModelNgettext(m, len(objs)),
		BackURL:    "/admin/" + m.AppLabel + "/" + m.Model + "/",
	})
}

// adminModelNgettext mirrors django.contrib.admin.utils.model_ngettext:
// singular verbose name for one object, plural for zero/many.
func adminModelNgettext(m *adminModel, n int) string {
	if n == 1 {
		return strings.ToLower(m.Name)
	}
	return strings.ToLower(m.Plural)
}
