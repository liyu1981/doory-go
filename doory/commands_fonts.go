// sync_fonts (apps/fonts/sync.py + catalog.py): register open-licensed
// (OFL/Apache/Ubuntu) font files as built-in families.
//
// The offline path (`--from-dir`) is the deterministic one the cli-driver
// exercises: already-downloaded Fontsource files named
// `<id>-<subset>-<weight>-<style>.woff2`. The network path (Fontsource via
// jsDelivr) has the same plan/store/register pipeline with an HTTP source and
// the same per-family output; its request shape lives in fontsSource below.

package doory

import (
	"crypto/tls"
	_ "embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed data/popular_fonts.json
var popularFontsJSON []byte

//go:embed data/unicode_ranges.json
var unicodeRangesJSON []byte

const (
	fontMaxWOFF2Bytes  = 5 * 1024 * 1024
	fontMinWOFF2Bytes  = 64
	fontUnranked       = 9999
	fontSourceName     = "fontsource"
	fontVariantFormat  = "woff2"
	fontDefaultLicense = "OFL-1.1"
)

// curatedFont is catalog.CuratedFont.
type curatedFont struct {
	ID         string   `json:"id"`
	Family     string   `json:"family"`
	Category   string   `json:"category"`
	Scripts    []string `json:"scripts"`
	Popularity int      `json:"popularity"`
}

var (
	curatedFontsOnce sync.Once
	curatedFontsList []curatedFont
	curatedFontsErr  error
	unicodeRangeMap  map[string]string
	unicodeOnce      sync.Once
)

func loadCuratedFonts() ([]curatedFont, error) {
	curatedFontsOnce.Do(func() {
		var rows []curatedFont
		if err := json.Unmarshal(popularFontsJSON, &rows); err != nil {
			curatedFontsErr = fmt.Errorf("Cannot read popular_fonts.json: %v", err)
			return
		}
		for _, row := range rows {
			if !containsString(fontCategories(), row.Category) {
				curatedFontsErr = fmt.Errorf("%s: unknown category %q", row.ID, row.Category)
				return
			}
		}
		sort.SliceStable(rows, func(i, j int) bool { return rows[i].Popularity < rows[j].Popularity })
		curatedFontsList = rows
	})
	return curatedFontsList, curatedFontsErr
}

func loadUnicodeRanges() map[string]string {
	unicodeOnce.Do(func() {
		unicodeRangeMap = map[string]string{}
		var raw map[string]any
		if err := json.Unmarshal(unicodeRangesJSON, &raw); err == nil {
			for key, value := range raw {
				unicodeRangeMap[key] = fmt.Sprint(value)
			}
		}
	})
	return unicodeRangeMap
}

func curatedByID() (map[string]curatedFont, error) {
	fonts, err := loadCuratedFonts()
	if err != nil {
		return nil, err
	}
	out := map[string]curatedFont{}
	for _, font := range fonts {
		out[font.ID] = font
	}
	return out, nil
}

// fontSubsetNames is sync.SUBSET_NAMES (the alternation of the FILENAME regex).
const fontSubsetNames = "latin-ext|latin|cyrillic-ext|cyrillic|greek-ext|greek|vietnamese|devanagari|bengali|" +
	"arabic|hebrew|thai|korean|japanese|chinese-simplified|chinese-traditional|chinese-hongkong|armenian|" +
	"ethiopic|georgian|gujarati|gurmukhi|kannada|khmer|lao|malayalam|myanmar|sinhala|tamil|telugu|math|symbols"

var fontFilename = regexp.MustCompile(
	`^(?P<id>[a-z0-9][a-z0-9-]*?)-(?P<subset>` + fontSubsetNames + `)-(?P<weight>[1-9]00)-(?P<style>normal|italic)\.woff2$`)

var permissiveLicense = regexp.MustCompile(`^(?i)(ofl|sil|apache|ufl|ubuntu)`)

var fontIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)

// systemFontPath is sync.system_file_path (storage path; the row's file name).
func systemFontPath(fontID, subset string, weight int, style string) string {
	return fmt.Sprintf("fonts/system/%s/%s-%s-%d-%s.woff2", fontID, fontID, subset, weight, style)
}

