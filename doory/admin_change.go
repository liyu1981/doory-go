// Add/change/delete/history views and the ElementPack bulk SVG upload.
package doory

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"
)

// ---- add / change ----

func (a *App) adminAdd(w http.ResponseWriter, r *http.Request, u *UserRow, m *adminModel) {
	if !a.adminCanAdd(u, m) {
		a.admin().render403(w, r, u, "You do not have permission to add this object.")
		return
	}
	// unfold's changeform_view @xframe_options_sameorigin (see changelist).
	adminSameOriginModel(w, m)
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		obj := &adminObject{Values: map[string]string{}, Display: map[string]template.HTML{}, Raw: map[string]any{}}
		if m.New != nil {
			if built := m.New(a); built != nil {
				obj = built
			}
		}
		a.renderAdminChange(w, r, u, m, obj, true, adminErrors{})
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "GET, POST, HEAD, OPTIONS")
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	form, err := newAdminForm(r)
	if err != nil {
		a.renderAdminChange(w, r, u, m, adminSubmittedObject(m, form), true, adminErrors{"__all__": {"Malformed upload."}})
		return
	}
	id, errs, err := m.Add(a, u, form)
	if err != nil {
		logerr(err, "admin.add."+m.Model)
		a.renderAdminChange(w, r, u, m, adminSubmittedObject(m, form), true,
			adminErrors{"__all__": {"Something went wrong on our side."}})
		return
	}
	if errs.any() {
		a.renderAdminChange(w, r, u, m, adminSubmittedObject(m, form), true, errs)
		return
	}
	repr := m.Repr(a, id)
	a.adminLog(u, m.AppLabel+"."+m.Model, id, repr, 1, "")
	a.flashAdminMessages(w, r, adminSuccess(fmt.Sprintf("The %s “%s” was added successfully.", m.Verbose, repr)))
	changeURL := "/admin/" + m.AppLabel + "/" + m.Model + "/" + adminURLID(m, id) + "/change/"
	switch {
	case form.Value("_continue") != "":
		adminRedirect(w, r, changeURL)
	case form.Value("_addanother") != "":
		adminRedirect(w, r, "/admin/"+m.AppLabel+"/"+m.Model+"/add/")
	default:
		adminRedirect(w, r, "/admin/"+m.AppLabel+"/"+m.Model+"/")
	}
}

func (a *App) adminChange(w http.ResponseWriter, r *http.Request, u *UserRow, m *adminModel, id string) {
	if m.Get == nil {
		a.admin().render404(w, r, u)
		return
	}
	obj, err := m.Get(a, id)
	isPost := r.Method == http.MethodPost
	if isPost {
		if !a.adminCanChange(u, m) {
			a.admin().render403(w, r, u, "You do not have permission to change this object.")
			return
		}
	} else if !a.adminCanView(u, m) && !a.adminCanChange(u, m) {
		a.admin().render403(w, r, u, "You do not have permission to view this object.")
		return
	}
	// The object lookup precedes the permission check, but the sameorigin
	// decorator only sees responses: the missing-object redirect below carries
	// SAMEORIGIN while a PermissionDenied above keeps the stack's DENY.
	adminSameOriginModel(w, m)
	if errors.Is(err, errOutsideAdminScope) || err != nil || obj == nil {
		// ModelAdmin._get_obj_does_not_exist_redirect: warning + index.
		a.adminMissingObject(w, r, m, id)
		return
	}
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		a.renderAdminChange(w, r, u, m, obj, false, adminErrors{})
		return
	}
	if !isPost {
		w.Header().Set("Allow", "GET, POST, HEAD, OPTIONS")
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	form, ferr := newAdminForm(r)
	if ferr != nil {
		a.renderAdminChange(w, r, u, m, adminSubmittedObject(m, form), false,
			adminErrors{"__all__": {"Malformed upload."}})
		return
	}
	errs, err := m.Change(a, u, id, form)
	if err != nil {
		logerr(err, "admin.change."+m.Model)
		errs = adminErrors{"__all__": {"Something went wrong on our side."}}
	}
	if errs.any() {
		a.renderAdminChange(w, r, u, m, adminSubmittedObject(m, form), false, errs)
		return
	}
	repr := m.Repr(a, id)
	a.adminLog(u, m.AppLabel+"."+m.Model, id, repr, 2, "")
	a.flashAdminMessages(w, r, adminSuccess(fmt.Sprintf("The %s “%s” was changed successfully.", m.Verbose, repr)))
	if form.Value("_continue") != "" {
		adminRedirect(w, r, "/admin/"+m.AppLabel+"/"+m.Model+"/"+adminURLID(m, id)+"/change/")
		return
	}
	adminRedirect(w, r, "/admin/"+m.AppLabel+"/"+m.Model+"/")
}

