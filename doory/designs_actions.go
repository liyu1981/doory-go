// Design actions ported from views.py after the CRUD slice: template fields,
// bulk actions and the dashboard thumbnail upload (0.2.0 closure).
package doory

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	thumbnailMaxBytes  = 2 * 1024 * 1024 // services.THUMBNAIL_MAX_BYTES
	thumbnailMaxPixels = 16_000_000      // services.THUMBNAIL_MAX_PIXELS
	thumbnailSide      = 480             // services.THUMBNAIL_SIDE
	maxBulkIDs         = 200             // serializers.MAX_BULK_IDS
)

// designsFields ports DesignViewSet.fields: the template fields of the
// design's current document (tagged objects one entry per key, plus untagged
// photo placeholders).
func (a *App) designsFields(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	d, ok := a.getDesignFor(w, r, u)
	if !ok {
		return
	}
	document, err := a.readCurrentDocument(d)
	if err != nil {
		logerr(err, "designsFields.document")
		writeDRF(w, http.StatusInternalServerError, "server_error", "Something went wrong on our side.")
		return
	}
	writeJSON(w, http.StatusOK, listFields(document))
}

// readCurrentDocument ports edits.read_current: the design's stored document
// (the port keeps a single current document column, like the pinned pin's
// read path) decoded into a generic map.
func (a *App) readCurrentDocument(d *designRow) (map[string]any, error) {
	var document map[string]any
	if err := json.Unmarshal([]byte(d.Document), &document); err != nil {
		return nil, err
	}
	return document, nil
}

// bulkBody is BulkSerializer.
type bulkBody struct {
	IDs    json.RawMessage `json:"ids"`
	Action *string         `json:"action"`
	Folder json.RawMessage `json:"folder"` // absent = untouched, "null" = top level
}

// bulkActions mirrors services.BULK_ACTIONS (order matters for the choice
// message).
var bulkActions = []string{"trash", "restore", "delete", "star", "unstar", "move"}

// designsBulk ports DesignViewSet.bulk: run one action on the caller's own
// designs and report how many changed.
func (a *App) designsBulk(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	var body bulkBody
	if err := jsonBody(r, &body); err != nil {
		writeDRF(w, http.StatusBadRequest, "parse_error", "Malformed request body.")
		return
	}
	var fields []FieldError

	// ids: ListField(child=UUIDField(), min_length=1, max_length=200).
	var ids []string
	if body.IDs == nil {
		fields = append(fields, SimpleField("ids", "This field is required.", "required"))
	} else {
		var raw []any
		if err := json.Unmarshal(body.IDs, &raw); err != nil {
			fields = append(fields, SimpleField("ids", "Expected a list of items.", "not_a_list"))
		} else if len(raw) == 0 {
			fields = append(fields, SimpleField("ids", "This list may not be empty.", "empty"))
		} else {
			if len(raw) > maxBulkIDs {
				fields = append(fields, SimpleField("ids",
					fmt.Sprintf("Ensure this field has no more than %d elements.", maxBulkIDs), "max_length"))
			}
			for _, item := range raw {
				text, ok := item.(string)
				if !ok || !uuidShape.MatchString(strings.ToLower(text)) {
					fields = append(fields, SimpleField("ids",
						`"`+pyStr(item)+`" is not a valid UUID.`, "invalid"))
					continue
				}
				ids = append(ids, strings.ToLower(text))
			}
		}
	}

	action := ""
	if body.Action == nil {
		fields = append(fields, SimpleField("action", "This field is required.", "required"))
	} else {
		action = *body.Action
		if !choiceValid(bulkActions, action) {
			fields = append(fields, SimpleField("action",
				fmt.Sprintf("%q is not a valid choice.", action), "invalid_choice"))
		}
	}

	folderPresent := body.Folder != nil
	folderClear := false
	folderID := ""
	if folderPresent && string(body.Folder) != "null" {
		var raw any
		if err := json.Unmarshal(body.Folder, &raw); err != nil {
			fields = append(fields, SimpleField("folder", "Incorrect type. Expected a folder id.", "incorrect_type"))
		} else if text, ok := raw.(string); !ok || !uuidShape.MatchString(strings.ToLower(text)) {
			fields = append(fields, SimpleField("folder", "Incorrect type. Expected a folder id.", "incorrect_type"))
		} else {
			folderID = strings.ToLower(text)
		}
	} else if folderPresent {
		folderClear = true
	}

	if action == "move" && !folderPresent {
		// validate() raises a plain ValidationError: DRF gives it the
		// generic "invalid" code (not the field's "required").
		fields = append(fields, SimpleField("folder",
			"Required for the move action (null = top level).", "invalid"))
	}
	if action != "move" && folderPresent && len(fields) == 0 {
		// OwnedFolderField validates an id it was given even for other
		// actions; a folder that is not the caller's is a field error.
		if !folderClear {
			exists, err := a.ownFolder(folderID, u.ID)
			if err != nil {
				logerr(err, "designsBulk.folder")
				writeDRF(w, http.StatusInternalServerError, "server_error", "Something went wrong on our side.")
				return
			}
			if !exists {
				fields = append(fields, SimpleField("folder",
					`Invalid pk "`+folderID+`" - object does not exist.`, "does_not_exist"))
			}
		}
	} else if action == "move" && !folderClear && len(fields) == 0 {
		exists, err := a.ownFolder(folderID, u.ID)
		if err != nil {
			logerr(err, "designsBulk.folder")
			writeDRF(w, http.StatusInternalServerError, "server_error", "Something went wrong on our side.")
			return
		}
		if !exists {
			fields = append(fields, SimpleField("folder",
				`Invalid pk "`+folderID+`" - object does not exist.`, "does_not_exist"))
		}
	}
	if len(fields) > 0 {
		writeDRFValidation(w, fields)
		return
	}

	affected, err := a.bulkApply(u.ID, uniqueStrings(ids), action, folderID, folderClear)
	if err != nil {
		logerr(err, "designsBulk.apply")
		writeDRF(w, http.StatusInternalServerError, "server_error", "Something went wrong on our side.")
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Affected int64 `json:"affected"`
	}{Affected: affected})
}

