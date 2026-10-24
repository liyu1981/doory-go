// Font upload + delete (apps/fonts/uploads.py add_user_font + the
// FontUploadSerializer validation + UserFontDelete).
//
// Probe-proven messages (probes/0006-fonts): per-file size/junk prefixes,
// the sorted family-mismatch sentence, the409 font_name_taken/variant_exists
// envelopes, the403 quota codes (font_limit_reached vs font_storage_limit).
package doory

import (
	"database/sql"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"unicode"

	"doory-go/internal/fontparse"
	"golang.org/x/text/unicode/norm"
)

// described is one validated upload file.
type described struct {
	data []byte
	info *fontparse.Info
	name string
}

// familyEnvelopes builds the windowed page of family envelopes (variants
// prefetched for the WHOLE list like the serializer's prefetch, then sliced).
func (a *App) familyEnvelopes(list []fontFamilyRow, bounds pageBounds) ([]fontFamilyEnvelope, error) {
	ids := make([]string, len(list))
	for i := range list {
		ids[i] = list[i].ID
	}
	variants, err := a.variantsOf(ids)
	if err != nil {
		return nil, err
	}
	from, to := bounds.from, bounds.to
	if from > len(list) {
		from = len(list)
	}
	if to > len(list) {
		to = len(list)
	}
	out := make([]fontFamilyEnvelope, 0, to-from)
	for _, f := range list[from:to] {
		out = append(out, f.envelope(variants[f.ID]))
	}
	return out, nil
}

