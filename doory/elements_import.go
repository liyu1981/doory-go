// Importing SVG into element packs: the importer (apps/elements/importer.py),
// the sanctioned bulk path (bulk.py) and the pack sync pipeline
// (readers.py, packsync.py).
//
// Every stored SVG goes through the sanitiser: the file written is the
// sanitised output, never the input. Imports are batched and each batch
// commits, so an interrupted run keeps what it finished and a repeat run only
// does the remainder.

package doory

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"

	"doory-go/internal/svg"
)

const (
	elementMaxKeyLength  = 200
	elementMaxNameLength = 120
	elementMaxSvgBytes   = 5 * 1024 * 1024
	elementBatchSize     = 500
)

// elementImportEntry is bulk.ImportEntry.
type elementImportEntry struct {
	Key string
	// Label is the file's relative path (progress lines print it).
	Label     string
	Name      string
	SVG       []byte
	Kind      string
	Style     string
	Tags      []string
	SortOrder int
}

// elementImportSummary is importer.ImportSummary.
// skipLabel names an entry the way its source does: the file label when the
// import came from files (admin bulk upload, directory walk), else the key.
func (e elementImportEntry) skipLabel() string {
	if e.Label != "" {
		return e.Label
	}
	return e.Key
}

type elementImportSummary struct {
	Created   int
	Updated   int
	Unchanged int
	Skipped   [][2]string
}

func (s elementImportSummary) String() string {
	return fmt.Sprintf("%d created, %d updated, %d unchanged, %d skipped",
		s.Created, s.Updated, s.Unchanged, len(s.Skipped))
}

func (s *elementImportSummary) record(outcome string) {
	switch outcome {
	case "created":
		s.Created++
	case "updated":
		s.Updated++
	default:
		s.Unchanged++
	}
}

// processedSVG is svgdata.ProcessedSvg.
type processedSVG struct {
	Data   []byte
	Width  float64
	Height float64
	Colors []string
}

func (p processedSVG) isRecolorable() bool { return len(p.Colors) > 0 }

// processSVG is svgdata.process_svg: sanitise, then derive size and palette.
func processSVG(raw []byte) (processedSVG, error) {
	clean, err := svg.Sanitize(raw, false)
	if err != nil {
		return processedSVG{}, err
	}
	info, ierr := svg.Info(clean)
	if ierr != nil {
		return processedSVG{}, ierr
	}
	colors, cerr := svg.Colors(clean)
	if cerr != nil {
		return processedSVG{}, cerr
	}
	return processedSVG{Data: clean, Width: info.Width, Height: info.Height, Colors: colors}, nil
}

// ---- naming (importer.py) ----------------------------------------------------------------------------------

var elementWordPattern = regexp.MustCompile(`[^\W\d_]{2,}`)

// elementName is importer.element_name: `arrow-right_2` -> `Arrow Right 2`.
func elementName(stem string) string {
	words := regexp.MustCompile(`[-_\s]+`).Split(strings.TrimSpace(stem), -1)
	var parts []string
	for _, word := range words {
		if word == "" {
			continue
		}
		runes := []rune(word)
		parts = append(parts, strings.ToUpper(string(runes[0]))+string(runes[1:]))
	}
	name := strings.Join(parts, " ")
	if len([]rune(name)) > 120 {
		name = string([]rune(name)[:120])
	}
	if name == "" {
		return "Element"
	}
	return name
}

// elementTagsFrom is importer.element_tags: folder names and name words.
func elementTagsFrom(directories []string, name string) []string {
	seen := map[string]bool{}
	var tags []string
	add := func(text string) {
		for _, word := range elementWordPattern.FindAllString(strings.ToLower(text), -1) {
			if !seen[word] {
				seen[word] = true
				tags = append(tags, word)
			}
		}
	}
	for _, directory := range directories {
		add(directory)
	}
	add(name)
	if len(tags) > 20 {
		tags = tags[:20]
	}
	return tags
}

