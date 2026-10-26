// gallery.TemplateCategory / Template admin pages, including the JSON
// document upload, thumbnail re-encoding, duplicate and validate actions.
package doory

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"strings"
	"time"
)

func adminModelsGallery() []*adminModel {
	return []*adminModel{adminTemplateCategoryModel(), adminTemplateModel()}
}

// ---- TemplateCategory ----

func adminTemplateCategoryModel() *adminModel {
	m := &adminModel{
		AppLabel: "gallery", Model: "templatecategory", Verbose: "template category",
		Plural: "Template categories", Name: "template category", Table: "gallery_templatecategory",
		Columns: []adminColumn{
			{Name: "name", Label: "Name", Sort: "c.name", Link: true},
			{Name: "slug", Label: "Slug", Sort: "c.slug"},
			{Name: "icon", Label: "Icon", Sort: "c.icon"},
			{Name: "published_templates", Label: "Published"},
			{Name: "total_templates", Label: "Templates"},
		},
		LinkName:     "name",
		SearchFields: []adminSearchField{{SQL: "c.name"}, {SQL: "c.slug"}},
		Ordering:     []string{"c.sort_order ASC", "c.name ASC"},
		SelectSQL: "c.id, c.name, c.slug, c.icon, " +
			"(SELECT COUNT(*) FROM gallery_template t WHERE t.category_id = c.id AND t.is_published = TRUE), " +
			"(SELECT COUNT(*) FROM gallery_template t WHERE t.category_id = c.id)",
		FromSQL: "gallery_templatecategory c",
		ScanList: func(a *App, rows *sql.Rows) (adminListRow, error) {
			var id, name, slug, icon string
			var published, total int
			if err := rows.Scan(&id, &name, &slug, &icon, &published, &total); err != nil {
				return adminListRow{}, err
			}
			return adminListRow{ID: id, Cells: map[string]adminCell{
				"name":                adminTextCell(name),
				"slug":                adminTextCell(slug),
				"icon":                adminTextCell(icon),
				"published_templates": adminIntCell(published),
				"total_templates":     adminIntCell(total),
			}}, nil
		},
		AllowAdd: true, AllowChange: true, AllowDelete: true,
	}
	m.Get = func(a *App, id string) (*adminObject, error) {
		var name, slug, icon string
		var sortOrder int
		err := a.DB.QueryRow(`SELECT name, slug, icon, sort_order FROM gallery_templatecategory WHERE id = ?`,
			id).Scan(&name, &slug, &icon, &sortOrder)
		if err != nil {
			return nil, err
		}
		return &adminObject{
			ID: id, Repr: name,
			Values:  map[string]string{"name": name, "slug": slug, "icon": icon},
			Display: map[string]template.HTML{}, Raw: map[string]any{},
		}, nil
	}
	m.Fieldsets = []adminFieldset{{Title: "", Fields: []adminField{
		{Name: "name", Label: "Name", Kind: adminText, Required: true, Wide: true},
		{Name: "slug", Label: "Slug", Kind: adminText, Required: true, Wide: true},
		{Name: "icon", Label: "Icon", Kind: adminText, Wide: true},
	}}}
	m.Add = func(a *App, actor *UserRow, form *adminForm) (string, adminErrors, error) {
		values, errs := adminCategoryValues(a, form, "")
		if errs.any() {
			return "", errs, nil
		}
		id := newUUID4Hex()
		now := formatDjangoDateTime(time.Now())
		_, err := a.DB.Exec(`INSERT INTO gallery_templatecategory
			(id, created_at, updated_at, slug, name, icon, sort_order) VALUES (?, ?, ?, ?, ?, ?, 0)`,
			id, now, now, values["slug"], values["name"], values["icon"])
		return id, nil, err
	}
	m.Change = func(a *App, actor *UserRow, id string, form *adminForm) (adminErrors, error) {
		values, errs := adminCategoryValues(a, form, id)
		if errs.any() {
			return errs, nil
		}
		_, err := a.DB.Exec(`UPDATE gallery_templatecategory SET name = ?, slug = ?, icon = ?, updated_at = ?
			WHERE id = ?`, values["name"], values["slug"], values["icon"], formatDjangoDateTime(time.Now()), id)
		return nil, err
	}
	m.Delete = func(a *App, actor *UserRow, id string) error {
		_, _ = a.DB.Exec(`UPDATE gallery_template SET category_id = NULL WHERE category_id = ?`, id)
		_, err := a.DB.Exec(`DELETE FROM gallery_templatecategory WHERE id = ?`, id)
		return err
	}
	m.Repr = func(a *App, id string) string {
		var name string
		_ = a.DB.QueryRow(`SELECT name FROM gallery_templatecategory WHERE id = ?`, id).Scan(&name)
		return name
	}
	return m
}

