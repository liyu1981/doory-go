// Registered admin models (the oracle's 19 + auth.Group) and the shared
// list/form helpers they use.
package doory

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"html/template"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// adminRegistry is AdminSite._registry in registration order. The order drives
// the index page grouping; Django sorts apps/models alphabetically on the
// index but keeps registration order inside an app.
func adminRegistry() []*adminModel {
	var models []*adminModel
	models = append(models, adminModelsAuth()...)
	models = append(models, adminModelsAccounts()...)
	models = append(models, adminModelsDesigns()...)
	models = append(models, adminModelsAssets()...)
	models = append(models, adminModelsGallery()...)
	models = append(models, adminModelsElements()...)
	models = append(models, adminModelsFonts()...)
	models = append(models, adminModelsBrand()...)
	models = append(models, adminModelsSharing()...)
	models = append(models, adminModelsPresets()...)
	models = append(models, adminModelsApiTokens()...)
	models = append(models, adminModelsAI()...)
	return models
}

// ---- shared formatting helpers ----

func adminBoolText(v int) string {
	if v != 0 {
		return "True"
	}
	return "False"
}

func adminBoolInt(v sql.NullInt64) int {
	if v.Valid {
		return int(v.Int64)
	}
	return 0
}

func adminNullText(v sql.NullString) string {
	if v.Valid {
		return v.String
	}
	return ""
}

func adminMediaURL(rel string) string {
	if rel == "" {
		return ""
	}
	if strings.HasPrefix(rel, "http://") || strings.HasPrefix(rel, "https://") || strings.HasPrefix(rel, "/") {
		return rel
	}
	return mediaURL + rel
}

// adminJSONList decodes a JSON array column into strings.
func adminJSONList(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	var out []string
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil
	}
	return out
}

// adminJSONText is a JSONField's stored text. Django serialises with
// json.dumps, which separates items with ", " (not encoding/json's ",").
func adminJSONText(values []string) string {
	if values == nil {
		values = []string{}
	}
	return pyJSONStringList(values)
}

// adminPrettyJSON ports admin pretty_document/document_json: sorted keys,
// two-space indent, HTML escaped, truncated at 200k characters.
func adminPrettyJSON(raw string) template.HTML {
	var value any
	if err := json.Unmarshal([]byte(raw), &value); err != nil {
		return template.HTML(djangoEscape(raw))
	}
	var buf strings.Builder
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(value); err != nil {
		return template.HTML(djangoEscape(raw))
	}
	text := strings.TrimSuffix(buf.String(), "\n")
	note := ""
	if len(text) > 200000 {
		text, note = text[:200000], "\n... (truncated)"
	}
	return template.HTML("<pre>" + djangoEscape(text+note) + "</pre>")
}

// adminDecodeTime turns a stored timestamp into a date cell.
func adminDateCell(raw sql.NullString) adminCell {
	if !raw.Valid || raw.String == "" {
		return adminTextCell("-")
	}
	return adminTextCell(raw.String)
}

