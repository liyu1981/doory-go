// Design lifecycle: CRUD with optimistic concurrency, trash, duplicates,
// versions (apps/designs/{views,services,serializers,models}.py).
//
// Probe-first: probes/0004-designs (15 transcripts) fixed every shape —
// create-with-blank-document bytes, paginated {count,next,previous,
// results} with alphabetized absolute next/previous links
// (rest_framework.utils.urls.replace_query_param sorts query items),
// validator envelopes (invalid_document / inline_asset_not_allowed),
// the custom 409 conflict body with `current`, blank-title vs
// empty-after-clean title errors.
//
// Document passthrough: responses re-emit the stored JSON preserving key
// order and number literals (compactStoredJSON); the oracle round-trips
// through Python dicts the same way (probe: submitted key order survives).
//
// Ports backend/apps/designs/views.py
// Ports backend/apps/designs/services.py
// Ports backend/apps/designs/serializers.py
package doory

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	defaultDesignTitle = "Untitled design" // services.DEFAULT_TITLE
	defaultDesignSize  = 1080              // services.DEFAULT_SIZE
	autoVersionGap     = 10 * time.Minute  // AUTO_VERSION_INTERVAL
	editVersionGap     = 2 * time.Minute   // EDIT_VERSION_INTERVAL
	maxAutoVersions    = 50
	maxManualVersions  = 100
	designPageSize     = 24 // core/pagination.py StandardPagination
	maxPageSize        = 100
)

// designRow maps designs_design.
type designRow struct {
	ID        string
	OwnerID   string
	Title     string
	Width     int64
	Height    int64
	Preset    string
	Document  string // stored JSON text (emitted compact, order-preserving)
	PageCount int64
	Thumbnail string
	FolderID  sql.NullString
	Starred   bool
	Revision  int64
	CreatedAt string
	UpdatedAt string
	TrashedAt sql.NullString
}

const designCols = `id, owner_id, title, width, height, preset, document, page_count,
	thumbnail, folder_id, starred, revision, created_at, updated_at, trashed_at`

func scanDesign(s scanner) (designRow, error) {
	var d designRow
	err := s.Scan(&d.ID, &d.OwnerID, &d.Title, &d.Width, &d.Height, &d.Preset, &d.Document,
		&d.PageCount, &d.Thumbnail, &d.FolderID, &d.Starred, &d.Revision,
		&d.CreatedAt, &d.UpdatedAt, &d.TrashedAt)
	return d, err
}

// designSummary is DesignSummarySerializer field order.
type designSummary struct {
	ID           string  `json:"id"`
	Title        string  `json:"title"`
	Width        int64   `json:"width"`
	Height       int64   `json:"height"`
	Preset       string  `json:"preset"`
	PageCount    int64   `json:"page_count"`
	ThumbnailURL *string `json:"thumbnail_url"`
	Folder       *string `json:"folder"`
	Starred      bool    `json:"starred"`
	Revision     int64   `json:"revision"`
	CreatedAt    string  `json:"created_at"`
	UpdatedAt    string  `json:"updated_at"`
	TrashedAt    *string `json:"trashed_at"`
}

// designFull = summary + document LAST (DesignSerializer field tuple).
type designFull struct {
	designSummary
	Document json.RawMessage `json:"document"`
}

func (d *designRow) summary() designSummary {
	thumb := (*string)(nil)
	if d.Thumbnail != "" {
		s := mediaURL + d.Thumbnail
		thumb = &s
	}
	var folder *string
	if d.FolderID.Valid && d.FolderID.String != "" {
		f := dashedUUID(d.FolderID.String)
		folder = &f
	}
	return designSummary{
		ID: dashedUUID(d.ID), Title: d.Title, Width: d.Width, Height: d.Height,
		Preset: d.Preset, PageCount: d.PageCount, ThumbnailURL: thumb, Folder: folder,
		Starred: d.Starred, Revision: d.Revision,
		CreatedAt: renderDRFDatetime(d.CreatedAt),
		UpdatedAt: renderDRFDatetime(d.UpdatedAt),
		TrashedAt: nullableDatetime(d.TrashedAt),
	}
}

func (d *designRow) full() designFull {
	return designFull{designSummary: d.summary(),
		Document: json.RawMessage(compactStoredJSON([]byte(d.Document)))}
}