// fontUpload ports UserFontListCreate.post + add_user_font.
func (a *App) fontUpload(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	if err := r.ParseMultipartForm(maxFontFileBytes); err != nil {
		writeDRF(w, http.StatusBadRequest, "parse_error", "Malformed request body.")
		return
	}
	defer func() {
		if r.MultipartForm != nil {
			_ = r.MultipartForm.RemoveAll()
		}
	}()

	// FontUploadSerializer: files (ListField of FileField), family, category.
	var fields []FieldError
	files := r.MultipartForm.File["files"]
	if len(files) == 0 {
		fields = append(fields, SimpleField("files", "This field is required.", "required"))
	} else if len(files) > maxFilesPerUpload {
		fields = append(fields, SimpleField("files",
			"Ensure this field has no more than 5 elements.", "max_length"))
	}
	familyRaw := ""
	familyGiven := false
	if raw := r.MultipartForm.Value["family"]; len(raw) > 0 {
		familyGiven = true
		if len([]rune(raw[0])) > 100 {
			fields = append(fields, SimpleField("family",
				"Ensure this field has no more than 100 characters.", "max_length"))
		} else {
			familyRaw = raw[0]
		}
	}
	category := ""
	if raw := r.MultipartForm.Value["category"]; len(raw) > 0 && raw[0] != "" {
		if !choiceValid(fontCategories(), raw[0]) {
			// DRF serializer ChoiceField (probe0006 step12): straight quotes.
			fields = append(fields, SimpleField("category",
				quoteChoice(raw[0])+" is not a valid choice.", "invalid_choice"))
		} else {
			category = raw[0]
		}
	}
	if len(fields) > 0 {
		writeDRFValidation(w, fields)
		return
	}

	// _describe: bounded read, content inspection, family/slot consistency.
	entries := make([]described, 0, len(files))
	for _, fh := range files {
		data, err := readUpload(fh)
		if err != nil {
			writeDRFValidation(w, []FieldError{SimpleField("files", err.Error(), "invalid")})
			return
		}
		info, perr := fontparse.Inspect(data)
		if perr != nil {
			writeDRFValidation(w, []FieldError{SimpleField("files",
				fh.Filename+": "+perr.Error(), "invalid")})
			return
		}
		entries = append(entries, described{data: data, info: info, name: fh.Filename})
	}
	names := map[string]bool{}
	for _, e := range entries {
		names[e.info.Family] = true
	}
	// family override: clean_family_name first (add_user_font order).
	override := ""
	if familyGiven && strings.TrimSpace(familyRaw) != "" {
		override = cleanFamilyName(familyRaw)
		if override == "" {
			writeDRFValidation(w, []FieldError{SimpleField("family",
				"The family name has no usable letters or digits.", "invalid")})
			return
		}
	}
	if len(names) > 1 && override == "" {
		joined := make([]string, 0, len(names))
		for n := range names {
			joined = append(joined, n)
		}
		sort.Strings(joined) // probe: "(Noto Sans Bassa Vah, Noto Sans Buginese)" sorted
		writeDRFValidation(w, []FieldError{SimpleField("files",
			"These files belong to different families ("+strings.Join(joined, ", ")+
				"). Upload one family at a time or choose a family name.", "invalid")})
		return
	}
	slots := map[string]bool{}
	for _, e := range entries {
		key := itoa(int(e.info.Weight)) + "|" + e.info.Style()
		if slots[key] {
			writeDRFValidation(w, []FieldError{SimpleField("files",
				"Two of the files have the same weight and style.", "invalid")})
			return
		}
		slots[key] = true
	}
	name := override
	if name == "" {
		name = entries[0].info.Family
	}

	// add_user_font: built-in conflict -> family/storage quotas ->
	// merge-or-create -> variant clash.
	var builtinCount int
	_ = a.DB.QueryRow(
		`SELECT COUNT(*) FROM fonts_fontfamily WHERE owner_id IS NULL AND LOWER(family) = LOWER(?)`,
		name).Scan(&builtinCount)
	if builtinCount > 0 {
		writeDRF(w, http.StatusConflict, "font_name_taken",
			`A built-in font named "`+name+`" already exists. Choose a different family name.`)
		return
	}

	existing, err := a.findOwnedFamily(u.ID, name)
	if err != nil {
		logerr(err, "fontUpload.findFamily")
		writeDRF(w, http.StatusInternalServerError, "server_error", "Something went wrong on our side.")
		return
	}
	if existing == nil {
		var owned int
		_ = a.DB.QueryRow(`SELECT COUNT(*) FROM fonts_fontfamily WHERE owner_id = ?`, u.ID).Scan(&owned)
		if owned >= maxFamiliesPerUser {
			writeDRF(w, http.StatusForbidden, "font_limit_reached",
				"You can keep at most 50 uploaded font families.")
			return
		}
	}
	var usedBytes int64
	_ = a.DB.QueryRow(
		`SELECT COALESCE(SUM(v.size), 0) FROM fonts_fontvariant v
		 JOIN fonts_fontfamily f ON f.id = v.family_id WHERE f.owner_id = ?`,
		u.ID).Scan(&usedBytes)
	var incoming int64
	for _, e := range entries {
		incoming += int64(len(e.data))
	}
	if usedBytes+incoming > maxFontBytesPerUser {
		writeDRF(w, http.StatusForbidden, "font_storage_limit",
			"Your uploaded fonts would exceed the storage limit.")
		return
	}

	family := existing
	if family != nil {
		taken, verr := a.variantSlots(family.ID)
		if verr != nil {
			logerr(verr, "fontUpload.slots")
			writeDRF(w, http.StatusInternalServerError, "server_error", "Something went wrong on our side.")
			return
		}
		var clash []string
		for _, e := range entries {
			key := itoa(int(e.info.Weight)) + "|" + e.info.Style()
			if taken[key] {
				clash = append(clash, itoa(int(e.info.Weight))+" "+e.info.Style())
			}
		}
		if len(clash) > 0 {
			sort.Strings(clash)
			writeDRF(w, http.StatusConflict, "variant_exists",
				`"`+family.Family+`" already has `+strings.Join(clash, ", ")+
					`. Delete it first to replace.`)
			return
		}
	} else {
		created, cerr := a.createFontFamily(u.ID, name, category, entries[0].info.Category)
		if cerr != nil {
			logerr(cerr, "fontUpload.createFamily")
			writeDRF(w, http.StatusInternalServerError, "server_error", "Something went wrong on our side.")
			return
		}
		family = created
	}

	// Store files under MEDIA_ROOT/fonts/user/<owner>/<variant>.<format>
	// (variant_file_path: unguessable name, extension from the SNIFFED
	// format — never the client's filename).
	ownerDir := filepath.Join(a.Settings.MediaRoot, "fonts", "user", dashedUUID(u.ID))
	if err := os.MkdirAll(ownerDir, 0o755); err != nil {
		logerr(err, "fontUpload.mkdir")
		writeDRF(w, http.StatusInternalServerError, "server_error", "Something went wrong on our side.")
		return
	}
	stored := []string{}
	for _, e := range entries {
		id := newUUID4Hex()
		// upload_to uses family.owner_id and instance.id as uuid STRING
		// values: dashed (Django str(uuid.UUID)).
		rel := "fonts/user/" + dashedUUID(u.ID) + "/" + dashedUUID(id) + "." + e.info.Format
		abs := filepath.Join(a.Settings.MediaRoot, rel)
		if err := os.WriteFile(abs, e.data, 0o644); err != nil {
			logerr(err, "fontUpload.write")
			writeDRF(w, http.StatusInternalServerError, "server_error", "Something went wrong on our side.")
			return
		}
		stored = append(stored, abs)
		if _, err := a.DB.Exec(
			`INSERT INTO fonts_fontvariant
			 (id, created_at, updated_at, weight, style, format, size, subset, unicode_range, file, family_id)
			 VALUES (?, ?, ?, ?, ?, ?, ?, '', '', ?, ?)`,
			id, formatDjangoDateTime(time.Now()), formatDjangoDateTime(time.Now()),
			e.info.Weight, e.info.Style(), e.info.Format, len(e.data),
			rel, family.ID); err != nil {
			logerr(err, "fontUpload.variant")
			for _, p := range stored {
				_ = os.Remove(p)
			}
			writeDRF(w, http.StatusInternalServerError, "server_error", "Something went wrong on our side.")
			return
		}
	}

	// existing.scripts / is_variable / category merge (transaction body in
	// upstream; SQLite serializes writers here).
	scriptSet := map[string]bool{}
	if family.Scripts != "" {
		for _, s := range parseJSONArray(family.Scripts) {
			scriptSet[s] = true
		}
	}
	variable := family.IsVariable
	for _, e := range entries {
		for _, s := range e.info.Scripts {
			scriptSet[s] = true
		}
		variable = variable || e.info.IsVariable
	}
	scripts := make([]string, 0, len(scriptSet))
	for s := range scriptSet {
		scripts = append(scripts, s)
	}
	sort.Strings(scripts)
	newCategory := family.Category
	if category != "" {
		newCategory = category
	}
	if _, err := a.DB.Exec(
		`UPDATE fonts_fontfamily SET scripts = ?, is_variable = ?, category = ?, updated_at = ?
		 WHERE id = ?`,
		string(encodeCompactJSON(scripts)), variable, newCategory,
		formatDjangoDateTime(time.Now()), family.ID); err != nil {
		logerr(err, "fontUpload.merge")
		writeDRF(w, http.StatusInternalServerError, "server_error", "Something went wrong on our side.")
		return
	}

	// 201 with the fresh family envelope (probe0006 step07).
	a.presentFont(w, r, family.ID, http.StatusCreated)
}

