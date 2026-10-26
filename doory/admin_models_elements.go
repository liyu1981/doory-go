// elements.ElementPack / Element admin pages. The element SVG always goes
// through processSVG (sanitise + metadata) exactly like the importer; the
// pack's bulk upload reuses the importer itself.
package doory

import (
	"database/sql"
	"fmt"
	"html/template"
	"net/http"
	"strings"
	"time"
)

var adminElementKinds = []adminFilterChoice{
	adminOption("shape", "Shape"), adminOption("line", "Line"), adminOption("icon", "Icon"),
	adminOption("sticker", "Sticker"), adminOption("illustration", "Illustration"),
	adminOption("frame", "Frame"), adminOption("graphic", "Graphic"),
}

var adminElementStyles = []adminFilterChoice{
	adminOption("", "---------"), adminOption("outline", "Outline"), adminOption("filled", "Filled"),
	adminOption("duotone", "Duotone"), adminOption("colour", "Colour"),
}

func adminModelsElements() []*adminModel {
	return []*adminModel{adminElementPackModel(), adminElementModel()}
}

func adminElementPackChoices(a *App) []adminFilterChoice {
	rows, err := a.DB.Query(`SELECT id, name FROM elements_elementpack ORDER BY sort_order, name`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	out := []adminFilterChoice{adminOption("", "---------")}
	for rows.Next() {
		var id, name string
		if err := rows.Scan(&id, &name); err == nil {
			out = append(out, adminOption(id, name))
		}
	}
	return out
}

func adminElementPackModel() *adminModel {
	m := &adminModel{
		AppLabel: "elements", Model: "elementpack", Verbose: "element pack", Plural: "Element packs",
		Name: "element pack", Table: "elements_elementpack",
		Columns: []adminColumn{
			{Name: "name", Label: "Name", Sort: "p.name", Link: true},
			{Name: "slug", Label: "Slug", Sort: "p.slug"},
			{Name: "kind", Label: "Kind", Sort: "p.kind"},
			{Name: "license", Label: "License", Sort: "p.license"},
			{Name: "published_elements", Label: "Published", Sort: "_published"},
			{Name: "total_elements", Label: "Elements", Sort: "_total"},
			{Name: "is_published", Label: "Is published", Sort: "p.is_published"},
		},
		LinkName:     "name",
		SearchFields: []adminSearchField{{SQL: "p.name"}, {SQL: "p.slug"}, {SQL: "p.description"}},
		ListEditable: []string{"is_published"},
		Filters: []adminFilter{
			{Label: "Kind", Param: "kind__exact", Kind: "choice", Column: "p.kind", Options: adminElementKinds},
			{Label: "Is published", Param: "is_published__exact", Kind: "bool", Column: "p.is_published"},
		},
		Ordering: []string{"p.sort_order ASC", "p.name ASC"},
		SelectSQL: "p.id, p.name, p.slug, p.kind, p.license, " +
			"(SELECT COUNT(*) FROM elements_element e WHERE e.pack_id = p.id AND e.is_published = TRUE), " +
			"(SELECT COUNT(*) FROM elements_element e WHERE e.pack_id = p.id), p.is_published",
		FromSQL: "elements_elementpack p",
		ScanList: func(a *App, rows *sql.Rows) (adminListRow, error) {
			var id, name, slug, kind, license string
			var published, total, isPublished int
			if err := rows.Scan(&id, &name, &slug, &kind, &license, &published, &total, &isPublished); err != nil {
				return adminListRow{}, err
			}
			return adminListRow{ID: id, Cells: map[string]adminCell{
				"name":               adminTextCell(name),
				"slug":               adminTextCell(slug),
				"kind":               adminTextCell(kind),
				"license":            adminTextCell(license),
				"published_elements": adminIntCell(published),
				"total_elements":     adminIntCell(total),
				"is_published":       adminTextCell(adminBoolText(isPublished)),
			}}, nil
		},
		AllowAdd: true, AllowChange: true, AllowDelete: true,
		Actions: []adminAction{
			{Name: "publish", Label: "Publish selected packs",
				Run: func(a *App, actor *UserRow, ids []string) ([]adminMessage, error) {
					n, err := a.adminUpdateSelected("elements_elementpack", "is_published", "TRUE", ids, "")
					return []adminMessage{adminSuccess(fmt.Sprintf("%d pack(s) published.", n))}, err
				}},
			{Name: "unpublish", Label: "Unpublish selected packs",
				Run: func(a *App, actor *UserRow, ids []string) ([]adminMessage, error) {
					n, err := a.adminUpdateSelected("elements_elementpack", "is_published", "FALSE", ids, "")
					return []adminMessage{adminSuccess(fmt.Sprintf("%d pack(s) unpublished.", n))}, err
				}},
		},
		DetailActions: []adminDetailAction{
			{Name: "upload-svgs", Label: "Upload SVGs", Perm: "change",
				Run: func(a *App, w http.ResponseWriter, r *http.Request, actor *UserRow, obj *adminObject) {
					adminRedirect(w, r, "/admin/elements/elementpack/"+adminURLID(mustPackModel(a), obj.ID)+"/upload/")
				}},
		},
	}
	m.Get = func(a *App, id string) (*adminObject, error) {
		var name, slug, kind, license, licenseURL, attribution, sourceURL, description, thumbnail string
		var sourceRef string
		var syncedAt sql.NullString
		var sortOrder, isPublished int
		var createdAt, updatedAt string
		err := a.DB.QueryRow(`SELECT name, slug, kind, license, license_url, attribution, source_url,
			description, thumbnail, source_ref, synced_at, sort_order, is_published, created_at, updated_at
			FROM elements_elementpack WHERE id = ?`, id).Scan(&name, &slug, &kind, &license, &licenseURL,
			&attribution, &sourceURL, &description, &thumbnail, &sourceRef, &syncedAt, &sortOrder,
			&isPublished, &createdAt, &updatedAt)
		if err != nil {
			return nil, err
		}
		display := map[string]template.HTML{
			"preview_grid": adminPackPreviewGrid(a, id, slug),
			"source_ref":   template.HTML("—"),
			"synced_at":    template.HTML("—"),
			"created_at":   template.HTML(djangoEscape(createdAt)),
			"updated_at":   template.HTML(djangoEscape(updatedAt)),
		}
		if sourceRef != "" {
			display["source_ref"] = template.HTML(djangoEscape(sourceRef))
		}
		if syncedAt.Valid && syncedAt.String != "" {
			display["synced_at"] = template.HTML(djangoEscape(syncedAt.String))
		}
		return &adminObject{
			ID: id, Repr: name,
			Values: map[string]string{
				"name": name, "slug": slug, "kind": kind, "license": license,
				"license_url": licenseURL, "attribution": attribution, "source_url": sourceURL,
				"description": description, "thumbnail": thumbnail,
				"is_published": boolFlag(isPublished),
			},
			Display: display,
			Raw:     map[string]any{},
		}, nil
	}
	m.New = func(a *App) *adminObject {
		return &adminObject{Values: map[string]string{"is_published": "1"}, Display: map[string]template.HTML{
			"preview_grid": template.HTML("—"), "source_ref": template.HTML("—"), "synced_at": template.HTML("—"),
		}, Raw: map[string]any{}}
	}
	m.Fieldsets = []adminFieldset{
		{Title: "", Fields: []adminField{
			{Name: "name", Label: "Name", Kind: adminText, Required: true, Wide: true},
			{Name: "slug", Label: "Slug", Kind: adminText, Required: true, Wide: true},
			{Name: "kind", Label: "Kind", Kind: adminSelect, Choices: adminElementKinds},
			{Name: "description", Label: "Description", Kind: adminTextarea, Wide: true},
			{Name: "is_published", Label: "Is published", Kind: adminBool},
		}},
		{Title: "Licence", Fields: []adminField{
			{Name: "license", Label: "License", Kind: adminText, Required: true, Wide: true},
			{Name: "license_url", Label: "License URL", Kind: adminText, Wide: true},
			{Name: "attribution", Label: "Attribution", Kind: adminTextarea, Wide: true},
			{Name: "source_url", Label: "Source URL", Kind: adminText, Wide: true},
		}},
		{Title: "Synced content", Fields: []adminField{
			{Name: "source_ref", Label: "Source ref", Kind: adminReadonly},
			{Name: "synced_at", Label: "Synced at", Kind: adminReadonly},
		}, Collapse: true},
		{Title: "Thumbnail", Fields: []adminField{
			{Name: "thumbnail", Label: "Thumbnail", Kind: adminFile, Wide: true},
		}},
		{Title: "Elements", Fields: []adminField{
			{Name: "preview_grid", Label: "Preview", Kind: adminReadonly, Wide: true},
		}},
	}
	m.Add = func(a *App, actor *UserRow, form *adminForm) (string, adminErrors, error) {
		values, errs := adminPackValues(a, form, "")
		if errs.any() {
			return "", errs, nil
		}
		id := newUUID4Hex()
		now := formatDjangoDateTime(time.Now())
		_, err := a.DB.Exec(`INSERT INTO elements_elementpack
			(id, created_at, updated_at, slug, name, kind, license, source_url, description, thumbnail,
			 sort_order, is_published, attribution, license_url, source_ref, sync_fingerprint, synced_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, '', 0, ?, ?, ?, '', '', NULL)`,
			id, now, now, values["slug"], values["name"], values["kind"], values["license"],
			values["source_url"], values["description"], boolSQL(form.Has("is_published")),
			values["attribution"], values["license_url"])
		if err != nil {
			return "", nil, err
		}
		if err := a.adminStorePackThumb(a, id, values["slug"], form); err != nil {
			return "", nil, err
		}
		return id, nil, nil
	}
	m.Change = func(a *App, actor *UserRow, id string, form *adminForm) (adminErrors, error) {
		values, errs := adminPackValues(a, form, id)
		if errs.any() {
			return errs, nil
		}
		_, err := a.DB.Exec(`UPDATE elements_elementpack SET slug = ?, name = ?, kind = ?, license = ?,
			license_url = ?, attribution = ?, source_url = ?, description = ?, is_published = ?, updated_at = ?
			WHERE id = ?`, values["slug"], values["name"], values["kind"], values["license"],
			values["license_url"], values["attribution"], values["source_url"], values["description"],
			boolSQL(form.Has("is_published")), formatDjangoDateTime(time.Now()), id)
		if err != nil {
			return nil, err
		}
		return nil, a.adminStorePackThumb(a, id, values["slug"], form)
	}
	m.Delete = func(a *App, actor *UserRow, id string) error {
		var thumbnail string
		_ = a.DB.QueryRow(`SELECT thumbnail FROM elements_elementpack WHERE id = ?`, id).Scan(&thumbnail)
		rows, _ := a.DB.Query(`SELECT svg FROM elements_element WHERE pack_id = ?`, id)
		var files []string
		if rows != nil {
			for rows.Next() {
				var name string
				if err := rows.Scan(&name); err == nil && name != "" {
					files = append(files, name)
				}
			}
			_ = rows.Close()
		}
		_, _ = a.DB.Exec(`DELETE FROM elements_element WHERE pack_id = ?`, id)
		if _, err := a.DB.Exec(`DELETE FROM elements_elementpack WHERE id = ?`, id); err != nil {
			return err
		}
		a.deleteMediaFile(thumbnail)
		for _, name := range files {
			a.deleteMediaFile(name)
		}
		return nil
	}
	m.Repr = func(a *App, id string) string {
		var name string
		_ = a.DB.QueryRow(`SELECT name FROM elements_elementpack WHERE id = ?`, id).Scan(&name)
		return name
	}
	return m
}

func mustPackModel(a *App) *adminModel { return a.admin().byKey["elements.elementpack"] }

func adminPackPreviewGrid(a *App, packID, slug string) template.HTML {
	rows, err := a.DB.Query(`SELECT name, svg, is_published FROM elements_element
		WHERE pack_id = ? ORDER BY sort_order, name, id LIMIT 120`, packID)
	if err != nil {
		return template.HTML("—")
	}
	defer rows.Close()
	var cells strings.Builder
	count := 0
	for rows.Next() {
		var name, svg string
		var published int
		if err := rows.Scan(&name, &svg, &published); err != nil {
			continue
		}
		count++
		opacity := ""
		if published == 0 {
			opacity = ";opacity:.35"
		}
		cells.WriteString(`<div title="` + djangoEscape(name) + `" style="display:inline-block;padding:6px">`)
		cells.WriteString(`<img src="` + djangoEscape(adminMediaURL(svg)) + `" alt="` + djangoEscape(name) +
			`" loading="lazy" style="max-width:72px;max-height:72px` + opacity + `">`)
		cells.WriteString(`</div>`)
	}
	_ = slug
	if count == 0 {
		return template.HTML(djangoEscape("No elements yet. Use “Upload SVGs” to add some."))
	}
	return template.HTML(`<div style="display:flex;flex-wrap:wrap;gap:8px;max-width:56rem">` + cells.String() + `</div>`)
}

func adminPackValues(a *App, form *adminForm, id string) (map[string]string, adminErrors) {
	errs := adminErrors{}
	values := map[string]string{
		"name":        strings.TrimSpace(form.Value("name")),
		"slug":        strings.TrimSpace(form.Value("slug")),
		"kind":        form.Value("kind"),
		"license":     strings.TrimSpace(form.Value("license")),
		"license_url": strings.TrimSpace(form.Value("license_url")),
		"attribution": form.Value("attribution"),
		"source_url":  strings.TrimSpace(form.Value("source_url")),
		"description": form.Value("description"),
	}
	if values["name"] == "" {
		errs.add("name", "This field is required.")
	}
	if values["slug"] == "" {
		errs.add("slug", "This field is required.")
	} else if !templateSlugPattern.MatchString(values["slug"]) {
		errs.add("slug", "Enter a valid “slug” consisting of letters, numbers, underscores or hyphens.")
	} else {
		var taken int
		_ = a.DB.QueryRow(`SELECT COUNT(*) FROM elements_elementpack WHERE slug = ? AND id <> ?`,
			values["slug"], id).Scan(&taken)
		if taken > 0 {
			errs.add("slug", "Element pack with this Slug already exists.")
		}
	}
	if !adminKindValid(values["kind"]) {
		errs.add("kind", "Select a valid choice. That choice is not one of the available choices.")
	}
	if values["license"] == "" {
		errs.add("license", "This field is required.")
	} else if !adminLicenseValid(values["license"]) {
		errs.add("license", "Use an SPDX licence identifier such as CC0-1.0, MIT or Apache-2.0.")
	}
	for _, field := range []string{"license_url", "source_url"} {
		if values[field] != "" && !strings.HasPrefix(values[field], "http://") &&
			!strings.HasPrefix(values[field], "https://") {
			errs.add(field, "Enter a valid URL.")
		}
	}
	return values, errs
}

func adminKindValid(kind string) bool {
	for _, choice := range adminElementKinds {
		if choice.Value == kind {
			return true
		}
	}
	return false
}

func adminLicenseValid(value string) bool {
	for _, r := range value {
		if !(r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9' ||
			r == '.' || r == '+' || r == '-' || r == ' ') {
			return false
		}
	}
	return value != ""
}

// adminStorePackThumb re-encodes an uploaded pack thumbnail to a 480 px WebP
// at elements/packs/<slug>.webp.
func (a *App) adminStorePackThumb(app *App, id, slug string, form *adminForm) error {
	upload := form.File("thumbnail")
	if upload == nil {
		return nil
	}
	if len(upload.Data) == 0 || len(upload.Data) > 4*1024*1024 {
		return nil
	}
	width, height, _, err := decodeImageConfig(upload.Data)
	if err != nil || width < 1 || height < 1 {
		return nil
	}
	img, _, err := decodeImageData(upload.Data)
	if err != nil {
		return nil
	}
	data, err := encodeWebP(fitThumbnail(img, 480))
	if err != nil {
		return err
	}
	name := availableThumbnailName(a.Settings.MediaRoot, "elements/packs/"+slug+".webp")
	if err := adminWriteMedia(a.Settings.MediaRoot, name, data); err != nil {
		return err
	}
	_, err = a.DB.Exec(`UPDATE elements_elementpack SET thumbnail = ? WHERE id = ?`, name, id)
	return err
}

// ---- Element ----

func adminElementModel() *adminModel {
	m := &adminModel{
		AppLabel: "elements", Model: "element", Verbose: "element", Plural: "Elements", Name: "element",
		Table: "elements_element",
		Columns: []adminColumn{
			{Name: "preview", Label: "Preview", Link: true},
			{Name: "name", Label: "Name", Sort: "e.name", Link: true},
			{Name: "pack", Label: "Pack", Sort: "p.name"},
			{Name: "kind", Label: "Kind", Sort: "e.kind"},
			{Name: "style", Label: "Style", Sort: "e.style"},
			{Name: "tag_list", Label: "Tags"},
			{Name: "size", Label: "Size"},
			{Name: "is_recolorable", Label: "Is recolorable", Sort: "e.is_recolorable"},
			{Name: "is_published", Label: "Is published", Sort: "e.is_published"},
		},
		LinkName:     "name",
		SearchFields: []adminSearchField{{SQL: "e.name"}, {SQL: "e.key"}, {SQL: "e.tags"}},
		ListEditable: []string{"is_published"},
		PerPage:      60,
		Filters: []adminFilter{
			{Label: "Pack", Param: "pack__exact", Kind: "fk", Column: "e.pack_id",
				OptionsFn: adminElementPackChoices},
			{Label: "Kind", Param: "kind__exact", Kind: "choice", Column: "e.kind", Options: adminElementKinds},
			{Label: "Style", Param: "style__exact", Kind: "choice", Column: "e.style",
				Options: []adminFilterChoice{adminOption("outline", "Outline"), adminOption("filled", "Filled"),
					adminOption("duotone", "Duotone"), adminOption("colour", "Colour")}},
			{Label: "Is published", Param: "is_published__exact", Kind: "bool", Column: "e.is_published"},
			{Label: "Is recolorable", Param: "is_recolorable__exact", Kind: "bool", Column: "e.is_recolorable"},
		},
		Ordering: []string{"e.sort_order ASC", "e.name ASC"},
		SelectSQL: "e.id, e.svg, e.name, COALESCE(p.name, ''), e.kind, e.style, e.tags, e.width, e.height, " +
			"e.is_recolorable, e.is_published",
		FromSQL: "elements_element e LEFT JOIN elements_elementpack p ON p.id = e.pack_id",
		ScanList: func(a *App, rows *sql.Rows) (adminListRow, error) {
			var id, svg, name, pack, kind, style, tags string
			var width, height float64
			var recolorable, published int
			if err := rows.Scan(&id, &svg, &name, &pack, &kind, &style, &tags, &width, &height,
				&recolorable, &published); err != nil {
				return adminListRow{}, err
			}
			tagsList := adminJSONList(tags)
			if len(tagsList) > 6 {
				tagsList = tagsList[:6]
			}
			size := "—"
			if width != 0 {
				size = fmt.Sprintf("%g x %g", width, height)
			}
			return adminListRow{ID: id, Cells: map[string]adminCell{
				"preview":        adminHTMLCell(adminElementPreview(svg, 40)),
				"name":           adminTextCell(name),
				"pack":           adminTextCell(pack),
				"kind":           adminTextCell(kind),
				"style":          adminTextCell(style),
				"tag_list":       adminTextCell(strings.Join(tagsList, ", ")),
				"size":           adminTextCell(size),
				"is_recolorable": adminTextCell(adminBoolText(recolorable)),
				"is_published":   adminTextCell(adminBoolText(published)),
			}}, nil
		},
		AllowAdd: true, AllowChange: true, AllowDelete: true,
		Choices: map[string]func(a *App) []adminFilterChoice{
			"pack":  adminElementPackChoices,
			"kind":  func(a *App) []adminFilterChoice { return adminElementKinds },
			"style": func(a *App) []adminFilterChoice { return adminElementStyles },
		},
		Actions: []adminAction{
			{Name: "publish", Label: "Publish selected elements",
				Run: func(a *App, actor *UserRow, ids []string) ([]adminMessage, error) {
					n, err := a.adminUpdateSelected("elements_element", "is_published", "TRUE", ids, "")
					return []adminMessage{adminSuccess(fmt.Sprintf("%d element(s) published.", n))}, err
				}},
			{Name: "unpublish", Label: "Unpublish selected elements",
				Run: func(a *App, actor *UserRow, ids []string) ([]adminMessage, error) {
					n, err := a.adminUpdateSelected("elements_element", "is_published", "FALSE", ids, "")
					return []adminMessage{adminSuccess(fmt.Sprintf("%d element(s) unpublished.", n))}, err
				}},
		},
	}
	m.Get = func(a *App, id string) (*adminObject, error) {
		var packID, packSlug, key, name, kind, style, tags, svg, colors string
		var width, height float64
		var recolorable, published, sortOrder int
		var createdAt, updatedAt string
		err := a.DB.QueryRow(`SELECT e.pack_id, COALESCE(p.slug, ''), e.key, e.name, e.kind, e.style,
			e.tags, e.svg, e.width, e.height, e.colors, e.is_recolorable, e.is_published, e.sort_order,
			e.created_at, e.updated_at
			FROM elements_element e LEFT JOIN elements_elementpack p ON p.id = e.pack_id WHERE e.id = ?`,
			id).Scan(&packID, &packSlug, &key, &name, &kind, &style, &tags, &svg, &width, &height,
			&colors, &recolorable, &published, &sortOrder, &createdAt, &updatedAt)
		if err != nil {
			return nil, err
		}
		_ = packSlug
		size := "—"
		if width != 0 {
			size = fmt.Sprintf("%g x %g", width, height)
		}
		palette := template.HTML(djangoEscape("None (not recolourable)"))
		if list := adminJSONList(colors); len(list) > 0 {
			var b strings.Builder
			for _, color := range list {
				b.WriteString(`<span title="` + djangoEscape(color) + `" style="display:inline-block;width:1.25rem;` +
					`height:1.25rem;border-radius:4px;background:` + djangoEscape(color) + `"></span> `)
			}
			palette = template.HTML(b.String())
		}
		return &adminObject{
			ID: id, Repr: name,
			Values: map[string]string{
				"pack": packID, "name": name, "kind": kind, "style": style,
				"tags": strings.Join(adminJSONList(tags), ", "), "is_published": boolFlag(published),
			},
			Display: map[string]template.HTML{
				"preview_large":  adminElementPreview(svg, 160),
				"size":           template.HTML(djangoEscape(size)),
				"palette":        palette,
				"is_recolorable": template.HTML(djangoEscape(adminBoolText(recolorable))),
				"key":            template.HTML(djangoEscape(key)),
				"created_at":     template.HTML(djangoEscape(createdAt)),
				"updated_at":     template.HTML(djangoEscape(updatedAt)),
			},
			Raw: map[string]any{},
		}, nil
	}
	m.New = func(a *App) *adminObject {
		return &adminObject{Values: map[string]string{"is_published": "1"}, Display: map[string]template.HTML{
			"preview_large": "—", "size": "—", "palette": template.HTML("None (not recolourable)"),
			"is_recolorable": "False", "key": "—", "created_at": "—", "updated_at": "—",
		}, Raw: map[string]any{}}
	}
	m.Fieldsets = []adminFieldset{
		{Title: "", Fields: []adminField{
			{Name: "pack", Label: "Pack", Kind: adminSelect, Required: true, Wide: true},
			{Name: "name", Label: "Name", Kind: adminText, Required: true, Wide: true},
			{Name: "kind", Label: "Kind", Kind: adminSelect, Required: true},
			{Name: "style", Label: "Style", Kind: adminSelect},
			{Name: "tags", Label: "Tags", Kind: adminText, Wide: true, Help: "Comma-separated."},
			{Name: "is_published", Label: "Is published", Kind: adminBool},
		}},
		{Title: "Artwork", Fields: []adminField{
			{Name: "svg", Label: "SVG", Kind: adminFile, Wide: true},
			{Name: "preview_large", Label: "Preview", Kind: adminReadonly},
			{Name: "size", Label: "Size", Kind: adminReadonly},
			{Name: "palette", Label: "Colours", Kind: adminReadonly, Wide: true},
			{Name: "is_recolorable", Label: "Is recolorable", Kind: adminReadonly},
		}},
		{Title: "Identity", Fields: []adminField{
			{Name: "key", Label: "Key", Kind: adminReadonly},
			{Name: "created_at", Label: "Created at", Kind: adminReadonly},
			{Name: "updated_at", Label: "Updated at", Kind: adminReadonly},
		}, Collapse: true},
	}
	m.Add = func(a *App, actor *UserRow, form *adminForm) (string, adminErrors, error) {
		values, errs := adminElementValues(a, form, "", true)
		if errs.any() {
			return "", errs, nil
		}
		id := newUUID4Hex()
		now := formatDjangoDateTime(time.Now())
		storage := a.adminElementStoragePath(values.packSlug, id)
		if err := adminWriteMedia(a.Settings.MediaRoot, storage, values.svgData); err != nil {
			return "", nil, err
		}
		if _, err := a.DB.Exec(`INSERT INTO elements_element
			(id, created_at, updated_at, key, name, kind, style, tags, search_name, search_tags,
			 content_hash, svg, width, height, colors, is_recolorable, is_published, sort_order, pack_id)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, '', ?, ?, ?, ?, ?, ?, 0, ?)`,
			id, now, now, values.key, values.name, values.kind, values.style,
			values.tagsJSON, elementSearchName(values.name), elementSearchTags(values.tagList),
			storage, values.width, values.height, values.colorsJSON,
			values.recolorable, boolSQL(form.Has("is_published")), values.pack); err != nil {
			return "", nil, err
		}
		return id, nil, nil
	}
	m.Change = func(a *App, actor *UserRow, id string, form *adminForm) (adminErrors, error) {
		values, errs := adminElementValues(a, form, id, false)
		if errs.any() {
			return errs, nil
		}
		var previous string
		_ = a.DB.QueryRow(`SELECT svg FROM elements_element WHERE id = ?`, id).Scan(&previous)
		storage := previous
		if len(values.svgData) > 0 {
			storage = a.adminElementStoragePath(values.packSlug, id)
			if err := adminWriteMedia(a.Settings.MediaRoot, storage, values.svgData); err != nil {
				return nil, err
			}
		}
		query := `UPDATE elements_element SET pack_id = ?, name = ?, kind = ?, style = ?, tags = ?,
			search_name = ?, search_tags = ?, is_published = ?, svg = ?, updated_at = ?`
		args := []any{values.pack, values.name, values.kind, values.style, values.tagsJSON,
			elementSearchName(values.name), elementSearchTags(values.tagList),
			boolSQL(form.Has("is_published")), storage, formatDjangoDateTime(time.Now())}
		if len(values.svgData) > 0 {
			query += `, width = ?, height = ?, colors = ?, is_recolorable = ?`
			args = append(args, values.width, values.height, values.colorsJSON, values.recolorable)
		}
		query += ` WHERE id = ?`
		args = append(args, id)
		if _, err := a.DB.Exec(query, args...); err != nil {
			return nil, err
		}
		if len(values.svgData) > 0 && previous != "" && previous != storage {
			a.deleteMediaFile(previous)
		}
		return nil, nil
	}
	m.Delete = func(a *App, actor *UserRow, id string) error {
		var svg string
		_ = a.DB.QueryRow(`SELECT svg FROM elements_element WHERE id = ?`, id).Scan(&svg)
		if _, err := a.DB.Exec(`DELETE FROM elements_element WHERE id = ?`, id); err != nil {
			return err
		}
		a.deleteMediaFile(svg)
		return nil
	}
	m.Repr = func(a *App, id string) string {
		var name string
		_ = a.DB.QueryRow(`SELECT name FROM elements_element WHERE id = ?`, id).Scan(&name)
		return name
	}
	return m
}

func (a *App) adminElementStoragePath(packSlug, id string) string {
	return "elements/" + packSlug + "/" + dashedUUID(id) + ".svg"
}

func adminElementPreview(svg string, height int) template.HTML {
	if svg == "" {
		return "—"
	}
	return template.HTML(fmt.Sprintf(`<img src="%s" alt="" loading="lazy" style="height:%dpx;max-width:120px">`,
		djangoEscape(adminMediaURL(svg)), height))
}

// adminElementInput is the validated element form (typed; the map form was
// error-prone before the first compile).
type adminElementInput struct {
	pack        string
	packSlug    string
	name        string
	kind        string
	style       string
	tagList     []string
	tagsJSON    string
	key         string
	svgData     []byte
	width       float64
	height      float64
	colorsJSON  string
	recolorable bool
}

// adminElementValues validates the shared element form. `requireSVG` is true
// for add (Django's Element.save raises without artwork).
func adminElementValues(a *App, form *adminForm, id string, requireSVG bool) (adminElementInput, adminErrors) {
	errs := adminErrors{}
	values := adminElementInput{
		pack:    hexUUID(strings.TrimSpace(form.Value("pack"))),
		name:    strings.TrimSpace(form.Value("name")),
		kind:    form.Value("kind"),
		style:   form.Value("style"),
		svgData: []byte(nil),
	}
	{
		var slug string
		_ = a.DB.QueryRow(`SELECT slug FROM elements_elementpack WHERE id = ?`, values.pack).Scan(&slug)
		if slug == "" {
			errs.add("pack", "Select a valid choice. That choice is not one of the available choices.")
		}
		values.packSlug = slug
	}
	if values.name == "" {
		errs.add("name", "This field is required.")
	} else if len([]rune(values.name)) > elementMaxNameLength {
		errs.add("name", fmt.Sprintf("Ensure this value has at most %d characters.", elementMaxNameLength))
	}
	if !adminKindValid(values.kind) {
		errs.add("kind", "Select a valid choice. That choice is not one of the available choices.")
	}
	if style := values.style; style != "" {
		valid := false
		for _, choice := range adminElementStyles {
			if choice.Value == style {
				valid = true
			}
		}
		if !valid {
			errs.add("style", "Select a valid choice. That choice is not one of the available choices.")
		}
	}
	tags, tagErr := adminElementTagList(form.Value("tags"))
	if tagErr != "" {
		errs.add("tags", tagErr)
	}
	values.tagList = tags
	values.tagsJSON = pyJSONStringList(tags)

	upload := form.File("svg")
	if upload != nil {
		processed, perr := processSVG(upload.Data)
		if perr != nil {
			errs.add("svg", perr.Error())
		} else {
			values.svgData = processed.Data
			values.width = processed.Width
			values.height = processed.Height
			values.colorsJSON = pyJSONStringList(processed.Colors)
			values.recolorable = processed.isRecolorable()
		}
	} else if requireSVG {
		errs.add("svg", "This field is required.")
	}
	if requireSVG {
		// key from the name, unique within the pack
		base := slugifyElementKeyPart(values.name)
		if base == "" {
			base = "element"
		}
		if len(base) > 150 {
			base = base[:150]
		}
		candidate := base
		counter := 1
		for {
			var taken int
			_ = a.DB.QueryRow(`SELECT COUNT(*) FROM elements_element WHERE pack_id = ? AND key = ?`,
				values.pack, candidate).Scan(&taken)
			if taken == 0 {
				break
			}
			counter++
			candidate = fmt.Sprintf("%s-%d", base, counter)
		}
		values.key = candidate
	} else {
		var key string
		_ = a.DB.QueryRow(`SELECT key FROM elements_element WHERE id = ?`, id).Scan(&key)
		values.key = key
	}
	return values, errs
}

// adminElementTagList is the admin's clean_tags: lowercase, collapse spaces,
// dedupe, at most 30 tags of up to 40 characters.
func adminElementTagList(raw string) ([]string, string) {
	var tags []string
	seen := map[string]bool{}
	for _, part := range strings.Split(raw, ",") {
		tag := strings.ToLower(strings.Join(strings.Fields(part), " "))
		if tag == "" || seen[tag] {
			continue
		}
		if len([]rune(tag)) > 40 {
			return nil, "Use at most 30 tags of up to 40 characters."
		}
		seen[tag] = true
		tags = append(tags, tag)
	}
	if len(tags) > 30 {
		return nil, "Use at most 30 tags of up to 40 characters."
	}
	return tags, ""
}