// elementKey is importer.element_key: the relative path without extension,
// each part slugified.
func elementKey(rel string) (string, error) {
	noExt := strings.TrimSuffix(filepath.ToSlash(rel), filepath.Ext(rel))
	parts := strings.Split(noExt, "/")
	for i, part := range parts {
		parts[i] = slugifyElementKeyPart(part)
		if parts[i] == "" {
			return "", fmt.Errorf("the file name has no usable letters or digits")
		}
	}
	key := strings.Join(parts, "/")
	if len(key) > elementMaxKeyLength {
		return "", fmt.Errorf("the path is too long")
	}
	return key, nil
}

// slugifyElementKeyPart is django.utils.text.slugify(part, allow_unicode=True).
func slugifyElementKeyPart(value string) string {
	lowered := strings.ToLower(strings.TrimSpace(value))
	var out strings.Builder
	dash := false
	for _, r := range lowered {
		if isUnicodeWordRune(r) {
			out.WriteRune(r)
			dash = false
			continue
		}
		if !dash {
			out.WriteByte('-')
			dash = true
		}
	}
	return strings.Trim(out.String(), "-_")
}

// isUnicodeWordRune reports letters, digits and underscores (Python's \w).
func isUnicodeWordRune(r rune) bool {
	if r == '_' || (r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
		return true
	}
	return r > 0x7F && (isLetterOrDigit(r))
}

// isLetterOrDigit is the Unicode check slugify needs for non-ASCII names.
func isLetterOrDigit(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r)
}

// ---- packing the artwork -----------------------------------------------------------------------------------

// elementItem is packsync._Item.
type elementItem struct {
	Key       string
	Name      string
	SVG       []byte
	Tags      []string
	Canonical bool
}

// elementPackFiles loads the files of one pack from the right source: the
// embedded pinned artwork for generated sets, or the given source.
func (a *App) elementPackFiles(pack elementPackSpec, source packSource, generated bool,
) (map[string][]byte, error) {
	if generated {
		spec, ok := pack.Source.(generatedSource)
		if !ok {
			return nil, packSourceErrorf("%s is not a generated pack.", pack.ID)
		}
		return generatedFiles(spec)
	}
	if source == nil {
		return nil, packSourceErrorf("%s needs a download source; run sync_element_packs.", pack.ID)
	}
	return source.fetch(pack.Source, pack.patterns())
}

// checkPackLicense is packsync.check_license.
func checkPackLicense(pack elementPackSpec, files map[string][]byte) error {
	spec := pack.License
	if spec.File == "" {
		return nil
	}
	data, ok := files[spec.File]
	if !ok {
		return packSourceErrorf("The licence file %s is missing from the release.", spec.File)
	}
	match, err := regexp.Compile(spec.Match)
	if err != nil || !match.Match(data) {
		return packSourceErrorf("%s no longer matches /%s/: the licence of this release may have changed. "+
			"Read it, then update the pack's license entry in the manifest.", spec.File, spec.Match)
	}
	return nil
}

// stripNaming is packsync._strip.
func stripNaming(stem string, pack elementPackSpec) string {
	if pack.Naming.StripPrefix != "" && strings.HasPrefix(stem, pack.Naming.StripPrefix) {
		stem = stem[len(pack.Naming.StripPrefix):]
	}
	if pack.Naming.StripSuffix != "" && strings.HasSuffix(stem, pack.Naming.StripSuffix) {
		stem = stem[:len(stem)-len(pack.Naming.StripSuffix)]
	}
	return stem
}

// svgPathsFor is packsync._svg_paths.
func svgPathsFor(pack elementPackSpec, files map[string][]byte) []string {
	include := matcher(pack.Files.Include)
	exclude := matcher(pack.Files.Exclude)
	var out []string
	for path := range files {
		if include(path) && !exclude(path) {
			out = append(out, path)
		}
	}
	sort.Strings(out)
	return out
}

