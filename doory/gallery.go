// Gallery: template categories, published templates, detail, use, and the
// root sitemap (apps/gallery/{views,sitemap}.py).
//
// Ports backend/apps/gallery/views.py
// Ports backend/apps/gallery/sitemap.py
// Ports backend/apps/gallery/filters.py
// Ports backend/apps/gallery/serializers.py
//
// Catalog CONTENT comes from `manage.py seed_templates` (ops family): a
// fresh install serves empty lists — asserted by parity:designs/08-catalog.
// Template creation from a template (`use` on a seeded row, create_from_
// template) stays pending with content seeding; the not-found path is
// probe-proven (probes/0005 step09: "Template not found.").
package doory

import (
	"database/sql"
	"net/http"
	"strings"
)

var (
	templateOrientations = []string{"landscape", "portrait", "square"}
	templateOrdering     = []string{"use_count", "-use_count", "created_at", "-created_at", "title", "-title"}
)

const staticPages = "/\x00/features\x00/faq\x00/contact\x00/templates"

// templateCategoriesList ports TemplateCategoryListView: NOT paginated,
// counted, anonymous, cacheable? — gallery's PublicCatalogView does NOT set
// Cache-Control (only elements does), so the middleware's no-store applies
// (probe-verified live by the pair run).
func (a *App) templateCategoriesList(w http.ResponseWriter, r *http.Request) {
	rows, err := a.DB.Query(`
		SELECT c.id, c.slug, c.name, c.icon, c.sort_order,
		       (SELECT COUNT(*) FROM gallery_template t
		        WHERE t.category_id = c.id AND t.is_published = TRUE)
		FROM gallery_templatecategory c
		ORDER BY c.sort_order, c.name, c.id`)
	if err != nil {
		logerr(err, "templateCategoriesList")
		writeDRF(w, http.StatusInternalServerError, "server_error", "Something went wrong on our side.")
		return
	}
	defer rows.Close()
	type categoryEnvelope struct {
		ID            string `json:"id"`
		Slug          string `json:"slug"`
		Name          string `json:"name"`
		Icon          string `json:"icon"`
		TemplateCount int64  `json:"template_count"`
		SortOrder     int64  `json:"-"`
	}
	list := []categoryEnvelope{}
	for rows.Next() {
		var c categoryEnvelope
		if err := rows.Scan(&c.ID, &c.Slug, &c.Name, &c.Icon, &c.SortOrder, &c.TemplateCount); err != nil {
			logerr(err, "templateCategoriesList.scan")
			writeDRF(w, http.StatusInternalServerError, "server_error", "Something went wrong on our side.")
			return
		}
		c.ID = dashedUUID(c.ID)
		list = append(list, c)
	}
	writeJSON(w, http.StatusOK, list)
}