// checkWOFF2 is sync.check_woff2.
func checkWOFF2(data []byte) string {
	if len(data) < fontMinWOFF2Bytes || len(data) > fontMaxWOFF2Bytes {
		return "unexpected file size"
	}
	if len(data) < 4 || string(data[:4]) != "wOF2" {
		return "not a woff2 file"
	}
	return ""
}

// subsetSortKey is sync.subset_sort_key (latin first, then latin-ext).
func subsetSortKey(subset string) (int, string) {
	switch subset {
	case "latin":
		return 0, subset
	case "latin-ext":
		return 1, subset
	}
	return 2, subset
}

// fontSyncReport is sync.SyncReport (the counters the command prints).
type fontSyncReport struct {
	Downloaded int
	Existing   int
	Families   int
	Skipped    int
	NotFound   []string
	Aborted    string
	Failures   []string
}

// ensureFontFamily is sync.ensure_family: get or create the built-in family,
// refreshing its catalogue metadata.
func (a *App) ensureFontFamily(fontID, family, category, license string, rank int) (string, error) {
	var id string
	_ = a.DB.QueryRow(`SELECT id FROM fonts_fontfamily WHERE owner_id IS NULL AND slug = ?`,
		fontID).Scan(&id)
	if id == "" {
		_ = a.DB.QueryRow(`SELECT id FROM fonts_fontfamily WHERE owner_id IS NULL AND family = ?`,
			family).Scan(&id)
	}
	now := formatDjangoDateTime(time.Now())
	if license == "" {
		license = fontDefaultLicense
	}
	if id == "" {
		id = newUUID4Hex()
		if _, err := a.DB.Exec(`INSERT INTO fonts_fontfamily
		     (id, created_at, updated_at, family, slug, category, scripts, is_variable, popularity,
		      license, source, is_published, owner_id)
		     VALUES (?, ?, ?, ?, ?, ?, '[]', FALSE, ?, ?, ?, TRUE, NULL)`,
			id, now, now, family, fontID, category, rank, license, fontSourceName); err != nil {
			return "", err
		}
		return id, nil
	}
	if _, err := a.DB.Exec(`UPDATE fonts_fontfamily SET slug = ?, family = ?, category = ?, license = ?,
	     source = ?, popularity = ?, updated_at = ? WHERE id = ?`,
		fontID, family, category, license, fontSourceName, rank, now, id); err != nil {
		return "", err
	}
	return id, nil
}