// tagTableFor is packsync._tag_table.
func tagTableFor(pack elementPackSpec, files map[string][]byte, warnings *[]string) map[string][]string {
	if pack.Tags.Format == "none" {
		return nil
	}
	data, ok := files[pack.Tags.File]
	if !ok {
		*warnings = append(*warnings, fmt.Sprintf(
			"tag file %s not found in the release; using name words only", pack.Tags.File))
		return nil
	}
	table, err := parseElementTags(pack.Tags.Format, data)
	if err != nil {
		*warnings = append(*warnings, fmt.Sprintf("tag file %s: %v; using name words only", pack.Tags.File, err))
		return nil
	}
	return table
}

// nameWords is packsync._name_words.
func nameWords(key string) []string {
	var out []string
	for _, word := range regexp.MustCompile(`[-_/\s]+`).Split(strings.ToLower(key), -1) {
		if len(word) > 1 && !allDigits(word) {
			out = append(out, word)
		}
	}
	return out
}

func allDigits(text string) bool {
	for _, r := range text {
		if r < '0' || r > '9' {
			return false
		}
	}
	return text != ""
}

// packItems is packsync._items (the non-generated readers).
func packItems(pack elementPackSpec, files map[string][]byte, warnings *[]string,
	brandLogos bool,
) ([]elementItem, [][2]string) {
	var skipped [][2]string
	table := tagTableFor(pack, files, warnings)
	var skipRe, brandRe *elementPattern
	if pack.Naming.Skip != "" {
		skipRe, _ = compileElementPattern(pack.Naming.Skip)
	}
	if pack.Naming.Brand != "" {
		brandRe, _ = compileElementPattern(pack.Naming.Brand)
	}
	wanted := func(key string) bool {
		if skipRe != nil && skipRe.match(key) {
			return false
		}
		if brandRe != nil && brandRe.match(key) && !brandLogos {
			return false
		}
		return true
	}
	extra := func(key string) []string {
		if brandRe != nil && brandRe.match(key) {
			return []string{"brand", "logo"}
		}
		return nil
	}
	var items []elementItem
	if pack.Files.Reader == "iconify-json" {
		for _, path := range svgPathsFor(pack, files) {
			icons, err := readIconifyJSON(files[path])
			if err != nil {
				skipped = append(skipped, [2]string{path, err.Error()})
				continue
			}
			for _, icon := range icons {
				key := slugifyElementKeyPart(icon.Name)
				if key == "" || !wanted(key) {
					continue
				}
				hints := append(extra(key), icon.AliasesAsWords()...)
				items = append(items, buildElementItem(key, icon.Name, icon.SVG, table, hints))
			}
		}
		return items, skipped
	}
	hintsByStem := map[string][]string{}
	if src, ok := pack.Source.(urlsSource); ok {
		for _, file := range src.Files {
			hintsByStem[strings.TrimSuffix(file.Name, ".svg")] = file.Tags
		}
	}
	for _, path := range svgPathsFor(pack, files) {
		stem := stripNaming(strings.TrimSuffix(filepath.Base(path), filepath.Ext(path)), pack)
		key := slugifyElementKeyPart(stem)
		switch {
		case key == "":
			skipped = append(skipped, [2]string{path, "the file name has no usable letters or digits"})
		case !wanted(key):
			// left out on purpose
		case len(files[path]) > 2*1024*1024:
			skipped = append(skipped, [2]string{path, "the file is larger than 2 MB"})
		default:
			hints := append([]string{}, extra(key)...)
			hints = append(hints, hintsByStem[stem]...)
			items = append(items, buildElementItem(key, stem, files[path], table, hints))
		}
	}
	return items, skipped
}

// buildElementItem is packsync._item.
func buildElementItem(key, stem string, raw []byte, table map[string][]string, hints []string) elementItem {
	var brand []string
	for _, hint := range hints {
		if hint == "brand" || hint == "logo" {
			brand = append(brand, hint)
		}
	}
	candidates := append([]string{}, brand...)
	candidates = append(candidates, table[stem]...)
	candidates = append(candidates, hints...)
	candidates = append(candidates, nameWords(key)...)
	_, hasTable := table[stem]
	return elementItem{
		Key: key, Name: elementName(stem), SVG: raw,
		Tags:      cleanElementTags(candidates, elementMaxTags),
		Canonical: hasTable || len(table) == 0,
	}
}