// compactStoredJSON re-emits stored JSON compactly, preserving key order and
// number literals (Python's parse->dump round trip does the same for stored
// text; string escapes normalize through the re-render on both sides).
func compactStoredJSON(raw []byte) []byte {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	out, err := readJSONValue(dec)
	if err != nil {
		return []byte(raw) // unparsable stored text: better to expose than crash
	}
	return out
}

func readJSONValue(dec *json.Decoder) ([]byte, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			out := []byte{'{'}
			first := true
			for dec.More() {
				keyTok, err := dec.Token()
				if err != nil {
					return nil, err
				}
				key, ok := keyTok.(string)
				if !ok {
					return nil, fmt.Errorf("object key not a string")
				}
				if !first {
					out = append(out, ',')
				}
				first = false
				out = append(out, jsonStr(key)...)
				out = append(out, ':')
				val, err := readJSONValue(dec)
				if err != nil {
					return nil, err
				}
				out = append(out, val...)
			}
			if _, err := dec.Token(); err != nil { // consume '}'
				return nil, err
			}
			return append(out, '}'), nil
		case '[':
			out := []byte{'['}
			first := true
			for dec.More() {
				if !first {
					out = append(out, ',')
				}
				first = false
				val, err := readJSONValue(dec)
				if err != nil {
					return nil, err
				}
				out = append(out, val...)
			}
			if _, err := dec.Token(); err != nil { // consume ']'
				return nil, err
			}
			return append(out, ']'), nil
		}
		return nil, fmt.Errorf("unexpected delim %v", t)
	case json.Number:
		return []byte(t.String()), nil
	case string:
		return []byte(jsonStr(t)), nil
	case bool:
		if t {
			return []byte("true"), nil
		}
		return []byte("false"), nil
	case nil:
		return []byte("null"), nil
	}
	return nil, fmt.Errorf("unexpected token %v", tok)
}

// ---- pagination (StandardPagination semantics) ----

type pagedResponse struct {
	Count    int     `json:"count"`
	Next     *string `json:"next"`
	Previous *string `json:"previous"`
	Results  any     `json:"results"`
}

// paginate applies page/page_size (defaultSize default, 100 max, "last"
// accepted) and builds alphabetized absolute links like DRF
// (replace_query_param sorts query items — probe0004 step05).
func paginate(r *http.Request, total int, items any) (any, error) {
	return paginateSized(r, total, items, designPageSize)
}

func paginateSized(r *http.Request, total int, items any, defaultSize int) (any, error) {
	query := r.URL.Query()
	pageSize := defaultSize
	if raw := query.Get("page_size"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			pageSize = n
			if pageSize > maxPageSize {
				pageSize = maxPageSize
			}
		}
	}
	numPages := (total + pageSize - 1) / pageSize
	if numPages == 0 {
		numPages = 1 // allow_empty_first_page
	}
	page := 1
	raw := query.Get("page")
	if raw != "" {
		if raw == "last" {
			page = numPages
		} else {
			n, err := strconv.Atoi(raw)
			if err != nil || n < 1 || n > numPages {
				return nil, &DocumentError{Code: "not_found", Status: 404, Message: "Invalid page."}
			}
			page = n
		}
	}
	resp := pagedResponse{Count: total, Results: items}
	if page < numPages {
		resp.Next = pageLink(r, page+1)
	}
	if page > 1 {
		if page == 2 {
			resp.Previous = pageLink(r, 0) // 0 = remove the param (page 1 default)
		} else {
			resp.Previous = pageLink(r, page-1)
		}
	}
	return resp, nil
}

// pageLink builds http://<host><path>?<sorted query> with page set/removed.
func pageLink(r *http.Request, page int) *string {
	q := r.URL.Query()
	if page == 0 {
		q.Del("page")
	} else {
		q.Set("page", strconv.Itoa(page))
	}
	// Alphabetize like rest_framework.utils.urls.replace_query_param
	// (sorted(query_dict.items())).
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		for _, v := range q[k] {
			parts = append(parts, url.QueryEscape(k)+"="+url.QueryEscape(v))
		}
	}
	link := "http://" + r.Host + r.URL.Path
	if len(parts) > 0 {
		link += "?" + strings.Join(parts, "&")
	}
	return &link
}

// ---- ordering (assets/ordering.py: whitelist + pk tie-break) ----

var designOrdering = map[string]bool{"updated_at": true, "created_at": true, "title": true}

