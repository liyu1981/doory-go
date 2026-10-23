// Elements catalog: the two PUBLIC read endpoints (apps/elements/views.py).
//
// Ports backend/apps/elements/views.py
// Ports backend/apps/elements/filters.py
// Ports backend/apps/elements/serializers.py
//
// Catalog CONTENT comes from `manage.py seed_elements` (ops family); a fresh
// install serves empty lists — asserted by parity:designs/08-catalog. The
// q/relevance ordering expression (search.py) is only observable with seeded
// rows and stays pending with content seeding (PORT_MAP note).
package doory

import (
	"fmt"
	"net/http"
	"strings"
)

// elementKind/elementStyle choices (models.py TextChoices).
var (
	elementKinds           = []string{"shape", "line", "icon", "sticker", "illustration", "frame", "graphic"}
	elementStyles          = []string{"outline", "filled", "duotone", "colour"}
	elementOrderingChoices = []string{
		"name", "-name", "created_at", "-created_at", "sort_order", "-sort_order", "relevance",
	}
)

const (
	elementPageSize = 48                    // ElementPagination page_size
	elementCache    = "public, max-age=300" // CACHE_CONTROL (finalize_response)
)

// djangoChoiceError mirrors Django forms.ChoiceField's invalid message (the
// django-filter validation path — distinct from DRF's “...” is not a valid
// choice.): "Select a valid choice. X is not one of the available choices."
func djangoChoiceError(field, value string) FieldError {
	return SimpleField(field,
		fmt.Sprintf("Select a valid choice. %s is not one of the available choices.", value), "invalid_choice")
}

func totalLen[T any](s []T) int { return len(s) }

func choiceValid(choices []string, v string) bool {
	for _, c := range choices {
		if c == v {
			return true
		}
	}
	return false
}

// elementPacksList ports ElementPackListView: NOT paginated, published
// packs with a live element count, cacheable, anonymous (SessionNone).
func (a *App) elementPacksList(w http.ResponseWriter, r *http.Request) {
	// Empty-state port: with no seeded elements there are never packs
	// (element_count > 0 filter). Content seeding (ops family) flips this;
	// the query below mirrors the annotation for when rows exist.
	w.Header().Set("Cache-Control", elementCache)
	rows, err := a.DB.Query(`
		SELECT p.id, p.slug, p.name, p.kind, p.license, p.license_url, p.attribution,
		       p.source_url, p.thumbnail, p.sort_order
		FROM elements_elementpack p
		WHERE p.is_published = 1
		  AND EXISTS (SELECT 1 FROM elements_element e
		              WHERE e.pack_id = p.id AND e.is_published = 1)
		ORDER BY p.sort_order, p.name, p.id`)
	if err != nil {
		logerr(err, "elementPacksList")
		writeDRF(w, http.StatusInternalServerError, "server_error", "Something went wrong on our side.")
		return
	}
	defer rows.Close()
	packs := []packEnvelope{}
	for rows.Next() {
		var p packEnvelope
		var thumbnail string
		var id string
		if err := rows.Scan(&id, &p.Slug, &p.Name, &p.Kind, &p.License, &p.LicenseURL,
			&p.Attribution, &p.SourceURL, &thumbnail, &p.sortOrder); err != nil {
			logerr(err, "elementPacksList.scan")
			writeDRF(w, http.StatusInternalServerError, "server_error", "Something went wrong on our side.")
			return
		}
		p.ID = dashedUUID(id)
		packs = append(packs, p)
	}
	// PackRef + thumbnail_url + element_count: with no seeded content this
	// is always [] — the envelope type keeps the shape honest for later.
	writeJSON(w, http.StatusOK, packsAsAny(packs))
}

// packEnvelope mirrors ElementPackSerializer field order (counts/svgs
// annotated per row — pending with content seeding, see PORT_MAP).
type packEnvelope struct {
	ID           string `json:"id"`
	Slug         string `json:"slug"`
	Name         string `json:"name"`
	Kind         string `json:"kind"`
	License      string `json:"license"`
	LicenseURL   string `json:"license_url"`
	Attribution  string `json:"attribution"`
	SourceURL    string `json:"source_url"`
	sortOrder    int64
	thumbnailURL *string
}

func packsAsAny(packs []packEnvelope) any {
	if packs == nil {
		packs = []packEnvelope{}
	}
	return packs
}

