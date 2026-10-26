// ai.Provider / UserProvider / AICall admin pages: operator providers with
// their model inline and the Load models / Test connection pages, the
// read-only support views, and the environment providers listed above the
// changelist.
package doory

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"strconv"
	"strings"
	"time"
)

var adminAIProviderStyles = []adminFilterChoice{
	adminOption("doory", "Doory-compatible"), adminOption("multipart", "Multipart → image"),
	adminOption("json", "JSON → base64"), adminOption("mask", "Mask"), adminOption("custom", "Custom"),
}

var adminAIAudiences = []adminFilterChoice{
	adminOption("everyone", "Every signed-in user"), adminOption("staff", "Staff only"),
	adminOption("users", "Selected users"),
}

var adminAITasks = []adminFilterChoice{
	adminOption("background-removal", "Background removal"),
}

func adminModelsAI() []*adminModel {
	return []*adminModel{adminAIProviderModel(), adminAIUserProviderModel(), adminAICallModel()}
}

// adminEnvPanel is ProviderAdmin.changelist_view's env rows: the frozen
// AI_PROVIDER_n_* providers, with the message for each row left out.
func (a *App) adminEnvPanel() ([]adminEnvRow, []string) {
	providers, errors := a.Settings.aiEnvLoad()
	var rows []adminEnvRow
	for i := range providers {
		p := &providers[i]
		tasks := "—"
		if len(p.tasks) > 0 {
			tasks = strings.Join(p.tasks, ", ")
		}
		var labels []string
		for _, model := range p.models {
			label := model[1]
			if label == "" {
				label = model[0]
			}
			if label == "" {
				label = "server default"
			}
			labels = append(labels, label)
		}
		models := strings.Join(labels, ", ")
		if models == "" {
			if p.style == "doory" {
				models = "from the server"
			} else {
				models = "server default"
			}
		}
		quota := "—"
		if p.dailyQuota != 0 {
			quota = strconv.Itoa(p.dailyQuota)
		}
		rows = append(rows, adminEnvRow{
			ID: p.id, Name: p.name, Style: p.style, Tasks: tasks, Models: models,
			Quota: quota, TestURL: "/admin/ai/provider/env/" + p.id + "/test/",
		})
	}
	return rows, errors
}

// ---- Provider ----

