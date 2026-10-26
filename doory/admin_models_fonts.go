// fonts.FontFamily / FontVariant admin pages: the family form with a
// tabular variant formset, the sample preview (one @font-face per family)
// and the publish actions.
package doory

import (
	"database/sql"
	"fmt"
	"html/template"
	"regexp"
	"sort"
	"strings"
	"time"

	"doory-go/internal/fontparse"
)

const (
	adminFontPreviewText  = "The quick brown fox jumps over the lazy dog"
	adminFontMaxFileBytes = 10 * 1024 * 1024
)

var adminSafeFontURL = regexp.MustCompile(`^[A-Za-z0-9/_.:%?=&~+-]+$`)

var adminFontCategories = []adminFilterChoice{
	adminOption("sans-serif", "Sans serif"), adminOption("serif", "Serif"),
	adminOption("display", "Display"), adminOption("handwriting", "Handwriting"),
	adminOption("monospace", "Monospace"),
}

var adminFontSources = []adminFilterChoice{
	adminOption("fontsource", "Fontsource"), adminOption("upload", "User upload"),
}

var adminFontFormats = map[string]string{
	"woff2": "WOFF2", "woff": "WOFF", "ttf": "TrueType", "otf": "OpenType",
}

var adminFontCSSFormats = map[string]string{
	"woff2": "woff2", "woff": "woff", "ttf": "truetype", "otf": "opentype",
}

func adminModelsFonts() []*adminModel { return []*adminModel{adminFontFamilyModel()} }

type adminFontVariant struct {
	id          string
	weight      int
	style       string
	subset      string
	format      string
	size        int
	file        string
	unicodeText string
}