// adminMediaFile writes an uploaded file under MEDIA_ROOT (Django's storage
// keeps the name and appends a random suffix on collision).
func (a *App) adminStoreUpload(dir, filename string, data []byte) (string, error) {
	name := dir + "/" + filepath.Base(filepath.FromSlash(filename))
	name = availableThumbnailName(a.Settings.MediaRoot, name)
	absolute := filepath.Join(a.Settings.MediaRoot, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(absolute), 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(absolute, data, 0o644); err != nil {
		return "", err
	}
	return name, nil
}

func adminIntCell(v int) adminCell { return adminTextCell(strconv.Itoa(v)) }

// adminOption builds a select choice.
func adminOption(value, label string) adminFilterChoice {
	return adminFilterChoice{Value: value, Label: label}
}

// adminPermissionChoices lists every auth_permission as "app | name".
func (a *App) adminPermissionChoices() []adminFilterChoice {
	rows, err := a.DB.Query(`SELECT p.id, ct.app_label, p.name FROM auth_permission p
		JOIN django_content_type ct ON ct.id = p.content_type_id ORDER BY p.id`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []adminFilterChoice
	for rows.Next() {
		var id int
		var app, name string
		if err := rows.Scan(&id, &app, &name); err != nil {
			continue
		}
		out = append(out, adminOption(strconv.Itoa(id), app+" | "+name))
	}
	return out
}

func (a *App) adminGroupChoices() []adminFilterChoice {
	rows, err := a.DB.Query(`SELECT id, name FROM auth_group ORDER BY name`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []adminFilterChoice
	for rows.Next() {
		var id int
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			continue
		}
		out = append(out, adminOption(strconv.Itoa(id), name))
	}
	return out
}

// m2mIDs returns the selected ids from a repeated form value.
func adminIDValues(values []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	return out
}

// adminReplaceM2M rewrites a join table for one owner id.
func (a *App) adminReplaceM2M(table, ownerColumn, ownerID, targetColumn string, ids []string) error {
	if _, err := a.DB.Exec("DELETE FROM "+table+" WHERE "+ownerColumn+" = ?", ownerID); err != nil {
		return err
	}
	for _, id := range ids {
		if _, err := a.DB.Exec("INSERT INTO "+table+" ("+ownerColumn+", "+targetColumn+") VALUES (?, ?)",
			ownerID, id); err != nil {
			return err
		}
	}
	return nil
}

func (a *App) adminM2MIDs(table, ownerColumn, ownerID, targetColumn string) []string {
	rows, err := a.DB.Query("SELECT "+targetColumn+" FROM "+table+" WHERE "+ownerColumn+" = ?", ownerID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id int
		if err := rows.Scan(&id); err == nil {
			out = append(out, strconv.Itoa(id))
		}
	}
	return out
}

// ---- auth.Group ----

func adminModelsAuth() []*adminModel {
	group := &adminModel{
		AppLabel: "auth", Model: "group", Verbose: "group", Plural: "Groups", Name: "group",
		Table: "auth_group", IntPK: true,
		PlainAdmin: true,
		Columns: []adminColumn{
			{Name: "name", Label: "Name", Sort: "g.name"},
		},
		LinkName:     "name",
		SearchFields: []adminSearchField{{SQL: "g.name"}},
		Ordering:     []string{"g.name ASC"},
		SelectSQL:    "g.id, g.name",
		FromSQL:      "auth_group g",
		ScanList: func(a *App, rows *sql.Rows) (adminListRow, error) {
			var id int
			var name string
			if err := rows.Scan(&id, &name); err != nil {
				return adminListRow{}, err
			}
			return adminListRow{ID: strconv.Itoa(id), Cells: map[string]adminCell{"name": adminTextCell(name)}}, nil
		},
		AllowAdd: true, AllowChange: true, AllowDelete: true,
		Choices: map[string]func(a *App) []adminFilterChoice{
			"permissions": func(a *App) []adminFilterChoice { return a.adminPermissionChoices() },
		},
		Get: func(a *App, id string) (*adminObject, error) {
			var gid int
			var name string
			if err := a.DB.QueryRow(`SELECT id, name FROM auth_group WHERE id = ?`, id).Scan(&gid, &name); err != nil {
				return nil, err
			}
			perms := a.adminM2MIDs("auth_group_permissions", "group_id", strconv.Itoa(gid), "permission_id")
			return &adminObject{
				ID:      strconv.Itoa(gid),
				Repr:    name,
				Values:  map[string]string{"name": name},
				Display: map[string]template.HTML{},
				Raw:     map[string]any{"permissions": perms},
			}, nil
		},
		Add: func(a *App, actor *UserRow, form *adminForm) (string, adminErrors, error) {
			errs := adminErrors{}
			name := strings.TrimSpace(form.Value("name"))
			if name == "" {
				errs.add("name", "This field is required.")
			}
			if errs.any() {
				return "", errs, nil
			}
			res, err := a.DB.Exec(`INSERT INTO auth_group (name) VALUES (?)`, name)
			if err != nil {
				return "", nil, err
			}
			id, _ := res.LastInsertId()
			if err := a.adminReplaceM2M("auth_group_permissions", "group_id", strconv.FormatInt(id, 10),
				"permission_id", adminIDValues(form.All("permissions"))); err != nil {
				return "", nil, err
			}
			return strconv.FormatInt(id, 10), nil, nil
		},
		Change: func(a *App, actor *UserRow, id string, form *adminForm) (adminErrors, error) {
			errs := adminErrors{}
			name := strings.TrimSpace(form.Value("name"))
			if name == "" {
				errs.add("name", "This field is required.")
			}
			if errs.any() {
				return errs, nil
			}
			if _, err := a.DB.Exec(`UPDATE auth_group SET name = ? WHERE id = ?`, name, id); err != nil {
				return nil, err
			}
			return nil, a.adminReplaceM2M("auth_group_permissions", "group_id", id,
				"permission_id", adminIDValues(form.All("permissions")))
		},
		Delete: func(a *App, actor *UserRow, id string) error {
			_, err := a.DB.Exec(`DELETE FROM auth_group WHERE id = ?`, id)
			return err
		},
		Repr: func(a *App, id string) string {
			var name string
			_ = a.DB.QueryRow(`SELECT name FROM auth_group WHERE id = ?`, id).Scan(&name)
			return name
		},
	}
	group.Fieldsets = []adminFieldset{
		{Title: "", Fields: []adminField{
			{Name: "name", Label: "Name", Kind: adminText, Required: true, Wide: true},
		}},
		{Title: "Permissions", Fields: []adminField{
			{Name: "permissions", Label: "Permissions", Kind: adminMultiSelect, Choices: nil, Wide: true},
		}},
	}
	return []*adminModel{group}
}

// ---- accounts.User ----

func adminModelsAccounts() []*adminModel {
	user := &adminModel{
		AppLabel: "accounts", Model: "user", Verbose: "user", Plural: "Users", Name: "user",
		Table: "accounts_user",
		Columns: []adminColumn{
			{Name: "email", Label: "Email", Sort: "u.email", Link: true},
			{Name: "display_name", Label: "Display name", Sort: "u.display_name"},
			{Name: "is_email_verified", Label: "Is email verified", Sort: "u.is_email_verified"},
			{Name: "is_active", Label: "Is active", Sort: "u.is_active"},
			{Name: "is_staff", Label: "Is staff", Sort: "u.is_staff"},
			{Name: "google", Label: "Google"},
			{Name: "created_at", Label: "Created at", Sort: "u.created_at"},
		},
		LinkName:     "email",
		SearchFields: []adminSearchField{{SQL: "u.email"}, {SQL: "u.display_name"}},
		Filters: []adminFilter{
			{Label: "Is active", Param: "is_active__exact", Kind: "bool", Column: "u.is_active"},
			{Label: "Is staff", Param: "is_staff__exact", Kind: "bool", Column: "u.is_staff"},
			{Label: "Is superuser", Param: "is_superuser__exact", Kind: "bool", Column: "u.is_superuser"},
			{Label: "Is email verified", Param: "is_email_verified__exact", Kind: "bool", Column: "u.is_email_verified"},
			{Label: "Created at", Param: "created_at", Kind: "date", Column: "u.created_at"},
		},
		DateHierarchy: "u.created_at",
		Ordering:      []string{"u.created_at DESC"},
		PerPage:       50,
		ListEditable:  nil,
		SelectSQL: "u.id, u.email, u.display_name, u.is_email_verified, u.is_active, u.is_staff, " +
			"u.google_sub, u.created_at",
		FromSQL: "accounts_user u",
		ScanList: func(a *App, rows *sql.Rows) (adminListRow, error) {
			var id, email, displayName, createdAt string
			var verified, active, staff int
			var googleSub sql.NullString
			if err := rows.Scan(&id, &email, &displayName, &verified, &active, &staff, &googleSub, &createdAt); err != nil {
				return adminListRow{}, err
			}
			return adminListRow{ID: id, Cells: map[string]adminCell{
				"email":             adminTextCell(email),
				"display_name":      adminTextCell(displayName),
				"is_email_verified": adminTextCell(adminBoolText(verified)),
				"is_active":         adminTextCell(adminBoolText(active)),
				"is_staff":          adminTextCell(adminBoolText(staff)),
				"google":            adminTextCell(adminBoolText(boolInt(googleSub.Valid))),
				"created_at":        adminTextCell(createdAt),
			}}, nil
		},
		AllowAdd: true, AllowChange: true, AllowDelete: true,
		Choices: map[string]func(a *App) []adminFilterChoice{
			"groups":           func(a *App) []adminFilterChoice { return a.adminGroupChoices() },
			"user_permissions": func(a *App) []adminFilterChoice { return a.adminPermissionChoices() },
		},
		Actions: []adminAction{
			{Name: "mark_verified", Label: "Mark selected users as email-verified",
				Run: func(a *App, actor *UserRow, ids []string) ([]adminMessage, error) {
					n, err := a.adminUpdateSelected("accounts_user", "is_email_verified", "TRUE", ids, "is_email_verified = FALSE")
					return []adminMessage{adminSuccess(fmt.Sprintf("%d user(s) marked as verified.", n))}, err
				}},
			{Name: "activate_users", Label: "Activate selected users",
				Run: func(a *App, actor *UserRow, ids []string) ([]adminMessage, error) {
					n, err := a.adminUpdateSelected("accounts_user", "is_active", "TRUE", ids, "is_active = FALSE")
					return []adminMessage{adminSuccess(fmt.Sprintf("%d user(s) activated.", n))}, err
				}},
			{Name: "deactivate_users", Label: "Deactivate selected users",
				Run: func(a *App, actor *UserRow, ids []string) ([]adminMessage, error) {
					ids = adminWithout(ids, actor.ID)
					n, err := a.adminUpdateSelected("accounts_user", "is_active", "FALSE", ids, "is_active = TRUE")
					return []adminMessage{adminSuccess(fmt.Sprintf("%d user(s) deactivated.", n))}, err
				}},
		},
	}
	user.Get = func(a *App, id string) (*adminObject, error) {
		var row struct {
			id, email, password, displayName, avatar, createdAt, updatedAt string
			googleSub                                                      sql.NullString
			lastLogin                                                      sql.NullString
			verified, active, staff, superuser                             int
		}
		err := a.DB.QueryRow(`SELECT id, email, password, display_name, avatar, google_sub, last_login,
			created_at, updated_at, is_email_verified, is_active, is_staff, is_superuser
			FROM accounts_user WHERE id = ?`, id).Scan(&row.id, &row.email, &row.password, &row.displayName,
			&row.avatar, &row.googleSub, &row.lastLogin, &row.createdAt, &row.updatedAt,
			&row.verified, &row.active, &row.staff, &row.superuser)
		if err != nil {
			return nil, err
		}
		display := map[string]template.HTML{
			"created_at": template.HTML(djangoEscape(row.createdAt)),
			"updated_at": template.HTML(djangoEscape(row.updatedAt)),
			"last_login": template.HTML("-"),
			"google_sub": template.HTML("-"),
		}
		if row.lastLogin.Valid && row.lastLogin.String != "" {
			display["last_login"] = template.HTML(djangoEscape(row.lastLogin.String))
		}
		if row.googleSub.Valid && row.googleSub.String != "" {
			display["google_sub"] = template.HTML(djangoEscape(row.googleSub.String))
		}
		if row.avatar != "" {
			display["avatar_preview"] = template.HTML(fmt.Sprintf(
				`<img src="%s" alt="" width="64" height="64" style="border-radius:50%%;object-fit:cover">`,
				djangoEscape(adminMediaURL(row.avatar))))
		} else {
			display["avatar_preview"] = template.HTML("-")
		}
		obj := &adminObject{
			ID:   row.id,
			Repr: row.email,
			Values: map[string]string{
				"email": row.email, "password": row.password, "display_name": row.displayName,
				"avatar": row.avatar, "is_email_verified": boolFlag(row.verified),
				"is_active": boolFlag(row.active), "is_staff": boolFlag(row.staff),
				"is_superuser": boolFlag(row.superuser), "created_at": row.createdAt,
				"updated_at": row.updatedAt,
			},
			Display: display,
			Raw: map[string]any{
				"groups":           a.adminM2MIDs("accounts_user_groups", "user_id", row.id, "group_id"),
				"user_permissions": a.adminM2MIDs("accounts_user_user_permissions", "user_id", row.id, "permission_id"),
			},
		}
		return obj, nil
	}
	user.New = func(a *App) *adminObject {
		obj := &adminObject{
			Values:  map[string]string{"usable_password": "true", "is_active": "1"},
			Display: map[string]template.HTML{},
			Raw:     map[string]any{},
		}
		return obj
	}
	user.Fieldsets = []adminFieldset{
		{Title: "", Fields: []adminField{
			{Name: "email", Label: "Email", Kind: adminEmail, Required: true, Wide: true},
			{Name: "password", Label: "Password", Kind: adminReadonly, Wide: true},
		}},
		{Title: "Profile", Fields: []adminField{
			{Name: "display_name", Label: "Display name", Kind: adminText, Wide: true},
			{Name: "avatar", Label: "Avatar", Kind: adminFile},
			{Name: "avatar_preview", Label: "Avatar", Kind: adminReadonly},
		}},
		{Title: "Verification", Fields: []adminField{
			{Name: "is_email_verified", Label: "Is email verified", Kind: adminBool},
			{Name: "google_sub", Label: "Google sub", Kind: adminReadonly},
		}},
		{Title: "Permissions", Fields: []adminField{
			{Name: "is_active", Label: "Is active", Kind: adminBool},
			{Name: "is_staff", Label: "Is staff", Kind: adminBool},
			{Name: "is_superuser", Label: "Is superuser", Kind: adminBool},
			{Name: "groups", Label: "Groups", Kind: adminMultiSelect, Wide: true},
			{Name: "user_permissions", Label: "User permissions", Kind: adminMultiSelect, Wide: true},
		}},
		{Title: "Dates", Fields: []adminField{
			{Name: "last_login", Label: "Last login", Kind: adminReadonly},
			{Name: "created_at", Label: "Created at", Kind: adminReadonly},
			{Name: "updated_at", Label: "Updated at", Kind: adminReadonly},
		}},
	}
	user.AddFieldsets = []adminFieldset{
		{Title: "", Fields: []adminField{
			{Name: "email", Label: "Email", Kind: adminEmail, Required: true, Wide: true},
			{Name: "display_name", Label: "Display name", Kind: adminText, Wide: true},
			{Name: "usable_password", Label: "Password-based authentication", Kind: adminSelect,
				Choices: []adminFilterChoice{adminOption("true", "Yes"), adminOption("false", "No")}, Wide: true},
			{Name: "password1", Label: "Password", Kind: adminPassword},
			{Name: "password2", Label: "Password confirmation", Kind: adminPassword},
		}},
	}
	user.Add = func(a *App, actor *UserRow, form *adminForm) (string, adminErrors, error) {
		errs := adminErrors{}
		email := strings.ToLower(strings.TrimSpace(form.Value("email")))
		displayName := strings.TrimSpace(form.Value("display_name"))
		if email == "" {
			errs.add("email", "This field is required.")
		} else if !validEmail(email) {
			errs.add("email", "Enter a valid email address.")
		} else {
			var existing int
			_ = a.DB.QueryRow(`SELECT COUNT(*) FROM accounts_user WHERE LOWER(email) = ?`, email).Scan(&existing)
			if existing > 0 {
				errs.add("email", "User with this Email already exists.")
			}
		}
		usable := form.Value("usable_password") != "false"
		password := ""
		if usable {
			p1, p2 := form.Value("password1"), form.Value("password2")
			switch {
			case p1 == "":
				errs.add("password1", "This field is required.")
			case p2 == "":
				errs.add("password2", "This field is required.")
			case p1 != p2:
				errs.add("password2", "The two password fields didn’t match.")
			default:
				for _, perr := range validateNewPassword(p1, email) {
					errs.add("password1", perr.Message)
				}
				password = p1
			}
		}
		if errs.any() {
			return "", errs, nil
		}
		if !usable {
			password = "!" + randomAlphaNumeric(40)
		}
		created, err := a.createUser(email, password, displayName)
		if err != nil {
			return "", nil, err
		}
		if !usable {
			_, _ = a.DB.Exec(`UPDATE accounts_user SET password = ? WHERE id = ?`, "!"+randomAlphaNumeric(40), created.ID)
		}
		return created.ID, nil, nil
	}
	user.Change = func(a *App, actor *UserRow, id string, form *adminForm) (adminErrors, error) {
		errs := adminErrors{}
		email := strings.ToLower(strings.TrimSpace(form.Value("email")))
		if email == "" {
			errs.add("email", "This field is required.")
		} else if !validEmail(email) {
			errs.add("email", "Enter a valid email address.")
		} else {
			var existing int
			_ = a.DB.QueryRow(`SELECT COUNT(*) FROM accounts_user WHERE LOWER(email) = ? AND id <> ?`,
				email, id).Scan(&existing)
			if existing > 0 {
				errs.add("email", "User with this Email already exists.")
			}
		}
		if errs.any() {
			return errs, nil
		}
		displayName := strings.TrimSpace(form.Value("display_name"))
		avatar := ""
		if err := a.DB.QueryRow(`SELECT avatar FROM accounts_user WHERE id = ?`, id).Scan(&avatar); err != nil {
			return nil, err
		}
		if upload := form.File("avatar"); upload != nil {
			if len(upload.Data) == 0 {
				errs.add("avatar", "The submitted file is empty.")
				return errs, nil
			}
			stored, err := a.adminStoreUpload("avatars", upload.Filename, upload.Data)
			if err != nil {
				return nil, err
			}
			if avatar != "" {
				a.deleteMediaFile(avatar)
			}
			avatar = stored
		}
		_, err := a.DB.Exec(`UPDATE accounts_user SET email = ?, display_name = ?, avatar = ?,
			google_sub = ?, is_email_verified = ?, is_active = ?, is_staff = ?, is_superuser = ?, updated_at = ?
			WHERE id = ?`, email, displayName, avatar, nullIfEmpty(form.Value("google_sub")),
			boolSQL(form.Has("is_email_verified")), boolSQL(form.Has("is_active")),
			boolSQL(form.Has("is_staff")), boolSQL(form.Has("is_superuser")),
			formatDjangoDateTime(time.Now()), id)
		if err != nil {
			return nil, err
		}
		if err := a.adminReplaceM2M("accounts_user_groups", "user_id", id, "group_id",
			adminIDValues(form.All("groups"))); err != nil {
			return nil, err
		}
		return nil, a.adminReplaceM2M("accounts_user_user_permissions", "user_id", id,
			"permission_id", adminIDValues(form.All("user_permissions")))
	}
	user.Delete = func(a *App, actor *UserRow, id string) error {
		var avatar string
		_ = a.DB.QueryRow(`SELECT avatar FROM accounts_user WHERE id = ?`, id).Scan(&avatar)
		if _, err := a.DB.Exec(`DELETE FROM accounts_user WHERE id = ?`, id); err != nil {
			return err
		}
		if avatar != "" {
			a.deleteMediaFile(avatar)
		}
		return nil
	}
	user.Repr = func(a *App, id string) string {
		var email string
		_ = a.DB.QueryRow(`SELECT email FROM accounts_user WHERE id = ?`, id).Scan(&email)
		return email
	}
	return []*adminModel{user}
}

func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

func boolFlag(v int) string {
	if v != 0 {
		return "1"
	}
	return ""
}

// boolSQL is the value bound to a boolean column. It must be a real boolean
// (not the SQL literals TRUE/FALSE, which SQLite stores as text): Django
// writes 1/0 on SQLite and true/false on Postgres.
func boolSQL(v bool) bool { return v }

func nullIfEmpty(v string) any {
	if strings.TrimSpace(v) == "" {
		return nil
	}
	return strings.TrimSpace(v)
}

func adminWithout(ids []string, drop string) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if id != drop {
			out = append(out, id)
		}
	}
	return out
}

// adminUpdateSelected updates the given rows, optionally constrained by a
// WHERE expression, and returns the affected count.
func (a *App) adminUpdateSelected(table, column, value string, ids []string, extraWhere string) (int, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	clauses := []string{"id IN (" + strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",") + ")"}
	args := make([]any, 0, len(ids))
	for _, id := range ids {
		args = append(args, id)
	}
	if extraWhere != "" {
		clauses = append(clauses, extraWhere)
	}
	res, err := a.DB.Exec("UPDATE "+table+" SET "+column+" = "+value+" WHERE "+strings.Join(clauses, " AND "), args...)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// adminWriteMedia writes a media-relative path under MEDIA_ROOT.
func adminWriteMedia(mediaRoot, name string, data []byte) error {
	absolute := filepath.Join(mediaRoot, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(absolute), 0o755); err != nil {
		return err
	}
	return os.WriteFile(absolute, data, 0o644)
}

// adminReadMedia reads a media-relative path from MEDIA_ROOT.
func adminReadMedia(mediaRoot, name string) ([]byte, error) {
	return os.ReadFile(filepath.Join(mediaRoot, filepath.FromSlash(name)))
}