func (a *App) renderAdminChange(w http.ResponseWriter, r *http.Request, u *UserRow,
	m *adminModel, obj *adminObject, isAdd bool, errs adminErrors,
) {
	base := a.adminBasePage(r, u)
	base.Title = m.Verbose
	base.Breadcrumbs = []adminCrumb{{Label: m.Plural, URL: "/admin/" + m.AppLabel + "/" + m.Model + "/"}}
	if isAdd || obj.ID == "" {
		base.Breadcrumbs = append(base.Breadcrumbs, adminCrumb{Label: "Add " + m.Verbose})
	} else {
		base.Breadcrumbs = append(base.Breadcrumbs, adminCrumb{Label: obj.Repr})
	}
	fieldsets := m.Fieldsets
	if isAdd && len(m.AddFieldsets) > 0 {
		fieldsets = m.AddFieldsets
	}
	page := &adminChangePage{
		adminBase:     base,
		Model:         m,
		Object:        obj,
		IsAdd:         isAdd,
		Fieldsets:     a.adminResolveFieldsets(m, fieldsets),
		Inlines:       obj.Inlines,
		Errors:        errs,
		SubmitLabel:   "Save",
		ShowSaveAsNew: !isAdd && a.adminCanAdd(u, m),
		Multi:         map[string][]string{},
	}
	if !isAdd && obj.ID != "" {
		page.ShowDelete = a.adminCanDelete(u, m)
		page.ShowHistory = a.adminCanView(u, m)
		page.DeleteURL = "/admin/" + m.AppLabel + "/" + m.Model + "/" + adminURLID(m, obj.ID) + "/delete/"
		page.HistoryURL = "/admin/" + m.AppLabel + "/" + m.Model + "/" + adminURLID(m, obj.ID) + "/history/"
		if m.AppLabel == "accounts" && m.Model == "user" {
			page.PasswordURL = "/admin/" + m.AppLabel + "/" + m.Model + "/" + adminURLID(m, obj.ID) + "/password/"
		}
	}
	a.admin().render(w, r, http.StatusOK, "change_form", page)
}

// ---- delete ----

func (a *App) adminDelete(w http.ResponseWriter, r *http.Request, u *UserRow, m *adminModel, id string) {
	if !a.adminCanDelete(u, m) {
		a.admin().render403(w, r, u, "You do not have permission to delete this object.")
		return
	}
	obj, err := m.Get(a, id)
	if err != nil || obj == nil {
		a.adminMissingObject(w, r, m, id)
		return
	}
	if r.Method == http.MethodPost {
		form, err := newAdminForm(r)
		if err != nil {
			form = &adminForm{Values: map[string][]string{}}
		}
		if _, confirmed := form.Values["post"]; confirmed || len(form.Values) == 0 {
			if err := m.Delete(a, u, id); err != nil {
				logerr(err, "admin.delete."+m.Model)
				a.admin().render(w, r, http.StatusInternalServerError, "error", &adminErrorPage{
					adminBase: adminBase{Title: "Server error", User: u, HasPermission: true},
					Message:   "Something went wrong on our side.",
				})
				return
			}
			a.adminLog(u, m.AppLabel+"."+m.Model, id, obj.Repr, 3, "")
			a.flashAdminMessages(w, r, adminSuccess(fmt.Sprintf("The %s “%s” was deleted successfully.", m.Verbose, obj.Repr)))
			adminRedirect(w, r, "/admin/"+m.AppLabel+"/"+m.Model+"/")
			return
		}
	} else if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, POST, HEAD, OPTIONS")
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	base := a.adminBasePage(r, u)
	base.Title = "Are you sure?"
	base.Breadcrumbs = []adminCrumb{
		{Label: m.Plural, URL: "/admin/" + m.AppLabel + "/" + m.Model + "/"},
		{Label: obj.Repr, URL: "/admin/" + m.AppLabel + "/" + m.Model + "/" + adminURLID(m, id) + "/change/"},
		{Label: "Delete"},
	}
	a.admin().render(w, r, http.StatusOK, "delete_confirmation", &adminDeletePage{
		adminBase: base,
		Model:     m,
		Object:    obj,
		DeleteURL: "/admin/" + m.AppLabel + "/" + m.Model + "/" + adminURLID(m, id) + "/delete/",
		BackURL:   "/admin/" + m.AppLabel + "/" + m.Model + "/",
	})
}