// ownFolder reports whether `id` is one of the caller's folders.
func (a *App) ownFolder(id, owner string) (bool, error) {
	var count int
	err := a.DB.QueryRow(`SELECT COUNT(*) FROM designs_folder WHERE id = ? AND owner_id = ?`,
		id, owner).Scan(&count)
	return count > 0, err
}

// bulkApply ports services.bulk_apply.
func (a *App) bulkApply(owner string, ids []string, action, folderID string, clearFolder bool) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?, ", len(ids)), ", ")
	args := []any{owner}
	for _, id := range ids {
		// The API speaks dashed uuids; the rows store Django's hex form.
		args = append(args, hexUUID(id))
	}
	now := formatDjangoDateTime(time.Now().UTC())
	switch action {
	case "trash":
		res, err := a.DB.Exec(`UPDATE designs_design SET trashed_at = ?, updated_at = ?
			WHERE owner_id = ? AND id IN (`+placeholders+`) AND trashed_at IS NULL`, append([]any{now, now}, args...)...)
		if err != nil {
			return 0, err
		}
		return res.RowsAffected()
	case "restore":
		res, err := a.DB.Exec(`UPDATE designs_design SET trashed_at = NULL, updated_at = ?
			WHERE owner_id = ? AND id IN (`+placeholders+`) AND trashed_at IS NOT NULL`,
			append([]any{now}, args...)...)
		if err != nil {
			return 0, err
		}
		return res.RowsAffected()
	case "delete":
		// purge_designs of the trashed subset: collect the ids first (the
		// shared purge helper removes media/versions), then delete.
		rows, err := a.DB.Query(`SELECT id FROM designs_design
			WHERE owner_id = ? AND id IN (`+placeholders+`) AND trashed_at IS NOT NULL`, args...)
		if err != nil {
			return 0, err
		}
		var trashed []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return 0, err
			}
			trashed = append(trashed, id)
		}
		rows.Close()
		var affected int64
		for _, id := range trashed {
			if err := a.purgeDesign(id); err != nil {
				return affected, err
			}
			affected++
		}
		return affected, nil
	case "star", "unstar":
		starred := action == "star"
		res, err := a.DB.Exec(`UPDATE designs_design SET starred = ?, updated_at = ?
			WHERE owner_id = ? AND id IN (`+placeholders+`) AND trashed_at IS NULL`,
			append([]any{starred, now}, args...)...)
		if err != nil {
			return 0, err
		}
		return res.RowsAffected()
	case "move":
		var folder any
		if !clearFolder {
			folder = folderID
		}
		res, err := a.DB.Exec(`UPDATE designs_design SET folder_id = ?, updated_at = ?
			WHERE owner_id = ? AND id IN (`+placeholders+`) AND trashed_at IS NULL`,
			append([]any{folder, now}, args...)...)
		if err != nil {
			return 0, err
		}
		return res.RowsAffected()
	}
	return 0, nil
}

