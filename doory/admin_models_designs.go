// designs.Design / DesignVersion / Folder admin pages.
package doory

import (
	"database/sql"
	"fmt"
	"html/template"
	"net/url"
	"strings"
	"time"
)

func adminThumbCell(path string, maxH, maxW int) adminCell {
	if path == "" {
		return adminTextCell("-")
	}
	return adminHTMLCell(template.HTML(fmt.Sprintf(
		`<img src="%s" alt="" style="max-height:%dpx;max-width:%dpx;border-radius:6px">`,
		djangoEscape(adminMediaURL(path)), maxH, maxW)))
}

func adminModelsDesigns() []*adminModel {
	return []*adminModel{adminDesignModel(), adminDesignVersionModel(), adminFolderModel()}
}

func adminDesignModel() *adminModel {
	m := &adminModel{
		AppLabel: "designs", Model: "design", Verbose: "design", Plural: "Designs", Name: "design",
		Table: "designs_design",
		Columns: []adminColumn{
			{Name: "thumb", Label: "Preview"},
			{Name: "title", Label: "Title", Sort: "d.title", Link: true},
			{Name: "owner", Label: "Owner", Sort: "u.email"},
			{Name: "dimensions", Label: "Size"},
			{Name: "page_count", Label: "Page count", Sort: "d.page_count"},
			{Name: "revision", Label: "Revision", Sort: "d.revision"},
			{Name: "starred", Label: "Starred", Sort: "d.starred"},
			{Name: "in_trash", Label: "Trashed"},
			{Name: "updated_at", Label: "Updated at", Sort: "d.updated_at"},
		},
		LinkName:     "title",
		SearchFields: []adminSearchField{{SQL: "d.title"}, {SQL: "u.email"}},
		Filters: []adminFilter{
			{Label: "Trash", Param: "trashed", Kind: "custom",
				Options: []adminFilterChoice{adminOption("yes", "In trash"), adminOption("no", "Not in trash")},
				Custom: func(values url.Values) (string, []any) {
					switch values.Get("trashed") {
					case "yes":
						return "d.trashed_at IS NOT NULL", nil
					case "no":
						return "d.trashed_at IS NULL", nil
					}
					return "", nil
				}},
			{Label: "Starred", Param: "starred__exact", Kind: "bool", Column: "d.starred"},
			{Label: "Preset", Param: "preset__exact", Kind: "choice", Column: "d.preset",
				OptionsFn: func(a *App) []adminFilterChoice {
					return adminDistinctChoices(a, "designs_design", "preset")
				}},
			{Label: "Created at", Param: "created_at", Kind: "date", Column: "d.created_at"},
		},
		DateHierarchy: "d.created_at",
		Ordering:      []string{"d.updated_at DESC"},
		SelectSQL: "d.id, d.thumbnail, d.title, COALESCE(u.email, ''), d.width, d.height, d.page_count, " +
			"d.revision, d.starred, d.trashed_at, d.updated_at",
		FromSQL: "designs_design d LEFT JOIN accounts_user u ON u.id = d.owner_id",
		ScanList: func(a *App, rows *sql.Rows) (adminListRow, error) {
			var id, thumbnail, title, owner, updatedAt string
			var width, height, pageCount, revision, starred int
			var trashedAt sql.NullString
			if err := rows.Scan(&id, &thumbnail, &title, &owner, &width, &height, &pageCount,
				&revision, &starred, &trashedAt, &updatedAt); err != nil {
				return adminListRow{}, err
			}
			return adminListRow{ID: id, Cells: map[string]adminCell{
				"thumb":      adminThumbCell(thumbnail, 48, 96),
				"title":      adminTextCell(title),
				"owner":      adminTextCell(owner),
				"dimensions": adminTextCell(fmt.Sprintf("%d x %d", width, height)),
				"page_count": adminIntCell(pageCount),
				"revision":   adminIntCell(revision),
				"starred":    adminTextCell(adminBoolText(starred)),
				"in_trash":   adminTextCell(adminBoolText(boolInt(trashedAt.Valid))),
				"updated_at": adminTextCell(updatedAt),
			}}, nil
		},
		AllowAdd: false, AllowChange: true, AllowDelete: true,
		Actions: []adminAction{
			{Name: "move_to_trash", Label: "Move selected designs to the trash",
				Run: func(a *App, actor *UserRow, ids []string) ([]adminMessage, error) {
					n, err := a.adminUpdateSelected("designs_design", "trashed_at",
						"'"+formatDjangoDateTime(time.Now())+"'", ids, "trashed_at IS NULL")
					return []adminMessage{adminSuccess(fmt.Sprintf("%d design(s) moved to the trash.", n))}, err
				}},
			{Name: "restore_from_trash", Label: "Restore selected designs from the trash",
				Run: func(a *App, actor *UserRow, ids []string) ([]adminMessage, error) {
					n, err := a.adminUpdateSelected("designs_design", "trashed_at", "NULL", ids, "trashed_at IS NOT NULL")
					return []adminMessage{adminSuccess(fmt.Sprintf("%d design(s) restored.", n))}, err
				}},
		},
	}
	m.Get = func(a *App, id string) (*adminObject, error) {
		var title, preset, document, thumbnail, owner, createdAt, updatedAt string
		var folder sql.NullString
		var width, height, pageCount, revision, starred int
		var trashedAt sql.NullString
		err := a.DB.QueryRow(`SELECT d.title, d.width, d.height, d.preset, d.document, d.page_count,
			d.thumbnail, d.starred, d.revision, d.trashed_at, d.created_at, d.updated_at,
			COALESCE(u.email, ''), d.folder_id
			FROM designs_design d LEFT JOIN accounts_user u ON u.id = d.owner_id WHERE d.id = ?`, id).Scan(
			&title, &width, &height, &preset, &document, &pageCount, &thumbnail, &starred, &revision,
			&trashedAt, &createdAt, &updatedAt, &owner, &folder)
		if err != nil {
			return nil, err
		}
		display := map[string]template.HTML{
			"thumb_large":     adminThumbCell(thumbnail, 160, 240).Render(),
			"document_pretty": adminPrettyJSON(document),
			"dimensions":      template.HTML(djangoEscape(fmt.Sprintf("%d x %d", width, height))),
			"trashed_at":      template.HTML("-"),
			"folder":          template.HTML("-"),
		}
		if trashedAt.Valid {
			display["trashed_at"] = template.HTML(djangoEscape(trashedAt.String))
		}
		if folder.Valid && folder.String != "" {
			var name string
			if err := a.DB.QueryRow(`SELECT name FROM designs_folder WHERE id = ?`, folder.String).Scan(&name); err == nil {
				display["folder"] = template.HTML(djangoEscape(name))
			}
		}
		return &adminObject{
			ID: id, Repr: title,
			Values: map[string]string{
				"title": title, "starred": boolFlag(starred), "owner": owner, "preset": preset,
				"page_count": fmt.Sprint(pageCount), "revision": fmt.Sprint(revision),
				"created_at": createdAt, "updated_at": updatedAt,
			},
			Display: display,
			Raw:     map[string]any{},
		}, nil
	}
	m.Fieldsets = []adminFieldset{
		{Title: "", Fields: []adminField{
			{Name: "title", Label: "Title", Kind: adminText, Required: true, Wide: true},
			{Name: "starred", Label: "Starred", Kind: adminBool},
			{Name: "owner", Label: "Owner", Kind: adminReadonly},
			{Name: "folder", Label: "Folder", Kind: adminReadonly},
			{Name: "dimensions", Label: "Size", Kind: adminReadonly},
			{Name: "preset", Label: "Preset", Kind: adminReadonly},
			{Name: "page_count", Label: "Page count", Kind: adminReadonly},
			{Name: "revision", Label: "Revision", Kind: adminReadonly},
			{Name: "thumb_large", Label: "Thumbnail", Kind: adminReadonly},
			{Name: "document_pretty", Label: "Document (read-only)", Kind: adminReadonly, Wide: true},
			{Name: "trashed_at", Label: "Trashed at", Kind: adminReadonly},
			{Name: "created_at", Label: "Created at", Kind: adminReadonly},
			{Name: "updated_at", Label: "Updated at", Kind: adminReadonly},
		}},
	}
	m.Change = func(a *App, actor *UserRow, id string, form *adminForm) (adminErrors, error) {
		errs := adminErrors{}
		title := strings.TrimSpace(form.Value("title"))
		if title == "" {
			errs.add("title", "This field is required.")
			return errs, nil
		}
		_, err := a.DB.Exec(`UPDATE designs_design SET title = ?, starred = ?, updated_at = ? WHERE id = ?`,
			title, boolSQL(form.Has("starred")), formatDjangoDateTime(time.Now()), id)
		return nil, err
	}
	m.Delete = func(a *App, actor *UserRow, id string) error {
		var thumbnail string
		_ = a.DB.QueryRow(`SELECT thumbnail FROM designs_design WHERE id = ?`, id).Scan(&thumbnail)
		for _, table := range []string{"designs_designversion", "sharing_comment", "sharing_sharelink"} {
			_, _ = a.DB.Exec("DELETE FROM "+table+" WHERE design_id = ?", id)
		}
		if _, err := a.DB.Exec(`DELETE FROM designs_design WHERE id = ?`, id); err != nil {
			return err
		}
		a.deleteMediaFile(thumbnail)
		return nil
	}
	m.Repr = func(a *App, id string) string {
		var title string
		_ = a.DB.QueryRow(`SELECT title FROM designs_design WHERE id = ?`, id).Scan(&title)
		return title
	}
	return m
}