// generatedItems is packsync._generated_items: names and tags from the
// artwork's folders and file names (the artwork is already final).
func generatedItems(files map[string][]byte) ([]elementItem, [][2]string) {
	var items []elementItem
	var skipped [][2]string
	paths := make([]string, 0, len(files))
	for path := range files {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, path := range paths {
		key, err := elementKey(path)
		if err != nil {
			skipped = append(skipped, [2]string{path, err.Error()})
			continue
		}
		stem := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
		name := elementName(stem)
		directories := strings.Split(filepath.ToSlash(filepath.Dir(path)), "/")
		if len(directories) == 1 && directories[0] == "." {
			directories = nil
		}
		items = append(items, elementItem{Key: key, Name: name, SVG: files[path],
			Tags: elementTagsFrom(directories, name)})
	}
	return items, skipped
}

// buildElementEntries is packsync.build_entries: normalise the pack's files
// into import entries, merging entries that end up identical.
func buildElementEntries(pack elementPackSpec, files map[string][]byte, warnings *[]string,
	brandLogos bool,
) ([]elementImportEntry, [][2]string, int) {
	if err := checkPackLicense(pack, files); err != nil {
		return nil, nil, 0
	}
	var items []elementItem
	var skipped [][2]string
	if pack.isGenerated() {
		items, skipped = generatedItems(files)
	} else {
		items, skipped = packItems(pack, files, warnings, brandLogos)
	}
	type preparedItem struct {
		item elementItem
		data []byte
	}
	var prepared []preparedItem
	for _, item := range items {
		data := item.SVG
		if !pack.isGenerated() {
			normalized, err := normalizeSVG(item.SVG, pack.Normalize)
			if err != nil {
				skipped = append(skipped, [2]string{item.Key, err.Error()})
				continue
			}
			data = normalized
		}
		prepared = append(prepared, preparedItem{item: item, data: data})
	}
	groups := map[string][]preparedItem{}
	var digests []string
	for _, entry := range prepared {
		sum := sha256.Sum256(entry.data)
		digest := hex.EncodeToString(sum[:])
		if _, seen := groups[digest]; !seen {
			digests = append(digests, digest)
		}
		groups[digest] = append(groups[digest], entry)
	}
	type chosenItem struct {
		item   elementItem
		data   []byte
		merged []elementItem
	}
	var chosen []chosenItem
	duplicates := 0
	for _, digest := range digests {
		group := groups[digest]
		sort.SliceStable(group, func(i, j int) bool {
			if group[i].item.Canonical != group[j].item.Canonical {
				return group[i].item.Canonical
			}
			return group[i].item.Key < group[j].item.Key
		})
		merged := make([]elementItem, 0, len(group)-1)
		for _, other := range group[1:] {
			merged = append(merged, other.item)
		}
		chosen = append(chosen, chosenItem{item: group[0].item, data: group[0].data, merged: merged})
		duplicates += len(group) - 1
	}
	sort.SliceStable(chosen, func(i, j int) bool { return chosen[i].item.Key < chosen[j].item.Key })
	entries := make([]elementImportEntry, 0, len(chosen))
	for _, row := range chosen {
		tags := append([]string{}, row.item.Tags...)
		for _, other := range row.merged {
			tags = append(tags, other.Name)
			tags = append(tags, other.Tags...)
		}
		entries = append(entries, elementImportEntry{
			Key: row.item.Key, Name: row.item.Name, SVG: row.data, Style: pack.Style,
			Tags: cleanElementTags(tags, elementMaxTags),
		})
	}
	return entries, skipped, duplicates
}

// ---- database side -----------------------------------------------------------------------------------------

// ensureElementPack is importer.ensure_pack.
func (a *App) ensureElementPack(slug string, name, kind, license, sourceURL, description string,
	sortOrder int, licenseURL, attribution string,
) (string, bool, error) {
	if kind != "" && !containsString(elementKindValues, kind) {
		return "", false, fmt.Errorf("kind must be one of: %s.", strings.Join(elementKindValues, ", "))
	}
	var id string
	_ = a.DB.QueryRow(`SELECT id FROM elements_elementpack WHERE slug = ?`, slug).Scan(&id)
	created := id == ""
	now := formatDjangoDateTime(time.Now())
	if created {
		if license == "" {
			return "", false, fmt.Errorf("A licence (SPDX identifier) is required to create a new pack.")
		}
		if name == "" {
			name = elementName(slug)
		}
		id = newUUID4Hex()
		kindArg := "shape"
		if kind != "" {
			kindArg = kind
		}
		if _, err := a.DB.Exec(`INSERT INTO elements_elementpack
		     (id, created_at, updated_at, slug, name, kind, license, license_url, attribution,
		      source_url, source_ref, sync_fingerprint, synced_at, description, thumbnail,
		      sort_order, is_published)
		     VALUES (?, ?, ?, ?, ?, ?, ?, '', '', '', '', '', NULL, '', '', 0, TRUE)`,
			id, now, now, slug, name, kindArg, license); err != nil {
			return "", false, err
		}
	}
	updates := []string{}
	args := []any{}
	set := func(column string, value any) {
		updates = append(updates, column+" = ?")
		args = append(args, value)
	}
	if name != "" {
		set("name", name)
	}
	if kind != "" {
		set("kind", kind)
	}
	if license != "" {
		set("license", license)
	}
	if sourceURL != "" {
		set("source_url", sourceURL)
	}
	if description != "" {
		set("description", description)
	}
	if sortOrder != 0 {
		set("sort_order", sortOrder)
	}
	if licenseURL != "" {
		set("license_url", licenseURL)
	}
	if attribution != "" {
		set("attribution", attribution)
	}
	if len(updates) > 0 {
		updates = append(updates, "updated_at = ?")
		args = append(args, now, id)
		if _, err := a.DB.Exec(`UPDATE elements_elementpack SET `+strings.Join(updates, ", ")+
			` WHERE id = ?`, args...); err != nil {
			return "", false, err
		}
	}
	return id, created, nil
}

// elementRow is the fields bulk_import compares.
type elementRow struct {
	ID            string
	Key           string
	Name          string
	Kind          string
	Style         string
	Tags          []string
	SortOrder     int
	Width         float64
	Height        float64
	Colors        []string
	IsRecolorable bool
	ContentHash   string
	SVG           string
}

// loadElementRows reads the pack's rows for the given keys.
func (a *App) loadElementRows(packID string, keys []string) (map[string]*elementRow, error) {
	if len(keys) == 0 {
		return map[string]*elementRow{}, nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(keys)), ",")
	args := make([]any, 0, len(keys)+1)
	args = append(args, packID)
	for _, key := range keys {
		args = append(args, key)
	}
	rows, err := a.DB.Query(`SELECT id, key, name, kind, style, tags, sort_order, width, height, colors,
	      is_recolorable, content_hash, svg FROM elements_element
	      WHERE pack_id = ? AND key IN (`+placeholders+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]*elementRow{}
	for rows.Next() {
		var row elementRow
		var tags, colors string
		if err := rows.Scan(&row.ID, &row.Key, &row.Name, &row.Kind, &row.Style, &tags, &row.SortOrder,
			&row.Width, &row.Height, &colors, &row.IsRecolorable, &row.ContentHash, &row.SVG); err != nil {
			return nil, err
		}
		row.Tags = parseJSONStringList(tags)
		row.Colors = parseJSONStringList(colors)
		out[row.Key] = &row
	}
	return out, rows.Err()
}