// presetCode mirrors DesignCreateSerializer's RegexField pattern.
var presetCode = regexp.MustCompile(`^[A-Za-z0-9._-]*$`)

// applyOrdering validates ?ordering= and returns the ORDER BY tail.
func applyOrdering(param string) (string, error) {
	key := strings.TrimSpace(param)
	if key == "" {
		key = "-updated_at"
	}
	desc := strings.HasPrefix(key, "-")
	col := strings.TrimPrefix(key, "-")
	if !designOrdering[col] {
		choices := []string{"created_at", "title", "updated_at"}
		return "", &FieldErrors{Fields: []FieldError{SimpleField("ordering",
			fmt.Sprintf("Unsupported ordering. Use one of: %s (prefix - for descending).",
				strings.Join(choices, ", ")), "invalid")}}
	}
	direction := "ASC"
	if desc {
		direction = "DESC"
	}
	// Ties always break on pk ASC (order_by(expr, "pk")).
	return col + " " + direction + ", id ASC", nil
}

// FieldErrors is a validation error carrying ordered fields (helper).
type FieldErrors struct{ Fields []FieldError }

func (e *FieldErrors) Error() string { return "validation_error" }

// ---- query parsing (DesignQuerySerializer) ----

type designQuery struct {
	Q         string
	Folder    string
	Starred   *bool
	Trashed   bool
	Ordering  string
	hasFolder bool
}

func (a *App) parseDesignQuery(r *http.Request) (designQuery, *FieldErrors) {
	q := designQuery{}
	query := r.URL.Query()
	var errs []FieldError

	if v := query.Get("q"); v != "" {
		if len([]rune(v)) > 200 {
			errs = append(errs, SimpleField("q", "Ensure this field has no more than 200 characters.", "max_length"))
		}
		q.Q = v
	}
	if v, ok := query["folder"]; ok {
		q.hasFolder = true
		val := v[0]
		q.Folder = val
		if val != "root" {
			if !uuidShape.MatchString(strings.ToLower(val)) || val == "" {
				errs = append(errs, SimpleField("folder", "Must be a valid UUID.", "invalid"))
			}
		}
	}
	if v, ok := query["starred"]; ok {
		b, err := parseDRFBool(v[0])
		if err != nil {
			errs = append(errs, SimpleField("starred",
				fmt.Sprintf("%q is not a valid boolean.", v[0]), "invalid"))
		} else {
			q.Starred = &b
		}
	}
	if v, ok := query["trashed"]; ok {
		b, err := parseDRFBool(v[0])
		if err != nil {
			errs = append(errs, SimpleField("trashed",
				fmt.Sprintf("%q is not a valid boolean.", v[0]), "invalid"))
		} else {
			q.Trashed = b
		}
	}
	if v := query.Get("ordering"); v != "" {
		if len([]rune(v)) > 32 {
			errs = append(errs, SimpleField("ordering", "Ensure this field has no more than 32 characters.", "max_length"))
		}
		q.Ordering = v
	}
	if len(errs) > 0 {
		return q, &FieldErrors{Fields: errs}
	}
	return q, nil
}

// parseDRFBool mirrors DRF's BooleanField truth set.
func parseDRFBool(v string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "true", "t", "1", "yes", "y", "on":
		return true, nil
	case "false", "f", "0", "no", "n", "off":
		return false, nil
	}
	return false, fmt.Errorf("invalid boolean")
}

// ---- views ----

type createDesignBody struct {
	Title          *string         `json:"title"`
	Width          *int64          `json:"width"`
	Height         *int64          `json:"height"`
	Preset         *string         `json:"preset"`
	Document       json.RawMessage `json:"document"` // non-nil only when the key is present
	Folder         *string         `json:"folder"`
	Template       *string         `json:"template"`
	documentParsed any
}

