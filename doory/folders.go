// Folder tree rules + views (apps/designs/{folders,folder_views}.py):
// depth limit, cycle prevention, per-parent unique names (lower-cased),
// delete-and-reparent with clash renaming.
//
// Ports backend/apps/designs/folders.py
// Ports backend/apps/designs/folder_views.py
package doory

import (
	"database/sql"
	"fmt"
	"net/http"
	"strings"
	"time"
)

const (
	maxFolderDepth    = 5
	maxFoldersPerUser = 500
	folderNameLength  = 120
	folderPageSize    = 100 // FolderPagination page_size
)

// folderRow maps designs_folder.
type folderRow struct {
	ID          string
	OwnerID     string
	Name        string
	ParentID    sql.NullString
	CreatedAt   string
	UpdatedAt   string
	DesignCount int64 // annotated on list (live designs only)
}

type folderEnvelope struct {
	ID          string  `json:"id"`
	Name        string  `json:"name"`
	Parent      *string `json:"parent"`
	DesignCount int64   `json:"design_count"`
	CreatedAt   string  `json:"created_at"`
}

func (f *folderRow) envelope() folderEnvelope {
	var parent *string
	if f.ParentID.Valid && f.ParentID.String != "" {
		p := dashedUUID(f.ParentID.String)
		parent = &p
	}
	return folderEnvelope{
		ID: dashedUUID(f.ID), Name: f.Name, Parent: parent,
		DesignCount: f.DesignCount, CreatedAt: renderDRFDatetime(f.CreatedAt),
	}
}