// templatesList ports TemplateListView + TemplateFilter: category/q/
// orientation/preset/featured filters, DEFAULT_ORDERING with total
// tie-breaks, standard pagination, document deferred for list rows.
func (a *App) templatesList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	var fields []FieldError

	where := []string{"t.is_published = TRUE"}
	args := []any{}

	if category := q.Get("category"); category != "" {
		where = append(where, "c.slug = ?")
		args = append(args, category)
	}
	if preset := q.Get("preset"); preset != "" {
		where = append(where, "t.preset = ?")
		args = append(args, preset)
	}
	if orientation := q.Get("orientation"); orientation != "" {
		if !choiceValid(templateOrientations, orientation) {
			fields = append(fields, djangoChoiceError("orientation", orientation))
		} else {
			switch orientation {
			case "square":
				where = append(where, "t.width = t.height")
			case "landscape":
				where = append(where, "t.width > t.height")
			case "portrait":
				where = append(where, "t.width < t.height")
			}
		}
	}
	if featured, ok := q["featured"]; ok {
		b, err := parseDRFBool(featured[0])
		if err != nil {
			// django-filter BooleanField: Django's form message.
			fields = append(fields, SimpleField("featured",
				"Enter a valid boolean.", "invalid"))
		} else if b {
			where = append(where, "t.is_featured = TRUE")
		} else {
			where = append(where, "t.is_featured = FALSE")
		}
	}
	if words := q.Get("q"); words != "" {
		// Every word must appear in title/description/tags (max 8 words).
		split := strings.Fields(words)
		if len(split) > 8 {
			split = split[:8]
		}
		for _, word := range split {
			where = append(where,
				"(t.title LIKE ? OR t.description LIKE ? OR t.tags LIKE ?)")
			pat := "%" + word + "%"
			args = append(args, pat, pat, pat)
		}
	}
	ordering := q.Get("ordering")
	if ordering != "" {
		if !choiceValid(templateOrdering, ordering) {
			// django-filter OrderingFilter invalid choice (django form path).
			fields = append(fields, djangoChoiceError("ordering", ordering))
		}
	}
	if len(fields) > 0 {
		writeDRFValidation(w, fields)
		return
	}

	// DEFAULT_ORDERING (-featured, -use_count, -created_at, id); the filter
	// appends missing tie-breaks so pagination is total.
	order := "t.is_featured DESC, t.use_count DESC, t.created_at DESC, t.id ASC"
	if ordering != "" {
		col := strings.TrimPrefix(ordering, "-")
		direction := "ASC"
		if strings.HasPrefix(ordering, "-") {
			direction = "DESC"
		}
		columns := map[string]string{"use_count": "t.use_count", "created_at": "t.created_at", "title": "t.title"}
		order = columns[col] + " " + direction + ", t.created_at DESC, t.id ASC"
	}

	rows, err := a.DB.Query(`
		SELECT t.id, t.slug, t.title, t.tags, t.width, t.height, t.preset, t.page_count,
		       t.thumbnail, t.is_featured, t.use_count, t.created_at,
		       c.id, c.slug, c.name
		FROM gallery_template t
		LEFT JOIN gallery_templatecategory c ON c.id = t.category_id
		WHERE `+strings.Join(where, " AND ")+`
		ORDER BY `+order, args...)
	if err != nil {
		logerr(err, "templatesList")
		writeDRF(w, http.StatusInternalServerError, "server_error", "Something went wrong on our side.")
		return
	}
	defer rows.Close()
	items := []templateEnvelope{}
	for rows.Next() {
		var raw struct {
			ID, Slug, Title, Tags              string
			Width, Height, PageCount, UseCount int64
			Preset, Thumbnail                  string
			IsFeatured                         bool
			CreatedAt                          string
			CatID, CatSlug, CatName            string
			CatNull                            sqlNullString
		}
		if err := rows.Scan(&raw.ID, &raw.Slug, &raw.Title, &raw.Tags, &raw.Width, &raw.Height,
			&raw.Preset, &raw.PageCount, &raw.Thumbnail, &raw.IsFeatured, &raw.UseCount,
			&raw.CreatedAt, &raw.CatID, &raw.CatSlug, &raw.CatName); err != nil {
			logerr(err, "templatesList.scan")
			writeDRF(w, http.StatusInternalServerError, "server_error", "Something went wrong on our side.")
			return
		}
		items = append(items, templateEnvelope{
			ID: dashedUUID(raw.ID), Slug: raw.Slug, Title: raw.Title,
			Category: categoryRefOrNull(raw.CatID, raw.CatSlug, raw.CatName, raw.CatID == ""),
			Tags:     rawCompacted(compactStoredJSON([]byte(raw.Tags))),
			Width:    raw.Width, Height: raw.Height,
			Orientation: orientationOf(raw.Width, raw.Height),
			Preset:      raw.Preset, PageCount: raw.PageCount,
			ThumbnailURL: fileURL(raw.Thumbnail),
			IsFeatured:   raw.IsFeatured, UseCount: raw.UseCount,
			CreatedAt: renderDRFDatetime(raw.CreatedAt),
		})
	}
	page, ferr := windowSlice(r, len(items), designPageSize)
	if ferr != nil {
		writeDRF(w, ferr.Status, ferr.Code, ferr.Message)
		return
	}
	resp, err := paginateSized(r, len(items), items[page.from:page.to], designPageSize)
	if err != nil {
		if de, ok := err.(*DocumentError); ok {
			writeDRF(w, de.Status, de.Code, de.Message)
			return
		}
		writeDRF(w, http.StatusInternalServerError, "server_error", "Something went wrong on our side.")
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

type sqlNullString struct {
	Valid  bool
	String string
}

func fileURL(path string) *string {
	if path == "" {
		return nil
	}
	u := mediaURL + path
	return &u
}

func orientationOf(w, h int64) string {
	switch {
	case w == h:
		return "square"
	case w > h:
		return "landscape"
	}
	return "portrait"
}

func categoryRefOrNull(id, slug, name string, null bool) *categoryRef {
	if null {
		return nil
	}
	return &categoryRef{ID: dashedUUID(id), Slug: slug, Name: name}
}

type categoryRef struct {
	ID   string `json:"id"`
	Slug string `json:"slug"`
	Name string `json:"name"`
}

// templateEnvelope mirrors TemplateSummarySerializer field order.
type templateEnvelope struct {
	ID           string       `json:"id"`
	Slug         string       `json:"slug"`
	Title        string       `json:"title"`
	Category     *categoryRef `json:"category"`
	Tags         rawCompacted `json:"tags"`
	Width        int64        `json:"width"`
	Height       int64        `json:"height"`
	Orientation  string       `json:"orientation"`
	Preset       string       `json:"preset"`
	PageCount    int64        `json:"page_count"`
	ThumbnailURL *string      `json:"thumbnail_url"`
	IsFeatured   bool         `json:"is_featured"`
	UseCount     int64        `json:"use_count"`
	CreatedAt    string       `json:"created_at"`
}

// templateDetail ports TemplateDetailView (GET /templates/<slug>/): unknown
// slug -> DRF NotFound "Not found." (probe0005 step04); a segment that is
// not a slug at all never matches the route (resolver 404 dialect).
func (a *App) templateDetail(w http.ResponseWriter, r *http.Request) {
	slug := routeParam(r, "slug")
	row := a.DB.QueryRow(`
		SELECT t.id, t.slug, t.title, t.description, t.tags, t.width, t.height, t.preset,
		       t.page_count, t.thumbnail, t.is_featured, t.use_count, t.created_at,
		       t.document, c.id, c.slug, c.name
		FROM gallery_template t
		LEFT JOIN gallery_templatecategory c ON c.id = t.category_id
		WHERE t.slug = ? AND t.is_published = TRUE`, slug)
	var raw struct {
		ID, Slug, Title, Description, Tags, Preset, Thumbnail string
		Width, Height, PageCount, UseCount                    int64
		IsFeatured                                            bool
		CreatedAt, Document                                   string
		CatID, CatSlug, CatName                               string
	}
	err := row.Scan(&raw.ID, &raw.Slug, &raw.Title, &raw.Description, &raw.Tags, &raw.Width,
		&raw.Height, &raw.Preset, &raw.PageCount, &raw.Thumbnail, &raw.IsFeatured,
		&raw.UseCount, &raw.CreatedAt, &raw.Document, &raw.CatID, &raw.CatSlug, &raw.CatName)
	if err != nil {
		if err == sql.ErrNoRows {
			writeDRF(w, http.StatusNotFound, "not_found", "Not found.")
			return
		}
		logerr(err, "templateDetail")
		writeDRF(w, http.StatusInternalServerError, "server_error", "Something went wrong on our side.")
		return
	}
	type templateDetailEnvelope struct {
		templateEnvelope
		Description string       `json:"description"`
		Document    rawCompacted `json:"document"`
	}
	base := templateEnvelope{
		ID: dashedUUID(raw.ID), Slug: raw.Slug, Title: raw.Title,
		Category: categoryRefOrNull(raw.CatID, raw.CatSlug, raw.CatName, raw.CatID == ""),
		Tags:     rawCompacted(compactStoredJSON([]byte(raw.Tags))),
		Width:    raw.Width, Height: raw.Height,
		Orientation: orientationOf(raw.Width, raw.Height),
		Preset:      raw.Preset, PageCount: raw.PageCount,
		ThumbnailURL: fileURL(raw.Thumbnail),
		IsFeatured:   raw.IsFeatured, UseCount: raw.UseCount,
		CreatedAt: renderDRFDatetime(raw.CreatedAt),
	}
	writeJSON(w, http.StatusOK, templateDetailEnvelope{
		templateEnvelope: base,
		Description:      raw.Description,
		Document:         rawCompacted(compactStoredJSON([]byte(raw.Document))),
	})
}

// templateUse ports TemplateUseView (POST /templates/<slug>/use/): a signed-
// in caller creates a design from the template; unknown slug answers the
// DRF NotFound with its custom message (probe0005: "Template not found.").
// Creating FROM a seeded template (create_from_template happy path) stays
// pending with content seeding (PORT_MAP note).
func (a *App) templateUse(w http.ResponseWriter, r *http.Request) {
	slug := routeParam(r, "slug")
	var exists int
	if err := a.DB.QueryRow(
		`SELECT COUNT(*) FROM gallery_template WHERE slug = ? AND is_published = TRUE`,
		slug).Scan(&exists); err != nil {
		logerr(err, "templateUse")
		writeDRF(w, http.StatusInternalServerError, "server_error", "Something went wrong on our side.")
		return
	}
	if exists == 0 {
		writeDRF(w, http.StatusNotFound, "not_found", "Template not found.")
		return
	}
	// Seeded-template creation: pending with content seeding (gallery ops).
	writeDRF(w, http.StatusInternalServerError, "server_error",
		"route registered but not ported: gallery.create_from_template (content seeding pending)")
}

// sitemap ports gallery/sitemap.py: the landing/features/FAQ/contact/
// templates entries built from SITE_URL plus every published template
// (none until content is seeded). GET only; NOT a DRF view — no Allow on
// success (route flag AllowNone).
func (a *App) sitemap(w http.ResponseWriter, r *http.Request) {
	base := strings.TrimRight(a.Settings.SiteURL, "/")
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	b.WriteString(`<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">`)
	for _, path := range strings.Split(staticPages, "\x00") {
		b.WriteString("<url><loc>" + xmlEscape(base+path) + "</loc></url>")
	}
	// Published templates (slug + lastmod), ordered by slug — none until
	// seed_templates runs (ops family).
	rows, err := a.DB.Query(
		`SELECT slug, updated_at FROM gallery_template WHERE is_published = TRUE
		 ORDER BY slug LIMIT ?`, 50000-5)
	if err == nil {
		for rows.Next() {
			var slug, updated string
			if rows.Scan(&slug, &updated) == nil {
				if t, err := parseDjangoDateTime(updated); err == nil {
					b.WriteString("<url><loc>" + xmlEscape(base+"/templates/"+slug) +
						"</loc><lastmod>" + t.UTC().Format("2006-01-02") + "</lastmod></url>")
				}
			}
		}
		rows.Close()
	}
	b.WriteString("</urlset>\n")
	body := b.String()
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	w.Header().Set("Content-Length", itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(body))
}

func xmlEscape(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch r {
		case '&':
			b.WriteString("&amp;")
		case '<':
			b.WriteString("&lt;")
		case '>':
			b.WriteString("&gt;")
		case '"':
			b.WriteString("&quot;")
		case '\'':
			b.WriteString("&apos;")
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
