// The gallery content commands (M13): seed_templates and import_template.
//
// seed_templates ships starter templates with Doory (`backend/data/templates`:
// manifest.json, one document per slug, thumbs/<slug>.webp, photos/*.webp).
// The Go binary embeds that tree byte-identically (doory/data/templates) and
// accepts --dir for an on-disk tree; every template goes through the same
// importer an operator uses with import_template.

package doory

import (
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

//go:embed data/templates
var templatesFS embed.FS

const (
	templatesManifestVersion = 1
	templatesMaxFileBytes    = 16 * 1024 * 1024
)

var starterPhotoName = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*\.webp$`)

// templateManifestEntry is one manifest entry (ENTRY_KEYS).
type templateManifestEntry struct {
	Slug        string   `json:"slug"`
	Title       string   `json:"title"`
	Category    string   `json:"category"`
	Tags        []string `json:"tags"`
	Description string   `json:"description"`
	Preset      string   `json:"preset"`
	IsFeatured  bool     `json:"is_featured"`
	Fonts       []string `json:"fonts"`
}

var templateEntryKeys = map[string]bool{
	"slug": true, "title": true, "category": true, "tags": true, "description": true,
	"preset": true, "is_featured": true, "fonts": true,
}

// templateTree abstracts the embedded and on-disk data directories.
type templateTree interface {
	ReadFile(name string) ([]byte, error)
	ReadDir(name string) ([]fs.DirEntry, error)
	Root() string
}

// embeddedTemplates is the binary's copy of backend/data/templates.
type embeddedTemplates struct{}

func (embeddedTemplates) Root() string { return "data/templates" }

func (embeddedTemplates) ReadFile(name string) ([]byte, error) {
	return templatesFS.ReadFile(path.Join("data/templates", name))
}

func (embeddedTemplates) ReadDir(name string) ([]fs.DirEntry, error) {
	return fs.ReadDir(templatesFS, path.Join("data/templates", name))
}

// diskTemplates is a --dir tree.
type diskTemplates struct{ dir string }

func (d diskTemplates) Root() string { return d.dir }

func (d diskTemplates) ReadFile(name string) ([]byte, error) {
	return os.ReadFile(filepath.Join(d.dir, filepath.FromSlash(name)))
}

func (d diskTemplates) ReadDir(name string) ([]fs.DirEntry, error) {
	return os.ReadDir(filepath.Join(d.dir, filepath.FromSlash(name)))
}

// loadTemplateManifest ports seed_templates.load_manifest.
func loadTemplateManifest(tree templateTree) ([]templateManifestEntry, error) {
	raw, err := tree.ReadFile("manifest.json")
	if err != nil {
		return nil, commandErrorf("manifest.json does not exist.")
	}
	if len(raw) > templatesMaxFileBytes {
		return nil, commandErrorf("manifest.json is larger than 16 MB.")
	}
	var doc struct {
		Version   int            `json:"version"`
		Templates []any          `json:"templates"`
		Extra     map[string]any `json:"-"`
	}
	var generic map[string]any
	if jerr := json.Unmarshal(raw, &generic); jerr != nil {
		return nil, commandErrorf("manifest.json is not valid JSON (%v).", jerr)
	}
	if jerr := json.Unmarshal(raw, &doc); jerr != nil {
		return nil, commandErrorf("manifest.json must be an object with version %d.", templatesManifestVersion)
	}
	if doc.Version != templatesManifestVersion {
		return nil, commandErrorf("manifest.json must be an object with version %d.", templatesManifestVersion)
	}
	entriesRaw, _ := generic["templates"].([]any)
	if len(entriesRaw) == 0 {
		return nil, commandErrorf("manifest.json needs a non-empty list of templates.")
	}
	seen := map[string]bool{}
	var entries []templateManifestEntry
	for _, rawEntry := range entriesRaw {
		entry, ok := rawEntry.(map[string]any)
		if !ok {
			return nil, commandErrorf("Every manifest entry must be an object.")
		}
		var unknown []string
		for key := range entry {
			if !templateEntryKeys[key] {
				unknown = append(unknown, key)
			}
		}
		if len(unknown) > 0 {
			sort.Strings(unknown)
			return nil, commandErrorf("Unknown manifest field(s): %s.", strings.Join(unknown, ", "))
		}
		slug, _ := entry["slug"].(string)
		if !templateSlugPattern.MatchString(slug) {
			return nil, commandErrorf("%q is not a valid template slug.", slug)
		}
		if seen[slug] {
			return nil, commandErrorf("The slug %s appears twice in the manifest.", slug)
		}
		seen[slug] = true
		parsed := templateManifestEntry{Slug: slug}
		for _, key := range []string{"title", "category", "description"} {
			value, isStr := entry[key].(string)
			if !isStr || strings.TrimSpace(value) == "" {
				return nil, commandErrorf("%s: %s must be a non-empty string.", slug, key)
			}
		}
		parsed.Title, _ = entry["title"].(string)
		parsed.Category, _ = entry["category"].(string)
		parsed.Description, _ = entry["description"].(string)
		tagsRaw, ok := entry["tags"].([]any)
		if !ok {
			return nil, commandErrorf("%s: tags must be a list of strings.", slug)
		}
		var tags []string
		for _, tag := range tagsRaw {
			s, isStr := tag.(string)
			if !isStr {
				return nil, commandErrorf("%s: tags must be a list of strings.", slug)
			}
			tags = append(tags, s)
		}
		parsed.Tags = tags
		if preset, ok := entry["preset"].(string); ok {
			parsed.Preset = preset
		}
		if featured, ok := entry["is_featured"].(bool); ok {
			parsed.IsFeatured = featured
		}
		if fontsRaw, present := entry["fonts"]; present {
			fonts, ok := fontsRaw.([]any)
			if !ok {
				return nil, commandErrorf("%s: fonts must be a list of family names.", slug)
			}
			for _, family := range fonts {
				s, isStr := family.(string)
				if !isStr {
					return nil, commandErrorf("%s: fonts must be a list of family names.", slug)
				}
				parsed.Fonts = append(parsed.Fonts, s)
			}
		}
		entries = append(entries, parsed)
	}
	return entries, nil
}

// loadTemplateDocument ports seed_templates.load_document; the node keeps the
// file's key order for storage.
func loadTemplateDocument(tree templateTree, slug string) (map[string]any, *jsonNode, error) {
	raw, err := tree.ReadFile(slug + ".json")
	if err != nil {
		return nil, nil, commandErrorf("%s.json does not exist.", slug)
	}
	if len(raw) > templatesMaxFileBytes {
		return nil, nil, commandErrorf("%s.json is larger than 16 MB.", slug)
	}
	var document map[string]any
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	if derr := dec.Decode(&document); derr != nil {
		return nil, nil, commandErrorf("%s.json is not valid JSON (%v).", slug, derr)
	}
	if _, verr := validateTemplateDocument(document); verr != nil {
		return nil, nil, commandErrorf("%s.json is not a valid design document: %v", slug, verr)
	}
	node, nerr := parseJSONNode(raw)
	if nerr != nil {
		return nil, nil, commandErrorf("%s.json is not valid JSON (%v).", slug, nerr)
	}
	return document, node, nil
}

// runSeedTemplates ports apps/gallery/management/commands/seed_templates.py.
func runSeedTemplates(c *CLI, argv []string) error {
	args, err := parseArgs(argv, []argSpec{
		{name: "update", kind: "bool"},
		{name: "dir", kind: "string", dest: "data_dir"},
	})
	if err != nil {
		return err
	}
	var tree templateTree = embeddedTemplates{}
	dirLabel := "backend/data/templates"
	if dir := args.str("data_dir"); dir != "" {
		st, serr := os.Stat(dir)
		if serr != nil || !st.IsDir() {
			return commandErrorf("%s does not exist.", dir)
		}
		tree = diskTemplates{dir: dir}
		dirLabel = dir
	}
	_ = dirLabel
	entries, merr := loadTemplateManifest(tree)
	if merr != nil {
		return merr
	}
	documents := map[string]map[string]any{}
	documentNodes := map[string]*jsonNode{}
	for _, entry := range entries {
		document, node, derr := loadTemplateDocument(tree, entry.Slug)
		if derr != nil {
			return derr
		}
		documents[entry.Slug] = document
		documentNodes[entry.Slug] = node
	}

	// seed_templates calls seed_gallery first (its line is part of the output).
	report, gerr := c.App.seedGalleryCategories(false)
	if gerr != nil {
		return commandErrorf("%v", gerr)
	}
	c.Printf("Template categories: %d created, %d updated, %d unchanged.\n",
		report.Created, report.Updated, report.Unchanged)

	copied, kept := c.App.installStarterPhotos(tree)
	if copied > 0 || kept > 0 {
		c.Printf("Starter photos: %d copied, %d already stored.\n", copied, kept)
	}
	shipped := map[string]bool{}
	if dirEntries, derr := tree.ReadDir("photos"); derr == nil {
		for _, entry := range dirEntries {
			if !entry.IsDir() && starterPhotoName.MatchString(entry.Name()) {
				shipped[entry.Name()] = true
			}
		}
	}
	existing := map[string]bool{}
	rows, qerr := c.App.DB.Query(`SELECT slug FROM gallery_template`)
	if qerr == nil {
		for rows.Next() {
			var slug string
			if err := rows.Scan(&slug); err == nil {
				existing[slug] = true
			}
		}
		rows.Close()
	}

	update := args.bool("update")
	created, updated, keptCount := 0, 0, 0
	var failures []string
	for _, entry := range entries {
		missing := missingStarterPhotos(documents[entry.Slug], shipped)
		if len(missing) > 0 {
			msg := fmt.Sprintf("%s: missing photos %s", entry.Slug, strings.Join(missing, ", "))
			failures = append(failures, msg)
			c.Errorf("FAILED  %s: missing photos %s\n", entry.Slug, strings.Join(missing, ", "))
			continue
		}
		if existing[entry.Slug] && !update {
			keptCount++
			c.Printf("kept    %s\n", entry.Slug)
			continue
		}
		published := true
		description := entry.Description
		data := map[string]any{
			"slug":         entry.Slug,
			"title":        entry.Title,
			"category":     entry.Category,
			"description":  description,
			"tags":         stringSliceToAny(entry.Tags),
			"preset":       entry.Preset,
			"is_featured":  entry.IsFeatured,
			"is_published": published,
			"document":     documents[entry.Slug],
			"thumbnail":    "thumbs/" + entry.Slug + ".webp",
		}
		_, wasCreated, ierr := c.App.importTemplate(data, treeThumbnails(tree), nil, false,
			documentNodes[entry.Slug])
		if ierr != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", entry.Slug, ierr))
			c.Errorf("FAILED  %s: %v\n", entry.Slug, ierr)
			continue
		}
		if wasCreated {
			created++
			c.Printf("%-8s%s\n", "created", entry.Slug)
		} else {
			updated++
			c.Printf("%-8s%s\n", "updated", entry.Slug)
		}
	}
	c.Printf("Starter templates: %d created, %d updated, %d kept, %d failed.\n",
		created, updated, keptCount, len(failures))
	families := map[string]bool{}
	for _, entry := range entries {
		for _, family := range entry.Fonts {
			families[family] = true
		}
	}
	if absent := c.App.missingFontFamilies(families); len(absent) > 0 {
		quoted := make([]string, len(absent))
		for i, family := range absent {
			quoted[i] = `"` + family + `"`
		}
		c.Printf("These fonts are not installed, so the templates use a fallback until they are: "+
			"%s. Install them with: doory sync_fonts %s\n", strings.Join(absent, ", "), strings.Join(quoted, " "))
	}
	if len(failures) > 0 {
		return commandErrorf("%d template(s) could not be imported.", len(failures))
	}
	return nil
}

// installStarterPhotos ports install_photos: copy `photos/*.webp` into media
// storage; returns (copied, kept).
func (a *App) installStarterPhotos(tree templateTree) (int, int) {
	entries, err := tree.ReadDir("photos")
	if err != nil {
		return 0, 0
	}
	copied, kept := 0, 0
	for _, entry := range entries {
		if entry.IsDir() || !starterPhotoName.MatchString(entry.Name()) {
			continue
		}
		data, rerr := tree.ReadFile("photos/" + entry.Name())
		if rerr != nil {
			continue
		}
		wasCopied, cerr := a.writeStarterPhoto(entry.Name(), data)
		if cerr != nil {
			continue
		}
		if wasCopied {
			copied++
		} else {
			kept++
		}
	}
	return copied, kept
}

// writeStarterPhoto writes one photo (same-size files are kept).
func (a *App) writeStarterPhoto(name string, data []byte) (bool, error) {
	target := filepath.Join(a.Settings.MediaRoot, filepath.FromSlash(templatePhotoDir+name))
	if st, serr := os.Stat(target); serr == nil && st.Size() == int64(len(data)) {
		return false, nil
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return false, err
	}
	if err := os.WriteFile(target, data, 0o644); err != nil {
		return false, err
	}
	return true, nil
}

// missingStarterPhotos is photos_used minus the shipped files.
func missingStarterPhotos(document map[string]any, shipped map[string]bool) []string {
	used := photosUsedInDocument(document)
	var missing []string
	for name := range used {
		if !shipped[name] {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)
	return missing
}

// photosUsedInDocument ports seed_templates.photos_used.
func photosUsedInDocument(document map[string]any) map[string]bool {
	const prefix = mediaURL + templatePhotoDir
	found := map[string]bool{}
	var stack []any
	stack = append(stack, document)
	for len(stack) > 0 {
		node := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		switch value := node.(type) {
		case map[string]any:
			if src, ok := value["src"].(string); ok && strings.HasPrefix(src, prefix) {
				found[src[len(prefix):]] = true
			}
			for _, item := range value {
				stack = append(stack, item)
			}
		case []any:
			stack = append(stack, value...)
		}
	}
	return found
}

// missingFontFamilies ports seed_templates.missing_fonts: families the
// templates use that the published catalogue lacks.
func (a *App) missingFontFamilies(families map[string]bool) []string {
	if len(families) == 0 {
		return nil
	}
	installed := map[string]bool{}
	rows, err := a.DB.Query(`SELECT DISTINCT family FROM fonts_fontfamily
	      WHERE owner_id IS NULL AND is_published = 1`)
	if err == nil {
		for rows.Next() {
			var family string
			if err := rows.Scan(&family); err == nil {
				installed[family] = true
			}
		}
		rows.Close()
	}
	var missing []string
	for family := range families {
		if !installed[family] {
			missing = append(missing, family)
		}
	}
	sort.Strings(missing)
	return missing
}

func stringSliceToAny(values []string) []any {
	out := make([]any, len(values))
	for i, value := range values {
		out[i] = value
	}
	return out
}

// runImportTemplate ports apps/gallery/management/commands/import_template.py.
func runImportTemplate(c *CLI, argv []string) error {
	args, err := parseArgs(argv, []argSpec{
		{name: "publish", kind: "bool"},
		{name: "create-categories", kind: "bool", dest: "create_categories"},
	})
	if err != nil {
		return err
	}
	if len(args.positionals) == 0 {
		return &usageError{message: "the following arguments are required: paths"}
	}
	files, ferr := templateJSONFiles(args.positionals)
	if ferr != nil {
		return ferr
	}
	if len(files) == 0 {
		return commandErrorf("No .json files found.")
	}
	created, updated := 0, 0
	var failures []string
	for _, file := range files {
		data, node, derr := readTemplateJSONFile(file)
		if derr != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", file, derr))
			c.Errorf("FAILED  %s: %v\n", file, derr)
			continue
		}
		var publish *bool
		if args.bool("publish") {
			yes := true
			publish = &yes
		}
		slug, wasCreated, ierr := c.App.importTemplate(data, filesystemThumbnails(filepath.Dir(file)),
			publish, args.bool("create_categories"), node)
		if ierr != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", file, ierr))
			c.Errorf("FAILED  %s: %v\n", file, ierr)
			continue
		}
		state := "draft"
		var published int
		_ = c.App.DB.QueryRow(`SELECT is_published FROM gallery_template WHERE slug = ?`, slug).Scan(&published)
		if published == 1 {
			state = "published"
		}
		if wasCreated {
			created++
			c.Printf("%-8s%s (%s)\n", "created", slug, state)
		} else {
			updated++
			c.Printf("%-8s%s (%s)\n", "updated", slug, state)
		}
	}
	c.Printf("Done: %d created, %d updated, %d failed.\n", created, updated, len(failures))
	if len(failures) > 0 {
		return commandErrorf("%d file(s) could not be imported.", len(failures))
	}
	return nil
}

// templateJSONFiles ports import_template._json_files.
func templateJSONFiles(paths []string) ([]string, error) {
	var files []string
	for _, raw := range paths {
		st, err := os.Stat(raw)
		if err != nil {
			return nil, commandErrorf("%s: no such file or directory.", raw)
		}
		if !st.IsDir() {
			files = append(files, raw)
			continue
		}
		var found []string
		_ = filepath.WalkDir(raw, func(p string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return nil
			}
			if !d.IsDir() && filepath.Ext(p) == ".json" && d.Type()&fs.ModeSymlink == 0 {
				found = append(found, p)
			}
			return nil
		})
		sort.Strings(found)
		files = append(files, found...)
	}
	return files, nil
}

// readTemplateJSONFile ports the size + JSON checks of import_template; the
// ordered node keeps the document's key order for storage.
func readTemplateJSONFile(file string) (map[string]any, *jsonNode, error) {
	st, err := os.Stat(file)
	if err != nil {
		return nil, nil, commandErrorf("%s: no such file or directory.", file)
	}
	if st.Size() > templatesMaxFileBytes {
		return nil, nil, templateImportErrorf("The file is larger than 16 MB.")
	}
	raw, rerr := os.ReadFile(file)
	if rerr != nil {
		return nil, nil, rerr
	}
	var data map[string]any
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	if derr := dec.Decode(&data); derr != nil {
		return nil, nil, templateImportErrorf("Not valid JSON (%v).", derr)
	}
	root, nerr := parseJSONNode(raw)
	if nerr != nil {
		return nil, nil, templateImportErrorf("Not valid JSON (%v).", nerr)
	}
	return data, nodeField(root, "document"), nil
}