// adminMissingObject ports ModelAdmin._get_obj_does_not_exist_redirect: a
// warning message naming the raw id, then a redirect to the admin index.
func (a *App) adminMissingObject(w http.ResponseWriter, r *http.Request, m *adminModel, id string) {
	display := adminURLID(m, id)
	if unescaped, err := url.PathUnescape(id); err == nil {
		display = adminURLID(m, unescaped)
	}
	msg := fmt.Sprintf("%s with ID “%s” doesn’t exist. Perhaps it was deleted?", m.Verbose, display)
	a.flashAdminMessages(w, r, adminWarning(msg))
	adminRedirect(w, r, "/admin/")
}

// ---- history ----

func (a *App) adminHistory(w http.ResponseWriter, r *http.Request, u *UserRow, m *adminModel, id string) {
	if !a.adminCanView(u, m) && !a.adminCanChange(u, m) {
		a.admin().render403(w, r, u, "You do not have permission to view this object.")
		return
	}
	obj, err := m.Get(a, id)
	if err != nil || obj == nil {
		a.adminMissingObject(w, r, m, id)
		return
	}
	rows, err := a.DB.Query(`SELECT l.action_time, l.user_id, l.action_flag, l.change_message, COALESCE(u.email, '')
		FROM django_admin_log l
		JOIN django_content_type ct ON ct.id = l.content_type_id
		LEFT JOIN accounts_user u ON u.id = l.user_id
		WHERE ct.app_label = ? AND ct.model = ? AND l.object_id = ?
		ORDER BY l.action_time DESC, l.id DESC`, m.AppLabel, m.Model, id)
	var entries []adminHistoryEntry
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var actionTime, userEmail, changeMessage string
			var actionFlag int
			var userID sql.NullString
			if err := rows.Scan(&actionTime, &userID, &actionFlag, &changeMessage, &userEmail); err != nil {
				continue
			}
			entries = append(entries, adminHistoryEntry{
				Time:    adminFormatHistoryTime(actionTime),
				User:    userEmail,
				Action:  adminActionLabel(actionFlag),
				Message: adminChangeMessageText(changeMessage),
			})
		}
	}
	base := a.adminBasePage(r, u)
	base.Title = "Change history: " + obj.Repr
	base.Breadcrumbs = []adminCrumb{
		{Label: m.Plural, URL: "/admin/" + m.AppLabel + "/" + m.Model + "/"},
		{Label: obj.Repr, URL: "/admin/" + m.AppLabel + "/" + m.Model + "/" + adminURLID(m, id) + "/change/"},
		{Label: "History"},
	}
	a.admin().render(w, r, http.StatusOK, "history", &adminHistoryPage{
		adminBase: base,
		Model:     m,
		Object:    obj,
		Entries:   entries,
		BackURL:   "/admin/" + m.AppLabel + "/" + m.Model + "/" + adminURLID(m, id) + "/change/",
	})
}

func adminActionLabel(flag int) string {
	switch flag {
	case 1:
		return "Addition"
	case 2:
		return "Change"
	case 3:
		return "Deletion"
	}
	return ""
}

func adminFormatHistoryTime(raw string) string {
	for _, layout := range []string{"2006-01-02 15:04:05.999999", "2006-01-02 15:04:05", time.RFC3339Nano} {
		if parsed, err := time.Parse(layout, raw); err == nil {
			return parsed.UTC().Format("Jan. 2, 2006, 3:04 p.m.")
		}
	}
	return raw
}

