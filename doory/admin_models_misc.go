// assets.Asset, brand.BrandKit, sharing.ShareLink/Comment admin pages.
package doory

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"html/template"
	"strings"
	"time"
)

func adminModelsAssets() []*adminModel { return []*adminModel{adminAssetModel()} }

func adminAssetModel() *adminModel {
	m := &adminModel{
		AppLabel: "assets", Model: "asset", Verbose: "asset", Plural: "Assets", Name: "asset",
		Table: "assets_asset",
		Columns: []adminColumn{
			{Name: "preview", Label: "Preview"},
			{Name: "display_name", Label: "Name", Sort: "a.name"},
			{Name: "owner", Label: "Owner", Sort: "u.email"},
			{Name: "kind", Label: "Kind", Sort: "a.kind"},
			{Name: "status", Label: "Status", Sort: "a.status"},
			{Name: "mime", Label: "Mime", Sort: "a.mime"},
			{Name: "size", Label: "Size", Sort: "a.size"},
			{Name: "created_at", Label: "Created at", Sort: "a.created_at"},
		},
		LinkName:     "display_name",
		SearchFields: []adminSearchField{{SQL: "a.name"}, {SQL: "u.email"}},
		Filters: []adminFilter{
			{Label: "Kind", Param: "kind__exact", Kind: "choice", Column: "a.kind",
				Options: []adminFilterChoice{adminOption("image", "Image"), adminOption("svg", "SVG"),
					adminOption("audio", "Audio"), adminOption("video", "Video")}},
			{Label: "Status", Param: "status__exact", Kind: "choice", Column: "a.status",
				Options: []adminFilterChoice{adminOption("ready", "Ready"), adminOption("processing", "Processing"),
					adminOption("failed", "Failed")}},
			{Label: "Mime", Param: "mime__exact", Kind: "choice", Column: "a.mime",
				OptionsFn: func(a *App) []adminFilterChoice { return adminDistinctChoices(a, "assets_asset", "mime") }},
			{Label: "Created at", Param: "created_at", Kind: "date", Column: "a.created_at"},
		},
		DateHierarchy: "a.created_at",
		Ordering:      []string{"a.created_at DESC"},
		SelectSQL: "a.id, a.file, a.thumbnail, a.poster, a.name, COALESCE(u.email, ''), a.kind, a.status, " +
			"a.mime, a.size, a.created_at",
		FromSQL: "assets_asset a LEFT JOIN accounts_user u ON u.id = a.owner_id",
		ScanList: func(a *App, rows *sql.Rows) (adminListRow, error) {
			var id, file, thumbnail, poster, name, owner, kind, status, mime, createdAt string
			var size int64
			if err := rows.Scan(&id, &file, &thumbnail, &poster, &name, &owner, &kind, &status,
				&mime, &size, &createdAt); err != nil {
				return adminListRow{}, err
			}
			label := name
			if label == "" {
				label = id
			}
			return adminListRow{ID: id, Cells: map[string]adminCell{
				"preview":      adminHTMLCell(assetPreview(file, thumbnail, poster, kind, status)),
				"display_name": adminTextCell(label),
				"owner":        adminTextCell(owner),
				"kind":         adminTextCell(kind),
				"status":       adminTextCell(status),
				"mime":         adminTextCell(mime),
				"size":         adminTextCell(fmt.Sprint(size)),
				"created_at":   adminTextCell(createdAt),
			}}, nil
		},
		AllowAdd: false, AllowChange: true, AllowDelete: true,
	}
	m.Get = func(a *App, id string) (*adminObject, error) {
		var owner, kind, name, file, thumbnail, poster, mime, status, errText, tags, meta, createdAt, updatedAt string
		var width, height int
		var size int64
		err := a.DB.QueryRow(`SELECT COALESCE(u.email, ''), a.kind, a.name, a.file, a.thumbnail, a.poster,
			a.width, a.height, a.size, a.mime, a.status, a.error, a.tags, a.meta, a.created_at, a.updated_at
			FROM assets_asset a LEFT JOIN accounts_user u ON u.id = a.owner_id WHERE a.id = ?`, id).Scan(
			&owner, &kind, &name, &file, &thumbnail, &poster, &width, &height, &size, &mime, &status,
			&errText, &tags, &meta, &createdAt, &updatedAt)
		if err != nil {
			return nil, err
		}
		tagList := strings.Join(adminJSONList(tags), ", ")
		if tagList == "" {
			tagList = "-"
		}
		attribution := "-"
		var metaMap map[string]any
		if jsonUnmarshalMap(meta, &metaMap) {
			if info, ok := metaMap["attribution"].(map[string]any); ok {
				author, _ := info["author"].(string)
				provider, _ := info["provider"].(string)
				attribution = fmt.Sprintf("%s (%s)", author, provider)
			}
		}
		return &adminObject{
			ID: id, Repr: nameOrID(name, id),
			Values: map[string]string{
				"owner": owner, "kind": kind, "name": name, "width": fmt.Sprint(width),
				"height": fmt.Sprint(height), "size": fmt.Sprint(size), "mime": mime,
				"status": status, "error": errText, "created_at": createdAt, "updated_at": updatedAt,
			},
			Display: map[string]template.HTML{
				"preview":     adminHTMLCell(assetPreview(file, thumbnail, poster, kind, status)).Render(),
				"tag_list":    template.HTML(djangoEscape(tagList)),
				"attribution": template.HTML(djangoEscape(attribution)),
			},
			Raw: map[string]any{},
		}, nil
	}
	m.Fieldsets = []adminFieldset{{Title: "", Fields: []adminField{
		{Name: "owner", Label: "Owner", Kind: adminReadonly},
		{Name: "kind", Label: "Kind", Kind: adminReadonly},
		{Name: "name", Label: "Name", Kind: adminReadonly},
		{Name: "preview", Label: "Preview", Kind: adminReadonly},
		{Name: "width", Label: "Width", Kind: adminReadonly},
		{Name: "height", Label: "Height", Kind: adminReadonly},
		{Name: "size", Label: "Size", Kind: adminReadonly},
		{Name: "mime", Label: "Mime", Kind: adminReadonly},
		{Name: "status", Label: "Status", Kind: adminReadonly},
		{Name: "error", Label: "Error", Kind: adminReadonly},
		{Name: "tag_list", Label: "Tags", Kind: adminReadonly},
		{Name: "attribution", Label: "Attribution", Kind: adminReadonly},
		{Name: "created_at", Label: "Created at", Kind: adminReadonly},
		{Name: "updated_at", Label: "Updated at", Kind: adminReadonly},
	}}}
	m.Change = func(a *App, actor *UserRow, id string, form *adminForm) (adminErrors, error) {
		_, err := a.DB.Exec(`UPDATE assets_asset SET updated_at = ? WHERE id = ?`,
			formatDjangoDateTime(time.Now()), id)
		return nil, err
	}
	m.Delete = func(a *App, actor *UserRow, id string) error {
		var file, thumbnail, poster, filmstrip, audio string
		_ = a.DB.QueryRow(`SELECT file, thumbnail, poster, filmstrip, audio FROM assets_asset WHERE id = ?`,
			id).Scan(&file, &thumbnail, &poster, &filmstrip, &audio)
		if _, err := a.DB.Exec(`DELETE FROM assets_asset WHERE id = ?`, id); err != nil {
			return err
		}
		for _, path := range []string{file, thumbnail, poster, filmstrip, audio} {
			a.deleteMediaFile(path)
		}
		return nil
	}
	m.Repr = func(a *App, id string) string {
		var name string
		_ = a.DB.QueryRow(`SELECT name FROM assets_asset WHERE id = ?`, id).Scan(&name)
		return nameOrID(name, id)
	}
	return m
}