func adminAIUserChoices(a *App) []adminFilterChoice {
	rows, err := a.DB.Query(`SELECT id, email FROM accounts_user ORDER BY email`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []adminFilterChoice
	for rows.Next() {
		var id, email string
		if err := rows.Scan(&id, &email); err == nil {
			out = append(out, adminOption(id, email))
		}
	}
	return out
}

func adminAIProviderModel() *adminModel {
	m := &adminModel{
		AppLabel: "ai", Model: "provider", Verbose: "provider", Plural: "Providers", Name: "provider",
		Table: "ai_provider",
		Columns: []adminColumn{
			{Name: "name", Label: "Name", Sort: "p.name", Link: true},
			{Name: "style", Label: "Style", Sort: "p.style"},
			{Name: "task_list", Label: "Tasks"},
			{Name: "audience", Label: "Audience", Sort: "p.audience"},
			{Name: "daily_quota", Label: "Daily quota", Sort: "p.daily_quota"},
			{Name: "sends_offsite", Label: "Sends offsite", Sort: "p.sends_offsite"},
			{Name: "enabled", Label: "Enabled", Sort: "p.enabled"},
		},
		LinkName:     "name",
		SearchFields: []adminSearchField{{SQL: "p.name"}, {SQL: "p.base_url"}},
		ListEditable: []string{"enabled"},
		Filters: []adminFilter{
			{Label: "Style", Param: "style__exact", Kind: "choice", Column: "p.style",
				Options: adminAIProviderStyles},
			{Label: "Enabled", Param: "enabled__exact", Kind: "bool", Column: "p.enabled"},
			{Label: "Audience", Param: "audience__exact", Kind: "choice", Column: "p.audience",
				Options: adminAIAudiences},
		},
		BaseWhere: "p.owner_id IS NULL",
		Ordering:  []string{"p.sort_order ASC", "p.name ASC"},
		SelectSQL: "p.id, p.name, p.style, p.tasks, p.audience, p.daily_quota, p.sends_offsite, p.enabled",
		FromSQL:   "ai_provider p",
		ScanList: func(a *App, rows *sql.Rows) (adminListRow, error) {
			var id, name, style, tasks, audience string
			var quota, offsite, enabled int
			if err := rows.Scan(&id, &name, &style, &tasks, &audience, &quota, &offsite,
				&enabled); err != nil {
				return adminListRow{}, err
			}
			taskList := strings.Join(adminJSONList(tasks), ", ")
			if taskList == "" {
				taskList = "—"
			}
			return adminListRow{ID: id, Cells: map[string]adminCell{
				"name":          adminTextCell(name),
				"style":         adminTextCell(style),
				"task_list":     adminTextCell(taskList),
				"audience":      adminTextCell(audience),
				"daily_quota":   adminIntCell(quota),
				"sends_offsite": adminTextCell(adminBoolText(offsite)),
				"enabled":       adminTextCell(adminBoolText(enabled)),
			}}, nil
		},
		AllowAdd: true, AllowChange: true, AllowDelete: true,
		Choices: map[string]func(a *App) []adminFilterChoice{
			"style":         func(a *App) []adminFilterChoice { return adminAIProviderStyles },
			"audience":      func(a *App) []adminFilterChoice { return adminAIAudiences },
			"tasks":         func(a *App) []adminFilterChoice { return adminAITasks },
			"allowed_users": adminAIUserChoices,
		},
		DetailActions: []adminDetailAction{
			{Name: "load_models", Label: "Load models", Perm: "change", Run: adminAILoadModels},
			{Name: "test_connection", Label: "Test connection", Perm: "change", Run: adminAITestConnection},
		},
	}
	m.Get = func(a *App, id string) (*adminObject, error) {
		var row aiProviderRow
		var owner sql.NullString
		var secret []byte
		err := a.DB.QueryRow(`SELECT id, created_at, updated_at, name, base_url, style, style_config,
			auth_header, secret_encrypted, tasks, timeout_seconds, max_image_bytes, sends_offsite,
			enabled, sort_order, audience, daily_quota, owner_id FROM ai_provider WHERE id = ?`,
			id).Scan(&row.id, &row.createdAt, &row.updatedAt, &row.name, &row.baseURL, &row.style,
			&row.styleConfig, &row.authHeader, &secret, &row.tasks, &row.timeout, &row.maxImage,
			&row.sendsOff, &row.enabled, &row.sortOrder, &row.audience, &row.dailyQuota, &owner)
		if err != nil {
			return nil, err
		}
		if owner.Valid {
			return nil, errOutsideAdminScope
		}
		row.secret = secret
		inline := adminAIProviderModelInline(a, id)
		secretState := "None"
		if row.hasSecret() {
			secretState = "Set (hidden)"
		}
		return &adminObject{
			ID: id, Repr: row.name,
			Values: map[string]string{
				"name": row.name, "base_url": row.baseURL, "style": row.style,
				"style_config": prettyInlineJSON(row.styleConfig), "timeout_seconds": strconv.Itoa(row.timeout),
				"max_image_bytes": strconv.FormatInt(row.maxImage, 10), "auth_header": row.authHeader,
				"enabled": boolFlag(boolInt(row.enabled)), "sort_order": strconv.Itoa(row.sortOrder),
				"audience": row.audience, "daily_quota": strconv.Itoa(row.dailyQuota),
				"sends_offsite": boolFlag(boolInt(row.sendsOff)),
			},
			Display: map[string]template.HTML{
				"secret_state": template.HTML(djangoEscape(secretState)),
				"created_at":   template.HTML(djangoEscape(row.createdAt)),
				"updated_at":   template.HTML(djangoEscape(row.updatedAt)),
			},
			Raw: map[string]any{
				"tasks":         adminJSONList(row.tasks),
				"allowed_users": a.adminM2MIDs("ai_provider_allowed_users", "provider_id", id, "user_id"),
			},
			Inlines: []adminInline{inline},
		}, nil
	}
	m.New = func(a *App) *adminObject {
		return &adminObject{Values: map[string]string{
			"style": "doory", "style_config": "{}", "timeout_seconds": "60", "max_image_bytes": "0",
			"audience": "everyone", "enabled": "1",
		}, Display: map[string]template.HTML{"secret_state": template.HTML("None")},
			Raw: map[string]any{}, Inlines: []adminInline{adminAIProviderModelInline(a, "")}}
	}
	m.Fieldsets = []adminFieldset{
		{Title: "Connection", Fields: []adminField{
			{Name: "name", Label: "Name", Kind: adminText, Required: true, Wide: true},
			{Name: "base_url", Label: "Base URL", Kind: adminText, Required: true, Wide: true},
			{Name: "style", Label: "Style", Kind: adminSelect},
			{Name: "style_config", Label: "Style settings", Kind: adminTextarea, Wide: true,
				Help: "JSON object with the style's settings."},
			{Name: "timeout_seconds", Label: "Timeout seconds", Kind: adminNumber},
			{Name: "max_image_bytes", Label: "Max image bytes", Kind: adminNumber},
		}},
		{Title: "Authentication", Fields: []adminField{
			{Name: "auth_header", Label: "Auth header", Kind: adminText, Wide: true},
			{Name: "secret", Label: "Secret", Kind: adminPassword, Wide: true,
				Help: "A secret is stored. Leave empty to keep it."},
			{Name: "clear_secret", Label: "Remove the stored secret", Kind: adminBool},
			{Name: "secret_state", Label: "Stored secret", Kind: adminReadonly},
		}},
		{Title: "Tasks", Fields: []adminField{
			{Name: "tasks", Label: "Tasks", Kind: adminCheckboxMulti},
			{Name: "enabled", Label: "Enabled", Kind: adminBool},
			{Name: "sort_order", Label: "Sort order", Kind: adminNumber},
		}},
		{Title: "Access", Fields: []adminField{
			{Name: "audience", Label: "Audience", Kind: adminSelect},
			{Name: "allowed_users", Label: "Allowed users", Kind: adminMultiSelect, Wide: true},
			{Name: "daily_quota", Label: "Daily quota", Kind: adminNumber},
		}},
		{Title: "Privacy", Fields: []adminField{
			{Name: "sends_offsite", Label: "Sends offsite", Kind: adminBool},
		}},
		{Title: "Audit", Fields: []adminField{
			{Name: "created_at", Label: "Created at", Kind: adminReadonly},
			{Name: "updated_at", Label: "Updated at", Kind: adminReadonly},
		}, Collapse: true},
	}
	m.Add = func(a *App, actor *UserRow, form *adminForm) (string, adminErrors, error) {
		row, errs := a.adminAIProviderValues(form, nil)
		if errs.any() {
			return "", errs, nil
		}
		row.id = newUUID4Hex()
		row.createdAt = formatDjangoDateTime(time.Now())
		row.updatedAt = row.createdAt
		if err := a.insertProvider(row, ""); err != nil {
			return "", nil, err
		}
		if err := a.adminApplyProviderAllowedUsers(row.id, form); err != nil {
			return "", nil, err
		}
		if err := a.adminApplyProviderModels(row.id, form); err != nil {
			return "", nil, err
		}
		return row.id, nil, nil
	}
	m.Change = func(a *App, actor *UserRow, id string, form *adminForm) (adminErrors, error) {
		var existing aiProviderRow
		var secret []byte
		var owner sql.NullString
		if err := a.DB.QueryRow(`SELECT id, created_at, updated_at, name, base_url, style, style_config,
			auth_header, secret_encrypted, tasks, timeout_seconds, max_image_bytes, sends_offsite,
			enabled, sort_order, audience, daily_quota, owner_id FROM ai_provider WHERE id = ?`,
			id).Scan(&existing.id, &existing.createdAt, &existing.updatedAt, &existing.name,
			&existing.baseURL, &existing.style, &existing.styleConfig, &existing.authHeader, &secret,
			&existing.tasks, &existing.timeout, &existing.maxImage, &existing.sendsOff, &existing.enabled,
			&existing.sortOrder, &existing.audience, &existing.dailyQuota, &owner); err != nil {
			return nil, err
		}
		if owner.Valid {
			return nil, errOutsideAdminScope
		}
		existing.secret = secret
		row, errs := a.adminAIProviderValues(form, &existing)
		if errs.any() {
			return errs, nil
		}
		row.id = id
		row.createdAt = existing.createdAt
		if err := a.updateProvider(row); err != nil {
			return nil, err
		}
		if err := a.adminApplyProviderAllowedUsers(id, form); err != nil {
			return nil, err
		}
		return nil, a.adminApplyProviderModels(id, form)
	}
	m.Delete = func(a *App, actor *UserRow, id string) error {
		_, _ = a.DB.Exec(`DELETE FROM ai_providermodel WHERE provider_id = ?`, id)
		_, _ = a.DB.Exec(`DELETE FROM ai_provider_allowed_users WHERE provider_id = ?`, id)
		_, err := a.DB.Exec(`DELETE FROM ai_provider WHERE id = ?`, id)
		return err
	}
	m.Repr = func(a *App, id string) string {
		var name string
		_ = a.DB.QueryRow(`SELECT name FROM ai_provider WHERE id = ?`, id).Scan(&name)
		return name
	}
	return m
}

// errOutsideAdminScope makes the change view redirect to the changelist, as
// Django does when get_object() returns None from a filtered queryset.
var errOutsideAdminScope = fmt.Errorf("object outside the admin queryset")

func adminAIProviderModelInline(a *App, providerID string) adminInline {
	inline := adminInline{
		Prefix: "models", Title: "Models", Extra: 1,
		Columns: []adminInlineColumn{
			{Name: "task", Label: "Task", Kind: adminSelect, Choices: adminAITasks},
			{Name: "remote_name", Label: "Remote name", Kind: adminText},
			{Name: "label", Label: "Label", Kind: adminText},
			{Name: "enabled", Label: "Enabled", Kind: adminBool},
			{Name: "sort_order", Label: "Sort order", Kind: adminNumber},
		},
		CanDelete: true,
	}
	if providerID == "" {
		inline.ExtraRows = []adminInlineRow{{Index: 0}}
		return inline
	}
	models, _ := a.loadProviderModels(providerID)
	for i, model := range models {
		inline.Rows = append(inline.Rows, adminInlineRow{
			Index: i, ID: model.id,
			Values: map[string]string{
				"task": model.task, "remote_name": model.remoteName, "label": model.label,
				"enabled": boolFlag(boolInt(model.enabled)), "sort_order": strconv.Itoa(model.sortOrder),
			},
		})
	}
	inline.ExtraRows = []adminInlineRow{{Index: len(models)}}
	return inline
}

func (a *App) adminApplyProviderAllowedUsers(providerID string, form *adminForm) error {
	return a.adminReplaceM2M("ai_provider_allowed_users", "provider_id", providerID, "user_id",
		adminIDValues(form.All("allowed_users")))
}

func (a *App) adminApplyProviderModels(providerID string, form *adminForm) error {
	total := form.Int("models-TOTAL_FORMS")
	now := formatDjangoDateTime(time.Now())
	for i := 0; i < total; i++ {
		prefix := fmt.Sprintf("models-%d-", i)
		id := strings.TrimSpace(form.Value(prefix + "id"))
		if form.Value(prefix+"DELETE") != "" {
			if id != "" {
				if _, err := a.DB.Exec(`DELETE FROM ai_providermodel WHERE id = ? AND provider_id = ?`,
					id, providerID); err != nil {
					return err
				}
			}
			continue
		}
		task := form.Value(prefix + "task")
		remote := strings.TrimSpace(form.Value(prefix + "remote_name"))
		label := strings.TrimSpace(form.Value(prefix + "label"))
		if task == "" && remote == "" && label == "" {
			continue
		}
		enabled := boolSQL(form.Has(prefix + "enabled"))
		sortOrder := form.Int(prefix + "sort_order")
		if id != "" {
			if _, err := a.DB.Exec(`UPDATE ai_providermodel SET task = ?, remote_name = ?, label = ?,
				enabled = ?, sort_order = ?, updated_at = ? WHERE id = ? AND provider_id = ?`,
				task, remote, label, enabled, sortOrder, now, id, providerID); err != nil {
				return err
			}
			continue
		}
		if _, err := a.DB.Exec(`INSERT INTO ai_providermodel
			(id, created_at, updated_at, task, remote_name, label, enabled, sort_order, provider_id)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, newUUID4Hex(), now, now, task, remote, label, enabled,
			sortOrder, providerID); err != nil {
			return err
		}
	}
	return nil
}

// adminAIProviderValues validates the submitted provider form.
func (a *App) adminAIProviderValues(form *adminForm, existing *aiProviderRow) (aiProviderRow, adminErrors) {
	errs := adminErrors{}
	row := aiProviderRow{}
	row.name = strings.TrimSpace(form.Value("name"))
	row.baseURL = strings.TrimSpace(form.Value("base_url"))
	row.style = form.Value("style")
	row.authHeader = strings.TrimSpace(form.Value("auth_header"))
	row.timeout = form.Int("timeout_seconds")
	row.maxImage = int64(form.Int("max_image_bytes"))
	row.sortOrder = form.Int("sort_order")
	row.audience = form.Value("audience")
	row.dailyQuota = form.Int("daily_quota")
	row.sendsOff = form.Has("sends_offsite")
	row.enabled = form.Has("enabled")

	if row.name == "" {
		errs.add("name", "This field is required.")
	} else if len([]rune(row.name)) > 80 {
		errs.add("name", "Ensure this value has at most 80 characters.")
	}
	if row.baseURL == "" {
		errs.add("base_url", "This field is required.")
	} else if _, terr := a.Settings.aiCheckURL(row.baseURL, a.Settings.aiOperatorPolicy(), false); terr != nil {
		errs.add("base_url", terr.message)
	} else if strings.ContainsAny(row.baseURL, "?#") {
		errs.add("base_url", "The server address may not have a query or fragment.")
	} else {
		row.baseURL = strings.TrimRight(row.baseURL, "/")
	}
	if !aiStyleValid(row.style) {
		errs.add("style", "Select a valid choice. That choice is not one of the available choices.")
	}
	configText := strings.TrimSpace(form.Value("style_config"))
	if configText == "" {
		configText = "{}"
	}
	var config any
	if err := json.Unmarshal([]byte(configText), &config); err != nil {
		errs.add("style_config", "The style settings must be a JSON object.")
	} else if msg := aiStyleConfigError(row.style, config); msg != nil {
		errs.add("style_config", *msg)
	} else if obj, ok := config.(map[string]any); ok {
		if pathValue, ok := obj["path"].(string); ok && pathValue != "" && !strings.HasPrefix(pathValue, "/") {
			errs.add("style_config", "path must start with /.")
		}
	}
	if compact, err := json.Marshal(config); err == nil {
		row.styleConfig = string(compact)
	} else {
		row.styleConfig = "{}"
	}
	if row.timeout == 0 {
		row.timeout = 60
	}
	if row.timeout < 1 || row.timeout > 300 {
		errs.add("timeout_seconds", "Ensure this value is between 1 and 300.")
	}
	if limit := a.Settings.AIMaxImageBytes(); row.maxImage > limit {
		errs.add("max_image_bytes", fmt.Sprintf("At most %d bytes (the server-wide limit).", limit))
	}
	if row.authHeader != "" && !aiHeaderNameOK(row.authHeader) {
		if aiHeaderNameTokenOK(row.authHeader) {
			errs.add("auth_header", fmt.Sprintf("The header name %s is set by Doory and cannot be changed.",
				row.authHeader))
		} else {
			errs.add("auth_header", "The header name is not a valid HTTP header name.")
		}
	}
	tasks := adminIDValues(form.All("tasks"))
	for _, task := range tasks {
		if !slicesContains(aiTaskIDs, task) {
			errs.add("tasks", fmt.Sprintf("Select a valid choice. %s is not one of the available choices.", task))
		}
	}
	if len(tasks) == 0 {
		errs.add("tasks", "This field is required.")
	}
	row.tasks = pyJSONStringList(tasks)
	validAudience := false
	for _, choice := range adminAIAudiences {
		if choice.Value == row.audience {
			validAudience = true
		}
	}
	if !validAudience {
		errs.add("audience", "Select a valid choice. That choice is not one of the available choices.")
	}

	// Secrets: a new value is encrypted; an empty value keeps the stored one;
	// clear_secret (or a new origin without a new secret) removes it.
	secretValue := form.Value("secret")
	if strings.ContainsAny(secretValue, "\r\n") {
		errs.add("secret", "The secret may not contain line breaks.")
	}
	if secretValue != "" && !a.Settings.canEncrypt() {
		errs.add("secret", "Secrets cannot be stored: AI_SECRETS_KEY is not a valid key.")
	}
	if existing != nil {
		row.secret = existing.secret
		moved := aiOrigin(existing.baseURL) != aiOrigin(row.baseURL)
		switch {
		case form.Has("clear_secret"):
			row.secret = []byte{}
		case moved && secretValue == "":
			row.secret = []byte{}
		case secretValue != "":
			encrypted, err := a.Settings.aiEncrypt(secretValue)
			if err != nil {
				errs.add("secret", err.Error())
			} else {
				row.secret = []byte(encrypted)
			}
		}
	} else if secretValue != "" {
		encrypted, err := a.Settings.aiEncrypt(secretValue)
		if err == nil {
			row.secret = []byte(encrypted)
		}
	}
	if row.secret == nil {
		row.secret = []byte{}
	}
	return row, errs
}

func aiHeaderNameTokenOK(name string) bool {
	for _, r := range name {
		if !(r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9' ||
			strings.ContainsRune("!#$%&'*+.^_`|~-", r)) {
			return false
		}
	}
	return name != ""
}

// ---- provider actions (network work lands with G9) ----

func adminAILoadModels(a *App, w http.ResponseWriter, r *http.Request, actor *UserRow, obj *adminObject) {
	back := "/admin/ai/provider/" + adminURLID(a.admin().byKey["ai.provider"], obj.ID) + "/change/"
	if r.Method != http.MethodPost {
		a.renderAdminConfirm(w, r, actor, a.admin().byKey["ai.provider"], obj,
			"Load models for "+obj.Repr,
			"Fetch the model list from the remote server and update the database? This makes an outgoing request to the server.",
			"Load models", r.URL.Path, back)
		return
	}
	a.flashAdminMessages(w, r, adminError("route registered but not ported: ai.provider load-models"))
	adminRedirect(w, r, back)
}

func adminAITestConnection(a *App, w http.ResponseWriter, r *http.Request, actor *UserRow, obj *adminObject) {
	back := "/admin/ai/provider/" + adminURLID(a.admin().byKey["ai.provider"], obj.ID) + "/change/"
	if r.Method != http.MethodPost {
		a.renderAdminConfirm(w, r, actor, a.admin().byKey["ai.provider"], obj,
			"Test "+obj.Repr,
			"Send a sample picture to test the connection to this provider? This makes an outgoing request to the provider's server.",
			"Test connection", r.URL.Path, back)
		return
	}
	a.renderAdminConfirmResult(w, r, actor, a.admin().byKey["ai.provider"], obj, back,
		"route registered but not ported: ai.provider test connection")
}

// adminAIEnvTest serves /admin/ai/provider/env/<id>/test/ (the environment
// providers are not rows, so this route is registered without an object id).
func (a *App) adminAIEnvTest(w http.ResponseWriter, r *http.Request, u *UserRow, envID string) {
	model := a.admin().byKey["ai.provider"]
	if !a.adminHasPerm(u, model.perm("change")) {
		a.admin().render403(w, r, u, "You do not have permission to perform this action.")
		return
	}
	providers := a.Settings.aiEnvProviders()
	if unknown := r.URL.Query().Get("x"); unknown != "" {
		_ = unknown
	}
	var found *envProvider
	for i := range providers {
		if providers[i].id == envID {
			found = &providers[i]
			break
		}
	}
	if found == nil {
		a.admin().render404(w, r, u)
		return
	}
	obj := &adminObject{ID: found.id, Repr: found.name}
	back := "/admin/ai/provider/"
	if r.Method != http.MethodPost {
		a.renderAdminConfirm(w, r, u, model, obj, "Test "+found.name,
			"Send a sample picture to test the connection to this provider? This makes an outgoing request to the provider's server.",
			"Test connection", r.URL.Path, back)
		return
	}
	a.renderAdminConfirmResult(w, r, u, model, obj, back,
		"route registered but not ported: ai.provider env test connection")
}

func (a *App) renderAdminConfirm(w http.ResponseWriter, r *http.Request, u *UserRow, model *adminModel,
	obj *adminObject, title, message, confirm, actionURL, backURL string,
) {
	base := a.adminBasePage(r, u)
	base.Title = title
	base.Breadcrumbs = []adminCrumb{{Label: model.Plural, URL: "/admin/" + model.AppLabel + "/" + model.Model + "/"},
		{Label: title}}
	a.admin().render(w, r, http.StatusOK, "confirm_action", &adminConfirmPage{
		adminBase: base, Model: model, Provider: obj, Message: message, Confirm: confirm,
		ActionURL: actionURL, BackURL: backURL,
	})
}

func (a *App) renderAdminConfirmResult(w http.ResponseWriter, r *http.Request, u *UserRow, model *adminModel,
	obj *adminObject, backURL, message string,
) {
	base := a.adminBasePage(r, u)
	base.Title = "Test of " + obj.Repr
	base.Breadcrumbs = []adminCrumb{{Label: model.Plural, URL: "/admin/" + model.AppLabel + "/" + model.Model + "/"},
		{Label: base.Title}}
	a.admin().render(w, r, http.StatusOK, "confirm_action", &adminConfirmPage{
		adminBase: base, Model: model, Provider: obj, BackURL: backURL, TestMessage: message,
	})
}

// ---- UserProvider (read only) ----

func adminAIUserProviderModel() *adminModel {
	m := &adminModel{
		AppLabel: "ai", Model: "userprovider", Verbose: "user provider", Plural: "User providers",
		Name: "user provider", Table: "ai_provider",
		Columns: []adminColumn{
			{Name: "name", Label: "Name", Sort: "p.name", Link: true},
			{Name: "owner", Label: "Owner", Sort: "u.email"},
			{Name: "style", Label: "Style", Sort: "p.style"},
			{Name: "task_list", Label: "Tasks"},
			{Name: "enabled", Label: "Enabled", Sort: "p.enabled"},
			{Name: "created_at", Label: "Created at", Sort: "p.created_at"},
		},
		LinkName:     "name",
		SearchFields: []adminSearchField{{SQL: "p.name"}, {SQL: "u.email"}},
		Filters: []adminFilter{
			{Label: "Style", Param: "style__exact", Kind: "choice", Column: "p.style",
				Options: adminAIProviderStyles},
			{Label: "Enabled", Param: "enabled__exact", Kind: "bool", Column: "p.enabled"},
		},
		BaseWhere: "p.owner_id IS NOT NULL",
		Ordering:  []string{"p.sort_order ASC", "p.name ASC"},
		SelectSQL: "p.id, p.name, COALESCE(u.email, ''), p.style, p.tasks, p.enabled, p.created_at",
		FromSQL:   "ai_provider p LEFT JOIN accounts_user u ON u.id = p.owner_id",
		ScanList: func(a *App, rows *sql.Rows) (adminListRow, error) {
			var id, name, owner, style, tasks, createdAt string
			var enabled int
			if err := rows.Scan(&id, &name, &owner, &style, &tasks, &enabled, &createdAt); err != nil {
				return adminListRow{}, err
			}
			taskList := strings.Join(adminJSONList(tasks), ", ")
			if taskList == "" {
				taskList = "—"
			}
			return adminListRow{ID: id, Cells: map[string]adminCell{
				"name":       adminTextCell(name),
				"owner":      adminTextCell(owner),
				"style":      adminTextCell(style),
				"task_list":  adminTextCell(taskList),
				"enabled":    adminTextCell(adminBoolText(enabled)),
				"created_at": adminTextCell(createdAt),
			}}, nil
		},
		AllowAdd: false, AllowChange: false, AllowDelete: false,
		NoChangePermission: true,
	}
	m.Get = func(a *App, id string) (*adminObject, error) {
		var row aiProviderRow
		var owner sql.NullString
		var secret []byte
		err := a.DB.QueryRow(`SELECT p.id, p.created_at, p.updated_at, p.name, p.base_url, p.style,
			p.style_config, p.auth_header, p.secret_encrypted, p.tasks, p.timeout_seconds,
			p.max_image_bytes, p.sends_offsite, p.enabled, p.sort_order, p.audience, p.daily_quota,
			p.owner_id FROM ai_provider p WHERE p.id = ?`, id).Scan(&row.id, &row.createdAt,
			&row.updatedAt, &row.name, &row.baseURL, &row.style, &row.styleConfig, &row.authHeader,
			&secret, &row.tasks, &row.timeout, &row.maxImage, &row.sendsOff, &row.enabled,
			&row.sortOrder, &row.audience, &row.dailyQuota, &owner)
		if err != nil {
			return nil, err
		}
		if !owner.Valid {
			return nil, errOutsideAdminScope
		}
		var email string
		_ = a.DB.QueryRow(`SELECT email FROM accounts_user WHERE id = ?`, owner.String).Scan(&email)
		row.secret = secret
		secretState := "None"
		if row.hasSecret() {
			secretState = "Set (hidden)"
		}
		taskList := strings.Join(adminJSONList(row.tasks), ", ")
		if taskList == "" {
			taskList = "—"
		}
		var modelNames []string
		models, _ := a.loadProviderModels(id)
		for _, model := range models {
			label := model.label
			if label == "" {
				label = model.remoteName
			}
			if label == "" {
				label = "Default"
			}
			modelNames = append(modelNames, label)
		}
		modelList := strings.Join(modelNames, ", ")
		if modelList == "" {
			modelList = "—"
		}
		return &adminObject{
			ID: id, Repr: row.name,
			Values: map[string]string{"name": row.name},
			Display: map[string]template.HTML{
				"owner":             template.HTML(djangoEscape(email)),
				"name":              template.HTML(djangoEscape(row.name)),
				"base_url":          template.HTML(djangoEscape(row.baseURL)),
				"style":             template.HTML(djangoEscape(row.style)),
				"style_config_json": adminPrettyJSON(row.styleConfig),
				"auth_header":       template.HTML(djangoEscape(row.authHeader)),
				"secret_state":      template.HTML(djangoEscape(secretState)),
				"task_list":         template.HTML(djangoEscape(taskList)),
				"model_list":        template.HTML(djangoEscape(modelList)),
				"timeout_seconds":   template.HTML(djangoEscape(strconv.Itoa(row.timeout))),
				"max_image_bytes":   template.HTML(djangoEscape(strconv.FormatInt(row.maxImage, 10))),
				"sends_offsite":     template.HTML(djangoEscape(adminBoolText(boolInt(row.sendsOff)))),
				"enabled":           template.HTML(djangoEscape(adminBoolText(boolInt(row.enabled)))),
				"created_at":        template.HTML(djangoEscape(row.createdAt)),
				"updated_at":        template.HTML(djangoEscape(row.updatedAt)),
			},
			Raw: map[string]any{},
		}, nil
	}
	m.Fieldsets = []adminFieldset{{Title: "", Fields: []adminField{
		{Name: "owner", Label: "Owner", Kind: adminReadonly},
		{Name: "name", Label: "Name", Kind: adminReadonly},
		{Name: "base_url", Label: "Base URL", Kind: adminReadonly, Wide: true},
		{Name: "style", Label: "Style", Kind: adminReadonly},
		{Name: "style_config_json", Label: "Style settings", Kind: adminReadonly, Wide: true},
		{Name: "auth_header", Label: "Auth header", Kind: adminReadonly},
		{Name: "secret_state", Label: "Stored secret", Kind: adminReadonly},
		{Name: "task_list", Label: "Tasks", Kind: adminReadonly},
		{Name: "model_list", Label: "Models", Kind: adminReadonly},
		{Name: "timeout_seconds", Label: "Timeout seconds", Kind: adminReadonly},
		{Name: "max_image_bytes", Label: "Max image bytes", Kind: adminReadonly},
		{Name: "sends_offsite", Label: "Sends offsite", Kind: adminReadonly},
		{Name: "enabled", Label: "Enabled", Kind: adminReadonly},
		{Name: "created_at", Label: "Created at", Kind: adminReadonly},
		{Name: "updated_at", Label: "Updated at", Kind: adminReadonly},
	}}}
	m.Repr = func(a *App, id string) string {
		var name string
		_ = a.DB.QueryRow(`SELECT name FROM ai_provider WHERE id = ?`, id).Scan(&name)
		return name
	}
	return m
}

// ---- AICall (read only) ----

func adminAICallModel() *adminModel {
	m := &adminModel{
		AppLabel: "ai", Model: "aicall", Verbose: "AI call", Plural: "AI calls", Name: "AI call",
		Table: "ai_aicall",
		Columns: []adminColumn{
			{Name: "created_at", Label: "Created at", Sort: "c.created_at", Link: true},
			{Name: "user", Label: "User", Sort: "u.email"},
			{Name: "task", Label: "Task", Sort: "c.task"},
			{Name: "provider_name", Label: "Provider", Sort: "c.provider_name"},
			{Name: "model", Label: "Model", Sort: "c.model"},
			{Name: "status", Label: "Status", Sort: "c.status"},
			{Name: "error_code", Label: "Error code", Sort: "c.error_code"},
			{Name: "duration_ms", Label: "Duration (ms)", Sort: "c.duration_ms"},
		},
		LinkName: "created_at",
		SearchFields: []adminSearchField{{SQL: "u.email"}, {SQL: "c.provider_ref"},
			{SQL: "c.provider_name"}, {SQL: "c.model"}},
		Filters: []adminFilter{
			{Label: "Status", Param: "status__exact", Kind: "choice", Column: "c.status",
				Options: []adminFilterChoice{adminOption("ok", "OK"), adminOption("error", "Error"),
					adminOption("denied", "Denied")}},
			{Label: "Task", Param: "task__exact", Kind: "choice", Column: "c.task", Options: adminAITasks},
			{Label: "Provider", Param: "provider_name__exact", Kind: "choice", Column: "c.provider_name",
				OptionsFn: func(a *App) []adminFilterChoice {
					return adminDistinctChoices(a, "ai_aicall", "provider_name")
				}},
			{Label: "Created at", Param: "created_at", Kind: "date", Column: "c.created_at"},
		},
		DateHierarchy: "c.created_at",
		Ordering:      []string{"c.created_at DESC"},
		SelectSQL: "c.id, c.created_at, COALESCE(u.email, ''), c.task, c.provider_name, c.model, " +
			"c.status, c.error_code, c.duration_ms",
		FromSQL: "ai_aicall c LEFT JOIN accounts_user u ON u.id = c.user_id",
		ScanList: func(a *App, rows *sql.Rows) (adminListRow, error) {
			var id, createdAt, user, task, provider, model, status, errorCode string
			var duration int
			if err := rows.Scan(&id, &createdAt, &user, &task, &provider, &model, &status,
				&errorCode, &duration); err != nil {
				return adminListRow{}, err
			}
			return adminListRow{ID: id, Cells: map[string]adminCell{
				"created_at":    adminTextCell(createdAt),
				"user":          adminTextCell(user),
				"task":          adminTextCell(task),
				"provider_name": adminTextCell(provider),
				"model":         adminTextCell(model),
				"status":        adminTextCell(status),
				"error_code":    adminTextCell(errorCode),
				"duration_ms":   adminIntCell(duration),
			}}, nil
		},
		AllowAdd: false, AllowChange: false, AllowDelete: false,
		NoChangePermission: true,
	}
	m.Get = func(a *App, id string) (*adminObject, error) {
		var createdAt, user, task, providerRef, providerName, model, status, errorCode string
		var duration int
		var bytesIn, bytesOut int64
		err := a.DB.QueryRow(`SELECT c.created_at, COALESCE(u.email, ''), c.task, c.provider_ref,
			c.provider_name, c.model, c.status, c.error_code, c.duration_ms, c.bytes_in, c.bytes_out
			FROM ai_aicall c LEFT JOIN accounts_user u ON u.id = c.user_id WHERE c.id = ?`, id).Scan(
			&createdAt, &user, &task, &providerRef, &providerName, &model, &status, &errorCode,
			&duration, &bytesIn, &bytesOut)
		if err != nil {
			return nil, err
		}
		field := func(value string) template.HTML { return template.HTML(djangoEscape(value)) }
		return &adminObject{
			ID: id, Repr: createdAt,
			Values: map[string]string{},
			Display: map[string]template.HTML{
				"created_at": field(createdAt), "user": field(user), "task": field(task),
				"provider_ref": field(providerRef), "provider_name": field(providerName),
				"model": field(model), "status": field(status), "error_code": field(errorCode),
				"duration_ms": field(strconv.Itoa(duration)),
				"bytes_in":    field(strconv.FormatInt(bytesIn, 10)),
				"bytes_out":   field(strconv.FormatInt(bytesOut, 10)),
			},
			Raw: map[string]any{},
		}, nil
	}
	m.Fieldsets = []adminFieldset{{Title: "", Fields: []adminField{
		{Name: "created_at", Label: "Created at", Kind: adminReadonly},
		{Name: "user", Label: "User", Kind: adminReadonly},
		{Name: "task", Label: "Task", Kind: adminReadonly},
		{Name: "provider_ref", Label: "Provider ref", Kind: adminReadonly},
		{Name: "provider_name", Label: "Provider name", Kind: adminReadonly},
		{Name: "model", Label: "Model", Kind: adminReadonly},
		{Name: "status", Label: "Status", Kind: adminReadonly},
		{Name: "error_code", Label: "Error code", Kind: adminReadonly},
		{Name: "duration_ms", Label: "Duration (ms)", Kind: adminReadonly},
		{Name: "bytes_in", Label: "Bytes in", Kind: adminReadonly},
		{Name: "bytes_out", Label: "Bytes out", Kind: adminReadonly},
	}}}
	m.Repr = func(a *App, id string) string {
		var createdAt string
		_ = a.DB.QueryRow(`SELECT created_at FROM ai_aicall WHERE id = ?`, id).Scan(&createdAt)
		return createdAt
	}
	return m
}