// getFolderByOwner resolves an owned folder (OwnedFolderField queryset).
func (a *App) getFolderByOwner(ownerID, id string) (*folderRow, error) {
	row := a.DB.QueryRow(
		`SELECT id, owner_id, name, parent_id, created_at, updated_at FROM designs_folder
		 WHERE id = ? AND owner_id = ?`, id, ownerID)
	var f folderRow
	err := row.Scan(&f.ID, &f.OwnerID, &f.Name, &f.ParentID, &f.CreatedAt, &f.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &f, nil
}

// parentMap ports _parent_map (id -> parent id) for chain/height checks.
func (a *App) parentMap(ownerID string) (map[string]string, error) {
	rows, err := a.DB.Query(`SELECT id, parent_id FROM designs_folder WHERE owner_id = ?`, ownerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	m := map[string]string{}
	for rows.Next() {
		var id string
		var parent sql.NullString
		if err := rows.Scan(&id, &parent); err != nil {
			return nil, err
		}
		m[id] = parent.String
	}
	return m, rows.Err()
}

// chainToRoot ports _chain_to_root (loop-safe).
func chainToRoot(parents map[string]string, start string) []string {
	var chain []string
	seen := map[string]bool{}
	for start != "" && !seen[start] {
		chain = append(chain, start)
		seen[start] = true
		start = parents[start]
	}
	return chain
}

// subtreeHeight ports _subtree_height.
func subtreeHeight(parents map[string]string, root string) int {
	children := map[string][]string{}
	for child, parent := range parents {
		if parent != "" {
			children[parent] = append(children[parent], child)
		}
	}
	height, frontier, seen := 0, []string{root}, map[string]bool{root: true}
	for len(frontier) > 0 {
		height++
		var next []string
		for _, node := range frontier {
			for _, c := range children[node] {
				if !seen[c] {
					next = append(next, c)
					seen[c] = true
				}
			}
		}
		frontier = next
	}
	return height
}

// checkPlacement ports folders.check_placement: no cycles, depth <= 5.
func (a *App) checkPlacement(ownerID string, folder *folderRow, parent *folderRow) *FieldErrors {
	parents, err := a.parentMap(ownerID)
	if err != nil {
		return &FieldErrors{Fields: []FieldError{SimpleField("parent", "server_error", "server_error")}}
	}
	if parent != nil && folder != nil {
		for _, id := range chainToRoot(parents, parent.ID) {
			if id == folder.ID {
				return &FieldErrors{Fields: []FieldError{SimpleField("parent",
					"A folder cannot be moved into itself or one of its subfolders.", "invalid")}}
			}
		}
	}
	above := 0
	if parent != nil {
		above = len(chainToRoot(parents, parent.ID))
	}
	below := 1
	if folder != nil {
		below = subtreeHeight(parents, folder.ID)
	}
	if above+below > maxFolderDepth {
		return &FieldErrors{Fields: []FieldError{SimpleField("parent",
			fmt.Sprintf("Folders can be nested at most %d levels deep.", maxFolderDepth), "invalid")}}
	}
	return nil
}

// nameTaken ports _name_taken (case-insensitive among siblings).
func (a *App) nameTaken(ownerID string, parentID *string, name string, excludeID string) (bool, error) {
	q := `SELECT COUNT(*) FROM designs_folder WHERE owner_id = ? AND LOWER(name) = LOWER(?)`
	args := []any{ownerID, name}
	if parentID == nil {
		q += " AND parent_id IS NULL"
	} else {
		q += " AND parent_id = ?"
		args = append(args, *parentID)
	}
	if excludeID != "" {
		q += " AND id != ?"
		args = append(args, excludeID)
	}
	var n int
	err := a.DB.QueryRow(q, args...).Scan(&n)
	return n > 0, err
}

func duplicateNameError() *FieldErrors {
	return &FieldErrors{Fields: []FieldError{SimpleField("name",
		"A folder with this name already exists here.", "invalid")}}
}

// createFolder ports folders.create_folder (lock, limit, placement, unique).
func (a *App) createFolder(ownerID, name string, parent *folderRow) (*folderRow, *FieldErrors, error) {
	var count int
	if err := a.DB.QueryRow(`SELECT COUNT(*) FROM designs_folder WHERE owner_id = ?`, ownerID).Scan(&count); err != nil {
		return nil, nil, err
	}
	if count >= maxFoldersPerUser {
		return nil, nil, &LimitReachedError{Max: maxFoldersPerUser, Noun: "folders"}
	}
	if fe := a.checkPlacement(ownerID, nil, parent); fe != nil {
		return nil, fe, nil
	}
	var parentArg any
	var parentID *string
	if parent != nil {
		parentArg = parent.ID
		parentID = &parent.ID
	}
	if taken, err := a.nameTaken(ownerID, parentID, name, ""); err != nil {
		return nil, nil, err
	} else if taken {
		return nil, duplicateNameError(), nil
	}
	id := newUUID4Hex()
	now := formatDjangoDateTime(time.Now())
	_, err := a.DB.Exec(
		`INSERT INTO designs_folder (id, owner_id, name, parent_id, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?)`, id, ownerID, name, parentArg, now, now)
	if err != nil {
		if isUniqueViolation(err) {
			return nil, duplicateNameError(), nil
		}
		return nil, nil, err
	}
	f, err := a.getFolderRow(ownerID, id)
	return f, nil, err
}

func (a *App) getFolderRow(ownerID, id string) (*folderRow, error) {
	row := a.DB.QueryRow(
		`SELECT id, owner_id, name, parent_id, created_at, updated_at FROM designs_folder
		 WHERE id = ? AND owner_id = ?`, id, ownerID)
	var f folderRow
	err := row.Scan(&f.ID, &f.OwnerID, &f.Name, &f.ParentID, &f.CreatedAt, &f.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return &f, err
}

// updateFolder ports folders.update_folder.
func (a *App) updateFolder(ownerID string, folder *folderRow, name *string, parentPresent bool, newParent *folderRow, clearParent bool) (*folderRow, *FieldErrors, error) {
	newName := folder.Name
	if name != nil {
		newName = *name
	}
	parentID := folder.ParentID
	if parentPresent {
		if clearParent || newParent == nil {
			parentID = sql.NullString{}
		} else {
			parentID = sql.NullString{String: newParent.ID, Valid: true}
		}
	}
	parentChanged := parentID != folder.ParentID
	if parentChanged {
		var newParentPtr *string
		if parentID.Valid {
			newParentPtr = &parentID.String
		}
		// placement: folder moves relative to the requested parent
		if fe := a.checkPlacement(ownerID, folder, parentOrNil(newParentPtr, clearParent, parentPresent)); fe != nil {
			return nil, fe, nil
		}
	}
	var excludePtr *string = &folder.ID
	var parentFilter *string
	if parentID.Valid {
		parentFilter = &parentID.String
	}
	_ = excludePtr
	if taken, err := a.nameTaken(ownerID, parentFilter, newName, folder.ID); err != nil {
		return nil, nil, err
	} else if taken {
		return nil, duplicateNameError(), nil
	}
	now := formatDjangoDateTime(time.Now())
	_, err := a.DB.Exec(
		`UPDATE designs_folder SET name = ?, parent_id = ?, updated_at = ? WHERE id = ? AND owner_id = ?`,
		newName, ifNull(parentID), now, folder.ID, ownerID)
	if err != nil {
		if isUniqueViolation(err) {
			return nil, duplicateNameError(), nil
		}
		return nil, nil, err
	}
	f, err := a.getFolderRow(ownerID, folder.ID)
	return f, nil, err
}

func parentOrNil(id *string, clear, present bool) *folderRow {
	if !present || clear || id == nil {
		return nil
	}
	return &folderRow{ID: *id}
}

func ifNull(v sql.NullString) any {
	if v.Valid {
		return v.String
	}
	return nil
}

// freeName ports _free_name (append " (n)" until free).
func freeName(name string, taken map[string]bool) string {
	candidate, counter := name, 1
	for taken[strings.ToLower(candidate)] {
		counter++
		suffix := fmt.Sprintf(" (%d)", counter)
		runes := []rune(name)
		cut := folderNameLength - len([]rune(suffix))
		if cut < 0 {
			cut = 0
		}
		if len(runes) > cut {
			candidate = string(runes[:cut]) + suffix
		} else {
			candidate = name + suffix
		}
	}
	return candidate
}

// deleteFolder ports folders.delete_folder: subfolders and designs move up
// to the parent, renaming on clashes.
func (a *App) deleteFolder(ownerID string, folder *folderRow) error {
	destination := ifNull(folder.ParentID)
	// existing sibling names at the destination (excluding this folder)
	taken := map[string]bool{}
	rows, err := a.DB.Query(
		`SELECT LOWER(name) FROM designs_folder WHERE owner_id = ? AND
		 ((? IS NULL AND parent_id IS NULL) OR parent_id = ?) AND id != ?`,
		ownerID, destination, destination, folder.ID)
	if err != nil {
		return err
	}
	for rows.Next() {
		var n string
		if rows.Scan(&n) == nil {
			taken[n] = true
		}
	}
	rows.Close()

	// children ordered by Lower(name), pk (Django order)
	childRows, err := a.DB.Query(
		`SELECT id, name FROM designs_folder WHERE parent_id = ? ORDER BY LOWER(name), id`, folder.ID)
	if err != nil {
		return err
	}
	type child struct{ id, name string }
	var children []child
	for childRows.Next() {
		var c child
		if childRows.Scan(&c.id, &c.name) == nil {
			children = append(children, c)
		}
	}
	childRows.Close()
	now := formatDjangoDateTime(time.Now())
	for _, c := range children {
		newName := freeName(c.name, taken)
		taken[strings.ToLower(newName)] = true
		if _, err := a.DB.Exec(
			`UPDATE designs_folder SET name = ?, parent_id = ?, updated_at = ? WHERE id = ?`,
			newName, destination, now, c.id); err != nil {
			return err
		}
	}
	if _, err := a.DB.Exec(`UPDATE designs_design SET folder_id = ? WHERE folder_id = ?`,
		destination, folder.ID); err != nil {
		return err
	}
	_, err = a.DB.Exec(`DELETE FROM designs_folder WHERE id = ?`, folder.ID)
	return err
}

// ---- folder views ----

// foldersList ports FolderViewSet.list (design_count annotation on LIVE
// designs, ordered Lower(name), pk; ?parent=root|uuid; invalid uuid answers
// via DRF's bare UUIDField run_validation -> non_field_errors).
func (a *App) foldersList(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	q := r.URL.Query()
	where := []string{"f.owner_id = ?"}
	args := []any{u.ID}
	if parent := q.Get("parent"); parent == "root" {
		where = append(where, "f.parent_id IS NULL")
	} else if parent != "" {
		if !uuidShape.MatchString(strings.ToLower(parent)) {
			// Bare UUIDField().run_validation in the view -> detail list ->
			// api_exception_handler wraps as non_field_errors.
			writeDRFValidation(w, []FieldError{{
				Name:     "non_field_errors",
				Messages: []string{"Must be a valid UUID."},
				Codes:    []string{"invalid"},
			}})
			return
		}
		where = append(where, "f.parent_id = ?")
		args = append(args, strings.ToLower(parent))
	}
	rows, err := a.DB.Query(
		`SELECT f.id, f.owner_id, f.name, f.parent_id, f.created_at, f.updated_at,
		        (SELECT COUNT(*) FROM designs_design d
		         WHERE d.folder_id = f.id AND d.trashed_at IS NULL) AS design_count
		 FROM designs_folder f WHERE `+strings.Join(where, " AND ")+`
		 ORDER BY LOWER(f.name), f.id`, args...)
	if err != nil {
		writeDRF(w, http.StatusInternalServerError, "server_error", "Something went wrong on our side.")
		return
	}
	defer rows.Close()
	var list []folderRow
	for rows.Next() {
		var f folderRow
		if err := rows.Scan(&f.ID, &f.OwnerID, &f.Name, &f.ParentID, &f.CreatedAt, &f.UpdatedAt, &f.DesignCount); err != nil {
			writeDRF(w, http.StatusInternalServerError, "server_error", "Something went wrong on our side.")
			return
		}
		list = append(list, f)
	}
	envs := make([]folderEnvelope, 0, len(list))
	for i := range list {
		envs = append(envs, list[i].envelope())
	}
	page, ferr := folderPageSlice(r, len(envs))
	if ferr != nil {
		writeDRF(w, ferr.Status, ferr.Code, ferr.Message)
		return
	}
	resp, err := folderPaginate(r, len(envs), envs[page.from:page.to])
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

// FolderPagination: page_size 100 (FolderPagination override).
func effectiveFolderPageSize(r *http.Request) int {
	if raw := r.URL.Query().Get("page_size"); raw != "" {
		if n := atoi(raw); n > 0 && n <= maxPageSize {
			return n
		}
	}
	return folderPageSizeConst
}

const folderPageSizeConst = 100

func folderPageSlice(r *http.Request, total int) (pageBounds, *DocumentError) {
	pageSize := effectiveFolderPageSize(r)
	numPages := (total + pageSize - 1) / pageSize
	if numPages == 0 {
		numPages = 1
	}
	page := 1
	raw := r.URL.Query().Get("page")
	if raw != "" {
		if raw == "last" {
			page = numPages
		} else {
			n := atoi(raw)
			if n < 1 || n > numPages {
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

func folderPaginate(r *http.Request, total int, items any) (any, error) {
	// Same link shape as designs, folder page size.
	query := r.URL.Query()
	pageSize := effectiveFolderPageSize(r)
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
			n := atoi(raw)
			if n < 1 || n > numPages {
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
			resp.Previous = pageLink(r, 0)
		} else {
			resp.Previous = pageLink(r, page-1)
		}
	}
	return resp, nil
}

type folderWriteBody struct {
	Name   *string `json:"name"`
	Parent *string `json:"parent"`
}

// foldersCreate ports FolderViewSet.create (201 + re-presented row).
func (a *App) foldersCreate(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	var body folderWriteBody
	if err := jsonBody(r, &body); err != nil {
		writeDRF(w, http.StatusBadRequest, "parse_error", "Malformed request body.")
		return
	}
	fields, name, parent, limErr := a.validateFolderWrite(u.ID, &body, false, nil)
	if limErr != nil {
		writeDRF(w, http.StatusConflict, "limit_reached", limErr.Error())
		return
	}
	if len(fields) > 0 {
		writeDRFValidation(w, fields)
		return
	}
	f, ferr, err := a.createFolder(u.ID, name, parent)
	if err != nil {
		if lr, ok := err.(*LimitReachedError); ok {
			writeDRF(w, http.StatusConflict, "limit_reached", lr.Error())
			return
		}
		writeDRF(w, http.StatusInternalServerError, "server_error", "Something went wrong on our side.")
		return
	}
	if ferr != nil {
		writeDRFValidation(w, ferr.Fields)
		return
	}
	a.presentFolder(w, r, f.ID, http.StatusCreated)
}

// presentFolder re-reads with the design_count annotation (self._present).
func (a *App) presentFolder(w http.ResponseWriter, r *http.Request, id string, status int) {
	u := currentUser(r)
	row := a.DB.QueryRow(
		`SELECT f.id, f.owner_id, f.name, f.parent_id, f.created_at, f.updated_at,
		        (SELECT COUNT(*) FROM designs_design d
		         WHERE d.folder_id = f.id AND d.trashed_at IS NULL)
		 FROM designs_folder f WHERE f.id = ? AND f.owner_id = ?`, id, u.ID)
	var f folderRow
	if err := row.Scan(&f.ID, &f.OwnerID, &f.Name, &f.ParentID, &f.CreatedAt, &f.UpdatedAt, &f.DesignCount); err != nil {
		writeDRF(w, http.StatusInternalServerError, "server_error", "Something went wrong on our side.")
		return
	}
	writeJSON(w, status, f.envelope())
}

// validateFolderWrite mirrors FolderWriteSerializer (name clean/empty/
// length; OwnedFolderField resolution). partial=true allows missing name.
func (a *App) validateFolderWrite(ownerID string, body *folderWriteBody, partial bool, exclude *folderRow) (fields []FieldError, name string, parent *folderRow, limErr *LimitReachedError) {
	if body.Name == nil {
		if !partial {
			fields = append(fields, SimpleField("name", "This field is required.", "required"))
		} else if exclude != nil {
			name = exclude.Name
		}
	} else if strings.TrimSpace(*body.Name) == "" {
		// CharField trims then rejects empty: "This field may not be
		// blank." (probe0006 step06) — distinct from a value that cleans
		// to empty (invisible chars only).
		fields = append(fields, SimpleField("name", "This field may not be blank.", "blank"))
	} else {
		cleaned := cleanText(*body.Name)
		switch {
		case cleaned == "":
			fields = append(fields, SimpleField("name", "The name cannot be empty.", "invalid"))
		case len([]rune(cleaned)) > folderNameLength:
			fields = append(fields, SimpleField("name",
				fmt.Sprintf("Ensure this field has no more than %d characters.", folderNameLength), "max_length"))
		default:
			name = cleaned
		}
	}
	if body.Parent != nil && *body.Parent != "" && *body.Parent != "null" {
		if !uuidShape.MatchString(strings.ToLower(*body.Parent)) {
			fields = append(fields, SimpleField("parent", uuidInvalidMsg(*body.Parent), "invalid"))
		} else {
			p, err := a.getFolderByOwner(ownerID, hexUUID(*body.Parent))
			if err != nil {
				limErr = nil
			}
			if p == nil {
				fields = append(fields, SimpleField("parent",
					fmt.Sprintf("Invalid pk \"%s\" - object does not exist.", *body.Parent), "does_not_exist"))
			}
			parent = p
		}
	}
	return fields, name, parent, nil
}

// foldersRetrieve ports FolderViewSet retrieve.
func (a *App) foldersRetrieve(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	f, ok := a.getFolderFor(w, r, u)
	if !ok {
		return
	}
	a.presentFolder(w, r, f.ID, http.StatusOK)
}

func (a *App) getFolderFor(w http.ResponseWriter, r *http.Request, u *UserRow) (*folderRow, bool) {
	id := hexUUID(routeParam(r, "uuid"))
	f, err := a.getFolderByOwner(u.ID, id)
	if err != nil {
		writeDRF(w, http.StatusInternalServerError, "server_error", "Something went wrong on our side.")
		return nil, false
	}
	if f == nil {
		writeDRF(w, http.StatusNotFound, "not_found", "Not found.")
		return nil, false
	}
	return f, true
}

// foldersPartialUpdate ports FolderViewSet.partial_update.
func (a *App) foldersPartialUpdate(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	f, ok := a.getFolderFor(w, r, u)
	if !ok {
		return
	}
	var body folderWriteBody
	if err := jsonBody(r, &body); err != nil {
		writeDRF(w, http.StatusBadRequest, "parse_error", "Malformed request body.")
		return
	}
	fields, name, parent, _ := a.validateFolderWrite(u.ID, &body, true, f)
	if len(fields) > 0 {
		writeDRFValidation(w, fields)
		return
	}
	// parent presence: folder:null clears (OwnedFolderField allow_null).
	parentPresent := body.Parent != nil && *body.Parent != ""
	clearParent := parentPresent && *body.Parent == "null"
	updated, ferr, err := a.updateFolder(u.ID, f, body.Name, parentPresent && !clearParent, parent, clearParent)
	if err != nil {
		writeDRF(w, http.StatusInternalServerError, "server_error", "Something went wrong on our side.")
		return
	}
	if ferr != nil {
		writeDRFValidation(w, ferr.Fields)
		return
	}
	_ = name
	a.presentFolder(w, r, updated.ID, http.StatusOK)
}

// foldersDestroy ports FolderViewSet.destroy (reparent semantics).
func (a *App) foldersDestroy(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	f, ok := a.getFolderFor(w, r, u)
	if !ok {
		return
	}
	if err := a.deleteFolder(u.ID, f); err != nil {
		writeDRF(w, http.StatusInternalServerError, "server_error", "Something went wrong on our side.")
		return
	}
	w.Header().Set("Content-Length", "0")
	w.WriteHeader(http.StatusNoContent)
}

// uuidInvalidMsg mirrors DRF 6.1's UUIDField invalid message with
// typographic quotes (probe0006 step08: “not-a-uuid” is not a valid UUID.).
func uuidInvalidMsg(value string) string {
	return "\u201c" + value + "\u201d is not a valid UUID."
}
