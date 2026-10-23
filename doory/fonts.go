// Fonts: public catalogue + the caller's uploaded families
// (apps/fonts/{views,serializers,uploads,filters}.py).
//
// Probe-first: probes/0006-fonts — empty catalogue shape, filter messages
// (django-FORM choice for the FilterSet vs DRF straight-quote for the
// upload serializer, plus the Django-core validator with code NULL), the
// upload201 envelope (variants/unicode_ranges/popularity9999), the flat
// family-mismatch and junk-file messages, and the409 variant clash.
//
// Metadata probing (sniff/name/cmap/OS-2/scripts/category + WOFF/WOFF2) is
// internal/fontparse — cross-proven against the oracle's own probe_worker
// (internal/fontparse tests).
//
// Ports backend/apps/fonts/views.py
// Ports backend/apps/fonts/serializers.py
// Ports backend/apps/fonts/uploads.py
// Ports backend/apps/fonts/filters.py
package doory

import (
	"database/sql"
	"net/http"
	"sort"
	"strings"
)

const (
	fontPageSize        = 50 // FontPagination page_size
	fontCache           = "public, max-age=300"
	maxFontFileBytes    = 10 * 1024 * 1024
	maxFilesPerUpload   = 5
	maxFamiliesPerUser  = 50
	maxFontBytesPerUser = 150 * 1024 * 1024
	userFontLicense     = "user-supplied"
	unranked            = 9999 // models.UNRANKED
	maxScriptWords      = 6
)

var fontOrderingChoices = []string{
	"popularity", "-popularity", "family", "-family", "created_at", "-created_at",
}

// fontFamilyRow maps fonts_fontfamily.
type fontFamilyRow struct {
	ID          string
	Family      string
	Slug        string
	Category    string
	Scripts     string
	IsVariable  bool
	Popularity  int64
	License     string
	Source      string
	OwnerID     sql.NullString
	IsPublished bool
	CreatedAt   string
	UpdatedAt   string
}

const familyCols = `id, family, slug, category, scripts, is_variable, popularity,
	license, source, owner_id, is_published, created_at, updated_at`

func scanFamily(s scanner) (fontFamilyRow, error) {
	var f fontFamilyRow
	err := s.Scan(&f.ID, &f.Family, &f.Slug, &f.Category, &f.Scripts, &f.IsVariable,
		&f.Popularity, &f.License, &f.Source, &f.OwnerID, &f.IsPublished,
		&f.CreatedAt, &f.UpdatedAt)
	return f, err
}

// fontVariantRow maps fonts_fontvariant.
type fontVariantRow struct {
	ID           string
	FamilyID     string
	Weight       int64
	Style        string
	Format       string
	Size         int64
	Subset       string
	UnicodeRange string
	File         string
}

const variantCols = `id, family_id, weight, style, format, size, subset, unicode_range, file`

func scanVariant(s scanner) (fontVariantRow, error) {
	var v fontVariantRow
	err := s.Scan(&v.ID, &v.FamilyID, &v.Weight, &v.Style, &v.Format, &v.Size,
		&v.Subset, &v.UnicodeRange, &v.File)
	return v, err
}

// subsetPriority mirrors SUBSET_PRIORITY ("" first, latin, latin-ext).
func subsetPriority(subset string) int {
	switch subset {
	case "":
		return 0
	case "latin":
		return 1
	case "latin-ext":
		return 2
	}
	return 10
}

// fontFamilyEnvelope mirrors FontFamilySerializer field order.
type fontFamilyEnvelope struct {
	ID            string             `json:"id"`
	Family        string             `json:"family"`
	Category      string             `json:"category"`
	Scripts       rawCompacted       `json:"scripts"`
	IsVariable    bool               `json:"is_variable"`
	IsUserFont    bool               `json:"is_user_font"`
	Variants      []fontVariantGroup `json:"variants"`
	UnicodeRanges rawCompacted       `json:"unicode_ranges"`
	Popularity    int64              `json:"popularity"`
}

type fontVariantGroup struct {
	Weight  int64             `json:"weight"`
	Style   string            `json:"style"`
	URL     string            `json:"url"`
	Subsets []fontSubsetEntry `json:"subsets"`
}

type fontSubsetEntry struct {
	Subset string `json:"subset"`
	URL    string `json:"url"`
}

