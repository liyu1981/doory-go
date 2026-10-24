// Template import (apps/gallery/importer.py + the document validator and
// thumbnail rules from validators.py / services.py): the shared body of
// `seed_templates` and `import_template`.
//
// Thumbnails: upstream re-encodes every thumbnail to WebP through Pillow
// (make_thumbnail, quality 82). There is no pure-Go WebP encoder (the same
// substitution M7 recorded for asset thumbnails), so the Go port validates
// the raster, bounds it (720 px longest side) and stores the source bytes when
// they already are WebP — the file name/URL and dimensions match, the bytes
// do not. The cli-driver compares those files structurally and M14's encoder
// decision removes the exception.

package doory

import (
	"bytes"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	_ "golang.org/x/image/webp"
)

const (
	templateMaxPages       = 100
	templateMaxObjects     = 5000
	templateMaxDimension   = 20000
	templateMaxTreeDepth   = 64
	templateMaxDataURI     = 64 * 1024
	templateMaxTags        = 20
	templateMaxTagLength   = 40
	templateThumbMaxSide   = 720
	templateThumbMaxBytes  = 8 * 1024 * 1024
	templateDocMaxBytes    = 2 * 1024 * 1024 // settings.MAX_DESIGN_DOCUMENT_BYTES (probe: 2 MB)
	templatePhotoDir       = "templates/photos/"
	templateThumbStorageAt = "templates/"
)

var templateSlugPattern = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)

// TemplateDocumentError is validators.TemplateDocumentError.
type TemplateDocumentError struct{ message string }

func (e *TemplateDocumentError) Error() string { return e.message }

// TemplateImportError is importer.TemplateImportError.
type TemplateImportError struct{ message string }

func (e *TemplateImportError) Error() string { return e.message }

func templateImportErrorf(format string, args ...any) error {
	return &TemplateImportError{message: fmt.Sprintf(format, args...)}
}

// TemplateSummary is validators.DocumentSummary.
type TemplateSummary struct {
	Width     int
	Height    int
	PageCount int
	Preset    string
}

// validateTemplateDocument ports validators.validate_template_document.
func validateTemplateDocument(document map[string]any) (TemplateSummary, error) {
	var summary TemplateSummary
	version, ok := wholeInt(document["schemaVersion"])
	if !ok || version < 1 {
		return summary, &TemplateDocumentError{message: "schemaVersion must be an integer of at least 1."}
	}
	for _, side := range []string{"width", "height"} {
		value, ok := wholeInt(document[side])
		if !ok || value < 1 || value > templateMaxDimension {
			return summary, &TemplateDocumentError{message: fmt.Sprintf(
				"%s must be an integer between 1 and %d.", side, templateMaxDimension)}
		}
	}
	width, _ := wholeInt(document["width"])
	height, _ := wholeInt(document["height"])
	pages, ok := document["pages"].([]any)
	if !ok || len(pages) < 1 || len(pages) > templateMaxPages {
		return summary, &TemplateDocumentError{message: fmt.Sprintf(
			"pages must be a list of 1 to %d pages.", templateMaxPages)}
	}
	for i, rawPage := range pages {
		page, isMap := rawPage.(map[string]any)
		if !isMap {
			return summary, &TemplateDocumentError{message: fmt.Sprintf("Page %d must be an object.", i+1)}
		}
		if id, isStr := page["id"].(string); !isStr || id == "" {
			return summary, &TemplateDocumentError{message: fmt.Sprintf("Page %d needs a string id.", i+1)}
		}
		objects, isList := page["objects"].([]any)
		if !isList || len(objects) > templateMaxObjects {
			return summary, &TemplateDocumentError{message: fmt.Sprintf(
				"Page %d objects must be a list of at most %d.", i+1, templateMaxObjects)}
		}
		for _, object := range objects {
			if _, isMap := object.(map[string]any); !isMap {
				return summary, &TemplateDocumentError{message: fmt.Sprintf(
					"Page %d contains an object that is not a JSON object.", i+1)}
			}
		}
	}
	encoded, err := json.Marshal(document)
	if err != nil {
		return summary, &TemplateDocumentError{message: "The document is not valid JSON data."}
	}
	if len(encoded) > templateDocMaxBytes {
		return summary, &TemplateDocumentError{message: "The document is too large."}
	}
	if err := scanTemplateStrings(document); err != nil {
		return summary, err
	}
	preset := ""
	if meta, ok := document["meta"].(map[string]any); ok {
		if id, ok := meta["presetId"].(string); ok {
			preset = truncateRunes(id, 64)
		}
	}
	return TemplateSummary{Width: width, Height: height, PageCount: len(pages), Preset: preset}, nil
}