func nameOrID(name, id string) string {
	if name != "" {
		return name
	}
	return id
}

func assetPreview(file, thumbnail, poster, kind, status string) template.HTML {
	if file == "" {
		return "-"
	}
	switch kind {
	case "audio":
		return template.HTML(fmt.Sprintf(`<audio controls preload="none" src="%s" style="height:32px;max-width:220px"></audio>`,
			djangoEscape(adminMediaURL(file))))
	case "video":
		if poster == "" {
			if status == "" {
				status = "ready"
			}
			return template.HTML(djangoEscape(status))
		}
		return template.HTML(fmt.Sprintf(
			`<video controls preload="none" src="%s" poster="%s" style="max-height:72px;max-width:128px"></video>`,
			djangoEscape(adminMediaURL(file)), djangoEscape(adminMediaURL(poster))))
	}
	url := file
	if thumbnail != "" {
		url = thumbnail
	}
	return template.HTML(fmt.Sprintf(`<img src="%s" alt="" style="max-height:48px;max-width:96px;border-radius:4px">`,
		djangoEscape(adminMediaURL(url))))
}

func jsonUnmarshalMap(raw string, out *map[string]any) bool {
	if strings.TrimSpace(raw) == "" {
		return false
	}
	return json.Unmarshal([]byte(raw), out) == nil
}