// storeFontVariant is sync.store_variant: write the file, then the row.
func (a *App) storeFontVariant(familyID, fontID, subset string, weight int, style string, data []byte,
	unicodeRange string,
) error {
	storage := systemFontPath(fontID, subset, weight, style)
	absolute := filepath.Join(a.Settings.MediaRoot, filepath.FromSlash(storage))
	if err := os.MkdirAll(filepath.Dir(absolute), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(absolute, data, 0o644); err != nil {
		return err
	}
	now := formatDjangoDateTime(time.Now())
	var variantID string
	_ = a.DB.QueryRow(`SELECT id FROM fonts_fontvariant WHERE family_id = ? AND weight = ? AND style = ?
	      AND subset = ?`, familyID, weight, style, subset).Scan(&variantID)
	if variantID == "" {
		variantID = newUUID4Hex()
		_, err := a.DB.Exec(`INSERT INTO fonts_fontvariant
		     (id, created_at, updated_at, weight, style, subset, unicode_range, file, format, size, family_id)
		     VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			variantID, now, now, weight, style, subset, unicodeRange, storage, fontVariantFormat, len(data),
			familyID)
		return err
	}
	_, err := a.DB.Exec(`UPDATE fonts_fontvariant SET unicode_range = ?, file = ?, format = ?, size = ?,
	     updated_at = ? WHERE id = ?`, unicodeRange, storage, fontVariantFormat, len(data), now, variantID)
	return err
}

// refreshFontScripts is sync.refresh_scripts: the family's scripts follow the
// stored subsets, ordered latin first.
func (a *App) refreshFontScripts(familyID string) error {
	rows, err := a.DB.Query(`SELECT DISTINCT subset FROM fonts_fontvariant WHERE family_id = ?`, familyID)
	if err != nil {
		return err
	}
	var subsets []string
	for rows.Next() {
		var subset string
		if err := rows.Scan(&subset); err == nil && subset != "" {
			subsets = append(subsets, subset)
		}
	}
	rows.Close()
	sort.Slice(subsets, func(i, j int) bool {
		oi, si := subsetSortKey(subsets[i])
		oj, sj := subsetSortKey(subsets[j])
		if oi != oj {
			return oi < oj
		}
		return si < sj
	})
	var current string
	_ = a.DB.QueryRow(`SELECT scripts FROM fonts_fontfamily WHERE id = ?`, familyID).Scan(&current)
	encoded := pyJSONStringList(subsets)
	if current != encoded {
		_, err = a.DB.Exec(`UPDATE fonts_fontfamily SET scripts = ?, updated_at = ? WHERE id = ?`,
			encoded, formatDjangoDateTime(time.Now()), familyID)
	}
	return err
}

// importFontDirectory is sync.import_font_directory: register already
// downloaded Fontsource files, offline.
func (a *App) importFontDirectory(directory, licenseName string, log func(string)) (*fontSyncReport, error) {
	curated, cerr := curatedByID()
	if cerr != nil {
		return nil, cerr
	}
	ranges := loadUnicodeRanges()
	if licenseName == "" {
		licenseName = fontDefaultLicense
	}
	type fontFile struct {
		path   string
		subset string
		weight int
		style  string
	}
	groups := map[string][]fontFile{}
	order := []string{}
	walkErr := filepath.WalkDir(directory, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() || d.Type()&fs.ModeSymlink != 0 || filepath.Ext(path) != ".woff2" {
			return nil
		}
		name := filepath.Base(path)
		match := fontFilename.FindStringSubmatch(name)
		if match == nil {
			log(fmt.Sprintf("skipped %s: does not match <id>-<subset>-<weight>-<style>.woff2", name))
			return nil
		}
		fontID := match[fontFilename.SubexpIndex("id")]
		weight, _ := strconv.Atoi(match[fontFilename.SubexpIndex("weight")])
		if _, seen := groups[fontID]; !seen {
			order = append(order, fontID)
		}
		groups[fontID] = append(groups[fontID], fontFile{
			path: path, subset: match[fontFilename.SubexpIndex("subset")], weight: weight,
			style: match[fontFilename.SubexpIndex("style")],
		})
		return nil
	})
	if walkErr != nil {
		return nil, walkErr
	}
	report := &fontSyncReport{}
	for _, fontID := range order {
		files := groups[fontID]
		sort.Slice(files, func(i, j int) bool { return files[i].path < files[j].path })
		entry, curatedEntry := curated[fontID]
		familyID := ""
		downloaded, existing := 0, 0
		var rejected []string
		for _, file := range files {
			name := filepath.Base(file.path)
			st, serr := os.Stat(file.path)
			var data []byte
			if serr == nil && st.Size() <= fontMaxWOFF2Bytes {
				data, _ = os.ReadFile(file.path)
			}
			if problem := checkWOFF2(data); problem != "" {
				rejected = append(rejected, fmt.Sprintf("%s: %s", name, problem))
				continue
			}
			if familyID == "" {
				familyName := fontID
				category := "display" // normalize_category(None)
				rank := fontUnranked
				if curatedEntry {
					familyName = entry.Family
					category = entry.Category
					rank = entry.Popularity
				} else {
					familyName = titleFromSlug(fontID)
				}
				var ferr error
				familyID, ferr = a.ensureFontFamily(fontID, familyName, category, licenseName, rank)
				if ferr != nil {
					return nil, ferr
				}
			}
			storage := systemFontPath(fontID, file.subset, file.weight, file.style)
			absolute := filepath.Join(a.Settings.MediaRoot, filepath.FromSlash(storage))
			upToDate := false
			if st, err := os.Stat(absolute); err == nil {
				var currentSize int
				_ = a.DB.QueryRow(`SELECT size FROM fonts_fontvariant WHERE family_id = ? AND weight = ?
				      AND style = ? AND subset = ?`,
					familyID, file.weight, file.style, file.subset).Scan(&currentSize)
				if currentSize == len(data) && st.Size() == int64(len(data)) {
					upToDate = true
				}
			}
			if upToDate {
				existing++
				continue
			}
			if err := a.storeFontVariant(familyID, fontID, file.subset, file.weight, file.style, data,
				ranges[file.subset]); err != nil {
				return nil, err
			}
			downloaded++
		}
		if familyID != "" {
			if err := a.refreshFontScripts(familyID); err != nil {
				return nil, err
			}
		}
		report.Families++
		report.Downloaded += downloaded
		report.Existing += existing
		report.Failures = append(report.Failures, rejected...)
		log(fmt.Sprintf("%s: %d imported, %d already stored, %d rejected", fontID, downloaded, existing,
			len(rejected)))
		for _, problem := range rejected {
			log("    ! " + problem)
		}
	}
	return report, nil
}

// titleFromSlug is sync's fallback name (id.replace("-", " ").title()).
func titleFromSlug(fontID string) string {
	words := strings.Split(strings.ReplaceAll(fontID, "-", " "), " ")
	for i, word := range words {
		if word == "" {
			continue
		}
		words[i] = strings.ToUpper(word[:1]) + word[1:]
	}
	return strings.Join(words, " ")
}

// runSyncFonts ports apps/fonts/management/commands/sync_fonts.py.
func runSyncFonts(c *CLI, argv []string) error {
	args, err := parseArgs(argv, []argSpec{
		{name: "all", kind: "bool"},
		{name: "popular", kind: "int"},
		{name: "from-dir", kind: "string", dest: "from_dir"},
		{name: "subsets", kind: "string"},
		{name: "weights", kind: "string"},
		{name: "styles", kind: "string"},
		{name: "concurrency", kind: "int"},
		{name: "license", kind: "string"},
	})
	if err != nil {
		return err
	}
	if fromDir := args.str("from_dir"); fromDir != "" {
		st, serr := os.Stat(fromDir)
		if serr != nil || !st.IsDir() {
			return commandErrorf("%s: not a directory.", fromDir)
		}
		licenseName := args.str("license")
		if licenseName == "" {
			licenseName = "OFL-1.1"
		}
		report, ierr := c.App.importFontDirectory(fromDir, licenseName, c.Println)
		if ierr != nil {
			return commandErrorf("%v", ierr)
		}
		if report.Families == 0 {
			return commandErrorf("No files named <id>-<subset>-<weight>-<style>.woff2 were found " +
				"(for example open-sans-latin-400-normal.woff2).")
		}
		c.Printf("Done: %d files imported, %d already stored, %d rejected.\n",
			report.Downloaded, report.Existing, len(report.Failures))
		if len(report.Failures) > 0 {
			return commandErrorf("%d file(s) were rejected.", len(report.Failures))
		}
		return nil
	}
	popular := args.intOption("popular", 0)
	if args.has("popular") && popular < 1 {
		return commandErrorf("--popular needs a positive number.")
	}
	if len(args.positionals) == 0 && !args.bool("all") && !args.has("popular") {
		return commandErrorf("Name families, or pass --popular N, --all or --from-dir DIR.")
	}
	styles := splitCSV(args.str("styles"))
	if args.str("styles") == "" {
		styles = []string{"normal", "italic"}
	}
	if len(styles) == 0 || !subsetOf(styles, []string{"normal", "italic"}) {
		return commandErrorf("--styles must be normal, italic or both.")
	}
	weights, werr := parseFontWeights(args.str("weights"))
	if werr != nil {
		return werr
	}
	ids, ierr := resolveFontIDs(args.positionals, args.bool("all"), popular)
	if ierr != nil {
		return commandErrorf("%v", ierr)
	}
	subsets := []string{"auto"} // the flag default
	if args.has("subsets") {
		subsets = splitCSV(args.str("subsets"))
		if len(subsets) == 0 {
			return commandErrorf("--subsets needs at least one subset, e.g. latin.")
		}
	}
	weightWords := make([]string, len(weights))
	for i, weight := range weights {
		weightWords[i] = strconv.Itoa(weight)
	}
	c.Printf("Syncing %d families (subsets %s; weights %s; styles %s; %d at a time).\n",
		len(ids), strings.Join(subsets, ","), strings.Join(weightWords, ","), strings.Join(styles, ","),
		maxInt(1, clampInt(args.intOption("concurrency", 4), 1, 8)))
	options := fontSyncOptions{Subsets: subsets, Weights: weights, Styles: styles}
	report, serr := c.App.syncFontFamilies(newFontHTTPClient(), ids, options, c.Println)
	if serr != nil {
		return commandErrorf("%v", serr)
	}
	c.Printf("Done: %d files downloaded, %d already stored, %d failed, %d families skipped.\n",
		report.Downloaded, report.Existing, len(report.Failures), report.Skipped)
	if report.Aborted != "" {
		return commandErrorf("%s. Check your network or proxy settings (HTTPS_PROXY), or download the "+
			"files elsewhere and import them with --from-dir. Progress is kept; run again to resume.",
			report.Aborted)
	}
	if len(report.Failures) > 0 {
		return commandErrorf("%d file(s) failed. Run the same command again to retry them.",
			len(report.Failures))
	}
	return nil
}

// fontSyncOptions is sync.SyncOptions.
type fontSyncOptions struct {
	Subsets []string
	Weights []int
	Styles  []string
}

// clampInt bounds a value between low and high.
func clampInt(value, low, high int) int {
	if value < low {
		return low
	}
	if value > high {
		return high
	}
	return value
}

func splitCSV(raw string) []string {
	var out []string
	for _, part := range strings.Split(raw, ",") {
		trimmed := strings.TrimSpace(part)
		if trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

func subsetOf(values, allowed []string) bool {
	for _, value := range values {
		if !containsString(allowed, value) {
			return false
		}
	}
	return true
}

// parseFontWeights ports _weights.
func parseFontWeights(raw string) ([]int, error) {
	if raw == "" {
		return []int{400, 700}, nil
	}
	var weights []int
	for _, part := range splitCSV(raw) {
		value, err := strconv.Atoi(part)
		if err != nil {
			return nil, commandErrorf("--weights must be a comma-separated list such as 400,700.")
		}
		if value < 100 || value > 900 || value%100 != 0 {
			return nil, commandErrorf("--weights values must be 100, 200, … 900.")
		}
		weights = append(weights, value)
	}
	if len(weights) == 0 {
		return nil, commandErrorf("--weights must be a comma-separated list such as 400,700.")
	}
	return weights, nil
}

// resolveFontIDs ports sync.resolve_ids and the command's --popular selection
// (curated ranks 1..N plus the editor and script fonts).
func resolveFontIDs(names []string, all bool, popular int) ([]string, error) {
	curated, err := loadCuratedFonts()
	if err != nil {
		return nil, err
	}
	byName := map[string]string{}
	for _, font := range curated {
		byName[strings.ToLower(font.Family)] = font.ID
	}
	var ids []string
	if all || popular > 0 {
		limit := len(curated)
		if popular > 0 && popular < limit {
			limit = popular
		}
		seen := map[string]bool{}
		add := func(id string) {
			if id != "" && !seen[id] {
				seen[id] = true
				ids = append(ids, id)
			}
		}
		for _, font := range curated {
			if font.Popularity <= limit {
				add(font.ID)
			}
		}
		// --popular always adds the editor's default fonts and the script faces.
		for _, family := range append(append([]string{}, editorFonts...), scriptFonts...) {
			if id := byName[strings.ToLower(family)]; id != "" {
				add(id)
			} else {
				add(slugifyFontID(family))
			}
		}
		return ids, nil
	}
	for _, name := range names {
		cleaned := strings.TrimSpace(name)
		if id, ok := byName[strings.ToLower(cleaned)]; ok {
			ids = append(ids, id)
			continue
		}
		ids = append(ids, slugifyFontID(cleaned))
	}
	return ids, nil
}

// slugifyFontID is the command's re.sub(r"[^a-z0-9]+", "-", name.lower()).strip("-").
func slugifyFontID(value string) string {
	lowered := strings.ToLower(strings.TrimSpace(value))
	var out strings.Builder
	lastDash := false
	for _, r := range lowered {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			out.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash {
			out.WriteByte('-')
			lastDash = true
		}
	}
	return strings.Trim(out.String(), "-")
}

// editorFonts / scriptFonts are catalog.EDITOR_FONTS / SCRIPT_FONTS.
var editorFonts = []string{
	"Plus Jakarta Sans", "Inter", "Playfair Display", "DM Sans", "Montserrat", "Open Sans", "Lora",
	"Lato", "Pacifico", "Poppins", "Bebas Neue", "Roboto", "Cormorant Garamond",
}

var scriptFonts = []string{
	"Cairo", "Noto Sans Arabic", "Amiri", "Heebo", "Noto Sans Hebrew", "Hind Siliguri",
	"Noto Sans Bengali", "Noto Sans Devanagari", "Noto Sans Thai",
}

// syncFontFamilies ports sync.sync_families over the Fontsource source.
func (a *App) syncFontFamilies(source *fontHTTPClient, ids []string, options fontSyncOptions,
	log func(string),
) (*fontSyncReport, error) {
	report := &fontSyncReport{}
	total := len(ids)
	width := len(strconv.Itoa(total))
	for index, fontID := range ids {
		if !fontIDPattern.MatchString(fontID) {
			report.NotFound = append(report.NotFound, fontID)
			log(fmt.Sprintf("[%*d/%d] %s: not a valid font id, skipped", width, index+1, total, fontID))
			continue
		}
		result, err := a.syncFontFamily(source, fontID, options)
		if err != nil {
			var unavailable *fontSourceUnavailable
			if asFontSourceUnavailable(err, &unavailable) {
				report.Aborted = err.Error()
				log(fmt.Sprintf("[%*d/%d] %s: %v. Stopping; run again to resume.",
					width, index+1, total, fontID, err))
				break
			}
			return nil, err
		}
		report.Downloaded += result.downloaded
		report.Existing += result.existing
		if result.skipped != "" {
			report.Skipped++
			if strings.HasPrefix(result.skipped, "not found") {
				report.NotFound = append(report.NotFound, fontID)
			}
			log(fmt.Sprintf("[%*d/%d] %s: skipped (%s)", width, index+1, total, fontID, result.skipped))
			continue
		}
		line := fmt.Sprintf("[%*d/%d] %s: %d downloaded, %d already stored",
			width, index+1, total, fontID, result.downloaded, result.existing)
		if result.notOffered > 0 {
			line += fmt.Sprintf(", %d not offered", result.notOffered)
		}
		if len(result.failed) > 0 {
			line += fmt.Sprintf(", %d FAILED", len(result.failed))
		}
		log(line)
		for _, problem := range result.failed {
			log("    ! " + problem)
		}
		report.Failures = append(report.Failures, result.failed...)
	}
	return report, nil
}

// fontFamilyResult is sync.FamilyResult.
type fontFamilyResult struct {
	downloaded int
	existing   int
	notOffered int
	failed     []string
	skipped    string
}

// syncFontFamily ports sync.sync_family for one family (metadata -> licence
// gate -> plan -> fetch -> store).
func (a *App) syncFontFamily(source *fontHTTPClient, fontID string, options fontSyncOptions,
) (*fontFamilyResult, error) {
	result := &fontFamilyResult{}
	meta, err := source.metadata(fontID)
	if err != nil {
		var notFound *fontNotFound
		if asFontNotFound(err, &notFound) {
			result.skipped = "not found at the source"
			return result, nil
		}
		return nil, err
	}
	if !meta.isFont {
		result.skipped = "not a text font"
		return result, nil
	}
	if !permissiveLicense.MatchString(strings.TrimSpace(meta.license)) {
		license := meta.license
		if license == "" {
			license = "unknown"
		}
		result.skipped = fmt.Sprintf("licence %q is not OFL/Apache/Ubuntu", license)
		return result, nil
	}
	curated, cerr := curatedByID()
	if cerr != nil {
		return nil, cerr
	}
	entry, known := curated[fontID]
	category := meta.category
	rank := fontUnranked
	if known {
		category = entry.Category
		rank = entry.Popularity
	}
	var familyID string
	_ = a.DB.QueryRow(`SELECT id FROM fonts_fontfamily WHERE owner_id IS NULL AND slug = ?`,
		fontID).Scan(&familyID)
	type triple struct {
		weight int
		style  string
		subset string
	}
	stored := map[triple]bool{}
	if familyID != "" {
		rows, qerr := a.DB.Query(`SELECT weight, style, subset, file FROM fonts_fontvariant WHERE family_id = ?`,
			familyID)
		if qerr == nil {
			for rows.Next() {
				var weight int
				var style, subset, file string
				if err := rows.Scan(&weight, &style, &subset, &file); err == nil {
					if fileExists(filepath.Join(a.Settings.MediaRoot, filepath.FromSlash(file))) {
						stored[triple{weight, style, subset}] = true
					}
				}
			}
			rows.Close()
		}
	}
	ranges := loadUnicodeRanges()
	var todo []triple
	for _, subset := range subsetsFor(meta, options) {
		for _, weight := range options.Weights {
			for _, style := range options.Styles {
				if !meta.hasWeight(weight) || !containsString(meta.styles, style) {
					continue
				}
				if meta.available != nil && !meta.available[fontTriple{weight, style, subset}] {
					continue
				}
				if stored[triple{weight, style, subset}] {
					result.existing++
					continue
				}
				todo = append(todo, triple{weight, style, subset})
			}
		}
	}
	for _, entry := range todo {
		data, ferr := source.fetch(meta, entry.subset, entry.weight, entry.style)
		if ferr != nil {
			var notOffered *fontFileNotAvailable
			if asFontFileNotAvailable(ferr, &notOffered) {
				result.notOffered++
				continue
			}
			result.failed = append(result.failed, fmt.Sprintf("%s %d %s: %v", entry.subset, entry.weight,
				entry.style, ferr))
			continue
		}
		if problem := checkWOFF2(data); problem != "" {
			result.failed = append(result.failed, fmt.Sprintf("%s %d %s: %s", entry.subset, entry.weight,
				entry.style, problem))
			continue
		}
		if familyID == "" {
			familyID, err = a.ensureFontFamily(fontID, meta.family, category, meta.license, rank)
			if err != nil {
				return nil, err
			}
		}
		unicodeRange := meta.unicodeRanges[entry.subset]
		if unicodeRange == "" {
			unicodeRange = ranges[entry.subset]
		}
		if err := a.storeFontVariant(familyID, fontID, entry.subset, entry.weight, entry.style, data,
			unicodeRange); err != nil {
			return nil, err
		}
		result.downloaded++
	}
	if familyID != "" {
		if err := a.refreshFontScripts(familyID); err != nil {
			return nil, err
		}
	}
	return result, nil
}

// subsetsFor ports sync._subsets_for ("auto" = every offered subset except CJK/math).
func subsetsFor(meta *fontFamilyMeta, options fontSyncOptions) []string {
	autoSkipped := map[string]bool{
		"japanese": true, "korean": true, "chinese-simplified": true, "chinese-traditional": true,
		"chinese-hongkong": true, "math": true, "symbols": true,
	}
	var wanted []string
	for _, subset := range options.Subsets {
		if subset == "auto" {
			for _, offered := range meta.subsets {
				if !autoSkipped[offered] && !containsString(wanted, offered) {
					wanted = append(wanted, offered)
				}
			}
			continue
		}
		if !containsString(wanted, subset) {
			wanted = append(wanted, subset)
		}
	}
	sort.Slice(wanted, func(i, j int) bool {
		oi, si := subsetSortKey(wanted[i])
		oj, sj := subsetSortKey(wanted[j])
		if oi != oj {
			return oi < oj
		}
		return si < sj
	})
	return wanted
}

// ------------------------------------------------------- Fontsource source ---

// fontFamilyMeta is sources.FamilyMeta (the fields the pipeline uses).
type fontFamilyMeta struct {
	id            string
	family        string
	category      string
	license       string
	isFont        bool
	subsets       []string
	weights       []int
	styles        []string
	unicodeRanges map[string]string
	available     map[fontTriple]bool
}

func (m *fontFamilyMeta) hasWeight(weight int) bool {
	for _, candidate := range m.weights {
		if candidate == weight {
			return true
		}
	}
	return false
}

// containsInt reports membership for integer slices.
func containsInt(values []int, want int) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

type fontTriple struct {
	weight int
	style  string
	subset string
}

// fontNotFound / fontFileNotAvailable / fontSourceUnavailable mirror sources.py.
type fontNotFound struct{ id string }

func (e *fontNotFound) Error() string { return fmt.Sprintf("font %s not found", e.id) }

type fontFileNotAvailable struct{ what string }

func (e *fontFileNotAvailable) Error() string { return "not offered: " + e.what }

type fontSourceUnavailable struct{ message string }

func (e *fontSourceUnavailable) Error() string { return e.message }

func asFontNotFound(err error, target **fontNotFound) bool {
	e, ok := err.(*fontNotFound)
	if ok {
		*target = e
	}
	return ok
}

func asFontFileNotAvailable(err error, target **fontFileNotAvailable) bool {
	e, ok := err.(*fontFileNotAvailable)
	if ok {
		*target = e
	}
	return ok
}

func asFontSourceUnavailable(err error, target **fontSourceUnavailable) bool {
	e, ok := err.(*fontSourceUnavailable)
	if ok {
		*target = e
	}
	return ok
}

// fontHTTPClient talks to the Fontsource API and jsDelivr (sources.py
// FontsourceSource): fixed hosts, bounded retries, hard timeouts, size caps.
type fontHTTPClient struct {
	client *http.Client
}

func newFontHTTPClient() *fontHTTPClient {
	return &fontHTTPClient{client: &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12},
			Proxy:           http.ProxyFromEnvironment,
		},
	}}
}

const fontsourceAPI = "https://api.fontsource.org/v1"

// metadata fetches /v1/fonts/<id> and maps the fields the pipeline uses.
func (s *fontHTTPClient) metadata(fontID string) (*fontFamilyMeta, error) {
	body, err := s.get(fontsourceAPI + "/fonts/" + url.PathEscape(fontID))
	if err != nil {
		return nil, err
	}
	var payload struct {
		ID         string            `json:"id"`
		Family     string            `json:"family"`
		Category   string            `json:"category"`
		License    any               `json:"license"`
		Type       string            `json:"type"`
		Subsets    []string          `json:"subsets"`
		Weights    []int             `json:"weights"`
		Styles     []string          `json:"styles"`
		UnicodeRng map[string]string `json:"unicodeRange"`
		Variants   []struct {
			Subset string `json:"subset"`
			Weight int    `json:"weight"`
			Style  string `json:"style"`
		} `json:"variants"`
	}
	if jerr := json.Unmarshal(body, &payload); jerr != nil {
		return nil, &fontSourceUnavailable{message: "The font source answered unexpectedly."}
	}
	license := ""
	switch value := payload.License.(type) {
	case string:
		license = strings.TrimSpace(value)
	case map[string]any:
		if text, ok := value["type"].(string); ok {
			license = strings.TrimSpace(text)
		} else if text, ok := value["id"].(string); ok {
			license = strings.TrimSpace(text)
		}
	}
	meta := &fontFamilyMeta{
		id: payload.ID, family: payload.Family, category: normalizeFontCategory(payload.Category),
		license: license, isFont: payload.Type == "" || payload.Type == "font",
		subsets: payload.Subsets, weights: payload.Weights, styles: payload.Styles,
		unicodeRanges: payload.UnicodeRng,
	}
	if len(payload.Variants) > 0 {
		meta.available = map[fontTriple]bool{}
		for _, variant := range payload.Variants {
			meta.available[fontTriple{variant.Weight, variant.Style, variant.Subset}] = true
		}
	}
	return meta, nil
}

// fetch downloads one subset/weight/style file from jsDelivr.
func (s *fontHTTPClient) fetch(meta *fontFamilyMeta, subset string, weight int, style string) ([]byte, error) {
	fileName := fmt.Sprintf("%s-%s-%d-%s.woff2", meta.id, subset, weight, style)
	endpoint := fmt.Sprintf("https://cdn.jsdelivr.net/fontsource/fonts/%s@latest/%s",
		url.PathEscape(meta.id), url.PathEscape(fileName))
	body, err := s.get(endpoint)
	if err != nil {
		var unavailable *fontSourceUnavailable
		if asFontSourceUnavailable(err, &unavailable) {
			return nil, err
		}
		return nil, &fontFileNotAvailable{what: fileName}
	}
	if len(body) > fontMaxWOFF2Bytes {
		return nil, fmt.Errorf("unexpected file size")
	}
	return body, nil
}

func (s *fontHTTPClient) get(rawURL string) ([]byte, error) {
	response, err := s.client.Get(rawURL)
	if err != nil {
		return nil, &fontSourceUnavailable{message: "The font source cannot be reached."}
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound {
		return nil, &fontNotFound{id: rawURL}
	}
	if response.StatusCode != http.StatusOK {
		return nil, &fontSourceUnavailable{message: fmt.Sprintf("The font source answered HTTP %d.",
			response.StatusCode)}
	}
	data := make([]byte, 0, 4096)
	buf := make([]byte, 32*1024)
	for {
		n, rerr := response.Body.Read(buf)
		if n > 0 {
			if len(data)+n > fontMaxWOFF2Bytes {
				return nil, fmt.Errorf("unexpected file size")
			}
			data = append(data, buf[:n]...)
		}
		if rerr != nil {
			break
		}
	}
	return data, nil
}

// normalizeFontCategory is sources.normalize_category.
func normalizeFontCategory(raw string) string {
	if containsString(fontCategories(), raw) {
		return raw
	}
	return "display"
}