// scanTemplateStrings ports _scan_strings: walk iteratively (no recursion
// bombs) and reject local-only or oversized inline assets.
func scanTemplateStrings(root any) error {
	type frame struct {
		node  any
		depth int
	}
	stack := []frame{{root, 0}}
	for len(stack) > 0 {
		current := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if current.depth > templateMaxTreeDepth {
			return &TemplateDocumentError{message: "The document is nested too deeply."}
		}
		switch node := current.node.(type) {
		case map[string]any:
			for _, value := range node {
				stack = append(stack, frame{value, current.depth + 1})
			}
		case []any:
			for _, value := range node {
				stack = append(stack, frame{value, current.depth + 1})
			}
		case string:
			head := strings.ToLower(strings.TrimLeft(safeHead(node, 16), " "))
			if strings.HasPrefix(head, "blob:") || strings.HasPrefix(head, "idb://") {
				return &TemplateDocumentError{message: "The document references a local-only asset (blob: or idb://)."}
			}
			if strings.HasPrefix(head, "data:") && len(node) > templateMaxDataURI {
				return &TemplateDocumentError{message: "The document embeds an inline asset larger than 64 KB."}
			}
		}
	}
	return nil
}

// safeHead returns at most n bytes of s (a UTF-8 boundary is not required for
// the ASCII prefixes checked here).
func safeHead(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func truncateRunes(s string, limit int) string {
	runes := []rune(s)
	if len(runes) <= limit {
		return s
	}
	return string(runes[:limit])
}

// wholeInt reports an integer JSON number (bools excluded).
func wholeInt(value any) (int, bool) {
	switch v := value.(type) {
	case json.Number:
		n, err := v.Int64()
		return int(n), err == nil
	case float64:
		if v != float64(int64(v)) {
			return 0, false
		}
		return int(v), true
	case int:
		return v, true
	case int64:
		return int(v), true
	}
	return 0, false
}

// normalizeTemplateTags ports gallery.validators.normalize_tags.
func normalizeTemplateTags(raw any) ([]string, error) {
	var items []string
	switch v := raw.(type) {
	case nil:
		return nil, nil
	case string:
		if v == "" {
			return nil, nil
		}
		items = strings.Split(v, ",")
	case []any:
		for _, entry := range v {
			s, ok := entry.(string)
			if !ok {
				return nil, fmt.Errorf("Tags must be a list of strings.")
			}
			items = append(items, s)
		}
	default:
		return nil, fmt.Errorf("Tags must be a list of strings.")
	}
	seen := map[string]bool{}
	out := []string{}
	for _, item := range items {
		cleaned := strings.ToLower(strings.Join(strings.Fields(item), " "))
		if cleaned == "" {
			continue
		}
		if len([]rune(cleaned)) > templateMaxTagLength {
			return nil, fmt.Errorf("A tag is longer than %d characters.", templateMaxTagLength)
		}
		if seen[cleaned] {
			continue
		}
		seen[cleaned] = true
		out = append(out, cleaned)
	}
	if len(out) > templateMaxTags {
		return nil, fmt.Errorf("Use at most %d tags.", templateMaxTags)
	}
	return out, nil
}

// templateImport is one parsed import description.
type templateImport struct {
	Slug        string
	Title       string
	Description *string
	Tags        []string
	HasTags     bool
	Category    *string
	Preset      *string
	Featured    *bool
	Published   *bool
	Document    map[string]any
	DocumentRaw string // Python-stored text with the FILE's key order
	Thumbnail   *string
}

var templateKnownKeys = map[string]bool{
	"title": true, "slug": true, "category": true, "description": true, "tags": true,
	"preset": true, "is_featured": true, "is_published": true, "document": true, "thumbnail": true,
}

// parseTemplateImport ports importer.import_template's validation half.
// docNode carries the document's original key order for storage (the map is
// only for validation).
func parseTemplateImport(data map[string]any, docNode *jsonNode) (*templateImport, error) {
	var unknown []string
	for key := range data {
		if !templateKnownKeys[key] {
			unknown = append(unknown, key)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return nil, templateImportErrorf("Unknown field(s): %s.", strings.Join(unknown, ", "))
	}
	title, err := templateText(data, "title", 200, true)
	if err != nil {
		return nil, err
	}
	slugRef, serr := templateText(data, "slug", 120, false)
	if serr != nil {
		return nil, serr
	}
	slug := ""
	if slugRef != nil {
		slug = *slugRef
	}
	if slug == "" {
		slug = slugifyTemplate(*title)
	}
	if !templateSlugPattern.MatchString(slug) {
		return nil, templateImportErrorf(
			`"%s" is not a valid slug (lower-case letters, digits and hyphens).`, slug)
	}
	out := &templateImport{Slug: slug, Title: *title}
	if description, derr := templateText(data, "description", 5000, false); derr != nil {
		return nil, derr
	} else if description != nil {
		out.Description = description
	}
	if rawTags, present := data["tags"]; present {
		tags, terr := normalizeTemplateTags(rawTags)
		if terr != nil {
			return nil, templateImportErrorf(`"tags": %v`, terr)
		}
		out.Tags, out.HasTags = tags, true
	}
	if rawCategory, present := data["category"]; present {
		if rawCategory == nil {
			out.Category = nil
		} else {
			ref, ok := rawCategory.(string)
			if !ok || strings.TrimSpace(ref) == "" {
				return nil, templateImportErrorf(`"category" must be a category slug or name.`)
			}
			trimmed := strings.TrimSpace(ref)
			out.Category = &trimmed
		}
	}
	if preset, perr := templateText(data, "preset", 64, false); perr != nil {
		return nil, perr
	} else if preset != nil {
		out.Preset = preset
	}
	if raw, present := data["is_featured"]; present {
		flag, ok := raw.(bool)
		if !ok {
			return nil, templateImportErrorf(`"is_featured" must be true or false.`)
		}
		out.Featured = &flag
	}
	if raw, present := data["is_published"]; present {
		flag, ok := raw.(bool)
		if !ok {
			return nil, templateImportErrorf(`"is_published" must be true or false.`)
		}
		out.Published = &flag
	}
	document, ok := data["document"].(map[string]any)
	if !ok {
		return nil, templateImportErrorf("Invalid document: The document must be a JSON object.")
	}
	out.Document = document
	if docNode != nil {
		out.DocumentRaw = pyDumpsASCII(docNode)
	} else if encoded, merr := json.Marshal(document); merr == nil {
		out.DocumentRaw = string(pythonStoredJSON(encoded))
	}
	if raw, present := data["thumbnail"]; present {
		ref, ok := raw.(string)
		if !ok || strings.TrimSpace(ref) == "" {
			return nil, templateImportErrorf(`"thumbnail" must be a file path relative to the JSON file.`)
		}
		out.Thumbnail = &ref
	}
	return out, nil
}

func templateText(data map[string]any, key string, limit int, required bool) (*string, error) {
	raw, present := data[key]
	if !present || raw == nil {
		if required {
			return nil, templateImportErrorf(`"%s" is required.`, key)
		}
		return nil, nil
	}
	text, ok := raw.(string)
	if !ok || (required && strings.TrimSpace(text) == "") || len([]rune(text)) > limit {
		return nil, templateImportErrorf(
			`"%s" must be a non-empty string of at most %d characters.`, key, limit)
	}
	out := text
	if key != "description" {
		out = strings.TrimSpace(text)
	}
	return &out, nil
}

var slugSeparators = regexp.MustCompile(`[^a-z0-9]+`)

// slugifyTemplate ports django.utils.text.slugify (ASCII, lower-case, "-"
// separators, trimmed).
func slugifyTemplate(value string) string {
	lowered := strings.ToLower(strings.TrimSpace(value))
	lowered = strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			return r
		}
		return ' '
	}, lowered)
	return strings.Trim(slugSeparators.ReplaceAllString(lowered, "-"), "-")
}