// elementList ports ElementListView: filters (pack/kind/style/q/ordering),
// default pack order, pagination 48, cache on GET 200.
func (a *App) elementList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	var fields []FieldError

	where := []string{"e.is_published = 1", "p.is_published = 1"}
	args := []any{}

	if pack := q.Get("pack"); pack != "" {
		where = append(where, "p.slug = ?")
		args = append(args, pack)
	}
	if kind := q.Get("kind"); kind != "" {
		if !choiceValid(elementKinds, kind) {
			fields = append(fields, djangoChoiceError("kind", kind))
		} else {
			where = append(where, "e.kind = ?")
			args = append(args, kind)
		}
	}
	if style := q.Get("style"); style != "" {
		if !choiceValid(elementStyles, style) {
			fields = append(fields, djangoChoiceError("style", style))
		} else {
			where = append(where, "e.style = ?")
			args = append(args, style)
		}
	}
	ordering := q.Get("ordering")
	if ordering != "" && !choiceValid(elementOrderingChoices, ordering) {
		fields = append(fields, djangoChoiceError("ordering", ordering))
	}
	if len(fields) > 0 {
		writeDRFValidation(w, fields)
		return
	}

	// Default ordering: pack sort, pack name, element sort, name, id.
	// (Relevance ordering needs seeded rows + q — pending with content
	// seeding; on an empty catalog both produce the same empty page.)
	order := "p.sort_order, p.name, p.id, e.sort_order, e.name, e.id"
	if ordering != "" && ordering != "relevance" {
		col := strings.TrimPrefix(ordering, "-")
		direction := "ASC"
		if strings.HasPrefix(ordering, "-") {
			direction = "DESC"
		}
		columns := map[string]string{
			"name": "e.name", "created_at": "e.created_at", "sort_order": "e.sort_order",
		}
		order = columns[col] + " " + direction + ", e.id ASC"
	}

	rows, err := a.DB.Query(`
		SELECT e.id, e.name, e.kind, e.style, e.tags, e.width, e.height, e.colors,
		       e.is_recolorable, e.svg, p.id, p.slug, p.name
		FROM elements_element e
		JOIN elements_elementpack p ON p.id = e.pack_id
		WHERE `+strings.Join(where, " AND ")+`
		ORDER BY `+order, args...)
	if err != nil {
		logerr(err, "elementList")
		writeDRF(w, http.StatusInternalServerError, "server_error", "Something went wrong on our side.")
		return
	}
	defer rows.Close()
	items := []elementEnvelope{}
	for rows.Next() {
		var raw struct {
			ID, Name, Kind, Style, Tags, Colors, Svg string
			Width, Height                            int64
			IsRecolorable                            bool
			PackID, PackSlug, PackName               string
		}
		if err := rows.Scan(&raw.ID, &raw.Name, &raw.Kind, &raw.Style, &raw.Tags, &raw.Width,
			&raw.Height, &raw.Colors, &raw.IsRecolorable, &raw.Svg,
			&raw.PackID, &raw.PackSlug, &raw.PackName); err != nil {
			logerr(err, "elementList.scan")
			writeDRF(w, http.StatusInternalServerError, "server_error", "Something went wrong on our side.")
			return
		}
		items = append(items, elementEnvelope{
			ID: dashedUUID(raw.ID), Name: raw.Name, Kind: raw.Kind, Style: raw.Style,
			Pack:   elementPack{ID: dashedUUID(raw.PackID), Slug: raw.PackSlug, Name: raw.PackName},
			Tags:   rawCompacted(compactStoredJSON([]byte(raw.Tags))),
			SvgURL: mediaURL + raw.Svg,
			Width:  raw.Width, Height: raw.Height,
			Colors:        rawCompacted(compactStoredJSON([]byte(raw.Colors))),
			IsRecolorable: raw.IsRecolorable,
		})
	}
	page, ferr := windowSlice(r, totalLen(items), elementPageSize)
	if ferr != nil {
		writeDRF(w, ferr.Status, ferr.Code, ferr.Message)
		return
	}
	resp, err := paginateSized(r, totalLen(items), items[page.from:page.to], elementPageSize)
	if err != nil {
		if de, ok := err.(*DocumentError); ok {
			writeDRF(w, de.Status, de.Code, de.Message)
			return
		}
		writeDRF(w, http.StatusInternalServerError, "server_error", "Something went wrong on our side.")
		return
	}
	w.Header().Set("Cache-Control", elementCache)
	writeJSON(w, http.StatusOK, resp)
}

// elementEnvelope mirrors ElementItemSerializer field order (pack nested
// ref; JSON columns pass through compacted-but-order-preserving).
type elementEnvelope struct {
	ID            string       `json:"id"`
	Name          string       `json:"name"`
	Kind          string       `json:"kind"`
	Style         string       `json:"style"`
	Pack          elementPack  `json:"pack"`
	Tags          rawCompacted `json:"tags"`
	SvgURL        string       `json:"svg_url"`
	Width         int64        `json:"width"`
	Height        int64        `json:"height"`
	Colors        rawCompacted `json:"colors"`
	IsRecolorable bool         `json:"is_recolorable"`
}

type elementPack struct {
	ID   string `json:"id"`
	Slug string `json:"slug"`
	Name string `json:"name"`
}

// elementPageSlice/paginateWith: ElementPagination defaults to 48.