// bulkImport is bulk.bulk_import: create or refresh the entries of a pack.
// `progress` (when set) receives the running total after every batch, like
// bulk_import's progress callback.
func (a *App) bulkImport(packID, packSlug string, entries []elementImportEntry, packKind string,
	dryRun bool, progress func(elementImportSummary),
) (elementImportSummary, error) {
	return a.bulkImportWithOutcomes(packID, packSlug, entries, packKind, dryRun, progress, nil)
}

// bulkImportWithOutcomes is bulkImport and also reports each key's outcome
// (created/updated/unchanged) in the order the entries were given.
func (a *App) bulkImportWithOutcomes(packID, packSlug string, entries []elementImportEntry, packKind string,
	dryRun bool, progress func(elementImportSummary), outcomes map[string]string,
) (elementImportSummary, error) {
	var summary elementImportSummary
	seen := map[string]bool{}
	for start := 0; start < len(entries); start += elementBatchSize {
		end := start + elementBatchSize
		if end > len(entries) {
			end = len(entries)
		}
		batch := entries[start:end]
		// Prepare (sanitise) the batch.
		var prepared []preparedBatchItem
		for _, entry := range batch {
			if entry.Key == "" || len(entry.Key) > elementMaxKeyLength {
				summary.Skipped = append(summary.Skipped, [2]string{entry.skipLabel(), "the key is empty or too long"})
				continue
			}
			if strings.TrimSpace(entry.Name) == "" {
				summary.Skipped = append(summary.Skipped, [2]string{entry.skipLabel(), "the name is empty"})
				continue
			}
			if seen[entry.Key] {
				summary.Skipped = append(summary.Skipped, [2]string{entry.skipLabel(), "duplicate key"})
				continue
			}
			kind := entry.Kind
			if kind == "" {
				kind = packKind
			}
			if !containsString(elementKindValues, kind) {
				summary.Skipped = append(summary.Skipped, [2]string{entry.skipLabel(),
					fmt.Sprintf("unknown kind %q", kind)})
				continue
			}
			if entry.Style != "" && !containsString(elementStyleValues, entry.Style) {
				summary.Skipped = append(summary.Skipped, [2]string{entry.skipLabel(),
					fmt.Sprintf("unknown style %q", entry.Style)})
				continue
			}
			clean, err := processSVG(entry.SVG)
			if err != nil {
				summary.Skipped = append(summary.Skipped, [2]string{entry.skipLabel(), err.Error()})
				continue
			}
			sum := sha256.Sum256(clean.Data)
			seen[entry.Key] = true
			prepared = append(prepared, preparedBatchItem{entry: entry, clean: clean, kind: kind,
				tags: cleanElementTags(entry.Tags, elementMaxTags), hash: hex.EncodeToString(sum[:])})
		}
		if len(prepared) == 0 {
			continue
		}
		keys := make([]string, len(prepared))
		for i, item := range prepared {
			keys[i] = item.entry.Key
		}
		existing, err := a.loadElementRows(packID, keys)
		if err != nil {
			return summary, err
		}
		var plans []elementPlanned
		for _, item := range prepared {
			name := strings.TrimSpace(item.entry.Name)
			if len([]rune(name)) > elementMaxNameLength {
				name = string([]rune(name)[:elementMaxNameLength])
			}
			item.entry.Name = name
			current, ok := existing[item.entry.Key]
			if !ok {
				plans = append(plans, elementPlanned{row: nil, item: item})
				summary.Created++
				if outcomes != nil {
					outcomes[item.entry.Key] = "created"
				}
				continue
			}
			currentTags := strings.Join(current.Tags, "\x00")
			same := current.ContentHash == item.hash && current.Name == item.entry.Name &&
				current.Kind == item.kind && current.Style == item.entry.Style &&
				currentTags == strings.Join(item.tags, "\x00") && current.SortOrder == item.entry.SortOrder
			if same {
				summary.Unchanged++
				if outcomes != nil {
					outcomes[item.entry.Key] = "unchanged"
				}
				continue
			}
			plans = append(plans, elementPlanned{row: current, item: item})
			summary.Updated++
			if outcomes != nil {
				outcomes[item.entry.Key] = "updated"
			}
		}
		if dryRun {
			if progress != nil {
				progress(summary)
			}
			continue
		}
		if len(plans) == 0 {
			if progress != nil {
				progress(summary)
			}
			continue
		}
		writes := make([]elementWrite, 0, len(plans))
		for _, planned := range plans {
			write := elementWrite{
				Key: planned.item.entry.Key, Name: planned.item.entry.Name, Kind: planned.item.kind,
				Style: planned.item.entry.Style, Tags: planned.item.tags,
				SortOrder: planned.item.entry.SortOrder, Width: planned.item.clean.Width,
				Height: planned.item.clean.Height, Colors: planned.item.clean.Colors,
				IsRecolorable: planned.item.clean.isRecolorable(), ContentHash: planned.item.hash,
				Data: planned.item.clean.Data, PackID: packID, IsNew: planned.row == nil,
			}
			if planned.row != nil {
				write.ID = planned.row.ID
				write.IsNew = false
			}
			writes = append(writes, write)
		}
		if err := a.writeElementPlans(packSlug, writes); err != nil {
			return summary, err
		}
		if progress != nil {
			progress(summary)
		}
	}
	return summary, nil
}