// resolveTemplateCategory ports importer._resolve_category.
func (a *App) resolveTemplateCategory(tx *sql.Tx, ref string, create bool) (string, bool, error) {
	slug := slugifyTemplate(ref)
	var id string
	if slug != "" {
		_ = tx.QueryRow(`SELECT id FROM gallery_templatecategory WHERE slug = ?`, slug).Scan(&id)
	}
	if id == "" {
		_ = tx.QueryRow(`SELECT id FROM gallery_templatecategory WHERE lower(name) = lower(?)`, ref).Scan(&id)
	}
	if id != "" {
		return id, false, nil
	}
	if !create {
		known, kerr := templateCategorySlugs(tx)
		if kerr != nil {
			return "", false, kerr
		}
		list := strings.Join(known, ", ")
		if list == "" {
			list = "none yet"
		}
		return "", false, templateImportErrorf(
			`Unknown category "%s" (known: %s). Run seed_gallery or pass --create-categories.`, ref, list)
	}
	if slug == "" {
		return "", false, templateImportErrorf(`"%s" is not a usable category name.`, ref)
	}
	id = newUUID4Hex()
	now := formatDjangoDateTime(time.Now())
	if _, err := tx.Exec(`INSERT INTO gallery_templatecategory
	     (id, created_at, updated_at, slug, name, icon, sort_order)
	     VALUES (?, ?, ?, ?, ?, '', 0)`, id, now, now, slug, truncateRunes(ref, 80)); err != nil {
		return "", false, err
	}
	return id, true, nil
}