func (a *App) adminFontVariants(familyID string) []adminFontVariant {
	rows, err := a.DB.Query(`SELECT id, weight, style, subset, format, size, file, unicode_range
		FROM fonts_fontvariant WHERE family_id = ? ORDER BY weight, style, subset`, familyID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []adminFontVariant
	for rows.Next() {
		var v adminFontVariant
		if err := rows.Scan(&v.id, &v.weight, &v.style, &v.subset, &v.format, &v.size, &v.file,
			&v.unicodeText); err == nil {
			out = append(out, v)
		}
	}
	return out
}

func adminFontFamilyModel() *adminModel {
	m := &adminModel{
		AppLabel: "fonts", Model: "fontfamily", Verbose: "font family", Plural: "Font families",
		Name: "font family", Table: "fonts_fontfamily",
		Columns: []adminColumn{
			{Name: "family", Label: "Family", Sort: "f.family", Link: true},
			{Name: "sample", Label: "Preview"},
			{Name: "category", Label: "Category", Sort: "f.category"},
			{Name: "scripts_list", Label: "Scripts"},
			{Name: "variant_count", Label: "Files"},
			{Name: "source", Label: "Source", Sort: "f.source"},
			{Name: "owner", Label: "Owner", Sort: "u.email"},
			{Name: "popularity", Label: "Popularity", Sort: "f.popularity"},
			{Name: "is_published", Label: "Is published", Sort: "f.is_published"},
		},
		LinkName:     "family",
		SearchFields: []adminSearchField{{SQL: "f.family"}, {SQL: "f.slug"}, {SQL: "u.email"}},
		ListEditable: []string{"is_published"},
		PerPage:      40,
		Filters: []adminFilter{
			{Label: "Source", Param: "source__exact", Kind: "choice", Column: "f.source", Options: adminFontSources},
			{Label: "Category", Param: "category__exact", Kind: "choice", Column: "f.category",
				Options: adminFontCategories},
			{Label: "Is published", Param: "is_published__exact", Kind: "bool", Column: "f.is_published"},
		},
		Ordering: []string{"f.popularity ASC", "f.family ASC"},
		SelectSQL: "f.id, f.family, f.category, f.scripts, " +
			"(SELECT COUNT(*) FROM fonts_fontvariant v WHERE v.family_id = f.id), f.source, " +
			"COALESCE(u.email, ''), f.popularity, f.is_published, f.slug, " +
			"fv.file, fv.weight, fv.style, fv.subset, fv.format",
		// The preview variant: normal style first, then the weight closest to
		// 400, then latin/empty subsets (adminFontSample's score), and file
		// order as the tie-breaker. Joined here so the changelist runs one
		// query (the upstream query-count contract) instead of one per row.
		FromSQL: "fonts_fontfamily f LEFT JOIN accounts_user u ON u.id = f.owner_id " +
			"LEFT JOIN fonts_fontvariant fv ON fv.id = (SELECT v.id FROM fonts_fontvariant v " +
			"WHERE v.family_id = f.id ORDER BY (v.style <> 'normal'), ABS(v.weight - 400), " +
			"(v.subset <> '' AND v.subset <> 'latin'), v.weight, v.style, v.subset LIMIT 1)",
		ScanList: func(a *App, rows *sql.Rows) (adminListRow, error) {
			var id, family, category, scripts, source, owner, slug string
			var variants, popularity, published int
			var file, variantStyle, subset, format sql.NullString
			var weight sql.NullInt64
			if err := rows.Scan(&id, &family, &category, &scripts, &variants, &source, &owner,
				&popularity, &published, &slug, &file, &weight, &variantStyle, &subset, &format); err != nil {
				return adminListRow{}, err
			}
			return adminListRow{ID: id, Cells: map[string]adminCell{
				"family": adminTextCell(family),
				"sample": adminHTMLCell(adminFontSample(id, file.String, int(weight.Int64),
					variantStyle.String, subset.String, format.String)),
				"category":      adminTextCell(category),
				"scripts_list":  adminTextCell(strings.Join(adminJSONList(scripts), ", ")),
				"variant_count": adminIntCell(variants),
				"source":        adminTextCell(source),
				"owner":         adminTextCell(owner),
				"popularity":    adminIntCell(popularity),
				"is_published":  adminTextCell(adminBoolText(published)),
			}}, nil
		},
		AllowAdd: true, AllowChange: true, AllowDelete: true,
		Choices: map[string]func(a *App) []adminFilterChoice{
			"category": func(a *App) []adminFilterChoice { return adminFontCategories },
			"source":   func(a *App) []adminFilterChoice { return adminFontSources },
		},
		Actions: []adminAction{
			{Name: "publish", Label: "Publish selected families",
				Run: func(a *App, actor *UserRow, ids []string) ([]adminMessage, error) {
					n, err := a.adminUpdateSelected("fonts_fontfamily", "is_published", "TRUE", ids, "")
					return []adminMessage{adminSuccess(fmt.Sprintf("%d family/families published.", n))}, err
				}},
			{Name: "unpublish", Label: "Unpublish selected families",
				Run: func(a *App, actor *UserRow, ids []string) ([]adminMessage, error) {
					n, err := a.adminUpdateSelected("fonts_fontfamily", "is_published", "FALSE", ids, "")
					return []adminMessage{adminSuccess(fmt.Sprintf("%d family/families unpublished.", n))}, err
				}},
		},
	}
	m.Get = func(a *App, id string) (*adminObject, error) {
		var family, slug, category, scripts, license, source, owner, createdAt, updatedAt string
		var isVariable, popularity, published int
		err := a.DB.QueryRow(`SELECT f.family, f.slug, f.category, f.scripts, f.is_variable, f.popularity,
			f.license, f.source, COALESCE(u.email, ''), f.is_published, f.created_at, f.updated_at
			FROM fonts_fontfamily f LEFT JOIN accounts_user u ON u.id = f.owner_id WHERE f.id = ?`,
			id).Scan(&family, &slug, &category, &scripts, &isVariable, &popularity, &license, &source,
			&owner, &published, &createdAt, &updatedAt)
		if err != nil {
			return nil, err
		}
		var inline adminInline
		inline = a.adminFontVariantInline(id, 1)
		return &adminObject{
			ID: id, Repr: family,
			Values: map[string]string{
				"family": family, "slug": slug, "category": category, "scripts": scripts,
				"is_variable": boolFlag(isVariable), "popularity": fmt.Sprint(popularity),
				"license": license, "source": source, "is_published": boolFlag(published),
				"created_at": createdAt, "updated_at": updatedAt,
			},
			Display: map[string]template.HTML{"owner": template.HTML(djangoEscape(ownerOrDash(owner)))},
			Raw:     map[string]any{},
			Inlines: []adminInline{inline},
		}, nil
	}
	m.New = func(a *App) *adminObject {
		return &adminObject{Values: map[string]string{
			"scripts": "[]", "popularity": "9999", "source": "fontsource", "license": "OFL-1.1",
			"category": "sans-serif", "is_published": "1",
		}, Display: map[string]template.HTML{"owner": "—"}, Raw: map[string]any{},
			Inlines: []adminInline{a.adminFontVariantInline("", 1)}}
	}
	m.Fieldsets = []adminFieldset{
		{Title: "", Fields: []adminField{
			{Name: "family", Label: "Family", Kind: adminText, Required: true, Wide: true},
			{Name: "slug", Label: "Slug", Kind: adminText, Required: true, Wide: true},
			{Name: "category", Label: "Category", Kind: adminSelect},
			{Name: "is_published", Label: "Is published", Kind: adminBool},
		}},
		{Title: "Coverage", Fields: []adminField{
			{Name: "scripts", Label: "Scripts", Kind: adminText, Wide: true, Help: "JSON list of subset names."},
			{Name: "is_variable", Label: "Is variable", Kind: adminBool},
			{Name: "popularity", Label: "Popularity", Kind: adminNumber},
		}},
		{Title: "Origin", Fields: []adminField{
			{Name: "source", Label: "Source", Kind: adminSelect},
			{Name: "license", Label: "License", Kind: adminText},
			{Name: "owner", Label: "Owner", Kind: adminReadonly},
		}},
		{Title: "Audit", Fields: []adminField{
			{Name: "created_at", Label: "Created at", Kind: adminReadonly},
			{Name: "updated_at", Label: "Updated at", Kind: adminReadonly},
		}, Collapse: true},
	}
	m.Add = func(a *App, actor *UserRow, form *adminForm) (string, adminErrors, error) {
		values, variants, errs := adminFontFamilyValues(a, form, "")
		if errs.any() {
			return "", errs, nil
		}
		id := newUUID4Hex()
		now := formatDjangoDateTime(time.Now())
		if _, err := a.DB.Exec(`INSERT INTO fonts_fontfamily
			(id, created_at, updated_at, family, slug, category, scripts, is_variable, popularity,
			 license, source, is_published, owner_id)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL)`,
			id, now, now, values["family"], values["slug"], values["category"], values["scripts"],
			boolSQL(form.Has("is_variable")), values["popularity"], values["license"], values["source"],
			boolSQL(form.Has("is_published"))); err != nil {
			return "", nil, err
		}
		if err := a.adminStoreFontVariants(id, values["slug"], variants); err != nil {
			return "", nil, err
		}
		return id, nil, nil
	}
	m.Change = func(a *App, actor *UserRow, id string, form *adminForm) (adminErrors, error) {
		values, variants, errs := adminFontFamilyValues(a, form, id)
		if errs.any() {
			return errs, nil
		}
		if _, err := a.DB.Exec(`UPDATE fonts_fontfamily SET family = ?, slug = ?, category = ?, scripts = ?,
			is_variable = ?, popularity = ?, license = ?, source = ?, is_published = ?, updated_at = ?
			WHERE id = ?`, values["family"], values["slug"], values["category"], values["scripts"],
			boolSQL(form.Has("is_variable")), values["popularity"], values["license"], values["source"],
			boolSQL(form.Has("is_published")), formatDjangoDateTime(time.Now()), id); err != nil {
			return nil, err
		}
		return nil, a.adminApplyFontVariants(id, values["slug"], form, variants)
	}
	m.Delete = func(a *App, actor *UserRow, id string) error {
		rows, _ := a.DB.Query(`SELECT file FROM fonts_fontvariant WHERE family_id = ?`, id)
		var files []string
		if rows != nil {
			for rows.Next() {
				var file string
				if err := rows.Scan(&file); err == nil && file != "" {
					files = append(files, file)
				}
			}
			_ = rows.Close()
		}
		_, _ = a.DB.Exec(`DELETE FROM fonts_fontvariant WHERE family_id = ?`, id)
		if _, err := a.DB.Exec(`DELETE FROM fonts_fontfamily WHERE id = ?`, id); err != nil {
			return err
		}
		for _, file := range files {
			a.deleteMediaFile(file)
		}
		return nil
	}
	m.Repr = func(a *App, id string) string {
		var family string
		_ = a.DB.QueryRow(`SELECT family FROM fonts_fontfamily WHERE id = ?`, id).Scan(&family)
		return family
	}
	return m
}

func ownerOrDash(owner string) string {
	if owner == "" {
		return "—"
	}
	return owner
}

func (a *App) adminFontVariantInline(familyID string, extra int) adminInline {
	inline := adminInline{
		Prefix: "variants", Title: "Variants", Extra: extra,
		Columns: []adminInlineColumn{
			{Name: "weight", Label: "Weight", Kind: adminNumber},
			{Name: "style", Label: "Style", Kind: adminSelect, Choices: []adminFilterChoice{
				adminOption("normal", "Normal"), adminOption("italic", "Italic")}},
			{Name: "subset", Label: "Subset", Kind: adminText},
			{Name: "file", Label: "File", Kind: adminFile},
			{Name: "file_format", Label: "Format", Kind: adminReadonly},
			{Name: "file_size", Label: "Size", Kind: adminReadonly},
		},
		CanDelete: true,
	}
	if familyID == "" {
		for i := 0; i < extra; i++ {
			inline.ExtraRows = append(inline.ExtraRows, adminInlineRow{Index: i})
		}
		return inline
	}
	variants := a.adminFontVariants(familyID)
	sort.SliceStable(variants, func(i, j int) bool {
		if variants[i].weight != variants[j].weight {
			return variants[i].weight < variants[j].weight
		}
		if variants[i].style != variants[j].style {
			return variants[i].style < variants[j].style
		}
		return variants[i].subset < variants[j].subset
	})
	for i, v := range variants {
		inline.Rows = append(inline.Rows, adminInlineRow{
			Index: i, ID: v.id,
			Values: map[string]string{
				"weight": fmt.Sprint(v.weight), "style": v.style, "subset": v.subset, "file": v.file,
			},
			Display: map[string]template.HTML{
				"file_format": template.HTML(djangoEscape(adminFontFormatLabel(v.format))),
				"file_size":   template.HTML(djangoEscape(adminFontSizeLabel(v.size))),
			},
		})
	}
	for i := 0; i < extra; i++ {
		inline.ExtraRows = append(inline.ExtraRows, adminInlineRow{Index: len(variants) + i})
	}
	return inline
}

func adminFontFormatLabel(format string) string {
	if label, ok := adminFontFormats[format]; ok {
		return label
	}
	return "—"
}

func adminFontSizeLabel(size int) string {
	return fmt.Sprintf("%.1f KB", float64(size)/1024)
}

// adminFontSample renders the sample text in the preview variant's face (the
// variant is chosen by the changelist join; see FromSQL).
func adminFontSample(familyID, file string, weight int, style, subset, format string) template.HTML {
	if file == "" {
		return "—"
	}
	url := adminMediaURL(file)
	if !adminSafeFontURL.MatchString(url) {
		return "—"
	}
	face := "doory-preview-" + familyID
	cssFormat, ok := adminFontCSSFormats[format]
	if !ok {
		return "—"
	}
	return template.HTML(fmt.Sprintf(
		`<style>@font-face{font-family:"%s";font-weight:%d;font-style:%s;src:url("%s") format("%s")}</style>`+
			`<span style="font-family:'%s',sans-serif;font-weight:%d;font-style:%s;font-size:1.1rem;`+
			`white-space:nowrap">%s</span>`,
		face, weight, style, djangoEscape(url), cssFormat, face, weight, style,
		adminFontPreviewText))
}

type adminFontVariantInput struct {
	ID     string
	Weight int
	Style  string
	Subset string
	Data   []byte
	Format string
	Size   int
	Delete bool
}

func adminFontFamilyValues(a *App, form *adminForm, id string) (map[string]string, []adminFontVariantInput, adminErrors) {
	errs := adminErrors{}
	values := map[string]string{
		"family":     strings.TrimSpace(form.Value("family")),
		"slug":       strings.TrimSpace(form.Value("slug")),
		"category":   form.Value("category"),
		"scripts":    strings.TrimSpace(form.Value("scripts")),
		"popularity": strings.TrimSpace(form.Value("popularity")),
		"license":    strings.TrimSpace(form.Value("license")),
		"source":     form.Value("source"),
	}
	if values["family"] == "" {
		errs.add("family", "This field is required.")
	}
	if values["slug"] == "" {
		errs.add("slug", "This field is required.")
	} else {
		var taken int
		_ = a.DB.QueryRow(`SELECT COUNT(*) FROM fonts_fontfamily WHERE slug = ? AND owner_id IS NULL AND id <> ?`,
			values["slug"], id).Scan(&taken)
		if taken > 0 {
			errs.add("slug", "Font family with this Slug already exists.")
		}
	}
	validCategory := false
	for _, choice := range adminFontCategories {
		if choice.Value == values["category"] {
			validCategory = true
		}
	}
	if !validCategory {
		errs.add("category", "Select a valid choice. That choice is not one of the available choices.")
	}
	validSource := false
	for _, choice := range adminFontSources {
		if choice.Value == values["source"] {
			validSource = true
		}
	}
	if !validSource {
		errs.add("source", "Select a valid choice. That choice is not one of the available choices.")
	}
	if values["scripts"] == "" {
		values["scripts"] = "[]"
	}
	if values["popularity"] == "" {
		values["popularity"] = "9999"
	}
	if values["license"] == "" {
		values["license"] = "OFL-1.1"
	}

	total := form.Int("variants-TOTAL_FORMS")
	var variants []adminFontVariantInput
	for i := 0; i < total; i++ {
		prefix := fmt.Sprintf("variants-%d-", i)
		upload := form.File(prefix + "file")
		existingID := strings.TrimSpace(form.Value(prefix + "id"))
		if form.Has(prefix+"DELETE") && form.Value(prefix+"DELETE") != "" {
			variants = append(variants, adminFontVariantInput{ID: existingID, Delete: true})
			continue
		}
		weight := form.Int(prefix + "weight")
		if weight == 0 {
			weight = 400
		}
		style := form.Value(prefix + "style")
		if style == "" {
			style = "normal"
		}
		subset := strings.TrimSpace(form.Value(prefix + "subset"))
		if upload == nil {
			if existingID == "" {
				// An untouched extra row is not an error (Django's empty form).
				continue
			}
			variants = append(variants, adminFontVariantInput{ID: existingID, Weight: weight,
				Style: style, Subset: subset})
			continue
		}
		if len(upload.Data) == 0 {
			errs.add(prefix+"file", "The submitted file is empty.")
			continue
		}
		if len(upload.Data) > adminFontMaxFileBytes {
			errs.add(prefix+"file", "The file is larger than 10 MB.")
			continue
		}
		info, ferr := fontparse.Inspect(upload.Data)
		if ferr != nil {
			errs.add(prefix+"file", ferr.Error())
			continue
		}
		variants = append(variants, adminFontVariantInput{ID: existingID, Weight: weight, Style: style,
			Subset: subset, Data: upload.Data, Format: info.Format, Size: len(upload.Data)})
	}
	return values, variants, errs
}

// adminStoreFontVariants writes new variant files for a fresh family.
func (a *App) adminStoreFontVariants(familyID, slug string, variants []adminFontVariantInput) error {
	now := formatDjangoDateTime(time.Now())
	for _, v := range variants {
		if len(v.Data) == 0 {
			continue
		}
		id := newUUID4Hex()
		storage := fmt.Sprintf("fonts/system/%s/%s.%s", slug, id, v.Format)
		if err := adminWriteMedia(a.Settings.MediaRoot, storage, v.Data); err != nil {
			return err
		}
		if _, err := a.DB.Exec(`INSERT INTO fonts_fontvariant
			(id, created_at, updated_at, weight, style, subset, unicode_range, file, format, size, family_id)
			VALUES (?, ?, ?, ?, ?, ?, '', ?, ?, ?, ?)`,
			id, now, now, v.Weight, v.Style, v.Subset, storage, v.Format, v.Size, familyID); err != nil {
			return err
		}
	}
	return nil
}

func (a *App) adminApplyFontVariants(familyID, slug string, form *adminForm,
	variants []adminFontVariantInput,
) error {
	now := formatDjangoDateTime(time.Now())
	for _, v := range variants {
		if v.Delete {
			if v.ID != "" {
				var file string
				_ = a.DB.QueryRow(`SELECT file FROM fonts_fontvariant WHERE id = ? AND family_id = ?`,
					v.ID, familyID).Scan(&file)
				if _, err := a.DB.Exec(`DELETE FROM fonts_fontvariant WHERE id = ? AND family_id = ?`,
					v.ID, familyID); err != nil {
					return err
				}
				a.deleteMediaFile(file)
			}
			continue
		}
		if v.ID != "" && len(v.Data) == 0 {
			if _, err := a.DB.Exec(`UPDATE fonts_fontvariant SET weight = ?, style = ?, subset = ?,
				updated_at = ? WHERE id = ? AND family_id = ?`, v.Weight, v.Style, v.Subset, now,
				v.ID, familyID); err != nil {
				return err
			}
			continue
		}
		if len(v.Data) == 0 {
			continue
		}
		if v.ID != "" {
			var previous string
			_ = a.DB.QueryRow(`SELECT file FROM fonts_fontvariant WHERE id = ? AND family_id = ?`,
				v.ID, familyID).Scan(&previous)
			storage := fmt.Sprintf("fonts/system/%s/%s.%s", slug, v.ID, v.Format)
			if err := adminWriteMedia(a.Settings.MediaRoot, storage, v.Data); err != nil {
				return err
			}
			if _, err := a.DB.Exec(`UPDATE fonts_fontvariant SET weight = ?, style = ?, subset = ?,
				file = ?, format = ?, size = ?, updated_at = ? WHERE id = ? AND family_id = ?`,
				v.Weight, v.Style, v.Subset, storage, v.Format, v.Size, now, v.ID, familyID); err != nil {
				return err
			}
			if previous != "" && previous != storage {
				a.deleteMediaFile(previous)
			}
			continue
		}
		if err := a.adminStoreFontVariants(familyID, slug, []adminFontVariantInput{v}); err != nil {
			return err
		}
	}
	return nil
}