// envelope builds the serializer shape: groups per (weight, style) ordered
// (weight, style!="normal"); url = primary file; subsets only for named
// subsets; unicode_ranges sorted by subset priority.
func (f *fontFamilyRow) envelope(variants []fontVariantRow) fontFamilyEnvelope {
	// group by (weight, style)
	type key struct {
		w int64
		s string
	}
	groups := map[key][]fontVariantRow{}
	var keys []key
	for _, v := range variants {
		k := key{v.Weight, v.Style}
		if _, ok := groups[k]; !ok {
			keys = append(keys, k)
		}
		groups[k] = append(groups[k], v)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].w != keys[j].w {
			return keys[i].w < keys[j].w
		}
		if (keys[i].s != "normal") != (keys[j].s != "normal") {
			return keys[i].s != "normal"
		}
		return false
	})
	out := []fontVariantGroup{}
	ranges := map[string]string{}
	for _, k := range keys {
		rows := groups[k]
		sort.Slice(rows, func(i, j int) bool {
			pi, pj := subsetPriority(rows[i].Subset), subsetPriority(rows[j].Subset)
			if pi != pj {
				return pi < pj
			}
			return rows[i].Subset < rows[j].Subset
		})
		group := fontVariantGroup{
			Weight: k.w, Style: k.s,
			URL:     mediaURL + rows[0].File, // primary = min by priority
			Subsets: []fontSubsetEntry{},
		}
		for _, r := range rows {
			if r.Subset != "" {
				group.Subsets = append(group.Subsets, fontSubsetEntry{
					Subset: r.Subset, URL: mediaURL + r.File,
				})
			}
			if r.Subset != "" && r.UnicodeRange != "" {
				if _, seen := ranges[r.Subset]; !seen {
					ranges[r.Subset] = r.UnicodeRange
				}
			}
		}
		out = append(out, group)
	}
	// unicode_ranges sorted by subset priority ("" never present).
	rangeKeys := make([]string, 0, len(ranges))
	for k := range ranges {
		rangeKeys = append(rangeKeys, k)
	}
	sort.Slice(rangeKeys, func(i, j int) bool {
		pi, pj := subsetPriority(rangeKeys[i]), subsetPriority(rangeKeys[j])
		if pi != pj {
			return pi < pj
		}
		return rangeKeys[i] < rangeKeys[j]
	})
	orderedRanges := map[string]string{}
	for _, k := range rangeKeys {
		orderedRanges[k] = ranges[k]
	}
	_ = orderedRanges
	return fontFamilyEnvelope{
		ID: dashedUUID(f.ID), Family: f.Family, Category: f.Category,
		Scripts:       rawCompacted(compactStoredJSON([]byte(f.Scripts))),
		IsVariable:    f.IsVariable,
		IsUserFont:    f.OwnerID.Valid && f.OwnerID.String != "",
		Variants:      out,
		UnicodeRanges: rangesJSON(rangeKeys, ranges),
		Popularity:    f.Popularity,
	}
}

// rangesJSON renders {"subset":"range",...} in priority order.
func rangesJSON(keys []string, m map[string]string) rawCompacted {
	var b strings.Builder
	b.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(jsonStr(k))
		b.WriteByte(':')
		b.WriteString(jsonStr(m[k]))
	}
	b.WriteByte('}')
	return rawCompacted(b.String())
}

// fontListQuery mirrors FontFilter: q (words, icontains family), category
// (django-FORM choice), script (Django-core validator with code NULL),
// ordering (django-FORM choice).
func fontListQuery(r *http.Request) (where []string, args []any, order string, fields []FieldError) {
	q := r.URL.Query()
	where = []string{}
	order = "popularity, family, id" // default ordering
	if words := q.Get("q"); words != "" {
		parts := strings.Fields(words)
		if len(parts) > maxScriptWords {
			parts = parts[:maxScriptWords]
		}
		for _, w := range parts {
			where = append(where, "family LIKE ? ESCAPE '\\'")
			args = append(args, "%"+escapeLike(w)+"%")
		}
	}
	if category := q.Get("category"); category != "" {
		if !choiceValid(fontCategories(), category) {
			fields = append(fields, djangoChoiceError("category", category))
		} else {
			where = append(where, "category = ?")
			args = append(args, category)
		}
	}
	if script := q.Get("script"); script != "" {
		if !validScript(script) {
			// Django-core validator: no code -> JSON null in field_codes
			// (probe0006 step03).
			fields = append(fields, FieldError{
				Name:     "script",
				Messages: []string{"Use a script name such as latin, cyrillic or devanagari."},
				Codes:    []string{""}, // "" renders as JSON null
			})
		} else {
			// Script filter: scripts JSON contains the word (LIKE on the
			// stored JSON text — observable only with seeded rows).
			where = append(where, "scripts LIKE ?")
			args = append(args, "%"+script+"%")
		}
	}
	if ordering := q.Get("ordering"); ordering != "" {
		if !choiceValid(fontOrderingChoices, ordering) {
			fields = append(fields, djangoChoiceError("ordering", ordering))
		} else {
			col := strings.TrimPrefix(ordering, "-")
			dirn := "ASC"
			if strings.HasPrefix(ordering, "-") {
				dirn = "DESC"
			}
			columns := map[string]string{"popularity": "popularity", "family": "family", "created_at": "created_at"}
			order = columns[col] + " " + dirn + ", id ASC"
		}
	}
	return where, args, order, fields
}

func fontCategories() []string {
	return []string{"sans-serif", "serif", "display", "handwriting", "monospace"}
}