func adminCategoryValues(a *App, form *adminForm, id string) (map[string]string, adminErrors) {
	errs := adminErrors{}
	values := map[string]string{
		"name": strings.TrimSpace(form.Value("name")),
		"slug": strings.TrimSpace(form.Value("slug")),
		"icon": strings.TrimSpace(form.Value("icon")),
	}
	if values["name"] == "" {
		errs.add("name", "This field is required.")
	}
	if values["slug"] == "" {
		errs.add("slug", "This field is required.")
	} else {
		var taken int
		_ = a.DB.QueryRow(`SELECT COUNT(*) FROM gallery_templatecategory WHERE slug = ? AND id <> ?`,
			values["slug"], id).Scan(&taken)
		if taken > 0 {
			errs.add("slug", "Template category with this Slug already exists.")
		}
	}
	return values, errs
}

// ---- Template ----

func adminTemplateModel() *adminModel {
	m := &adminModel{
		AppLabel: "gallery", Model: "template", Verbose: "template", Plural: "Templates", Name: "template",
		Table: "gallery_template",
		Columns: []adminColumn{
			{Name: "preview", Label: "Preview"},
			{Name: "title", Label: "Title", Sort: "t.title", Link: true},
			{Name: "category", Label: "Category", Sort: "c.name"},
			{Name: "size", Label: "Size (px)", Sort: "t.width"},
			{Name: "page_count", Label: "Page count", Sort: "t.page_count"},
			{Name: "is_featured", Label: "Is featured", Sort: "t.is_featured"},
			{Name: "is_published", Label: "Is published", Sort: "t.is_published"},
			{Name: "use_count", Label: "Use count", Sort: "t.use_count"},
			{Name: "created_at", Label: "Created at", Sort: "t.created_at"},
		},
		LinkName:     "title",
		SearchFields: []adminSearchField{{SQL: "t.title"}, {SQL: "t.slug"}, {SQL: "t.description"}, {SQL: "t.tags"}},
		ListEditable: []string{"is_featured", "is_published"},
		PerPage:      40,
		Ordering:     []string{"t.created_at DESC"},
		Filters: []adminFilter{
			{Label: "Is published", Param: "is_published__exact", Kind: "bool", Column: "t.is_published"},
			{Label: "Is featured", Param: "is_featured__exact", Kind: "bool", Column: "t.is_featured"},
			{Label: "Category", Param: "category__exact", Kind: "fk", Column: "t.category_id",
				OptionsFn: func(a *App) []adminFilterChoice {
					rows, err := a.DB.Query(`SELECT id, name FROM gallery_templatecategory ORDER BY sort_order, name`)
					if err != nil {
						return nil
					}
					defer rows.Close()
					var out []adminFilterChoice
					for rows.Next() {
						var id, name string
						if err := rows.Scan(&id, &name); err == nil {
							out = append(out, adminOption(id, name))
						}
					}
					return out
				}},
			{Label: "Preset", Param: "preset__exact", Kind: "choice", Column: "t.preset",
				OptionsFn: func(a *App) []adminFilterChoice { return adminDistinctChoices(a, "gallery_template", "preset") }},
		},
		SelectSQL: "t.id, t.thumbnail, t.title, COALESCE(c.name, ''), t.width, t.height, t.page_count, " +
			"t.is_featured, t.is_published, t.use_count, t.created_at",
		FromSQL: "gallery_template t LEFT JOIN gallery_templatecategory c ON c.id = t.category_id",
		ScanList: func(a *App, rows *sql.Rows) (adminListRow, error) {
			var id, thumbnail, title, category, createdAt string
			var width, height, pageCount, featured, published, useCount int
			if err := rows.Scan(&id, &thumbnail, &title, &category, &width, &height, &pageCount,
				&featured, &published, &useCount, &createdAt); err != nil {
				return adminListRow{}, err
			}
			size := "—"
			if width > 0 {
				size = fmt.Sprintf("%d x %d · %s", width, height, orientationOf(int64(width), int64(height)))
			}
			return adminListRow{ID: id, Cells: map[string]adminCell{
				"preview":      adminHTMLCell(adminThumbCellRaw(thumbnail)),
				"title":        adminTextCell(title),
				"category":     adminTextCell(category),
				"size":         adminTextCell(size),
				"page_count":   adminIntCell(pageCount),
				"is_featured":  adminTextCell(adminBoolText(featured)),
				"is_published": adminTextCell(adminBoolText(published)),
				"use_count":    adminIntCell(useCount),
				"created_at":   adminTextCell(createdAt),
			}}, nil
		},
		AllowAdd: true, AllowChange: true, AllowDelete: true,
		AutocompleteFields: map[string]string{"category": "gallery.templatecategory"},
		Choices: map[string]func(a *App) []adminFilterChoice{
			"category": func(a *App) []adminFilterChoice {
				rows, err := a.DB.Query(`SELECT id, name FROM gallery_templatecategory ORDER BY sort_order, name`)
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
			},
		},
		Actions: []adminAction{
			{Name: "publish", Label: "Publish selected templates",
				Run: func(a *App, actor *UserRow, ids []string) ([]adminMessage, error) {
					n, err := a.adminUpdateSelected("gallery_template", "is_published", "TRUE", ids, "")
					return []adminMessage{adminSuccess(fmt.Sprintf("%d template(s) published.", n))}, err
				}},
			{Name: "unpublish", Label: "Unpublish selected templates",
				Run: func(a *App, actor *UserRow, ids []string) ([]adminMessage, error) {
					n, err := a.adminUpdateSelected("gallery_template", "is_published", "FALSE", ids, "")
					return []adminMessage{adminSuccess(fmt.Sprintf("%d template(s) unpublished.", n))}, err
				}},
			{Name: "feature", Label: "Mark selected templates as featured",
				Run: func(a *App, actor *UserRow, ids []string) ([]adminMessage, error) {
					n, err := a.adminUpdateSelected("gallery_template", "is_featured", "TRUE", ids, "")
					return []adminMessage{adminSuccess(fmt.Sprintf("%d template(s) featured.", n))}, err
				}},
			{Name: "unfeature", Label: "Remove featured mark from selected templates",
				Run: func(a *App, actor *UserRow, ids []string) ([]adminMessage, error) {
					n, err := a.adminUpdateSelected("gallery_template", "is_featured", "FALSE", ids, "")
					return []adminMessage{adminSuccess(fmt.Sprintf("%d template(s) unfeatured.", n))}, err
				}},
			{Name: "duplicate", Label: "Duplicate selected templates (as drafts)",
				Run: func(a *App, actor *UserRow, ids []string) ([]adminMessage, error) {
					copies := 0
					for _, id := range ids {
						if _, err := adminDuplicateTemplate(a, id, actor); err != nil {
							return nil, err
						}
						copies++
					}
					return []adminMessage{adminSuccess(fmt.Sprintf("%d draft copy/copies created.", copies))}, nil
				}},
			{Name: "validate_documents", Label: "Validate the documents of selected templates",
				Run: func(a *App, actor *UserRow, ids []string) ([]adminMessage, error) {
					var msgs []adminMessage
					for _, id := range ids {
						msgs = append(msgs, adminValidateTemplate(a, id)...)
					}
					return msgs, nil
				}},
		},
		ExtraRoutes: []adminExtraRoute{
			{Suffix: "validate", Methods: []string{"GET", "HEAD"}, Perm: "change",
				Run: func(a *App, w http.ResponseWriter, r *http.Request, actor *UserRow, id string) {
					msgs := adminValidateTemplate(a, id)
					a.flashAdminMessages(w, r, msgs...)
					adminRedirect(w, r, "/admin/gallery/template/"+adminURLID(mustTemplateModel(a), id)+"/change/")
				}},
			{Suffix: "duplicate", Methods: []string{"GET", "HEAD"}, Perm: "add",
				Run: func(a *App, w http.ResponseWriter, r *http.Request, actor *UserRow, id string) {
					copyID, err := adminDuplicateTemplate(a, id, actor)
					if err != nil {
						a.admin().render404(w, r, actor)
						return
					}
					a.flashAdminMessages(w, r, adminSuccess("Draft copy created."))
					adminRedirect(w, r, "/admin/gallery/template/"+adminURLID(mustTemplateModel(a), copyID)+"/change/")
				}},
		},
	}
	m.Get = func(a *App, id string) (*adminObject, error) {
		var title, slug, description, tags, preset, document, thumbnail, createdBy, createdAt, updatedAt string
		var category sql.NullString
		var width, height, pageCount, featured, published, useCount int
		err := a.DB.QueryRow(`SELECT t.title, t.slug, t.description, t.tags, t.preset, t.document,
			t.width, t.height, t.page_count, t.thumbnail, t.is_featured, t.is_published, t.use_count,
			t.created_at, t.updated_at, COALESCE(u.email, ''), t.category_id
			FROM gallery_template t LEFT JOIN accounts_user u ON u.id = t.created_by_id WHERE t.id = ?`, id).Scan(
			&title, &slug, &description, &tags, &preset, &document, &width, &height, &pageCount,
			&thumbnail, &featured, &published, &useCount, &createdAt, &updatedAt, &createdBy, &category)
		if err != nil {
			return nil, err
		}
		categoryID := ""
		if category.Valid {
			categoryID = category.String
		}
		size := "—"
		if width > 0 {
			size = fmt.Sprintf("%d x %d · %s", width, height, orientationOf(int64(width), int64(height)))
		}
		preview := "—"
		if thumbnail != "" {
			preview = fmt.Sprintf(`<img src="%s" alt="" style="max-width:20rem;max-height:20rem;border-radius:0.5rem">`,
				djangoEscape(adminMediaURL(thumbnail)))
		}
		return &adminObject{
			ID: id, Repr: title,
			Values: map[string]string{
				"title": title, "slug": slug, "description": description,
				"category": categoryID, "tags": strings.Join(adminJSONList(tags), ", "),
				"preset": preset, "thumbnail": thumbnail, "is_featured": boolFlag(featured),
				"is_published": boolFlag(published), "created_by": createdBy,
				"created_at": createdAt, "updated_at": updatedAt,
			},
			Display: map[string]template.HTML{
				"preview_large": template.HTML(preview),
				"size":          template.HTML(djangoEscape(size)),
				"page_count":    template.HTML(djangoEscape(fmt.Sprint(pageCount))),
				"use_count":     template.HTML(djangoEscape(fmt.Sprint(useCount))),
				"document_json": adminTemplateDocumentPreview(document),
			},
			Raw: map[string]any{},
		}, nil
	}
	m.New = func(a *App) *adminObject {
		return &adminObject{Values: map[string]string{}, Display: map[string]template.HTML{
			"preview_large": template.HTML("—"), "size": template.HTML("—"), "page_count": template.HTML("—"),
			"use_count": template.HTML("0"), "document_json": template.HTML("—"),
		}, Raw: map[string]any{}}
	}
	m.Fieldsets = []adminFieldset{
		{Title: "", Fields: []adminField{
			{Name: "title", Label: "Title", Kind: adminText, Required: true, Wide: true},
			{Name: "slug", Label: "Slug", Kind: adminText, Required: true, Wide: true},
			{Name: "category", Label: "Category", Kind: adminSelect, Wide: true},
			{Name: "description", Label: "Description", Kind: adminTextarea, Wide: true},
			{Name: "tags", Label: "Tags", Kind: adminText, Wide: true, Help: "Comma-separated, at most 20."},
			{Name: "preset", Label: "Preset", Kind: adminText, Wide: true},
		}},
		{Title: "Thumbnail", Fields: []adminField{
			{Name: "thumbnail", Label: "Thumbnail", Kind: adminFile, Wide: true},
			{Name: "preview_large", Label: "Current thumbnail", Kind: adminReadonly, Wide: true},
		}},
		{Title: "Document", Fields: []adminField{
			{Name: "document_file", Label: "Document (JSON file)", Kind: adminFile, Wide: true,
				Help: "A Doory design document (JSON). Required when creating a template."},
			{Name: "size", Label: "Size (px)", Kind: adminReadonly},
			{Name: "page_count", Label: "Page count", Kind: adminReadonly},
			{Name: "document_json", Label: "Document (read-only)", Kind: adminReadonly, Wide: true},
		}},
		{Title: "Visibility", Fields: []adminField{
			{Name: "is_published", Label: "Is published", Kind: adminBool},
			{Name: "is_featured", Label: "Is featured", Kind: adminBool},
			{Name: "use_count", Label: "Use count", Kind: adminReadonly},
		}},
		{Title: "Audit", Fields: []adminField{
			{Name: "created_by", Label: "Created by", Kind: adminReadonly},
			{Name: "created_at", Label: "Created at", Kind: adminReadonly},
			{Name: "updated_at", Label: "Updated at", Kind: adminReadonly},
		}},
	}
	m.Add = func(a *App, actor *UserRow, form *adminForm) (string, adminErrors, error) {
		values, errs := adminTemplateValues(a, form, "", true)
		thumbnail, thumbErr := adminTemplateThumbnail(form)
		if thumbErr != "" {
			errs.add("thumbnail", thumbErr)
		}
		if errs.any() {
			return "", errs, nil
		}
		id := newUUID4Hex()
		now := formatDjangoDateTime(time.Now())
		if _, err := a.DB.Exec(`INSERT INTO gallery_template
			(id, created_at, updated_at, slug, title, description, tags, width, height, preset, document,
			 page_count, thumbnail, is_featured, is_published, use_count, created_by_id, category_id)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, ?, ?)`,
			id, now, now, values["slug"], values["title"], values["description"], values["tags"],
			values["width"], values["height"], values["preset"], values["document"], values["page_count"],
			"", boolSQL(form.Has("is_featured")), boolSQL(form.Has("is_published")),
			actor.ID, nullIfEmpty(values["category"])); err != nil {
			return "", nil, err
		}
		if err := a.adminStoreTemplateThumbnail(id, values["slug"], thumbnail, ""); err != nil {
			return "", nil, err
		}
		return id, nil, nil
	}
	m.Change = func(a *App, actor *UserRow, id string, form *adminForm) (adminErrors, error) {
		values, errs := adminTemplateValues(a, form, id, false)
		thumbnail, thumbErr := adminTemplateThumbnail(form)
		if thumbErr != "" {
			errs.add("thumbnail", thumbErr)
		}
		if errs.any() {
			return errs, nil
		}
		var previousThumbnail string
		_ = a.DB.QueryRow(`SELECT thumbnail FROM gallery_template WHERE id = ?`, id).Scan(&previousThumbnail)
		if _, err := a.DB.Exec(`UPDATE gallery_template SET slug = ?, title = ?, description = ?, tags = ?,
			width = ?, height = ?, preset = ?, document = ?, page_count = ?, is_featured = ?,
			is_published = ?, category_id = ?, updated_at = ? WHERE id = ?`,
			values["slug"], values["title"], values["description"], values["tags"], values["width"],
			values["height"], values["preset"], values["document"], values["page_count"],
			boolSQL(form.Has("is_featured")), boolSQL(form.Has("is_published")),
			nullIfEmpty(values["category"]), formatDjangoDateTime(time.Now()), id); err != nil {
			return nil, err
		}
		return nil, a.adminStoreTemplateThumbnail(id, values["slug"], thumbnail, previousThumbnail)
	}
	m.Delete = func(a *App, actor *UserRow, id string) error {
		var thumbnail string
		_ = a.DB.QueryRow(`SELECT thumbnail FROM gallery_template WHERE id = ?`, id).Scan(&thumbnail)
		if _, err := a.DB.Exec(`DELETE FROM gallery_template WHERE id = ?`, id); err != nil {
			return err
		}
		a.deleteMediaFile(thumbnail)
		return nil
	}
	m.Repr = func(a *App, id string) string {
		var title string
		_ = a.DB.QueryRow(`SELECT title FROM gallery_template WHERE id = ?`, id).Scan(&title)
		return title
	}
	return m
}