// designsList ports DesignViewSet.list.
func (a *App) designsList(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	q, ferr := a.parseDesignQuery(r)
	if ferr != nil {
		writeDRFValidation(w, ferr.Fields)
		return
	}
	where := []string{"owner_id = ?", "trashed_at IS " + map[bool]string{true: "NOT NULL", false: "NULL"}[q.Trashed]}
	args := []any{u.ID}
	if strings.TrimSpace(q.Q) != "" {
		where = append(where, "title LIKE ? ESCAPE '\\'")
		args = append(args, "%"+escapeLike(strings.TrimSpace(q.Q))+"%")
	}
	if q.Starred != nil {
		where = append(where, "starred = ?")
		args = append(args, *q.Starred)
	}
	if q.hasFolder {
		if q.Folder == "root" {
			where = append(where, "folder_id IS NULL")
		} else {
			where = append(where, "folder_id = ?")
			args = append(args, hexUUID(q.Folder))
		}
	}
	order, ferr2 := applyOrdering(q.Ordering)
	if ferr2 != nil {
		writeDRFValidation(w, ferr2.(*FieldErrors).Fields)
		return
	}
	// title ordering: SQL LOWER(title) matches Django's Lower(title).
	if strings.HasPrefix(strings.TrimPrefix(q.Ordering, "-"), "title") {
		order = strings.Replace(order, "title ", "LOWER(title) ", 1)
	}
	rows, err := a.DB.Query(fmt.Sprintf(
		"SELECT %s FROM designs_design WHERE %s ORDER BY %s", designCols,
		strings.Join(where, " AND "), order), args...)
	if err != nil {
		writeDRF(w, http.StatusInternalServerError, "server_error", "Something went wrong on our side.")
		return
	}
	defer rows.Close()
	var list []designRow
	for rows.Next() {
		d, err := scanDesign(rows)
		if err != nil {
			writeDRF(w, http.StatusInternalServerError, "server_error", "Something went wrong on our side.")
			return
		}
		list = append(list, d)
	}
	// Summaries (document deferred — never in list responses).
	summaries := make([]designSummary, 0, len(list))
	for i := range list {
		summaries = append(summaries, list[i].summary())
	}
	// Slice for the requested page AFTER counting (count = filtered total).
	page, ferr3 := pageSlice(r, len(summaries))
	if ferr3 != nil {
		writeDRF(w, ferr3.Status, ferr3.Code, ferr3.Message)
		return
	}
	sliced := summaries[page.from:page.to]
	resp, err := paginate(r, len(summaries), sliced)
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

type pageBounds struct{ from, to int }

// pageSlice resolves the requested window (mirrors paginate's math).
func pageSlice(r *http.Request, total int) (pageBounds, *DocumentError) {
	return windowSlice(r, total, designPageSize)
}

func windowSlice(r *http.Request, total, defaultSize int) (pageBounds, *DocumentError) {
	query := r.URL.Query()
	pageSize := defaultSize
	if raw := query.Get("page_size"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			pageSize = n
			if pageSize > maxPageSize {
				pageSize = maxPageSize
			}
		}
	}
	numPages := (total + pageSize - 1) / pageSize
	if numPages == 0 {
		numPages = 1
	}
	page := 1
	raw := query.Get("page")
	if raw != "" {
		if raw == "last" {
			page = numPages
		} else {
			n, err := strconv.Atoi(raw)
			if err != nil || n < 1 || n > numPages {
				return pageBounds{}, &DocumentError{Code: "not_found", Status: 404, Message: "Invalid page."}
			}
			page = n
		}
	}
	from := (page - 1) * pageSize
	if from > total {
		from = total
	}
	to := from + pageSize
	if to > total {
		to = total
	}
	return pageBounds{from, to}, nil
}

// designsCreate ports DesignViewSet.create (non-template path; template
// creation lands with the gallery family — see PORT_MAP pending note).
func (a *App) designsCreate(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	var body createDesignBody
	if err := jsonBody(r, &body); err != nil {
		writeDRF(w, http.StatusBadRequest, "parse_error", "Malformed request body.")
		return
	}
	fields, docErr := a.validateDesignCreate(&body, u.ID)
	if docErr != nil {
		writeDRF(w, docErr.Status, docErr.Code, docErr.Message)
		return
	}
	if len(fields) > 0 {
		writeDRFValidation(w, fields)
		return
	}
	var document any
	var documentRaw []byte
	if body.Document != nil {
		doc, err := parseJSONValue(body.Document)
		if err != nil {
			writeDRF(w, http.StatusBadRequest, "parse_error", "Malformed request body.")
			return
		}
		document = doc
		// Storage keeps the submitted key order (Python's json.loads ->
		// dict preserves it; mustMarshal of a Go map would not).
		documentRaw = compactStoredJSON(body.Document)
	} else {
		document, documentRaw = blankDocument(*body.Width, *body.Height, deref(body.Preset))
	}
	design, err := a.createDesign(u, derefTitle(body.Title, defaultDesignTitle), document, documentRaw, body.Folder)
	if err != nil {
		var de *DocumentError
		if asDocErr(err, &de) {
			writeDRF(w, de.Status, de.Code, de.Message)
			return
		}
		if lr, ok := err.(*LimitReachedError); ok {
			writeDRF(w, http.StatusConflict, "limit_reached", lr.Error())
			return
		}
		logerr(err, "designsCreate")
		writeDRF(w, http.StatusInternalServerError, "server_error", "Something went wrong on our side.")
		return
	}
	writeJSON(w, http.StatusCreated, design.full())
}