func adminDesignVersionModel() *adminModel {
	m := &adminModel{
		AppLabel: "designs", Model: "designversion", Verbose: "design version", Plural: "Design versions",
		Name: "design version", Table: "designs_designversion",
		Columns: []adminColumn{
			{Name: "design", Label: "Design", Sort: "d.title"},
			{Name: "label", Label: "Label", Sort: "v.label", Link: true},
			{Name: "is_auto", Label: "Is auto", Sort: "v.is_auto"},
			{Name: "page_count", Label: "Page count", Sort: "v.page_count"},
			{Name: "created_by", Label: "Created by", Sort: "u.email"},
			{Name: "created_at", Label: "Created at", Sort: "v.created_at"},
		},
		LinkName:     "label",
		SearchFields: []adminSearchField{{SQL: "v.label"}, {SQL: "d.title"}, {SQL: "u.email"}},
		Filters: []adminFilter{
			{Label: "Is auto", Param: "is_auto__exact", Kind: "bool", Column: "v.is_auto"},
			{Label: "Created at", Param: "created_at", Kind: "date", Column: "v.created_at"},
		},
		Ordering: []string{"v.created_at DESC"},
		SelectSQL: "v.id, COALESCE(d.title, ''), v.label, v.is_auto, v.page_count, " +
			"COALESCE(u.email, ''), v.created_at",
		FromSQL: "designs_designversion v LEFT JOIN designs_design d ON d.id = v.design_id " +
			"LEFT JOIN accounts_user u ON u.id = v.created_by_id",
		ScanList: func(a *App, rows *sql.Rows) (adminListRow, error) {
			var id, design, label, createdBy, createdAt string
			var isAuto, pageCount int
			if err := rows.Scan(&id, &design, &label, &isAuto, &pageCount, &createdBy, &createdAt); err != nil {
				return adminListRow{}, err
			}
			return adminListRow{ID: id, Cells: map[string]adminCell{
				"design":     adminTextCell(design),
				"label":      adminTextCell(label),
				"is_auto":    adminTextCell(adminBoolText(isAuto)),
				"page_count": adminIntCell(pageCount),
				"created_by": adminTextCell(createdBy),
				"created_at": adminTextCell(createdAt),
			}}, nil
		},
		AllowAdd: false, AllowChange: false, AllowDelete: true,
		NoChangePermission: true,
	}
	m.Get = func(a *App, id string) (*adminObject, error) {
		var design, label, document, thumbnail, createdBy, createdAt string
		var isAuto, pageCount int
		err := a.DB.QueryRow(`SELECT COALESCE(d.title, ''), v.label, v.is_auto, v.page_count, v.thumbnail,
			v.document, COALESCE(u.email, ''), v.created_at
			FROM designs_designversion v LEFT JOIN designs_design d ON d.id = v.design_id
			LEFT JOIN accounts_user u ON u.id = v.created_by_id WHERE v.id = ?`, id).Scan(
			&design, &label, &isAuto, &pageCount, &thumbnail, &document, &createdBy, &createdAt)
		if err != nil {
			return nil, err
		}
		return &adminObject{
			ID: id, Repr: label,
			Values: map[string]string{
				"design": design, "label": label, "is_auto": boolFlag(isAuto),
				"page_count": fmt.Sprint(pageCount), "created_by": createdBy, "created_at": createdAt,
			},
			Display: map[string]template.HTML{
				"thumb_large":     adminThumbCell(thumbnail, 160, 240).Render(),
				"document_pretty": adminPrettyJSON(document),
			},
			Raw: map[string]any{},
		}, nil
	}
	m.Fieldsets = []adminFieldset{{Title: "", Fields: []adminField{
		{Name: "design", Label: "Design", Kind: adminReadonly},
		{Name: "label", Label: "Label", Kind: adminReadonly},
		{Name: "is_auto", Label: "Is auto", Kind: adminReadonly},
		{Name: "page_count", Label: "Page count", Kind: adminReadonly},
		{Name: "created_by", Label: "Created by", Kind: adminReadonly},
		{Name: "thumb_large", Label: "Thumbnail", Kind: adminReadonly},
		{Name: "document_pretty", Label: "Document (read-only)", Kind: adminReadonly, Wide: true},
		{Name: "created_at", Label: "Created at", Kind: adminReadonly},
	}}}
	m.Delete = func(a *App, actor *UserRow, id string) error {
		_, err := a.DB.Exec(`DELETE FROM designs_designversion WHERE id = ?`, id)
		return err
	}
	m.Repr = func(a *App, id string) string {
		var label string
		_ = a.DB.QueryRow(`SELECT label FROM designs_designversion WHERE id = ?`, id).Scan(&label)
		return label
	}
	return m
}