func mustTemplateModel(a *App) *adminModel { return a.admin().byKey["gallery.template"] }

func adminThumbCellRaw(path string) template.HTML {
	if path == "" {
		return ""
	}
	return template.HTML(fmt.Sprintf(`<img src="%s" alt="" style="max-height:48px;max-width:96px;border-radius:4px">`,
		djangoEscape(adminMediaURL(path))))
}

func adminTemplateDocumentPreview(raw string) template.HTML {
	var document map[string]any
	if err := json.Unmarshal([]byte(raw), &document); err != nil {
		return template.HTML(djangoEscape(raw))
	}
	encoded, _ := json.Marshal(document)
	return adminPrettyJSON(string(encoded))
}

// adminTemplateValues validates the metadata/document; with `isAdd` the
// document file is required.
func adminTemplateValues(a *App, form *adminForm, id string, isAdd bool) (map[string]string, adminErrors) {
	errs := adminErrors{}
	values := map[string]string{
		"title":       strings.TrimSpace(form.Value("title")),
		"slug":        strings.TrimSpace(form.Value("slug")),
		"description": form.Value("description"),
		"category":    strings.TrimSpace(form.Value("category")),
		"preset":      strings.TrimSpace(form.Value("preset")),
	}
	if values["title"] == "" {
		errs.add("title", "This field is required.")
	}
	if values["slug"] == "" {
		errs.add("slug", "This field is required.")
	} else if !templateSlugPattern.MatchString(values["slug"]) {
		errs.add("slug", "Enter a valid “slug” consisting of lowercase letters, numbers, underscores or hyphens.")
	} else {
		var taken int
		_ = a.DB.QueryRow(`SELECT COUNT(*) FROM gallery_template WHERE slug = ? AND id <> ?`,
			values["slug"], id).Scan(&taken)
		if taken > 0 {
			errs.add("slug", "Template with this Slug already exists.")
		}
	}
	tags, tagErr := normalizeTemplateTags(form.Value("tags"))
	if tagErr != nil {
		errs.add("tags", tagErr.Error())
	}
	values["tags"] = adminJSONText(tags)
	if values["category"] != "" {
		var exists int
		_ = a.DB.QueryRow(`SELECT COUNT(*) FROM gallery_templatecategory WHERE id = ?`,
			hexUUID(values["category"])).Scan(&exists)
		if exists == 0 {
			errs.add("category", "Select a valid choice. That choice is not one of the available choices.")
		}
		values["category"] = hexUUID(values["category"])
	}

	upload := form.File("document_file")
	if upload == nil {
		if isAdd {
			errs.add("document_file", "Upload the template's JSON document.")
		}
		if !isAdd && id != "" {
			var width, height, pageCount int
			var document, preset string
			if err := a.DB.QueryRow(`SELECT width, height, page_count, document, preset
				FROM gallery_template WHERE id = ?`, id).Scan(&width, &height, &pageCount, &document, &preset); err == nil {
				values["width"], values["height"] = fmt.Sprint(width), fmt.Sprint(height)
				values["page_count"] = fmt.Sprint(pageCount)
				values["document"] = document
				if values["preset"] == "" {
					values["preset"] = preset
				}
			}
		}
		return values, errs
	}
	if len(upload.Data) > 16*1024*1024 {
		errs.add("document_file", "The file is larger than 16 MB.")
		return values, errs
	}
	var document map[string]any
	if err := json.Unmarshal(upload.Data, &document); err != nil {
		errs.add("document_file", "The file is not valid UTF-8 JSON.")
		return values, errs
	}
	summary, err := validateTemplateDocument(document)
	if err != nil {
		errs.add("document_file", err.Error())
		return values, errs
	}
	encoded, err := json.Marshal(document)
	if err != nil {
		errs.add("document_file", "The file is not valid UTF-8 JSON.")
		return values, errs
	}
	values["document"] = string(encoded)
	values["width"], values["height"] = fmt.Sprint(summary.Width), fmt.Sprint(summary.Height)
	values["page_count"] = fmt.Sprint(summary.PageCount)
	if values["preset"] == "" {
		values["preset"] = summary.Preset
	}
	return values, errs
}