// ---- brand.BrandKit ----

func adminModelsBrand() []*adminModel { return []*adminModel{adminBrandKitModel()} }

func adminBrandKitModel() *adminModel {
	m := &adminModel{
		AppLabel: "brand", Model: "brandkit", Verbose: "brand kit", Plural: "Brand kits", Name: "brand kit",
		Table: "brand_brandkit",
		Columns: []adminColumn{
			{Name: "name", Label: "Name", Sort: "b.name", Link: true},
			{Name: "owner", Label: "Owner", Sort: "u.email"},
			{Name: "is_default", Label: "Is default", Sort: "b.is_default"},
			{Name: "color_total", Label: "Colours"},
			{Name: "logo_total", Label: "Logos"},
			{Name: "created_at", Label: "Created at", Sort: "b.created_at"},
		},
		LinkName:     "name",
		SearchFields: []adminSearchField{{SQL: "b.name"}, {SQL: "u.email"}},
		Filters: []adminFilter{
			{Label: "Is default", Param: "is_default__exact", Kind: "bool", Column: "b.is_default"},
			{Label: "Created at", Param: "created_at", Kind: "date", Column: "b.created_at"},
		},
		Ordering: []string{"b.is_default DESC", "b.name ASC", "b.created_at ASC"},
		SelectSQL: "b.id, b.name, COALESCE(u.email, ''), b.is_default, b.colors, " +
			"(SELECT COUNT(*) FROM brand_brandkit_logos bl WHERE bl.brandkit_id = b.id), b.created_at",
		FromSQL: "brand_brandkit b LEFT JOIN accounts_user u ON u.id = b.owner_id",
		ScanList: func(a *App, rows *sql.Rows) (adminListRow, error) {
			var id, name, owner, colors, createdAt string
			var isDefault, logos int
			if err := rows.Scan(&id, &name, &owner, &isDefault, &colors, &logos, &createdAt); err != nil {
				return adminListRow{}, err
			}
			return adminListRow{ID: id, Cells: map[string]adminCell{
				"name":        adminTextCell(name),
				"owner":       adminTextCell(owner),
				"is_default":  adminTextCell(adminBoolText(isDefault)),
				"color_total": adminIntCell(len(adminJSONList(colors))),
				"logo_total":  adminIntCell(logos),
				"created_at":  adminTextCell(createdAt),
			}}, nil
		},
		AllowAdd: false, AllowChange: true, AllowDelete: true,
	}
	m.Get = func(a *App, id string) (*adminObject, error) {
		var name, owner, colors, fonts, createdAt, updatedAt string
		var isDefault int
		err := a.DB.QueryRow(`SELECT b.name, b.is_default, COALESCE(u.email, ''), b.colors, b.fonts,
			b.created_at, b.updated_at FROM brand_brandkit b
			LEFT JOIN accounts_user u ON u.id = b.owner_id WHERE b.id = ?`, id).Scan(
			&name, &isDefault, &owner, &colors, &fonts, &createdAt, &updatedAt)
		if err != nil {
			return nil, err
		}
		return &adminObject{
			ID: id, Repr: name,
			Values: map[string]string{"name": name, "is_default": boolFlag(isDefault), "owner": owner,
				"created_at": createdAt, "updated_at": updatedAt},
			Display: map[string]template.HTML{
				"colors_pretty": adminPrettyJSON(colors),
				"fonts_pretty":  adminPrettyJSON(fonts),
			},
			Raw: map[string]any{},
		}, nil
	}
	m.Fieldsets = []adminFieldset{{Title: "", Fields: []adminField{
		{Name: "name", Label: "Name", Kind: adminText, Required: true, Wide: true},
		{Name: "is_default", Label: "Is default", Kind: adminBool},
		{Name: "owner", Label: "Owner", Kind: adminReadonly},
		{Name: "colors_pretty", Label: "Colours", Kind: adminReadonly, Wide: true},
		{Name: "fonts_pretty", Label: "Fonts", Kind: adminReadonly, Wide: true},
		{Name: "created_at", Label: "Created at", Kind: adminReadonly},
		{Name: "updated_at", Label: "Updated at", Kind: adminReadonly},
	}}}
	m.Change = func(a *App, actor *UserRow, id string, form *adminForm) (adminErrors, error) {
		errs := adminErrors{}
		name := strings.TrimSpace(form.Value("name"))
		if name == "" {
			errs.add("name", "This field is required.")
			return errs, nil
		}
		_, err := a.DB.Exec(`UPDATE brand_brandkit SET name = ?, is_default = ?, updated_at = ? WHERE id = ?`,
			name, boolSQL(form.Has("is_default")), formatDjangoDateTime(time.Now()), id)
		return nil, err
	}
	m.Delete = func(a *App, actor *UserRow, id string) error {
		_, _ = a.DB.Exec(`DELETE FROM brand_brandkit_logos WHERE brandkit_id = ?`, id)
		_, err := a.DB.Exec(`DELETE FROM brand_brandkit WHERE id = ?`, id)
		return err
	}
	m.Repr = func(a *App, id string) string {
		var name string
		_ = a.DB.QueryRow(`SELECT name FROM brand_brandkit WHERE id = ?`, id).Scan(&name)
		return name
	}
	return m
}