// elementWrite is one row to write (create or update) with its sanitised SVG.
type elementWrite struct {
	ID            string
	PackID        string
	Key           string
	Name          string
	Kind          string
	Style         string
	Tags          []string
	SortOrder     int
	Width         float64
	Height        float64
	Colors        []string
	IsRecolorable bool
	ContentHash   string
	Data          []byte
	IsNew         bool
}

// elementPlanned is one prepared batch entry with the row it replaces (nil for
// a new element).
type elementPlanned struct {
	row  *elementRow
	item preparedBatchItem
}

// preparedBatchItem is one sanitised entry of a batch.
type preparedBatchItem struct {
	entry elementImportEntry
	clean processedSVG
	kind  string
	tags  []string
	hash  string
}

// writeElementPlans writes the rows and their SVG files (one transaction; the
// stored file is the sanitised output).
func (a *App) writeElementPlans(packSlug string, writes []elementWrite) error {
	tx, err := a.DB.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	now := formatDjangoDateTime(time.Now())
	var written []string
	cleanup := func() {
		for _, name := range written {
			a.deleteMediaFile(name)
		}
	}
	for _, write := range writes {
		id := write.ID
		storage := "elements/" + packSlug + "/" + dashedUUID(id) + ".svg"
		if write.IsNew {
			id = newUUID4Hex()
			storage = "elements/" + packSlug + "/" + dashedUUID(id) + ".svg"
		}
		absolute := filepath.Join(a.Settings.MediaRoot, filepath.FromSlash(storage))
		if err := os.MkdirAll(filepath.Dir(absolute), 0o755); err != nil {
			cleanup()
			return err
		}
		if err := os.WriteFile(absolute, write.Data, 0o644); err != nil {
			cleanup()
			return err
		}
		written = append(written, storage)
		tagsJSON := pyJSONStringList(write.Tags)
		colorsJSON := pyJSONStringList(write.Colors)
		searchName := elementSearchName(write.Name)
		searchTags := elementSearchTags(write.Tags)
		if write.IsNew {
			if _, ierr := tx.Exec(`INSERT INTO elements_element
			     (id, created_at, updated_at, key, name, kind, style, tags, search_name, search_tags,
			      content_hash, svg, width, height, colors, is_recolorable, is_published, sort_order, pack_id)
			     VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, TRUE, ?, ?)`,
				id, now, now, write.Key, write.Name, write.Kind, write.Style, tagsJSON, searchName,
				searchTags, write.ContentHash, storage, write.Width, write.Height, colorsJSON,
				write.IsRecolorable, write.SortOrder, write.PackID); ierr != nil {
				cleanup()
				return ierr
			}
			continue
		}
		if _, uerr := tx.Exec(`UPDATE elements_element SET name = ?, kind = ?, style = ?, tags = ?,
		     search_name = ?, search_tags = ?, content_hash = ?, svg = ?, width = ?, height = ?,
		     colors = ?, is_recolorable = ?, sort_order = ?, updated_at = ?
		     WHERE id = ?`,
			write.Name, write.Kind, write.Style, tagsJSON, searchName, searchTags, write.ContentHash,
			storage, write.Width, write.Height, colorsJSON, write.IsRecolorable,
			write.SortOrder, now, id); uerr != nil {
			cleanup()
			return uerr
		}
	}
	if err := tx.Commit(); err != nil {
		cleanup()
		return err
	}
	return nil
}