// adminTemplateThumbnail ports TemplateAdminForm.clean_thumbnail: decode the
// upload and re-encode it as the bounded WebP the model stores. Returns nil
// data when no file was uploaded.
func adminTemplateThumbnail(form *adminForm) ([]byte, string) {
	upload := form.File("thumbnail")
	if upload == nil {
		return nil, ""
	}
	if len(upload.Data) == 0 {
		return nil, "The file is empty."
	}
	if len(upload.Data) > templateThumbMaxBytes {
		return nil, "The file is too large."
	}
	img, _, err := decodeImageData(upload.Data)
	if err != nil {
		return nil, "Upload a valid image. The file you uploaded was either not an image or a corrupted image."
	}
	data, err := encodeWebP(fitThumbnail(img, templateThumbMaxSide))
	if err != nil {
		return nil, "The image could not be processed."
	}
	return data, ""
}

// adminStoreTemplateThumbnail stores a prepared thumbnail (see
// adminTemplateThumbnail) under templates/<slug>.webp.
func (a *App) adminStoreTemplateThumbnail(templateID, slug string, data []byte, previous string) error {
	if data == nil {
		return nil
	}
	name := availableThumbnailName(a.Settings.MediaRoot, templateThumbnailName(slug))
	if err := adminWriteMedia(a.Settings.MediaRoot, name, data); err != nil {
		return err
	}
	if _, err := a.DB.Exec(`UPDATE gallery_template SET thumbnail = ? WHERE id = ?`, name, templateID); err != nil {
		return err
	}
	if previous != "" && previous != name {
		a.deleteMediaFile(previous)
	}
	return nil
}