// validScript mirrors filters.validate_script (^[a-z]+(-[a-z]+)?$).
func validScript(v string) bool {
	parts := strings.Split(v, "-")
	if len(parts) > 2 || parts[0] == "" {
		return false
	}
	for _, p := range parts {
		if p == "" {
			return false
		}
		for _, r := range p {
			if r < 'a' || r > 'z' {
				return false
			}
		}
	}
	return true
}

// fontList ports PublicFontList: published system families with at least
// one variant, paginated50, cache 300, no session consult.
func (a *App) fontList(w http.ResponseWriter, r *http.Request) {
	where, args, order, fields := fontListQuery(r)
	if len(fields) > 0 {
		writeDRFValidation(w, fields)
		return
	}
	clause := "f.owner_id IS NULL AND f.is_published = 1"
	for _, cond := range where {
		clause += " AND " + cond
	}
	rows, err := a.DB.Query(
		`SELECT `+familyCols+` FROM fonts_fontfamily f WHERE `+clause+
			` AND EXISTS (SELECT 1 FROM fonts_fontvariant v WHERE v.family_id = f.id)
		 ORDER BY `+order, args...)
	if err != nil {
		logerr(err, "fontList")
		writeDRF(w, http.StatusInternalServerError, "server_error", "Something went wrong on our side.")
		return
	}
	defer rows.Close()
	list, err := a.collectFamilies(rows)
	if err != nil {
		logerr(err, "fontList.collect")
		writeDRF(w, http.StatusInternalServerError, "server_error", "Something went wrong on our side.")
		return
	}
	page, ferr := windowSlice(r, len(list), fontPageSize)
	if ferr != nil {
		writeDRF(w, ferr.Status, ferr.Code, ferr.Message)
		return
	}
	envs, err := a.familyEnvelopes(list, page)
	if err != nil {
		logerr(err, "familyEnvelopes")
		writeDRF(w, http.StatusInternalServerError, "server_error", "Something went wrong on our side.")
		return
	}
	resp, err := paginateSized(r, len(list), envs, fontPageSize)
	if err != nil {
		if de, ok := err.(*DocumentError); ok {
			writeDRF(w, de.Status, de.Code, de.Message)
			return
		}
		writeDRF(w, http.StatusInternalServerError, "server_error", "Something went wrong on our side.")
		return
	}
	w.Header().Set("Cache-Control", fontCache) // finalize_response on GET 200
	writeJSON(w, http.StatusOK, resp)
}

func (a *App) collectFamilies(rows *sql.Rows) ([]fontFamilyRow, error) {
	var list []fontFamilyRow
	for rows.Next() {
		f, err := scanFamily(rows)
		if err != nil {
			return nil, err
		}
		list = append(list, f)
	}
	return list, rows.Err()
}

func (a *App) variantsOf(familyIDs []string) (map[string][]fontVariantRow, error) {
	out := map[string][]fontVariantRow{}
	if len(familyIDs) == 0 {
		return out, nil
	}
	placeholders := strings.Repeat("?,", len(familyIDs)-1) + "?"
	args := make([]any, len(familyIDs))
	for i, id := range familyIDs {
		args[i] = id
	}
	rows, err := a.DB.Query(
		`SELECT `+variantCols+` FROM fonts_fontvariant WHERE family_id IN (`+placeholders+
			`) ORDER BY weight, style, subset`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		v, err := scanVariant(rows)
		if err != nil {
			return nil, err
		}
		out[v.FamilyID] = append(out[v.FamilyID], v)
	}
	return out, rows.Err()
}

// fontMine ports UserFontListCreate (GET list / POST multipart upload).
func (a *App) fontMine(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		a.fontUpload(w, r)
		return
	}
	u := currentUser(r)
	rows, err := a.DB.Query(
		`SELECT `+familyCols+` FROM fonts_fontfamily WHERE owner_id = ? ORDER BY family, id`, u.ID)
	if err != nil {
		logerr(err, "fontMine")
		writeDRF(w, http.StatusInternalServerError, "server_error", "Something went wrong on our side.")
		return
	}
	defer rows.Close()
	list, err := a.collectFamilies(rows)
	if err != nil {
		logerr(err, "fontMine.collect")
		writeDRF(w, http.StatusInternalServerError, "server_error", "Something went wrong on our side.")
		return
	}
	page, ferr := windowSlice(r, len(list), fontPageSize)
	if ferr != nil {
		writeDRF(w, ferr.Status, ferr.Code, ferr.Message)
		return
	}
	envs, err := a.familyEnvelopes(list, page)
	if err != nil {
		logerr(err, "familyEnvelopes")
		writeDRF(w, http.StatusInternalServerError, "server_error", "Something went wrong on our side.")
		return
	}
	resp, err := paginateSized(r, len(list), envs, fontPageSize)
	if err != nil {
		if de, ok := err.(*DocumentError); ok {
			writeDRF(w, de.Status, de.Code, de.Message)
			return
		}
		writeDRF(w, http.StatusInternalServerError, "server_error", "Something went wrong on our side.")
		return
	}
	// No cache header here (probe0006: middleware no-store on mine list).
	writeJSON(w, http.StatusOK, resp)
}
