// presets.SizePresetGroup / SizePreset admin pages (catalogue editing).
package doory

import (
	"database/sql"
	"fmt"
	"html/template"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var adminPresetUnits = []adminFilterChoice{
	adminOption("px", "Pixels"), adminOption("mm", "Millimetres"),
	adminOption("cm", "Centimetres"), adminOption("in", "Inches"),
}

func adminModelsPresets() []*adminModel {
	return []*adminModel{adminPresetGroupModel(), adminSizePresetModel()}
}

func adminPresetGroupPath(a *App, groupID string) string {
	var parts []string
	current := groupID
	for i := 0; i < 8 && current != ""; i++ {
		var label string
		var parent sql.NullString
		if err := a.DB.QueryRow(`SELECT label, parent_id FROM presets_sizepresetgroup WHERE id = ?`,
			current).Scan(&label, &parent); err != nil {
			break
		}
		parts = append([]string{label}, parts...)
		if !parent.Valid {
			break
		}
		current = parent.String
	}
	return strings.Join(parts, " → ")
}

func adminPresetGroupChoices(a *App) []adminFilterChoice {
	rows, err := a.DB.Query(`SELECT id, label FROM presets_sizepresetgroup ORDER BY sort_order, label`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	out := []adminFilterChoice{adminOption("", "---------")}
	for rows.Next() {
		var id, label string
		if err := rows.Scan(&id, &label); err == nil {
			out = append(out, adminOption(id, label))
		}
	}
	return out
}

func adminPresetGroupModel() *adminModel {
	m := &adminModel{
		AppLabel: "presets", Model: "sizepresetgroup", Verbose: "size preset group",
		Plural: "Size preset groups", Name: "size preset group", Table: "presets_sizepresetgroup",
		Columns: []adminColumn{
			{Name: "label", Label: "Label", Sort: "g.label", Link: true},
			{Name: "slug", Label: "Slug", Sort: "g.slug"},
			{Name: "path", Label: "Path"},
			{Name: "icon", Label: "Icon", Sort: "g.icon"},
			{Name: "preset_count", Label: "Presets", Sort: "_presets"},
			{Name: "is_active", Label: "Is active", Sort: "g.is_active"},
		},
		LinkName:     "label",
		SearchFields: []adminSearchField{{SQL: "g.label"}, {SQL: "g.slug"}},
		ListEditable: []string{"is_active"},
		Filters: []adminFilter{
			{Label: "Is active", Param: "is_active__exact", Kind: "bool", Column: "g.is_active"},
			{Label: "Parent", Param: "parent__exact", Kind: "fk", Column: "g.parent_id",
				OptionsFn: adminPresetGroupChoices},
		},
		Ordering: []string{"g.sort_order ASC", "g.label ASC"},
		SelectSQL: "g.id, g.label, g.slug, g.parent_id, g.icon, " +
			"(SELECT COUNT(*) FROM presets_sizepreset p WHERE p.group_id = g.id), g.is_active",
		FromSQL: "presets_sizepresetgroup g",
		ScanList: func(a *App, rows *sql.Rows) (adminListRow, error) {
			var id, label, slug, icon string
			var parent sql.NullString
			var count, active int
			if err := rows.Scan(&id, &label, &slug, &parent, &icon, &count, &active); err != nil {
				return adminListRow{}, err
			}
			path := ""
			if parent.Valid {
				path = adminPresetGroupPath(nil, "")
			}
			_ = path
			return adminListRow{ID: id, Cells: map[string]adminCell{
				"label":        adminTextCell(label),
				"slug":         adminTextCell(slug),
				"path":         adminTextCell(label),
				"icon":         adminTextCell(icon),
				"preset_count": adminIntCell(count),
				"is_active":    adminTextCell(adminBoolText(active)),
			}}, nil
		},
		AllowAdd: true, AllowChange: true, AllowDelete: true,
		AutocompleteFields: map[string]string{"parent": "presets.sizepresetgroup"},
		Choices: map[string]func(a *App) []adminFilterChoice{
			"parent": adminPresetGroupChoices,
		},
		Actions: []adminAction{
			{Name: "activate", Label: "Activate",
				Run: func(a *App, actor *UserRow, ids []string) ([]adminMessage, error) {
					n, err := a.adminUpdateSelected("presets_sizepresetgroup", "is_active", "TRUE", ids, "")
					return []adminMessage{adminSuccess(fmt.Sprintf("%d group(s) activated.", n))}, err
				}},
			{Name: "deactivate", Label: "Deactivate (hides everything inside)",
				Run: func(a *App, actor *UserRow, ids []string) ([]adminMessage, error) {
					n, err := a.adminUpdateSelected("presets_sizepresetgroup", "is_active", "FALSE", ids, "")
					return []adminMessage{adminSuccess(fmt.Sprintf("%d group(s) deactivated.", n))}, err
				}},
		},
	}
	m.Get = func(a *App, id string) (*adminObject, error) {
		var label, slug, icon string
		var parent sql.NullString
		var sortOrder, active int
		var createdAt, updatedAt string
		err := a.DB.QueryRow(`SELECT label, slug, icon, parent_id, sort_order, is_active, created_at,
			updated_at FROM presets_sizepresetgroup WHERE id = ?`, id).Scan(&label, &slug, &icon, &parent,
			&sortOrder, &active, &createdAt, &updatedAt)
		if err != nil {
			return nil, err
		}
		parentID := ""
		parentLabel := "—"
		if parent.Valid {
			parentID = parent.String
			parentLabel = adminPresetGroupPath(a, parent.String)
		}
		return &adminObject{
			ID: id, Repr: label,
			Values: map[string]string{"label": label, "slug": slug, "icon": icon, "parent": parentID,
				"is_active": boolFlag(active), "created_at": createdAt, "updated_at": updatedAt},
			Display: map[string]template.HTML{"parent": template.HTML(djangoEscape(parentLabel)),
				"created_at": template.HTML(djangoEscape(createdAt)),
				"updated_at": template.HTML(djangoEscape(updatedAt))},
			Raw: map[string]any{},
		}, nil
	}
	m.New = func(a *App) *adminObject {
		return &adminObject{Values: map[string]string{"is_active": "1"},
			Display: map[string]template.HTML{"parent": "—"}, Raw: map[string]any{}}
	}
	m.Fieldsets = []adminFieldset{{Title: "", Fields: []adminField{
		{Name: "label", Label: "Label", Kind: adminText, Required: true, Wide: true},
		{Name: "slug", Label: "Slug", Kind: adminText, Required: true, Wide: true},
		{Name: "parent", Label: "Parent", Kind: adminSelect, Wide: true},
		{Name: "icon", Label: "Icon", Kind: adminText, Wide: true},
		{Name: "is_active", Label: "Is active", Kind: adminBool},
	}}}
	m.Add = func(a *App, actor *UserRow, form *adminForm) (string, adminErrors, error) {
		values, errs := adminPresetGroupValues(a, form, "")
		if errs.any() {
			return "", errs, nil
		}
		id := newUUID4Hex()
		now := formatDjangoDateTime(time.Now())
		_, err := a.DB.Exec(`INSERT INTO presets_sizepresetgroup
			(id, created_at, updated_at, slug, label, icon, sort_order, is_active, parent_id)
			VALUES (?, ?, ?, ?, ?, ?, 0, ?, ?)`, id, now, now, values["slug"], values["label"],
			values["icon"], boolSQL(form.Has("is_active")), nullIfEmpty(values["parent"]))
		return id, nil, err
	}
	m.Change = func(a *App, actor *UserRow, id string, form *adminForm) (adminErrors, error) {
		values, errs := adminPresetGroupValues(a, form, id)
		if errs.any() {
			return errs, nil
		}
		_, err := a.DB.Exec(`UPDATE presets_sizepresetgroup SET label = ?, slug = ?, icon = ?, parent_id = ?,
			is_active = ?, updated_at = ? WHERE id = ?`, values["label"], values["slug"], values["icon"],
			nullIfEmpty(values["parent"]), boolSQL(form.Has("is_active")),
			formatDjangoDateTime(time.Now()), id)
		return nil, err
	}
	m.Delete = func(a *App, actor *UserRow, id string) error {
		_, _ = a.DB.Exec(`UPDATE presets_sizepresetgroup SET parent_id = NULL WHERE parent_id = ?`, id)
		_, _ = a.DB.Exec(`DELETE FROM presets_sizepreset WHERE group_id = ?`, id)
		_, err := a.DB.Exec(`DELETE FROM presets_sizepresetgroup WHERE id = ?`, id)
		return err
	}
	m.Repr = func(a *App, id string) string {
		var label string
		_ = a.DB.QueryRow(`SELECT label FROM presets_sizepresetgroup WHERE id = ?`, id).Scan(&label)
		return label
	}
	return m
}

func adminPresetGroupValues(a *App, form *adminForm, id string) (map[string]string, adminErrors) {
	errs := adminErrors{}
	values := map[string]string{
		"label":  strings.TrimSpace(form.Value("label")),
		"slug":   strings.TrimSpace(form.Value("slug")),
		"icon":   strings.TrimSpace(form.Value("icon")),
		"parent": strings.TrimSpace(form.Value("parent")),
	}
	if values["label"] == "" {
		errs.add("label", "This field is required.")
	}
	if values["slug"] == "" {
		errs.add("slug", "This field is required.")
	}
	values["parent"] = hexUUID(values["parent"])
	if values["parent"] != "" {
		if values["parent"] == id {
			errs.add("parent", "A group cannot be its own parent.")
		} else {
			var exists int
			_ = a.DB.QueryRow(`SELECT COUNT(*) FROM presets_sizepresetgroup WHERE id = ?`,
				values["parent"]).Scan(&exists)
			if exists == 0 {
				errs.add("parent", "Select a valid choice. That choice is not one of the available choices.")
			}
		}
	}
	_ = a
	return values, errs
}

func adminSizePresetModel() *adminModel {
	m := &adminModel{
		AppLabel: "presets", Model: "sizepreset", Verbose: "size preset", Plural: "Size presets",
		Name: "size preset", Table: "presets_sizepreset",
		Columns: []adminColumn{
			{Name: "label", Label: "Label", Sort: "p.label", Link: true},
			{Name: "slug", Label: "Slug", Sort: "p.slug"},
			{Name: "group_path", Label: "Group", Sort: "g.label"},
			{Name: "size", Label: "Size"},
			{Name: "pixels", Label: "Pixels"},
			{Name: "is_quick", Label: "Is quick", Sort: "p.is_quick"},
			{Name: "is_active", Label: "Is active", Sort: "p.is_active"},
		},
		LinkName: "label",
		SearchFields: []adminSearchField{{SQL: "p.label"}, {SQL: "p.slug"}, {SQL: "p.tags"},
			{SQL: "p.aliases"}, {SQL: "g.label"}},
		ListEditable: []string{"is_quick", "is_active"},
		PerPage:      100,
		Filters: []adminFilter{
			{Label: "Is active", Param: "is_active__exact", Kind: "bool", Column: "p.is_active"},
			{Label: "Is quick", Param: "is_quick__exact", Kind: "bool", Column: "p.is_quick"},
			{Label: "Unit", Param: "unit__exact", Kind: "choice", Column: "p.unit",
				Options: adminPresetUnits},
			{Label: "Group", Param: "group__exact", Kind: "fk", Column: "p.group_id",
				OptionsFn: adminPresetGroupChoices},
		},
		Ordering: []string{"g.sort_order ASC", "p.sort_order ASC", "p.label ASC"},
		SelectSQL: "p.id, p.label, p.slug, p.group_id, COALESCE(g.label, ''), p.width, p.height, p.unit, " +
			"p.is_quick, p.is_active",
		FromSQL: "presets_sizepreset p LEFT JOIN presets_sizepresetgroup g ON g.id = p.group_id",
		ScanList: func(a *App, rows *sql.Rows) (adminListRow, error) {
			var id, label, slug, group, unit string
			var groupID sql.NullString
			var width, height float64
			var quick, active int
			if err := rows.Scan(&id, &label, &slug, &groupID, &group, &width, &height, &unit,
				&quick, &active); err != nil {
				return adminListRow{}, err
			}
			return adminListRow{ID: id, Cells: map[string]adminCell{
				"label":      adminTextCell(label),
				"slug":       adminTextCell(slug),
				"group_path": adminTextCell(group),
				"size":       adminTextCell(fmt.Sprintf("%g x %g %s", width, height, unit)),
				"pixels":     adminTextCell(fmt.Sprintf("%d x %d px", toPx(width, unit), toPx(height, unit))),
				"is_quick":   adminTextCell(adminBoolText(quick)),
				"is_active":  adminTextCell(adminBoolText(active)),
			}}, nil
		},
		AllowAdd: true, AllowChange: true, AllowDelete: true,
		AutocompleteFields: map[string]string{"group": "presets.sizepresetgroup"},
		Choices: map[string]func(a *App) []adminFilterChoice{
			"group": adminPresetGroupChoices,
			"unit":  func(a *App) []adminFilterChoice { return adminPresetUnits },
		},
		Actions: []adminAction{
			{Name: "activate", Label: "Activate",
				Run: func(a *App, actor *UserRow, ids []string) ([]adminMessage, error) {
					n, err := a.adminUpdateSelected("presets_sizepreset", "is_active", "TRUE", ids,
						"is_active = FALSE")
					return []adminMessage{adminSuccess(fmt.Sprintf("%d preset(s) activated.", n))}, err
				}},
			{Name: "deactivate", Label: "Deactivate",
				Run: func(a *App, actor *UserRow, ids []string) ([]adminMessage, error) {
					n, err := a.adminUpdateSelected("presets_sizepreset", "is_active", "FALSE", ids, "")
					return []adminMessage{adminSuccess(fmt.Sprintf("%d preset(s) deactivated.", n))}, err
				}},
		},
	}
	m.Get = func(a *App, id string) (*adminObject, error) {
		var label, slug, group, unit, icon, tags, aliases, meta, createdAt, updatedAt string
		var groupID sql.NullString
		var width, height float64
		var quick, active int
		err := a.DB.QueryRow(`SELECT p.label, p.slug, p.group_id, COALESCE(g.label, ''), p.width, p.height,
			p.unit, p.icon, p.tags, p.aliases, p.meta, p.is_quick, p.is_active, p.created_at, p.updated_at
			FROM presets_sizepreset p LEFT JOIN presets_sizepresetgroup g ON g.id = p.group_id
			WHERE p.id = ?`, id).Scan(&label, &slug, &groupID, &group, &width, &height, &unit, &icon,
			&tags, &aliases, &meta, &quick, &active, &createdAt, &updatedAt)
		if err != nil {
			return nil, err
		}
		groupValue := ""
		if groupID.Valid {
			groupValue = groupID.String
		}
		return &adminObject{
			ID: id, Repr: label,
			Values: map[string]string{
				"label": label, "slug": slug, "group": groupValue, "width": trimFloat(width),
				"height": trimFloat(height), "unit": unit, "icon": icon,
				"tags":    strings.Join(adminJSONList(tags), ", "),
				"aliases": strings.Join(adminJSONList(aliases), ", "),
				"meta":    meta, "is_quick": boolFlag(quick), "is_active": boolFlag(active),
			},
			Display: map[string]template.HTML{
				"pixels":     template.HTML(djangoEscape(fmt.Sprintf("%d x %d px", toPx(width, unit), toPx(height, unit)))),
				"created_at": template.HTML(djangoEscape(createdAt)),
				"updated_at": template.HTML(djangoEscape(updatedAt)),
			},
			Raw: map[string]any{},
		}, nil
	}
	m.New = func(a *App) *adminObject {
		return &adminObject{Values: map[string]string{"unit": "px", "is_active": "1", "meta": "{}",
			"tags": "", "aliases": ""}, Display: map[string]template.HTML{"pixels": "—"},
			Raw: map[string]any{}}
	}
	m.Fieldsets = []adminFieldset{
		{Title: "", Fields: []adminField{
			{Name: "label", Label: "Label", Kind: adminText, Required: true, Wide: true},
			{Name: "slug", Label: "Slug", Kind: adminText, Required: true, Wide: true},
			{Name: "group", Label: "Group", Kind: adminSelect, Required: true, Wide: true},
			{Name: "icon", Label: "Icon", Kind: adminText, Wide: true},
		}},
		{Title: "Size", Fields: []adminField{
			{Name: "width", Label: "Width", Kind: adminDecimal},
			{Name: "height", Label: "Height", Kind: adminDecimal},
			{Name: "unit", Label: "Unit", Kind: adminSelect},
			{Name: "pixels", Label: "Pixels", Kind: adminReadonly},
		}},
		{Title: "Finding it", Fields: []adminField{
			{Name: "tags", Label: "Tags", Kind: adminText, Wide: true, Help: "Comma-separated, at most 20."},
			{Name: "aliases", Label: "Aliases", Kind: adminText, Wide: true,
				Help: "Old slugs that should still mean this preset; comma-separated, at most 20."},
			{Name: "is_quick", Label: "Is quick", Kind: adminBool},
			{Name: "is_active", Label: "Is active", Kind: adminBool},
		}},
		{Title: "Extras", Fields: []adminField{
			{Name: "meta", Label: "Meta", Kind: adminTextarea, Wide: true},
		}, Collapse: true},
		{Title: "Audit", Fields: []adminField{
			{Name: "created_at", Label: "Created at", Kind: adminReadonly},
			{Name: "updated_at", Label: "Updated at", Kind: adminReadonly},
		}, Collapse: true},
	}
	m.Add = func(a *App, actor *UserRow, form *adminForm) (string, adminErrors, error) {
		values, errs := adminSizePresetValues(a, form, "")
		if errs.any() {
			return "", errs, nil
		}
		id := newUUID4Hex()
		now := formatDjangoDateTime(time.Now())
		_, err := a.DB.Exec(`INSERT INTO presets_sizepreset
			(id, created_at, updated_at, slug, label, width, height, unit, icon, sort_order, is_active,
			 is_quick, tags, aliases, meta, group_id)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 0, ?, ?, ?, ?, ?, ?)`, id, now, now, values["slug"],
			values["label"], values["width"], values["height"], values["unit"], values["icon"],
			boolSQL(form.Has("is_active")), boolSQL(form.Has("is_quick")), values["tags_json"],
			values["aliases_json"], values["meta"], values["group"])
		return id, nil, err
	}
	m.Change = func(a *App, actor *UserRow, id string, form *adminForm) (adminErrors, error) {
		values, errs := adminSizePresetValues(a, form, id)
		if errs.any() {
			return errs, nil
		}
		_, err := a.DB.Exec(`UPDATE presets_sizepreset SET label = ?, slug = ?, group_id = ?, width = ?,
			height = ?, unit = ?, icon = ?, is_quick = ?, is_active = ?, tags = ?, aliases = ?, meta = ?,
			updated_at = ? WHERE id = ?`, values["label"], values["slug"], values["group"],
			values["width"], values["height"], values["unit"], values["icon"],
			boolSQL(form.Has("is_quick")), boolSQL(form.Has("is_active")), values["tags_json"],
			values["aliases_json"], values["meta"], formatDjangoDateTime(time.Now()), id)
		return nil, err
	}
	m.Delete = func(a *App, actor *UserRow, id string) error {
		_, err := a.DB.Exec(`DELETE FROM presets_sizepreset WHERE id = ?`, id)
		return err
	}
	m.Repr = func(a *App, id string) string {
		var label string
		_ = a.DB.QueryRow(`SELECT label FROM presets_sizepreset WHERE id = ?`, id).Scan(&label)
		return label
	}
	return m
}

func adminSizePresetValues(a *App, form *adminForm, id string) (map[string]string, adminErrors) {
	errs := adminErrors{}
	values := map[string]string{
		"label":  strings.TrimSpace(form.Value("label")),
		"slug":   strings.TrimSpace(form.Value("slug")),
		"group":  hexUUID(strings.TrimSpace(form.Value("group"))),
		"unit":   form.Value("unit"),
		"icon":   strings.TrimSpace(form.Value("icon")),
		"width":  strings.TrimSpace(form.Value("width")),
		"height": strings.TrimSpace(form.Value("height")),
		"meta":   strings.TrimSpace(form.Value("meta")),
	}
	if values["label"] == "" {
		errs.add("label", "This field is required.")
	}
	if values["slug"] == "" {
		errs.add("slug", "This field is required.")
	} else {
		var taken int
		_ = a.DB.QueryRow(`SELECT COUNT(*) FROM presets_sizepreset WHERE slug = ? AND id <> ?`,
			values["slug"], id).Scan(&taken)
		if taken > 0 {
			errs.add("slug", "Size preset with this Slug already exists.")
		}
	}
	if values["group"] == "" {
		errs.add("group", "This field is required.")
	} else {
		var exists int
		_ = a.DB.QueryRow(`SELECT COUNT(*) FROM presets_sizepresetgroup WHERE id = ?`,
			values["group"]).Scan(&exists)
		if exists == 0 {
			errs.add("group", "Select a valid choice. That choice is not one of the available choices.")
		}
	}
	validUnit := false
	for _, choice := range adminPresetUnits {
		if choice.Value == values["unit"] {
			validUnit = true
		}
	}
	if !validUnit {
		errs.add("unit", "Select a valid choice. That choice is not one of the available choices.")
	}
	for _, side := range []string{"width", "height"} {
		if values[side] == "" {
			errs.add(side, "This field is required.")
			continue
		}
		number, err := strconv.ParseFloat(values[side], 64)
		if err != nil || number <= 0 {
			errs.add(side, "The "+side+" must be a number greater than 0.")
		}
	}
	tags, tagErr := adminPresetLabelList(form.Value("tags"), "tags", 20, nil)
	if tagErr != "" {
		errs.add("tags", tagErr)
	}
	values["tags_json"] = pyJSONStringList(tags)
	aliasPattern := presetSlugPattern
	aliases, aliasErr := adminPresetLabelList(form.Value("aliases"), "aliases", 20, aliasPattern)
	if aliasErr != "" {
		errs.add("aliases", aliasErr)
	}
	values["aliases_json"] = pyJSONStringList(aliases)
	if values["meta"] == "" {
		values["meta"] = "{}"
	}
	return values, errs
}

// adminPresetLabelList is the admin's comma-separated label parser.
func adminPresetLabelList(raw, what string, limit int, pattern *regexp.Regexp) ([]string, string) {
	var out []string
	seen := map[string]bool{}
	for _, part := range strings.Split(raw, ",") {
		value := strings.Join(strings.Fields(part), " ")
		if value == "" || seen[value] {
			continue
		}
		if len([]rune(value)) > 32 {
			return nil, "Each " + what + " entry must be at most 32 characters."
		}
		if pattern != nil && !pattern.MatchString(value) {
			return nil, "Letters, digits, dots, hyphens and underscores."
		}
		seen[value] = true
		out = append(out, strings.ToLower(value))
	}
	if len(out) > limit {
		return nil, fmt.Sprintf("Use at most %d %s.", limit, what)
	}
	return out, ""
}
