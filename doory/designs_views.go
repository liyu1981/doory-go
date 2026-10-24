// Remaining design views and services: retrieve, optimistic-concurrency
// PATCH (409 conflict body with `current`), trash lifecycle, duplicate,
// trash-empty, and the version endpoints — plus shared helpers
// (writeJSON, parseJSONValue, blankDocument, escapeLike).
package doory

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// writeJSON renders a payload at status with the JSON content type.
func writeJSON(w http.ResponseWriter, status int, payload any) {
	b := encodeCompactJSON(payload)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", itoa(len(b)))
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

// parseJSONValue decodes keeping number literals (UseNumber), like Python's
// json.loads for later re-dump.
func parseJSONValue(raw []byte) (any, error) {
	dec := json.NewDecoder(bytesReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	return v, nil
}

func bytesReader(b []byte) *strings.Reader { return strings.NewReader(string(b)) }

// escapeLike escapes LIKE wildcards for the ESCAPE '\' clause (Django's
// icontains does not escape — mirrored: q values with % act as wildcards on
// both sides; only backslash itself is escaped so ESCAPE stays consistent).
func escapeLike(q string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(q)
}

// blankDocument ports services.blank_document as an ORDERED struct: JSON key
// order is the probe-proven upstream order (schemaVersion, width, height,
// unit, pages[...] then meta) — Go maps would randomize it (probe0004
// step03 response document bytes).
type blankDoc struct {
	SchemaVersion json.Number `json:"schemaVersion"`
	Width         json.Number `json:"width"`
	Height        json.Number `json:"height"`
	Unit          string      `json:"unit"`
	Pages         []blankPage `json:"pages"`
	Meta          *blankMeta  `json:"meta,omitempty"`
}

type blankPage struct {
	ID         string          `json:"id"`
	Background blankBackground `json:"background"`
	Objects    []any           `json:"objects"`
}

type blankBackground struct {
	Type  string `json:"type"`
	Color string `json:"color"`
}

type blankMeta struct {
	PresetID string `json:"presetId"`
}

func blankDocument(width, height int64, preset string) (parsed any, raw []byte) {
	doc := blankDoc{
		SchemaVersion: json.Number("1"),
		Width:         json.Number(itoa(int(width))),
		Height:        json.Number(itoa(int(height))),
		Unit:          "px",
		Pages: []blankPage{{
			ID:         newUUID4HexDashed(),
			Background: blankBackground{Type: "solid", Color: "#ffffff"},
			Objects:    []any{},
		}},
	}
	if preset != "" {
		doc.Meta = &blankMeta{PresetID: preset}
	}
	// Django's default_document() is a dict stored through json.dumps: the
	// column text carries Python's ", "/": " separators (the oracle's seeded
	// and created rows show it; the API re-canonicalizes on read).
	raw = pythonStoredJSON(encodeCompactJSON(doc))
	parsed, _ = parseJSONValue(raw)
	return parsed, raw
}

// newUUID4HexDashed returns the canonical uuid string (blank page ids are
// dashed — probe0004 create body: "id":"c08874a9-ec1b-..." in document).
func newUUID4HexDashed() string { return dashedUUID(newUUID4Hex()) }

// createDesign ports services.create_design (limit guard + document facts).
func (a *App) createDesign(owner *UserRow, title string, document any, documentRaw []byte, folder *string) (*designRow, error) {
	info, err := describeDocument(document)
	if err != nil {
		return nil, err
	}
	var count int
	if err := a.DB.QueryRow(`SELECT COUNT(*) FROM designs_design WHERE owner_id = ?`, owner.ID).Scan(&count); err != nil {
		return nil, err
	}
	if count >= a.Settings.MaxDesignsPerUser {
		return nil, &LimitReachedError{Max: a.Settings.MaxDesignsPerUser, Noun: "designs"}
	}
	id := newUUID4Hex()
	now := formatDjangoDateTime(time.Now())
	docText := documentRaw
	var folderArg any
	if folder != nil && *folder != "" {
		folderArg = hexUUID(*folder) // storage form (32 hex), like Django
	}
	_, err = a.DB.Exec(
		`INSERT INTO designs_design (`+designCols+`)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, '', ?, FALSE, 1, ?, ?, NULL)`,
		id, owner.ID, title, info.Width, info.Height, info.Preset, string(docText),
		info.PageCount, folderArg, now, now)
	if err != nil {
		return nil, err
	}
	return a.getDesignByID(id)
}

func mustMarshal(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

func (a *App) getDesignByID(id string) (*designRow, error) {
	row := a.DB.QueryRow("SELECT "+designCols+" FROM designs_design WHERE id = ?", id)
	d, err := scanDesign(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return &d, err
}

// getDesignFor is get_object_or_404 against the owner, writing the 404
// envelope when missing.
func (a *App) getDesignFor(w http.ResponseWriter, r *http.Request, u *UserRow) (*designRow, bool) {
	id := hexUUID(routeParam(r, "uuid"))
	d, err := a.getDesignByID(strings.ToLower(id))
	if err != nil {
		writeDRF(w, http.StatusInternalServerError, "server_error", "Something went wrong on our side.")
		return nil, false
	}
	if d == nil || d.OwnerID != u.ID {
		writeDRF(w, http.StatusNotFound, "not_found", "Not found.")
		return nil, false
	}
	return d, true
}

type updateDesignBody struct {
	Title        *string         `json:"title"`
	Document     json.RawMessage `json:"document"`
	BaseRevision *int64          `json:"base_revision"`
	Folder       json.RawMessage `json:"folder"` // absent = untouched, "null" = clear
	Starred      *bool           `json:"starred"`
}

// folderValue distinguishes absent / null / uuid for the update path.
func (b *updateDesignBody) folderValue() (present bool, clear bool, id string) {
	if b.Folder == nil {
		return false, false, ""
	}
	if string(b.Folder) == "null" {
		return true, true, ""
	}
	var s string
	if err := json.Unmarshal(b.Folder, &s); err == nil {
		return true, false, s
	}
	return true, false, ""
}

// designsPartialUpdate ports DesignViewSet.partial_update: title/starred/
// folder saves; document saves need base_revision == stored revision else
// the custom 409 body (probe0004 step13). A trashed design is a 409
// design_trashed (services._lock).
func (a *App) designsPartialUpdate(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	d, ok := a.getDesignFor(w, r, u)
	if !ok {
		return
	}
	var body updateDesignBody
	if err := jsonBody(r, &body); err != nil {
		writeDRF(w, http.StatusBadRequest, "parse_error", "Malformed request body.")
		return
	}
	var fields []FieldError
	if body.Title != nil {
		if f, out := cleanTitleField(*body.Title); f != nil {
			fields = append(fields, *f)
		} else {
			body.Title = &out
		}
	}
	var parsedDoc any
	hasDoc := body.Document != nil
	if hasDoc {
		if body.BaseRevision == nil {
			fields = append(fields, SimpleField("base_revision",
				"Required when saving a document.", "required"))
		}
		doc, err := parseJSONValue(body.Document)
		if err != nil {
			fields = append(fields, SimpleField("document", "Not a valid JSON object.", "invalid"))
		} else if verr := validateDocument(doc, maxDesignDocumentBytes); verr != nil {
			writeDRF(w, verr.Status, verr.Code, verr.Message)
			return
		} else {
			parsedDoc = doc
		}
	}
	if present, _, folderID := body.folderValue(); present && folderID != "" {
		if !uuidShape.MatchString(strings.ToLower(folderID)) {
			fields = append(fields, SimpleField("folder", uuidInvalidMsg(folderID), "invalid"))
		} else if f, _ := a.getFolderByOwner(u.ID, hexUUID(folderID)); f == nil {
			fields = append(fields, SimpleField("folder",
				fmt.Sprintf("Invalid pk \"%s\" - object does not exist.", folderID), "does_not_exist"))
		}
	}
	if len(fields) > 0 {
		writeDRFValidation(w, fields)
		return
	}
	if d.TrashedAt.Valid {
		// services._lock raises DesignTrashed (409, default detail).
		writeDRF(w, http.StatusConflict, "design_trashed",
			"This design is in the trash. Restore it before changing it.")
		return
	}

	now := formatDjangoDateTime(time.Now())
	if hasDoc && d.Revision != *body.BaseRevision {
		// RevisionConflict -> conflict_response body (probe0004 step13):
		// envelope + `current`, NOT a plain ApiError.
		writeConflict(w, d.Revision, d.UpdatedAt,
			"This design was changed elsewhere. Reload it or save your work as a copy.")
		return
	}
	updates := []string{}
	args := []any{}
	if body.Title != nil {
		updates = append(updates, "title = ?")
		args = append(args, *body.Title)
	}
	if body.Starred != nil {
		updates = append(updates, "starred = ?")
		args = append(args, *body.Starred)
	}
	if present, clear, folderID := body.folderValue(); present {
		updates = append(updates, "folder_id = ?")
		if clear {
			args = append(args, nil)
		} else {
			args = append(args, hexUUID(folderID))
		}
	}
	if hasDoc {
		info, _ := describeDocument(parsedDoc)
		updates = append(updates, "document = ?", "width = ?", "height = ?",
			"page_count = ?", "preset = ?", "revision = ?")
		args = append(args, string(pythonStoredJSON(body.Document)),
			info.Width, info.Height, info.PageCount, info.Preset, d.Revision+1)
		// _maybe_record_auto_version: first document save snapshots
		// (latest is None); later saves within 10 minutes skip.
		a.maybeRecordAutoVersion(d, string(pythonStoredJSON(body.Document)), u)
	}
	if len(updates) == 0 && !hasDoc {
		// Nothing touched: Django still wouldn't save — but validated_data
		// empty PATCH returns the design unchanged (save only when touched).
		writeJSON(w, http.StatusOK, d.full())
		return
	}
	updates = append(updates, "updated_at = ?")
	args = append(args, now)
	args = append(args, d.ID, u.ID)
	_, err := a.DB.Exec(fmt.Sprintf(
		"UPDATE designs_design SET %s WHERE id = ? AND owner_id = ?",
		strings.Join(updates, ", ")), args...)
	if err != nil {
		writeDRF(w, http.StatusInternalServerError, "server_error", "Something went wrong on our side.")
		return
	}
	updated, err := a.getDesignByID(d.ID)
	if err != nil {
		writeDRF(w, http.StatusInternalServerError, "server_error", "Something went wrong on our side.")
		return
	}
	// signals.design_saved: a title change tells the room (and its members)
	// the new title; a document/trash change makes it re-check.
	if body.Title != nil && updated.Title != d.Title {
		a.notifyDesignTitle(updated.ID, updated.Title)
	}
	if hasDoc {
		a.notifyDesignRecheck(updated.ID)
	}
	writeJSON(w, http.StatusOK, updated.full())
}

// writeConflict ports conflict_response (409 + current{revision,updated_at});
// ordered structs keep key order (probe0004 step13 bytes).
type conflictBody struct {
	Error   conflictError   `json:"error"`
	Current conflictCurrent `json:"current"`
}

type conflictError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type conflictCurrent struct {
	Revision  int64  `json:"revision"`
	UpdatedAt string `json:"updated_at"`
}

func writeConflict(w http.ResponseWriter, revision int64, updatedAt string, message string) {
	writeJSON(w, http.StatusConflict, conflictBody{
		Error: conflictError{
			Code:    "revision_conflict",
			Message: message,
		},
		Current: conflictCurrent{Revision: revision, UpdatedAt: renderDRFDatetime(updatedAt)},
	})
}

// maybeRecordAutoVersion ports services._maybe_record_auto_version.
func (a *App) maybeRecordAutoVersion(d *designRow, docRaw string, u *UserRow) {
	var latest sql.NullString
	_ = a.DB.QueryRow(
		`SELECT created_at FROM designs_designversion WHERE design_id = ?
		 ORDER BY created_at DESC LIMIT 1`, d.ID).Scan(&latest)
	if latest.Valid {
		if t, err := parseDjangoDateTime(latest.String); err == nil &&
			time.Since(t) < autoVersionGap {
			return
		}
	}
	pageCount := int64(0)
	if parsed, err := parseJSONValue([]byte(docRaw)); err == nil {
		if m, ok := parsed.(map[string]any); ok {
			if pages, ok := m["pages"].([]any); ok {
				pageCount = int64(len(pages))
			}
		}
	}
	a.recordVersion(d, docRaw, pageCount, u, true, "")
	a.pruneAutoVersions(d.ID)
}

// recordVersion ports services._record_version.
func (a *App) recordVersion(d *designRow, docText string, pageCount int64, u *UserRow, isAuto bool, label string) error {
	id := newUUID4Hex()
	now := formatDjangoDateTime(time.Now())
	_, err := a.DB.Exec(
		`INSERT INTO designs_designversion
		 (id, created_at, updated_at, label, is_auto, document, page_count, thumbnail, design_id, created_by_id)
		 VALUES (?, ?, ?, ?, ?, ?, ?, '', ?, ?)`,
		id, now, now, label, isAuto, docText, pageCount, d.ID, u.ID)
	return err
}

// pruneAutoVersions keeps the newest maxAutoVersions automatic versions
// (manual ones are never pruned).
func (a *App) pruneAutoVersions(designID string) {
	rows, err := a.DB.Query(
		`SELECT id FROM designs_designversion WHERE design_id = ? AND is_auto = TRUE
		 ORDER BY created_at DESC, id DESC`, designID)
	if err != nil {
		return
	}
	var ids []string
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	if len(ids) <= maxAutoVersions {
		return
	}
	stale := ids[maxAutoVersions:]
	for _, id := range stale {
		_, _ = a.DB.Exec(`DELETE FROM designs_designversion WHERE id = ?`, id)
	}
}

// designsDestroy ports DesignViewSet.destroy: ?permanent=1 hard-deletes
// (only from the trash), otherwise trashes.
func (a *App) designsDestroy(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	d, ok := a.getDesignFor(w, r, u)
	if !ok {
		return
	}
	permanent := strings.ToLower(r.URL.Query().Get("permanent"))
	if permanent == "1" || permanent == "true" || permanent == "yes" {
		if !d.TrashedAt.Valid {
			// services.delete_permanently raises NotInTrash (400 default).
			writeDRF(w, http.StatusBadRequest, "not_in_trash",
				"Only designs in the trash can be deleted permanently.")
			return
		}
		if _, err := a.purgeDesigns([]string{d.ID}); err != nil {
			writeDRF(w, http.StatusInternalServerError, "server_error", "Something went wrong on our side.")
			return
		}
		a.notifyDesignRecheck(d.ID) // signals.design_deleted
		w.Header().Set("Content-Length", "0")
		w.WriteHeader(http.StatusNoContent)
		return
	}
	// trash_design: only live rows update (already-trashed = no-op 204).
	_, err := a.DB.Exec(
		`UPDATE designs_design SET trashed_at = ?, updated_at = ?
		 WHERE id = ? AND owner_id = ? AND trashed_at IS NULL`,
		formatDjangoDateTime(time.Now()), formatDjangoDateTime(time.Now()), d.ID, u.ID)
	if err != nil {
		writeDRF(w, http.StatusInternalServerError, "server_error", "Something went wrong on our side.")
		return
	}
	if !d.TrashedAt.Valid {
		a.notifyDesignRecheck(d.ID) // signals.design_saved on trashed_at
	}
	w.Header().Set("Content-Length", "0")
	w.WriteHeader(http.StatusNoContent)
}

// designsRestore ports DesignViewSet.restore (untrash).
func (a *App) designsRestore(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	d, ok := a.getDesignFor(w, r, u)
	if !ok {
		return
	}
	if d.TrashedAt.Valid {
		if _, err := a.DB.Exec(
			`UPDATE designs_design SET trashed_at = NULL, updated_at = ? WHERE id = ?`,
			formatDjangoDateTime(time.Now()), d.ID); err != nil {
			writeDRF(w, http.StatusInternalServerError, "server_error", "Something went wrong on our side.")
			return
		}
		d, _ = a.getDesignByID(d.ID)
		a.notifyDesignRecheck(d.ID) // signals.design_saved on trashed_at
	}
	writeJSON(w, http.StatusOK, d.full())
}

// designsDuplicate ports DesignViewSet.duplicate ("Copy of ...", revision 1,
// unstarred, same folder; a trashed design refuses with 409).
func (a *App) designsDuplicate(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	d, ok := a.getDesignFor(w, r, u)
	if !ok {
		return
	}
	if d.TrashedAt.Valid {
		writeDRF(w, http.StatusConflict, "design_trashed",
			"This design is in the trash. Restore it before changing it.")
		return
	}
	title := "Copy of " + d.Title
	if runes := []rune(title); len(runes) > 200 {
		title = string(runes[:200])
	}
	var count int
	_ = a.DB.QueryRow(`SELECT COUNT(*) FROM designs_design WHERE owner_id = ?`, u.ID).Scan(&count)
	if count >= a.Settings.MaxDesignsPerUser {
		writeDRF(w, http.StatusConflict, "limit_reached",
			fmt.Sprintf("You can keep at most %d designs.", a.Settings.MaxDesignsPerUser))
		return
	}
	id := newUUID4Hex()
	now := formatDjangoDateTime(time.Now())
	var folderArg any
	if d.FolderID.Valid {
		folderArg = d.FolderID.String
	}
	_, err := a.copyDesignRow(u, d, title, folderArg, id, now)
	if err != nil {

		writeDRF(w, http.StatusInternalServerError, "server_error", "Something went wrong on our side.")
		return
	}
	out, err := a.getDesignByID(id)
	if err != nil {
		writeDRF(w, http.StatusInternalServerError, "server_error", "Something went wrong on our side.")
		return
	}
	writeJSON(w, http.StatusCreated, out.full())
}

func (a *App) copyDesignRow(u *UserRow, src *designRow, title string, folder any, id, now string) (*designRow, error) {
	_, err := a.DB.Exec(
		`INSERT INTO designs_design
		 (id, owner_id, title, width, height, preset, document, page_count, thumbnail,
		  folder_id, starred, revision, created_at, updated_at, trashed_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, '', ?, FALSE, 1, ?, ?, NULL)`,
		id, u.ID, title, src.Width, src.Height, src.Preset, src.Document, src.PageCount,
		folder, now, now)
	if err != nil {
		return nil, err
	}
	return a.getDesignByID(id)
}

// designsTrashEmpty ports DesignViewSet.trash (DELETE /designs/trash/):
// hard-delete every trashed design of the caller.
func (a *App) designsTrashEmpty(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	rows, err := a.DB.Query(
		`SELECT id FROM designs_design WHERE owner_id = ? AND trashed_at IS NOT NULL`, u.ID)
	if err != nil {
		writeDRF(w, http.StatusInternalServerError, "server_error", "Something went wrong on our side.")
		return
	}
	var ids []string
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	if _, err := a.purgeDesigns(ids); err != nil {
		writeDRF(w, http.StatusInternalServerError, "server_error", "Something went wrong on our side.")
		return
	}
	a.notifyDesignsRecheck(ids) // signals.design_deleted
	w.Header().Set("Content-Length", "0")
	w.WriteHeader(http.StatusNoContent)
}

// designsRoot switches list/create by method (router MethodNotAllowed
// already rejects others).
func (a *App) designsRoot(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		a.designsCreate(w, r)
		return
	}
	a.designsList(w, r)
}

// designsDetail switches retrieve/partial_update/destroy.
func (a *App) designsDetail(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		a.designsRetrieve(w, r)
	case http.MethodPatch:
		a.designsPartialUpdate(w, r)
	case http.MethodDelete:
		a.designsDestroy(w, r)
	}
}

// foldersRoot switches list/create.
func (a *App) foldersRoot(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		a.foldersCreate(w, r)
		return
	}
	a.foldersList(w, r)
}

// foldersDetail switches retrieve/partial_update/destroy.
func (a *App) foldersDetail(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		a.foldersRetrieve(w, r)
	case http.MethodPatch:
		a.foldersPartialUpdate(w, r)
	case http.MethodDelete:
		a.foldersDestroy(w, r)
	}
}

// versionsRoot switches version list/create.
func (a *App) versionsRoot(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		a.versionsCreate(w, r)
		return
	}
	a.versionsList(w, r)
}

// designsRetrieve ports DesignViewSet.retrieve (full document included).
func (a *App) designsRetrieve(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	d, ok := a.getDesignFor(w, r, u)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, d.full())
}
