// apitokens.ApiToken admin page: read-only support view plus the revoke
// action. Tokens are created by their owners through the API, so the add page
// does not exist (has_add_permission False).
package doory

import (
	"database/sql"
	"fmt"
	"html/template"
	"time"
)

func adminModelsApiTokens() []*adminModel { return []*adminModel{adminApiTokenModel()} }

func adminApiTokenModel() *adminModel {
	m := &adminModel{
		AppLabel: "apitokens", Model: "apitoken", Verbose: "API token", Plural: "API tokens",
		Name: "API token", Table: "apitokens_apitoken",
		Columns: []adminColumn{
			{Name: "name", Label: "Name", Sort: "t.name", Link: true},
			{Name: "prefix", Label: "Prefix", Sort: "t.prefix"},
			{Name: "owner", Label: "Owner", Sort: "u.email"},
			{Name: "created_at", Label: "Created at", Sort: "t.created_at"},
			{Name: "last_used_at", Label: "Last used at", Sort: "t.last_used_at"},
			{Name: "expires_at", Label: "Expires at", Sort: "t.expires_at"},
			{Name: "revoked_at", Label: "Revoked at", Sort: "t.revoked_at"},
		},
		LinkName:     "name",
		SearchFields: []adminSearchField{{SQL: "t.name"}, {SQL: "t.prefix"}, {SQL: "u.email"}},
		Filters: []adminFilter{
			{Label: "Created at", Param: "created_at", Kind: "date", Column: "t.created_at"},
			{Label: "Revoked at", Param: "revoked_at", Kind: "date", Column: "t.revoked_at"},
		},
		Ordering: []string{"t.created_at DESC"},
		SelectSQL: "t.id, t.name, t.prefix, COALESCE(u.email, ''), t.created_at, t.last_used_at, " +
			"t.expires_at, t.revoked_at",
		FromSQL: "apitokens_apitoken t LEFT JOIN accounts_user u ON u.id = t.owner_id",
		ScanList: func(a *App, rows *sql.Rows) (adminListRow, error) {
			var id, name, prefix, owner, createdAt string
			var lastUsed, expires, revoked sql.NullString
			if err := rows.Scan(&id, &name, &prefix, &owner, &createdAt, &lastUsed, &expires,
				&revoked); err != nil {
				return adminListRow{}, err
			}
			return adminListRow{ID: id, Cells: map[string]adminCell{
				"name":         adminTextCell(name),
				"prefix":       adminTextCell(prefix),
				"owner":        adminTextCell(owner),
				"created_at":   adminTextCell(createdAt),
				"last_used_at": adminDateCell(lastUsed),
				"expires_at":   adminDateCell(expires),
				"revoked_at":   adminDateCell(revoked),
			}}, nil
		},
		AllowAdd: false, AllowChange: false, AllowDelete: false,
		NoChangePermission: true,
		Actions: []adminAction{
			{Name: "revoke", Label: "Revoke the selected tokens",
				Run: func(a *App, actor *UserRow, ids []string) ([]adminMessage, error) {
					n, err := a.adminUpdateSelected("apitokens_apitoken", "revoked_at",
						"'"+formatDjangoDateTime(time.Now())+"'", ids, "revoked_at IS NULL")
					return []adminMessage{adminSuccess(fmt.Sprintf("%d token(s) revoked.", n))}, err
				}},
		},
	}
	m.Get = func(a *App, id string) (*adminObject, error) {
		var name, prefix, owner, scopes, createdAt string
		var lastUsed, expires, revoked sql.NullString
		err := a.DB.QueryRow(`SELECT t.name, t.prefix, COALESCE(u.email, ''), t.scopes, t.created_at,
			t.last_used_at, t.expires_at, t.revoked_at FROM apitokens_apitoken t
			LEFT JOIN accounts_user u ON u.id = t.owner_id WHERE t.id = ?`, id).Scan(&name, &prefix,
			&owner, &scopes, &createdAt, &lastUsed, &expires, &revoked)
		if err != nil {
			return nil, err
		}
		date := func(v sql.NullString) template.HTML {
			return template.HTML(djangoEscape(adminDateCell(v).Plain()))
		}
		return &adminObject{
			ID: id, Repr: name,
			Values: map[string]string{"name": name},
			Display: map[string]template.HTML{
				"prefix":       template.HTML(djangoEscape(prefix)),
				"owner":        template.HTML(djangoEscape(owner)),
				"scopes":       template.HTML(djangoEscape(prettyInlineJSON(scopes))),
				"created_at":   template.HTML(djangoEscape(createdAt)),
				"last_used_at": date(lastUsed),
				"expires_at":   date(expires),
				"revoked_at":   date(revoked),
			},
			Raw: map[string]any{},
		}, nil
	}
	m.Fieldsets = []adminFieldset{{Title: "", Fields: []adminField{
		{Name: "name", Label: "Name", Kind: adminReadonly, Wide: true},
		{Name: "prefix", Label: "Prefix", Kind: adminReadonly},
		{Name: "owner", Label: "Owner", Kind: adminReadonly},
		{Name: "scopes", Label: "Scopes", Kind: adminReadonly, Wide: true},
		{Name: "created_at", Label: "Created at", Kind: adminReadonly},
		{Name: "last_used_at", Label: "Last used at", Kind: adminReadonly},
		{Name: "expires_at", Label: "Expires at", Kind: adminReadonly},
		{Name: "revoked_at", Label: "Revoked at", Kind: adminReadonly},
	}}}
	m.Repr = func(a *App, id string) string {
		var name string
		_ = a.DB.QueryRow(`SELECT name FROM apitokens_apitoken WHERE id = ?`, id).Scan(&name)
		return name
	}
	return m
}

// prettyInlineJSON is Django admin's rendering of a JSONField in a read-only
// field: the sorted, indented document.
func prettyInlineJSON(raw string) string {
	if raw == "" {
		return "[]"
	}
	return string(adminPrettyJSON(raw))
}