// ---- sharing ----

func adminModelsSharing() []*adminModel {
	return []*adminModel{adminShareLinkModel(), adminCommentModel()}
}

func adminShareLinkModel() *adminModel {
	m := &adminModel{
		AppLabel: "sharing", Model: "sharelink", Verbose: "share link", Plural: "Share links",
		Name: "share link", Table: "sharing_sharelink",
		Columns: []adminColumn{
			{Name: "design", Label: "Design", Sort: "d.title", Link: true},
			{Name: "role", Label: "Role", Sort: "s.role"},
			{Name: "is_active", Label: "Is active", Sort: "s.is_active"},
			{Name: "expires_at", Label: "Expires at", Sort: "s.expires_at"},
			{Name: "access_count", Label: "Access count", Sort: "s.access_count"},
			{Name: "last_accessed_at", Label: "Last accessed at", Sort: "s.last_accessed_at"},
			{Name: "created_at", Label: "Created at", Sort: "s.created_at"},
		},
		LinkName:     "design",
		SearchFields: []adminSearchField{{SQL: "d.title"}, {SQL: "s.design_id"}},
		Filters: []adminFilter{
			{Label: "Role", Param: "role__exact", Kind: "choice", Column: "s.role",
				Options: []adminFilterChoice{adminOption("view", "View"), adminOption("edit", "Edit"),
					adminOption("comment", "Comment")}},
			{Label: "Is active", Param: "is_active__exact", Kind: "bool", Column: "s.is_active"},
		},
		Ordering: []string{"s.created_at DESC"},
		SelectSQL: "s.id, COALESCE(d.title, ''), s.role, s.is_active, s.expires_at, s.access_count, " +
			"s.last_accessed_at, s.created_at",
		FromSQL: "sharing_sharelink s LEFT JOIN designs_design d ON d.id = s.design_id",
		ScanList: func(a *App, rows *sql.Rows) (adminListRow, error) {
			var id, design, role, createdAt string
			var active, accessCount int
			var expiresAt, lastAccessed sql.NullString
			if err := rows.Scan(&id, &design, &role, &active, &expiresAt, &accessCount,
				&lastAccessed, &createdAt); err != nil {
				return adminListRow{}, err
			}
			return adminListRow{ID: id, Cells: map[string]adminCell{
				"design":           adminTextCell(design),
				"role":             adminTextCell(role),
				"is_active":        adminTextCell(adminBoolText(active)),
				"expires_at":       adminDateCell(expiresAt),
				"access_count":     adminIntCell(accessCount),
				"last_accessed_at": adminDateCell(lastAccessed),
				"created_at":       adminTextCell(createdAt),
			}}, nil
		},
		AllowAdd: false, AllowChange: true, AllowDelete: true,
	}
	m.Get = func(a *App, id string) (*adminObject, error) {
		var design, role, createdBy, createdAt, updatedAt string
		var active, accessCount int
		var expiresAt, lastAccessed sql.NullString
		err := a.DB.QueryRow(`SELECT COALESCE(d.title, ''), s.role, s.is_active, s.expires_at,
			COALESCE(u.email, ''), s.last_accessed_at, s.access_count, s.created_at, s.updated_at
			FROM sharing_sharelink s LEFT JOIN designs_design d ON d.id = s.design_id
			LEFT JOIN accounts_user u ON u.id = s.created_by_id WHERE s.id = ?`, id).Scan(
			&design, &role, &active, &expiresAt, &createdBy, &lastAccessed, &accessCount, &createdAt, &updatedAt)
		if err != nil {
			return nil, err
		}
		display := map[string]template.HTML{
			"design":           template.HTML(djangoEscape(design)),
			"role":             template.HTML(djangoEscape(role)),
			"expires_at":       template.HTML(djangoEscape(adminDateCell(expiresAt).String())),
			"created_by":       template.HTML(djangoEscape(createdBy)),
			"last_accessed_at": template.HTML(djangoEscape(adminDateCell(lastAccessed).String())),
			"created_at":       template.HTML(djangoEscape(createdAt)),
			"updated_at":       template.HTML(djangoEscape(updatedAt)),
		}
		return &adminObject{
			ID: id, Repr: design,
			Values:  map[string]string{"is_active": boolFlag(active), "access_count": fmt.Sprint(accessCount)},
			Display: display,
			Raw:     map[string]any{},
		}, nil
	}
	m.Fieldsets = []adminFieldset{{Title: "", Fields: []adminField{
		{Name: "design", Label: "Design", Kind: adminReadonly},
		{Name: "role", Label: "Role", Kind: adminReadonly},
		{Name: "is_active", Label: "Is active", Kind: adminBool},
		{Name: "expires_at", Label: "Expires at", Kind: adminReadonly},
		{Name: "created_by", Label: "Created by", Kind: adminReadonly},
		{Name: "last_accessed_at", Label: "Last accessed at", Kind: adminReadonly},
		{Name: "access_count", Label: "Access count", Kind: adminReadonly},
		{Name: "created_at", Label: "Created at", Kind: adminReadonly},
		{Name: "updated_at", Label: "Updated at", Kind: adminReadonly},
	}}}
	m.Change = func(a *App, actor *UserRow, id string, form *adminForm) (adminErrors, error) {
		_, err := a.DB.Exec(`UPDATE sharing_sharelink SET is_active = ?, updated_at = ? WHERE id = ?`,
			boolSQL(form.Has("is_active")), formatDjangoDateTime(time.Now()), id)
		return nil, err
	}
	m.Delete = func(a *App, actor *UserRow, id string) error {
		_, err := a.DB.Exec(`DELETE FROM sharing_sharelink WHERE id = ?`, id)
		return err
	}
	m.Repr = func(a *App, id string) string {
		var design string
		_ = a.DB.QueryRow(`SELECT COALESCE(d.title, '') FROM sharing_sharelink s
			LEFT JOIN designs_design d ON d.id = s.design_id WHERE s.id = ?`, id).Scan(&design)
		return design
	}
	return m
}