// adminValidateTemplate mirrors the _validate helper.
func adminValidateTemplate(a *App, id string) []adminMessage {
	var title, document string
	var width, height, pageCount int
	if err := a.DB.QueryRow(`SELECT title, document, width, height, page_count FROM gallery_template WHERE id = ?`,
		id).Scan(&title, &document, &width, &height, &pageCount); err != nil {
		return []adminMessage{adminError("Template not found.")}
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(document), &parsed); err != nil {
		return []adminMessage{adminError(title + ": The stored document is not valid JSON.")}
	}
	summary, err := validateTemplateDocument(parsed)
	if err != nil {
		return []adminMessage{adminError(title + ": " + err.Error())}
	}
	if summary.Width != width || summary.Height != height || summary.PageCount != pageCount {
		return []adminMessage{adminWarning(title + ": size or page count differs from the document.")}
	}
	return []adminMessage{adminSuccess(fmt.Sprintf("%s: valid (%d page(s), %d x %d).",
		title, summary.PageCount, summary.Width, summary.Height))}
}

// adminDuplicateTemplate ports services.duplicate_template.
func adminDuplicateTemplate(a *App, id string, actor *UserRow) (string, error) {
	var title, description, tags, preset, document, thumbnail string
	var category sql.NullString
	var width, height, pageCount int
	if err := a.DB.QueryRow(`SELECT title, description, tags, width, height, preset, document, page_count,
		thumbnail, category_id FROM gallery_template WHERE id = ?`, id).Scan(
		&title, &description, &tags, &width, &height, &preset, &document, &pageCount, &thumbnail,
		&category); err != nil {
		return "", err
	}
	copyTitle := "Copy of " + title
	if len([]rune(copyTitle)) > 200 {
		copyTitle = string([]rune(copyTitle)[:200])
	}
	slug := uniqueTemplateSlug(a, copyTitle, "")
	newID := newUUID4Hex()
	now := formatDjangoDateTime(time.Now())
	if _, err := a.DB.Exec(`INSERT INTO gallery_template
		(id, created_at, updated_at, slug, title, description, tags, width, height, preset, document,
		 page_count, thumbnail, is_featured, is_published, use_count, created_by_id, category_id)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, '', FALSE, FALSE, 0, ?, ?)`,
		newID, now, now, slug, copyTitle, description, tags, width, height, preset, document, pageCount,
		actor.ID, nullableString(category)); err != nil {
		return "", err
	}
	if thumbnail != "" {
		data, err := adminReadMedia(a.Settings.MediaRoot, thumbnail)
		if err == nil {
			name := availableThumbnailName(a.Settings.MediaRoot, templateThumbnailName(slug))
			if err := adminWriteMedia(a.Settings.MediaRoot, name, data); err == nil {
				_, _ = a.DB.Exec(`UPDATE gallery_template SET thumbnail = ? WHERE id = ?`, name, newID)
			}
		}
	}
	return newID, nil
}

func uniqueTemplateSlug(a *App, title, exclude string) string {
	base := slugifyTemplate(title)
	if len(base) > 114 {
		base = strings.Trim(base[:114], "-")
	}
	if base == "" {
		base = "template"
	}
	candidate := base
	counter := 1
	for {
		var taken int
		_ = a.DB.QueryRow(`SELECT COUNT(*) FROM gallery_template WHERE slug = ? AND id <> ?`,
			candidate, exclude).Scan(&taken)
		if taken == 0 {
			return candidate
		}
		counter++
		candidate = fmt.Sprintf("%s-%d", base, counter)
	}
}

func nullableString(v sql.NullString) any {
	if v.Valid {
		return v.String
	}
	return nil
}