func asDocErr(err error, target **DocumentError) bool {
	if de, ok := err.(*DocumentError); ok {
		*target = de
		return true
	}
	return false
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func derefTitle(p *string, def string) string {
	if p == nil {
		return def
	}
	return *p
}

// validateDesignCreate mirrors DesignCreateSerializer: TitleField trim ->
// blank/clean/length, IntegerField bounds, preset regex, DocumentField
// (document errors surface as ApiError envelopes, not field errors —
// probe0004 steps 07/08), template/document mutual exclusion, owned-folder
// lookup ("Invalid pk ..." for another user's or unknown folder).
// Returns (fields, documentError).
func (a *App) validateDesignCreate(body *createDesignBody, owner string) ([]FieldError, *DocumentError) {
	var fields []FieldError

	if body.Title != nil {
		if f, out := cleanTitleField(*body.Title); f != nil {
			fields = append(fields, *f)
		} else {
			body.Title = &out
		}
	}
	bound := func(name string, v *int64) {
		if v == nil {
			return
		}
		if *v < 1 {
			fields = append(fields, SimpleField(name,
				"Ensure this value is greater than or equal to 1.", "min_value"))
		} else if *v > maxDimension {
			fields = append(fields, SimpleField(name,
				fmt.Sprintf("Ensure this value is less than or equal to %d.", maxDimension), "max_value"))
		}
	}
	bound("width", body.Width)
	bound("height", body.Height)
	if body.Preset != nil && (!presetCode.MatchString(*body.Preset) || len([]rune(*body.Preset)) > maxPresetLength) {
		fields = append(fields, SimpleField("preset",
			fmt.Sprintf("Enter a value matching the pattern ^[A-Za-z0-9._-]*$ (limit %d characters).", maxPresetLength),
			"invalid"))
	}
	if body.Document != nil {
		doc, err := parseJSONValue(body.Document)
		if err != nil {
			return append(fields, SimpleField("document", "Not a valid JSON object.", "invalid")), nil
		}
		if verr := validateDocument(doc, maxDesignDocumentBytes); verr != nil {
			return fields, verr // ApiError envelope at the caller
		}
		body.documentParsed = doc
	}
	if body.Template != nil && body.Document != nil {
		fields = append(fields, SimpleField("template",
			"Send either `document` or `template`, not both.", "invalid"))
	}
	if body.Folder != nil && *body.Folder != "" {
		if !uuidShape.MatchString(strings.ToLower(*body.Folder)) {
			fields = append(fields, SimpleField("folder", uuidInvalidMsg(*body.Folder), "invalid"))
		} else {
			found, _ := a.getFolderByOwner(owner, hexUUID(*body.Folder))
			if found == nil {
				fields = append(fields, SimpleField("folder",
					fmt.Sprintf("Invalid pk \"%s\" - object does not exist.", *body.Folder), "does_not_exist"))
			}
		}
	}
	if len(fields) == 0 {
		if body.Width == nil {
			w := int64(defaultDesignSize)
			body.Width = &w
		}
		if body.Height == nil {
			h := int64(defaultDesignSize)
			body.Height = &h
		}
	}
	return fields, nil
}

// cleanTitleField ports TitleField: trim (CharField) -> blank error,
// clean_text -> empty -> length.
func cleanTitleField(raw string) (*FieldError, string) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		f := SimpleField("title", "This field may not be blank.", "blank")
		return &f, raw
	}
	cleaned := cleanText(raw)
	if cleaned == "" {
		f := SimpleField("title", "The title cannot be empty.", "invalid")
		return &f, raw
	}
	if len([]rune(cleaned)) > 200 {
		f := SimpleField("title", "Ensure this field has no more than 200 characters.", "max_length")
		return &f, raw
	}
	return nil, cleaned
}