func adminCommentModel() *adminModel {
	m := &adminModel{
		AppLabel: "sharing", Model: "comment", Verbose: "comment", Plural: "Comments", Name: "comment",
		Table: "sharing_comment",
		Columns: []adminColumn{
			{Name: "short_body", Label: "Comment", Link: true},
			{Name: "author", Label: "Author", Sort: "u.email"},
			{Name: "design", Label: "Design", Sort: "d.title"},
			{Name: "resolved", Label: "Resolved", Sort: "c.resolved"},
			{Name: "created_at", Label: "Created at", Sort: "c.created_at"},
		},
		LinkName:     "short_body",
		SearchFields: []adminSearchField{{SQL: "c.body"}, {SQL: "u.email"}, {SQL: "d.title"}},
		Filters: []adminFilter{
			{Label: "Resolved", Param: "resolved__exact", Kind: "bool", Column: "c.resolved"},
		},
		Ordering:  []string{"c.created_at DESC"},
		SelectSQL: "c.id, c.body, COALESCE(u.email, ''), COALESCE(d.title, ''), c.resolved, c.created_at",
		FromSQL: "sharing_comment c LEFT JOIN accounts_user u ON u.id = c.author_id " +
			"LEFT JOIN designs_design d ON d.id = c.design_id",
		ScanList: func(a *App, rows *sql.Rows) (adminListRow, error) {
			var id, body, author, design, createdAt string
			var resolved int
			if err := rows.Scan(&id, &body, &author, &design, &resolved, &createdAt); err != nil {
				return adminListRow{}, err
			}
			short := body
			if len(short) > 80 {
				short = short[:80]
			}
			return adminListRow{ID: id, Cells: map[string]adminCell{
				"short_body": adminTextCell(short),
				"author":     adminTextCell(author),
				"design":     adminTextCell(design),
				"resolved":   adminTextCell(adminBoolText(resolved)),
				"created_at": adminTextCell(createdAt),
			}}, nil
		},
		AllowAdd: false, AllowChange: false, AllowDelete: true,
		NoChangePermission: true,
	}
	m.Get = func(a *App, id string) (*adminObject, error) {
		var body, pageID, author, design, createdAt, updatedAt string
		var x, y sql.NullFloat64
		var resolved int
		var parent sql.NullString
		err := a.DB.QueryRow(`SELECT c.body, c.page_id, c.x, c.y, c.resolved, COALESCE(u.email, ''),
			COALESCE(d.title, ''), c.parent_id, c.created_at, c.updated_at
			FROM sharing_comment c LEFT JOIN accounts_user u ON u.id = c.author_id
			LEFT JOIN designs_design d ON d.id = c.design_id WHERE c.id = ?`, id).Scan(
			&body, &pageID, &x, &y, &resolved, &author, &design, &parent, &createdAt, &updatedAt)
		if err != nil {
			return nil, err
		}
		display := map[string]template.HTML{
			"design":     template.HTML(djangoEscape(design)),
			"author":     template.HTML(djangoEscape(author)),
			"body":       template.HTML(djangoEscape(body)),
			"page_id":    template.HTML(djangoEscape(pageID)),
			"x":          template.HTML(djangoEscape(nullFloatText(x))),
			"y":          template.HTML(djangoEscape(nullFloatText(y))),
			"resolved":   template.HTML(djangoEscape(adminBoolText(resolved))),
			"created_at": template.HTML(djangoEscape(createdAt)),
			"updated_at": template.HTML(djangoEscape(updatedAt)),
			"parent":     template.HTML("-"),
		}
		if parent.Valid && parent.String != "" {
			display["parent"] = template.HTML(djangoEscape(parent.String))
		}
		return &adminObject{
			ID: id, Repr: shortBody(body),
			Values: map[string]string{"body": body}, Display: display, Raw: map[string]any{},
		}, nil
	}
	m.Fieldsets = []adminFieldset{{Title: "", Fields: []adminField{
		{Name: "design", Label: "Design", Kind: adminReadonly},
		{Name: "author", Label: "Author", Kind: adminReadonly},
		{Name: "body", Label: "Body", Kind: adminReadonly, Wide: true},
		{Name: "page_id", Label: "Page id", Kind: adminReadonly},
		{Name: "x", Label: "X", Kind: adminReadonly},
		{Name: "y", Label: "Y", Kind: adminReadonly},
		{Name: "parent", Label: "Parent", Kind: adminReadonly},
		{Name: "resolved", Label: "Resolved", Kind: adminReadonly},
		{Name: "created_at", Label: "Created at", Kind: adminReadonly},
		{Name: "updated_at", Label: "Updated at", Kind: adminReadonly},
	}}}
	m.Delete = func(a *App, actor *UserRow, id string) error {
		_, _ = a.DB.Exec(`UPDATE sharing_comment SET parent_id = NULL WHERE parent_id = ?`, id)
		_, err := a.DB.Exec(`DELETE FROM sharing_comment WHERE id = ?`, id)
		return err
	}
	m.Repr = func(a *App, id string) string {
		var body string
		_ = a.DB.QueryRow(`SELECT body FROM sharing_comment WHERE id = ?`, id).Scan(&body)
		return shortBody(body)
	}
	return m
}

func shortBody(body string) string {
	if len(body) > 80 {
		return body[:80]
	}
	return body
}

func nullFloatText(v sql.NullFloat64) string {
	if !v.Valid {
		return "-"
	}
	return strings.TrimSuffix(strings.TrimSuffix(fmt.Sprintf("%f", v.Float64), "0"), ".")
}