func templateCategorySlugs(tx *sql.Tx) ([]string, error) {
	rows, err := tx.Query(`SELECT slug FROM gallery_templatecategory ORDER BY sort_order, name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var slug string
		if err := rows.Scan(&slug); err != nil {
			return nil, err
		}
		out = append(out, slug)
	}
	return out, rows.Err()
}

// templateThumbnail validates a thumbnail file the way thumbnail_from_upload
// does (a decodable raster, bounded) and returns the bytes to store plus the
// image dimensions. WebP sources are stored as-is (documented encoder gap).
func templateThumbnail(sourcePath string) ([]byte, int, int, error) {
	st, err := os.Stat(sourcePath)
	if err != nil || !st.Mode().IsRegular() {
		return nil, 0, 0, fmt.Errorf("not found")
	}
	if st.Size() > templateThumbMaxBytes {
		return nil, 0, 0, fmt.Errorf("The file is too large.")
	}
	data, rerr := os.ReadFile(sourcePath)
	if rerr != nil {
		return nil, 0, 0, rerr
	}
	if len(data) == 0 {
		return nil, 0, 0, fmt.Errorf("The file is empty.")
	}
	cfg, format, derr := image.DecodeConfig(bytes.NewReader(data))
	if derr != nil {
		return nil, 0, 0, fmt.Errorf("Unsupported image type. Use PNG, JPEG, WebP, GIF or AVIF.")
	}
	if cfg.Width < 1 || cfg.Height < 1 || cfg.Width > 12000 || cfg.Height > 12000 {
		return nil, 0, 0, fmt.Errorf("Unsupported image dimensions.")
	}
	if cfg.Width > templateThumbMaxSide || cfg.Height > templateThumbMaxSide {
		return nil, cfg.Width, cfg.Height, fmt.Errorf("thumbnail-oversize")
	}
	if format != "webp" {
		return nil, cfg.Width, cfg.Height, fmt.Errorf("thumbnail-reencode-needed")
	}
	return data, cfg.Width, cfg.Height, nil
}

// decodeThumbnailConfig validates a thumbnail raster (content decides the type).
func decodeThumbnailConfig(data []byte) (image.Config, string, error) {
	if len(data) == 0 {
		return image.Config{}, "", fmt.Errorf("The file is empty.")
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return image.Config{}, "", fmt.Errorf("Unsupported image type. Use PNG, JPEG, WebP, GIF or AVIF.")
	}
	if cfg.Width < 1 || cfg.Height < 1 || cfg.Width > 12000 || cfg.Height > 12000 {
		return image.Config{}, "", fmt.Errorf("Unsupported image dimensions.")
	}
	return cfg, format, nil
}

// templateThumbnailName is the storage path from gallery.models.template_thumbnail_path.
func templateThumbnailName(slug string) string {
	return templateThumbStorageAt + slug + ".webp"
}

// thumbnailLoader reads a thumbnail reference (a path next to the JSON file,
// or inside the embedded data tree).
type thumbnailLoader func(reference string) ([]byte, error)

// filesystemThumbnails resolves references relative to a directory, refusing
// paths that escape it (importer._read_thumbnail).
func filesystemThumbnails(baseDir string) thumbnailLoader {
	return func(reference string) ([]byte, error) {
		root, err := filepath.Abs(baseDir)
		if err != nil {
			return nil, templateImportErrorf(`"thumbnail" was not found next to the JSON file.`)
		}
		candidate := filepath.Join(root, filepath.FromSlash(reference))
		if !strings.HasPrefix(candidate, root+string(os.PathSeparator)) && candidate != root {
			return nil, templateImportErrorf(`Thumbnail "%s" was not found next to the JSON file.`, reference)
		}
		data, _, _, terr := templateThumbnail(candidate)
		if terr != nil {
			return nil, templateImportErrorf(`Thumbnail "%s": %v`, reference, terr)
		}
		return data, nil
	}
}

// treeThumbnails resolves references inside the embedded/disk data tree.
func treeThumbnails(tree templateTree) thumbnailLoader {
	return func(reference string) ([]byte, error) {
		data, err := tree.ReadFile(reference)
		if err != nil {
			return nil, templateImportErrorf(`Thumbnail "%s" was not found next to the JSON file.`, reference)
		}
		cfg, _, dims := decodeThumbnailConfig(data)
		if dims != nil {
			return nil, templateImportErrorf(`Thumbnail "%s": %v`, reference, dims)
		}
		if cfg.Width > templateThumbMaxSide || cfg.Height > templateThumbMaxSide {
			return nil, templateImportErrorf(`Thumbnail "%s": thumbnail-oversize`, reference)
		}
		return data, nil
	}
}

// importTemplate ports importer.import_template. Returns the template row's
// slug and whether it was created.
func (a *App) importTemplate(data map[string]any, loadThumbnail thumbnailLoader, publish *bool,
	createCategories bool, docNode *jsonNode,
) (slug string, created bool, err error) {
	parsed, perr := parseTemplateImport(data, docNode)
	if perr != nil {
		return "", false, perr
	}
	summary, verr := validateTemplateDocument(parsed.Document)
	if verr != nil {
		return "", false, templateImportErrorf("Invalid document: %v", verr)
	}
	var thumbBytes []byte
	if parsed.Thumbnail != nil {
		if loadThumbnail == nil {
			return "", false, templateImportErrorf(
				`"thumbnail" needs a JSON file to resolve its path against.`)
		}
		bytesOut, terr := loadThumbnail(*parsed.Thumbnail)
		if terr != nil {
			return "", false, terr
		}
		thumbBytes = bytesOut
	}

	tx, derr := a.DB.Begin()
	if derr != nil {
		return "", false, derr
	}
	defer func() { _ = tx.Rollback() }()
	now := formatDjangoDateTime(time.Now())
	var existingID string
	_ = tx.QueryRow(`SELECT id FROM gallery_template WHERE slug = ?`, parsed.Slug).Scan(&existingID)
	created = existingID == ""
	if created {
		existingID = newUUID4Hex()
		if _, ierr := tx.Exec(`INSERT INTO gallery_template
		     (id, created_at, updated_at, slug, title, description, tags, width, height, preset,
		      document, page_count, thumbnail, is_featured, is_published, use_count, category_id,
		      created_by_id)
		     VALUES (?, ?, ?, ?, ?, '', '[]', ?, ?, '', '{}', 1, '', FALSE, FALSE, 0, NULL, NULL)`,
			existingID, now, now, parsed.Slug, parsed.Title, summary.Width, summary.Height); ierr != nil {
			return "", false, ierr
		}
	}
	updates := []string{"title = ?", "width = ?", "height = ?", "page_count = ?", "updated_at = ?"}
	args := []any{parsed.Title, summary.Width, summary.Height, summary.PageCount, now}
	if parsed.Description != nil {
		updates = append(updates, "description = ?")
		args = append(args, *parsed.Description)
	}
	if parsed.HasTags {
		updates = append(updates, "tags = ?")
		args = append(args, pyJSONStringList(parsed.Tags))
	}
	if parsed.Category != nil {
		categoryID, catCreated, cerr := a.resolveTemplateCategory(tx, *parsed.Category, createCategories)
		if cerr != nil {
			return "", false, cerr
		}
		_ = catCreated
		updates = append(updates, "category_id = ?")
		args = append(args, categoryID)
	}
	if parsed.Preset != nil {
		updates = append(updates, "preset = ?")
		args = append(args, *parsed.Preset)
	} else if summary.Preset != "" {
		// apply_document mirrors the preset only when the row does not have one.
		updates = append(updates, "preset = CASE WHEN preset = '' THEN ? ELSE preset END")
		args = append(args, summary.Preset)
	}
	if parsed.Featured != nil {
		updates = append(updates, "is_featured = ?")
		args = append(args, *parsed.Featured)
	}
	effectivePublish := parsed.Published
	if publish != nil {
		effectivePublish = publish
	}
	if effectivePublish != nil {
		updates = append(updates, "is_published = ?")
		args = append(args, *effectivePublish)
	}
	updates = append(updates, "document = ?")
	args = append(args, parsed.DocumentRaw)
	if thumbBytes != nil {
		if serr := a.storeTemplateThumbnail(existingID, parsed.Slug, thumbBytes, tx, &updates, &args); serr != nil {
			return "", false, serr
		}
	}
	args = append(args, existingID)
	if _, uerr := tx.Exec(`UPDATE gallery_template SET `+strings.Join(updates, ", ")+` WHERE id = ?`,
		args...); uerr != nil {
		return "", false, uerr
	}
	if err := tx.Commit(); err != nil {
		return "", false, err
	}
	return parsed.Slug, created, nil
}

// docNodeOf rebuilds an ordered node for a description that arrived as a map
// (import_template keeps the file's own node instead — see parseTemplateImport).
func docNodeOf(data map[string]any) *jsonNode {
	document, ok := data["document"].(map[string]any)
	if !ok {
		return nil
	}
	encoded, err := json.Marshal(document)
	if err != nil {
		return nil
	}
	node, nerr := parseJSONNode(encoded)
	if nerr != nil {
		return nil
	}
	return node
}

// storeTemplateThumbnail writes the thumbnail file under MEDIA_ROOT and sets
// the column. Django's storage never overwrites: an existing name gets a
// random 7-character suffix, and replace_thumbnail then deletes the previous
// file when the name changed (so one file per template survives).
func (a *App) storeTemplateThumbnail(templateID, slug string, data []byte, tx *sql.Tx,
	updates *[]string, args *[]any,
) error {
	previous := ""
	_ = tx.QueryRow(`SELECT thumbnail FROM gallery_template WHERE id = ?`, templateID).Scan(&previous)
	name := availableThumbnailName(a.Settings.MediaRoot, templateThumbnailName(slug))
	absolute := filepath.Join(a.Settings.MediaRoot, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(absolute), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(absolute, data, 0o644); err != nil {
		return err
	}
	if previous != "" && previous != name {
		a.deleteMediaFile(previous)
	}
	*updates = append(*updates, "thumbnail = ?")
	*args = append(*args, name)
	return nil
}

// availableThumbnailName ports FileSystemStorage.get_available_name: a taken
// name gets "_" plus seven random characters before the extension.
func availableThumbnailName(mediaRoot, name string) string {
	exists := func(candidate string) bool {
		st, err := os.Stat(filepath.Join(mediaRoot, filepath.FromSlash(candidate)))
		return err == nil && st.Mode().IsRegular()
	}
	if !exists(name) {
		return name
	}
	dir, base := path.Split(name)
	ext := path.Ext(base)
	stem := strings.TrimSuffix(base, ext)
	for attempt := 0; attempt < 50; attempt++ {
		suffixed := dir + stem + "_" + randomAlphaNumeric(7) + ext
		if !exists(suffixed) {
			return suffixed
		}
	}
	return name
}

const alphaNumeric = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

// randomAlphaNumeric is Django's get_random_string alphabet (same source shape
// as the session key generator).
func randomAlphaNumeric(length int) string {
	rnd := make([]byte, length)
	if _, err := rand.Read(rnd); err != nil {
		panic(err)
	}
	out := make([]byte, length)
	for i, b := range rnd {
		out[i] = alphaNumeric[int(b)%len(alphaNumeric)]
	}
	return string(out)
}

// copyStarterPhoto copies one seeded photo into media storage (same-size files
// are kept) — install_photos.
func (a *App) copyStarterPhoto(sourcePath, name string) (copied bool, err error) {
	target := filepath.Join(a.Settings.MediaRoot, filepath.FromSlash(templatePhotoDir+name))
	if st, serr := os.Stat(target); serr == nil {
		if src, serr := os.Stat(sourcePath); serr == nil && src.Size() == st.Size() {
			return false, nil
		}
		_ = os.Remove(target)
	}
	data, rerr := os.ReadFile(sourcePath)
	if rerr != nil {
		return false, rerr
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return false, err
	}
	if err := os.WriteFile(target, data, 0o644); err != nil {
		return false, err
	}
	return true, nil
}

var _ = path.Ext
