// Version endpoints (apps/designs/views.py DesignVersion* + services
// version functions): list (paginated), create (manual label), detail
// (document), restore (snapshot-before + revision bump).
package doory

import (
	"database/sql"
	"net/http"
	"strings"
	"time"
)

type versionRow struct {
	ID        string
	DesignID  string
	Label     string
	IsAuto    bool
	Document  string
	PageCount int64
	Thumbnail string
	CreatedBy sql.NullString
	CreatedAt string
	UpdatedAt string
}

const versionCols = `id, design_id, label, is_auto, document, page_count, thumbnail,
	created_by_id, created_at, updated_at`

func scanVersion(s scanner) (versionRow, error) {
	var v versionRow
	err := s.Scan(&v.ID, &v.DesignID, &v.Label, &v.IsAuto, &v.Document, &v.PageCount,
		&v.Thumbnail, &v.CreatedBy, &v.CreatedAt, &v.UpdatedAt)
	return v, err
}

type versionSummary struct {
	ID           string  `json:"id"`
	Label        string  `json:"label"`
	IsAuto       bool    `json:"is_auto"`
	PageCount    int64   `json:"page_count"`
	ThumbnailURL *string `json:"thumbnail_url"`
	CreatedAt    string  `json:"created_at"`
}

type versionDetail struct {
	versionSummary
	Document jsonRaw `json:"document"`
}

// jsonRaw is json.RawMessage with the compact order-preserving render.
type jsonRaw = rawCompacted

type rawCompacted []byte

func (r rawCompacted) MarshalJSON() ([]byte, error) {
	if r == nil {
		return []byte("null"), nil
	}
	return r, nil
}

func (v *versionRow) summary() versionSummary {
	thumb := (*string)(nil)
	if v.Thumbnail != "" {
		s := mediaURL + v.Thumbnail
		thumb = &s
	}
	return versionSummary{
		ID: dashedUUID(v.ID), Label: v.Label, IsAuto: v.IsAuto, PageCount: v.PageCount,
		ThumbnailURL: thumb, CreatedAt: renderDRFDatetime(v.CreatedAt),
	}
}

// getOwnDesign resolves designs/<design_pk> for the owner (defer document
// for list; loaded for detail/restore).
func (a *App) getOwnDesign(w http.ResponseWriter, r *http.Request, u *UserRow, full bool) (*designRow, bool) {
	id := hexUUID(routeParam(r, "design_pk"))
	d, err := a.getDesignByID(id)
	if err != nil {
		writeDRF(w, http.StatusInternalServerError, "server_error", "Something went wrong on our side.")
		return nil, false
	}
	if d == nil || d.OwnerID != u.ID {
		writeDRF(w, http.StatusNotFound, "not_found", "Not found.")
		return nil, false
	}
	_ = full
	return d, true
}