// purgeDesign removes one design and its stored files (services.purge_designs
// for a single row; the purge_trash command uses the same helper).
func (a *App) purgeDesign(id string) error {
	var thumbnail sql.NullString
	if err := a.DB.QueryRow(`SELECT thumbnail FROM designs_design WHERE id = ?`, id).Scan(&thumbnail); err != nil {
		if err == sql.ErrNoRows {
			return nil
		}
		return err
	}
	if _, err := a.DB.Exec(`DELETE FROM designs_design WHERE id = ?`, id); err != nil {
		return err
	}
	if thumbnail.Valid && thumbnail.String != "" {
		_ = os.Remove(filepath.Join(a.Settings.MediaRoot, thumbnail.String))
	}
	return nil
}

// designsThumbnail ports DesignViewSet.thumbnail (PUT multipart): decode the
// upload, store a 480 px WebP and answer its URL. A trashed design is a 409;
// the previous file is removed (after a successful write).
func (a *App) designsThumbnail(w http.ResponseWriter, r *http.Request) {
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
	if r.ContentLength > thumbnailMaxBytes+uploadEnvelope {
		writeDRF(w, http.StatusRequestEntityTooLarge, "file_too_large", "The thumbnail is larger than 2 MB.")
		return
	}
	if err := r.ParseMultipartForm(thumbnailMaxBytes); err != nil {
		writeDRF(w, http.StatusBadRequest, "parse_error", "Malformed request body.")
		return
	}
	defer func() {
		if r.MultipartForm != nil {
			_ = r.MultipartForm.RemoveAll()
		}
	}()
	files := r.MultipartForm.File["image"]
	if len(files) == 0 {
		writeDRFValidation(w, []FieldError{SimpleField("image",
			"Upload the thumbnail as the `image` file field.", "required")})
		return
	}
	handle, err := files[0].Open()
	if err != nil {
		logerr(err, "designsThumbnail.open")
		writeDRF(w, http.StatusInternalServerError, "server_error", "Something went wrong on our side.")
		return
	}
	data, err := io.ReadAll(io.LimitReader(handle, thumbnailMaxBytes+1))
	handle.Close()
	if err != nil || len(data) > thumbnailMaxBytes {
		writeDRF(w, http.StatusRequestEntityTooLarge, "file_too_large", "The thumbnail is larger than 2 MB.")
		return
	}
	width, height, _, cerr := decodeImageConfig(data)
	if cerr != nil {
		writeDRF(w, http.StatusBadRequest, "invalid_file",
			"Unsupported image type. Use PNG, JPEG, WebP, GIF or AVIF.")
		return
	}
	if width < 1 || height < 1 || int64(width)*int64(height) > thumbnailMaxPixels {
		writeDRF(w, http.StatusBadRequest, "invalid_file", "The image dimensions are too large.")
		return
	}
	img, _, derr := decodeImageData(data)
	if derr != nil {
		writeDRF(w, http.StatusBadRequest, "invalid_file", "The file is not a valid image.")
		return
	}
	webp, eerr := encodeWebP(fitThumbnail(img, thumbnailSide))
	if eerr != nil {
		logerr(eerr, "designsThumbnail.encode")
		writeDRF(w, http.StatusInternalServerError, "server_error", "Something went wrong on our side.")
		return
	}

	// upload_to=design_thumbnail_path: designs/<owner>/<design-id>-<10 hex>.webp
	name := "designs/" + dashedUUID(u.ID) + "/" + dashedUUID(d.ID) + "-" +
		strings.ToLower(newUUID4Hex())[:10] + ".webp"
	path := filepath.Join(a.Settings.MediaRoot, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		logerr(err, "designsThumbnail.mkdir")
		writeDRF(w, http.StatusInternalServerError, "server_error", "Something went wrong on our side.")
		return
	}
	if err := os.WriteFile(path, webp, 0o644); err != nil {
		logerr(err, "designsThumbnail.write")
		writeDRF(w, http.StatusInternalServerError, "server_error", "Something went wrong on our side.")
		return
	}
	previous := d.Thumbnail
	if _, err := a.DB.Exec(`UPDATE designs_design SET thumbnail = ?, updated_at = ? WHERE id = ?`,
		name, formatDjangoDateTime(time.Now().UTC()), d.ID); err != nil {
		logerr(err, "designsThumbnail.update")
		writeDRF(w, http.StatusInternalServerError, "server_error", "Something went wrong on our side.")
		return
	}
	if previous != "" && previous != name {
		_ = os.Remove(filepath.Join(a.Settings.MediaRoot, filepath.FromSlash(previous)))
	}
	writeJSON(w, http.StatusOK, struct {
		ThumbnailURL string `json:"thumbnail_url"`
	}{ThumbnailURL: mediaURL + name})
}