// presentFont renders a single family envelope at status (upload 201).
func (a *App) presentFont(w http.ResponseWriter, r *http.Request, id string, status int) {
	row, err := a.getFontByID(id)
	if err != nil || row == nil {
		writeDRF(w, http.StatusInternalServerError, "server_error", "Something went wrong on our side.")
		return
	}
	envs, err := a.familyEnvelopes([]fontFamilyRow{*row}, pageBounds{from: 0, to: 1})
	if err != nil {
		writeDRF(w, http.StatusInternalServerError, "server_error", "Something went wrong on our side.")
		return
	}
	writeJSON(w, status, envs[0])
}

func (a *App) getFontByID(id string) (*fontFamilyRow, error) {
	row := a.DB.QueryRow(`SELECT `+familyCols+` FROM fonts_fontfamily WHERE id = ?`, id)
	f, err := scanFamily(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &f, nil
}

func (a *App) findOwnedFamily(ownerID, name string) (*fontFamilyRow, error) {
	rows, err := a.DB.Query(
		`SELECT `+familyCols+` FROM fonts_fontfamily WHERE owner_id = ? AND LOWER(family) = LOWER(?)`,
		ownerID, name)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	if rows.Next() {
		f, err := scanFamily(rows)
		if err != nil {
			return nil, err
		}
		return &f, nil
	}
	return nil, rows.Err()
}

// variantSlots returns "weight|style" keys of a family's variants.
func (a *App) variantSlots(familyID string) (map[string]bool, error) {
	rows, err := a.DB.Query(
		`SELECT weight, style FROM fonts_fontvariant WHERE family_id = ?`, familyID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var w int64
		var style string
		if err := rows.Scan(&w, &style); err != nil {
			return nil, err
		}
		out[itoa(int(w))+"|"+style] = true
	}
	return out, rows.Err()
}

// createFontFamily inserts the row (slug = slugify(family)).
func (a *App) createFontFamily(ownerID, name, categoryOverride, inferred string) (*fontFamilyRow, error) {
	category := categoryOverride
	if category == "" {
		category = inferred
	}
	id := newUUID4Hex()
	now := formatDjangoDateTime(time.Now())
	if _, err := a.DB.Exec(
		`INSERT INTO fonts_fontfamily
		 (id, created_at, updated_at, family, slug, category, scripts, is_variable,
		  popularity, license, source, owner_id, is_published)
		 VALUES (?, ?, ?, ?, ?, ?, '[]', FALSE, ?, ?, 'upload', ?, TRUE)`,
		id, now, now, name, slugify(name), category, unranked, userFontLicense,
		ownerID); err != nil {
		return nil, err
	}
	return a.getFontByID(id)
}

// slugify approximates django.utils.text.slugify: NFKD-ish normalize,
// lowercase, letters/digits/underscore kept, everything else collapses to
// hyphens. Not response-visible (serializer omits slug) — slug edge probes
// belong to the sync/content slice.
func slugify(s string) string {
	var b strings.Builder
	prevHyphen := false
	for _, r := range strings.ToLower(norm.NFKD.String(s)) {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_':
			b.WriteRune(r)
			prevHyphen = false
		default:
			if !prevHyphen && b.Len() > 0 {
				b.WriteRune('-')
				prevHyphen = true
			}
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "font"
	}
	return out
}

// cleanFamilyName ports inspector.clean_family_name: NFC, keep
// letters/marks/digits + safe punctuation, collapse whitespace, trim
// punctuation, clip to 100 chars then trim.
func cleanFamilyName(raw string) string {
	nfc := norm.NFC.String(raw)
	var kept []rune
	for _, r := range nfc {
		if unicode.Is(unicode.L, r) || unicode.Is(unicode.M, r) || unicode.Is(unicode.N, r) ||
			strings.ContainsRune(" -_.&+()", r) {
			kept = append(kept, r)
		}
	}
	collapsed := strings.Join(strings.Fields(string(kept)), " ")
	trimmed := strings.Trim(collapsed, " -_.")
	runes := []rune(trimmed)
	if len(runes) > 100 {
		runes = runes[:100]
	}
	return strings.TrimSpace(string(runes))
}

// parseJSONArray extracts top-level string items from stored JSON text.
func parseJSONArray(raw string) []string {
	var list []string
	if err := json.Unmarshal([]byte(raw), &list); err != nil {
		return nil
	}
	return list
}

// fontDelete ports UserFontDelete: only the caller's family; files and rows
// go together; other users' fonts are 404.
func (a *App) fontDelete(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	id := hexUUID(routeParam(r, "uuid"))
	family, err := a.findOwnedFamilyByID(u.ID, id)
	if err != nil {
		logerr(err, "fontDelete")
		writeDRF(w, http.StatusInternalServerError, "server_error", "Something went wrong on our side.")
		return
	}
	if family == nil {
		writeDRF(w, http.StatusNotFound, "not_found", "Not found.")
		return
	}
	variants, err := a.variantsOf([]string{family.ID})
	if err == nil {
		for _, v := range variants[family.ID] {
			_ = os.Remove(filepath.Join(a.Settings.MediaRoot, v.File))
		}
	}
	if _, err := a.DB.Exec(`DELETE FROM fonts_fontvariant WHERE family_id = ?`, family.ID); err != nil {
		logerr(err, "fontDelete.variants")
		writeDRF(w, http.StatusInternalServerError, "server_error", "Something went wrong on our side.")
		return
	}
	if _, err := a.DB.Exec(`DELETE FROM fonts_fontfamily WHERE id = ?`, family.ID); err != nil {
		logerr(err, "fontDelete.family")
		writeDRF(w, http.StatusInternalServerError, "server_error", "Something went wrong on our side.")
		return
	}
	w.Header().Set("Content-Length", "0")
	w.WriteHeader(http.StatusNoContent)
}

func (a *App) findOwnedFamilyByID(ownerID, id string) (*fontFamilyRow, error) {
	row := a.DB.QueryRow(
		`SELECT `+familyCols+` FROM fonts_fontfamily WHERE id = ? AND owner_id = ?`, id, ownerID)
	f, err := scanFamily(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &f, nil
}

// readUpload: bounded read with the probed per-file messages.
func readUpload(fh *multipart.FileHeader) ([]byte, error) {
	handle, err := fh.Open()
	if err != nil {
		return nil, err
	}
	defer handle.Close()
	data, err := io.ReadAll(io.LimitReader(handle, maxFontFileBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return nil, errText(fh.Filename + ": the file is empty.")
	}
	if len(data) > maxFontFileBytes {
		return nil, errText(fh.Filename + ": the file is larger than 10 MB.")
	}
	return data, nil
}

type errText string

func (e errText) Error() string { return string(e) }

func quoteChoice(v string) string { return `"` + v + `"` }