// versionsList ports DesignVersionListView.get (paginated, -created_at,-pk).
func (a *App) versionsList(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	d, ok := a.getOwnDesign(w, r, u, false)
	if !ok {
		return
	}
	rows, err := a.DB.Query(
		`SELECT `+versionCols+` FROM designs_designversion WHERE design_id = ?
		 ORDER BY created_at DESC, id DESC`, d.ID)
	if err != nil {
		writeDRF(w, http.StatusInternalServerError, "server_error", "Something went wrong on our side.")
		return
	}
	defer rows.Close()
	var list []versionRow
	for rows.Next() {
		v, err := scanVersion(rows)
		if err != nil {
			writeDRF(w, http.StatusInternalServerError, "server_error", "Something went wrong on our side.")
			return
		}
		list = append(list, v)
	}
	summaries := make([]versionSummary, 0, len(list))
	for i := range list {
		summaries = append(summaries, list[i].summary())
	}
	page, ferr := pageSlice(r, len(summaries))
	if ferr != nil {
		writeDRF(w, ferr.Status, ferr.Code, ferr.Message)
		return
	}
	resp, err := paginate(r, len(summaries), summaries[page.from:page.to])
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

type versionCreateBody struct {
	Label *string `json:"label"`
}

// versionsCreate ports DesignVersionListView.post (manual version).
func (a *App) versionsCreate(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	d, ok := a.getOwnDesign(w, r, u, false)
	if !ok {
		return
	}
	var body versionCreateBody
	if err := jsonBody(r, &body); err != nil {
		writeDRF(w, http.StatusBadRequest, "parse_error", "Malformed request body.")
		return
	}
	label := "Saved version" // DEFAULT_VERSION_LABEL
	if body.Label != nil {
		trimmed := strings.TrimSpace(*body.Label)
		if trimmed == "" {
			// allow_blank -> validate_label(clean) -> empty -> default
			label = "Saved version"
		} else {
			cleaned := cleanText(*body.Label)
			if len([]rune(cleaned)) > 120 {
				writeDRFValidation(w, []FieldError{SimpleField("label",
					"Ensure this field has no more than 120 characters.", "max_length")})
				return
			}
			if cleaned != "" {
				label = cleaned
			}
		}
	}
	// services.create_manual_version: _lock (trashed -> 409), manual limit.
	if d.TrashedAt.Valid {
		writeDRF(w, http.StatusConflict, "design_trashed",
			"This design is in the trash. Restore it before changing it.")
		return
	}
	var manualCount int
	_ = a.DB.QueryRow(
		`SELECT COUNT(*) FROM designs_designversion WHERE design_id = ? AND is_auto = 0`,
		d.ID).Scan(&manualCount)
	if manualCount >= maxManualVersions {
		writeDRF(w, http.StatusConflict, "limit_reached",
			"A design can keep at most 100 saved versions.")
		return
	}
	if err := a.recordVersion(d, d.Document, d.PageCount, u, false, label); err != nil {
		writeDRF(w, http.StatusInternalServerError, "server_error", "Something went wrong on our side.")
		return
	}
	// Return the just-created row (ordered -created_at,-pk: newest first).
	var v versionRow
	err := a.DB.QueryRow(
		`SELECT `+versionCols+` FROM designs_designversion WHERE design_id = ?
		 ORDER BY created_at DESC, id DESC LIMIT 1`, d.ID).Scan(
		&v.ID, &v.DesignID, &v.Label, &v.IsAuto, &v.Document, &v.PageCount,
		&v.Thumbnail, &v.CreatedBy, &v.CreatedAt, &v.UpdatedAt)
	if err != nil {
		writeDRF(w, http.StatusInternalServerError, "server_error", "Something went wrong on our side.")
		return
	}
	writeJSON(w, http.StatusCreated, v.summary())
}

// mustParseStored parses stored document text (already validated when saved).
func mustParseStored(text string) any {
	v, err := parseJSONValue([]byte(text))
	if err != nil {
		return map[string]any{}
	}
	return v
}

// versionsDetail ports DesignVersionDetailView.get (document included).
func (a *App) versionsDetail(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	d, ok := a.getOwnDesign(w, r, u, false)
	if !ok {
		return
	}
	v, ok := a.getOwnVersion(w, r, d)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, versionDetail{
		versionSummary: v.summary(),
		Document:       rawCompacted(compactStoredJSON([]byte(v.Document))),
	})
}

func (a *App) getOwnVersion(w http.ResponseWriter, r *http.Request, d *designRow) (*versionRow, bool) {
	id := hexUUID(routeParam(r, "version_pk"))
	row := a.DB.QueryRow(
		`SELECT `+versionCols+` FROM designs_designversion WHERE id = ? AND design_id = ?`, id, d.ID)
	v, err := scanVersion(row)
	if err == sql.ErrNoRows {
		writeDRF(w, http.StatusNotFound, "not_found", "Not found.")
		return nil, false
	}
	if err != nil {
		writeDRF(w, http.StatusInternalServerError, "server_error", "Something went wrong on our side.")
		return nil, false
	}
	return &v, true
}

// versionsRestore ports DesignVersionRestoreView.post: validate the stored
// document, snapshot "Before restore", apply + revision bump, prune autos.
func (a *App) versionsRestore(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	d, ok := a.getOwnDesign(w, r, u, true)
	if !ok {
		return
	}
	v, ok := a.getOwnVersion(w, r, d)
	if !ok {
		return
	}
	doc := mustParseStored(v.Document)
	if verr := validateDocument(doc, maxDesignDocumentBytes); verr != nil {
		writeDRF(w, verr.Status, verr.Code, verr.Message)
		return
	}
	if d.TrashedAt.Valid {
		writeDRF(w, http.StatusConflict, "design_trashed",
			"This design is in the trash. Restore it before changing it.")
		return
	}
	// _record_version BEFORE the restore snapshots the design's CURRENT
	// document (services.restore_version order), label "Before restore".
	if err := a.recordVersion(d, d.Document, d.PageCount, u, true, "Before restore"); err != nil {
		writeDRF(w, http.StatusInternalServerError, "server_error", "Something went wrong on our side.")
		return
	}
	info, _ := describeDocument(doc)
	now := formatDjangoDateTime(time.Now())
	_, err := a.DB.Exec(
		`UPDATE designs_design SET document = ?, width = ?, height = ?, page_count = ?,
		 preset = ?, revision = ?, updated_at = ? WHERE id = ?`,
		string(pythonStoredJSON([]byte(v.Document))), info.Width, info.Height, info.PageCount,
		info.Preset, d.Revision+1, now, d.ID)
	if err != nil {
		writeDRF(w, http.StatusInternalServerError, "server_error", "Something went wrong on our side.")
		return
	}
	a.pruneAutoVersions(d.ID)
	updated, err := a.getDesignByID(d.ID)
	if err != nil {
		writeDRF(w, http.StatusInternalServerError, "server_error", "Something went wrong on our side.")
		return
	}
	writeJSON(w, http.StatusOK, updated.full())
}