func adminChangeMessageText(raw string) string {
	if raw == "" || raw == "[]" {
		return ""
	}
	var entries []map[string]any
	if err := json.Unmarshal([]byte(raw), &entries); err != nil {
		return ""
	}
	var parts []string
	for _, entry := range entries {
		if changed, ok := entry["changed"].(map[string]any); ok {
			if fields, ok := changed["fields"].([]any); ok {
				var names []string
				for _, field := range fields {
					if text, ok := field.(string); ok {
						names = append(names, text)
					}
				}
				if len(names) > 0 {
					parts = append(parts, "Changed "+strings.Join(names, ", ")+".")
				}
			}
		}
		if added, ok := entry["added"].(map[string]any); ok {
			_ = added
			parts = append(parts, "Added.")
		}
	}
	return strings.Join(parts, " ")
}

// ---- ElementPack bulk upload ----

func (a *App) adminPackUpload(w http.ResponseWriter, r *http.Request, u *UserRow, m *adminModel, id string) {
	if !a.adminCanChange(u, m) || !a.adminHasPerm(u, "elements.add_element") {
		a.admin().render403(w, r, u, "You do not have permission to upload elements to this pack.")
		return
	}
	pack, err := m.Get(a, id)
	if err != nil || pack == nil {
		a.admin().render404(w, r, u)
		return
	}
	packSlug := pack.Values["slug"]
	packKind := pack.Values["kind"]
	changeURL := "/admin/" + m.AppLabel + "/" + m.Model + "/" + adminURLID(m, id) + "/change/"
	base := a.adminBasePage(r, u)
	base.Title = "Upload SVGs to " + pack.Repr
	base.Breadcrumbs = []adminCrumb{
		{Label: m.Plural, URL: "/admin/" + m.AppLabel + "/" + m.Model + "/"},
		{Label: pack.Repr, URL: changeURL},
		{Label: "Upload SVGs"},
	}
	page := &adminUploadPage{
		adminBase: base,
		Model:     m,
		Pack:      pack,
		Action:    "/admin/" + m.AppLabel + "/" + m.Model + "/" + adminURLID(m, id) + "/upload/",
		Cancel:    changeURL,
		MaxFile:   100,
	}
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		a.admin().render(w, r, http.StatusOK, "upload", page)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "GET, POST, HEAD, OPTIONS")
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	form, ferr := newAdminForm(r)
	if ferr != nil {
		page.Error = "Upload at most 100 files at a time."
		a.admin().render(w, r, http.StatusOK, "upload", page)
		return
	}
	uploads := form.Uploads("files")
	if len(uploads) == 0 {
		page.Error = "This field is required."
		a.admin().render(w, r, http.StatusOK, "upload", page)
		return
	}
	if len(uploads) > 100 {
		page.Error = "Upload at most 100 files at a time."
		a.admin().render(w, r, http.StatusOK, "upload", page)
		return
	}
	kind := form.Value("kind")
	var entries []elementImportEntry
	var skipped [][2]string
	for _, upload := range uploads {
		label := upload.Filename
		if len(upload.Data) > 5*1024*1024 {
			skipped = append(skipped, [2]string{label, "The file is larger than 5 MB."})
			continue
		}
		name := elementName(strings.TrimSuffix(path.Base(label), path.Ext(label)))
		key, kerr := elementKey(path.Base(label))
		if kerr != nil {
			skipped = append(skipped, [2]string{label, kerr.Error()})
			continue
		}
		entries = append(entries, elementImportEntry{
			Key:   key,
			Label: label,
			Name:  name,
			SVG:   upload.Data,
			Kind:  kind,
			Tags:  elementTagsFrom(nil, name),
		})
	}
	outcomes := map[string]string{}
	summary, err := a.bulkImportWithOutcomes(pack.ID, packSlug, entries, packKind, false, nil, outcomes)
	if err != nil {
		logerr(err, "admin.packUpload."+pack.ID)
		page.Error = "Something went wrong on our side."
		a.admin().render(w, r, http.StatusOK, "upload", page)
		return
	}
	var flash []adminMessage
	for _, pair := range skipped {
		flash = append(flash, adminError(pair[0]+": "+pair[1]))
	}
	for _, pair := range summary.Skipped {
		flash = append(flash, adminError(pair[0]+": "+pair[1]))
	}
	if summary.Created+summary.Updated+summary.Unchanged > 0 {
		flash = append(flash, adminSuccess(fmt.Sprintf("%d added, %d replaced, %d unchanged.",
			summary.Created, summary.Updated, summary.Unchanged)))
	}
	if len(flash) > 0 {
		a.flashAdminMessages(w, r, flash...)
	}
	adminRedirect(w, r, changeURL)
}