func adminFolderModel() *adminModel {
	m := &adminModel{
		AppLabel: "designs", Model: "folder", Verbose: "folder", Plural: "Folders", Name: "folder",
		Table: "designs_folder",
		Columns: []adminColumn{
			{Name: "name", Label: "Name", Sort: "f.name", Link: true},
			{Name: "owner", Label: "Owner", Sort: "u.email"},
			{Name: "parent", Label: "Parent", Sort: "p.name"},
			{Name: "design_total", Label: "Designs"},
			{Name: "created_at", Label: "Created at", Sort: "f.created_at"},
		},
		LinkName:     "name",
		SearchFields: []adminSearchField{{SQL: "f.name"}, {SQL: "u.email"}},
		Ordering:     []string{"f.name ASC"},
		SelectSQL: "f.id, f.name, COALESCE(u.email, ''), COALESCE(p.name, ''), " +
			"(SELECT COUNT(*) FROM designs_design d WHERE d.folder_id = f.id), f.created_at",
		FromSQL: "designs_folder f LEFT JOIN accounts_user u ON u.id = f.owner_id " +
			"LEFT JOIN designs_folder p ON p.id = f.parent_id",
		ScanList: func(a *App, rows *sql.Rows) (adminListRow, error) {
			var id, name, owner, parent, createdAt string
			var total int
			if err := rows.Scan(&id, &name, &owner, &parent, &total, &createdAt); err != nil {
				return adminListRow{}, err
			}
			return adminListRow{ID: id, Cells: map[string]adminCell{
				"name":         adminTextCell(name),
				"owner":        adminTextCell(owner),
				"parent":       adminTextCell(parent),
				"design_total": adminIntCell(total),
				"created_at":   adminTextCell(createdAt),
			}}, nil
		},
		AllowAdd: false, AllowChange: true, AllowDelete: true,
	}
	m.Get = func(a *App, id string) (*adminObject, error) {
		var name, owner, parentName, createdAt, updatedAt string
		var parentID sql.NullString
		err := a.DB.QueryRow(`SELECT f.name, COALESCE(u.email, ''), COALESCE(p.name, ''),
			f.created_at, f.updated_at, f.parent_id
			FROM designs_folder f LEFT JOIN accounts_user u ON u.id = f.owner_id
			LEFT JOIN designs_folder p ON p.id = f.parent_id WHERE f.id = ?`, id).Scan(
			&name, &owner, &parentName, &createdAt, &updatedAt, &parentID)
		if err != nil {
			return nil, err
		}
		if parentName == "" {
			parentName = "-"
		}
		return &adminObject{
			ID: id, Repr: name,
			Values:  map[string]string{"name": name, "owner": owner, "created_at": createdAt, "updated_at": updatedAt},
			Display: map[string]template.HTML{"parent": template.HTML(djangoEscape(parentName))},
			Raw:     map[string]any{},
		}, nil
	}
	m.Fieldsets = []adminFieldset{{Title: "", Fields: []adminField{
		{Name: "name", Label: "Name", Kind: adminText, Required: true, Wide: true},
		{Name: "owner", Label: "Owner", Kind: adminReadonly},
		{Name: "parent", Label: "Parent", Kind: adminReadonly},
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
		_, err := a.DB.Exec(`UPDATE designs_folder SET name = ?, updated_at = ? WHERE id = ?`,
			name, formatDjangoDateTime(time.Now()), id)
		return nil, err
	}
	m.Delete = func(a *App, actor *UserRow, id string) error {
		_, err := a.DB.Exec(`DELETE FROM designs_folder WHERE id = ?`, id)
		return err
	}
	m.Repr = func(a *App, id string) string {
		var name string
		_ = a.DB.QueryRow(`SELECT name FROM designs_folder WHERE id = ?`, id).Scan(&name)
		return name
	}
	return m
}

// adminDistinctChoices lists distinct values of a column as filter choices.
func adminDistinctChoices(a *App, table, column string) []adminFilterChoice {
	rows, err := a.DB.Query("SELECT DISTINCT " + column + " FROM " + table +
		" WHERE " + column + " <> '' ORDER BY " + column)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []adminFilterChoice
	for rows.Next() {
		var value string
		if err := rows.Scan(&value); err == nil {
			out = append(out, adminOption(value, value))
		}
	}
	return out
}
