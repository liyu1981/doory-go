// The element pack manifest (apps/elements/manifest.py): the single reviewed
// list of element content. Everything that decides what ends up in the
// catalogue is data here — the pinned upstream release (npm tarball integrity
// or per-file SHA-256), the licence and its notice, how to find tags and how
// to turn the artwork into a recolourable element — and loading validates all
// of it, including the licence allow-list.

package doory

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"

	"io/fs"
)

//go:embed data/element_packs.json
var elementPacksJSON []byte

//go:embed data/element_pack_files
var elementPackFilesFS embed.FS

var _ fs.FS = elementPackFilesFS

// urlParse is net/url.Parse (kept behind a helper so the manifest code reads
// like the Python it ports).
func urlParse(raw string) (*url.URL, error) { return url.Parse(raw) }

const (
	elementsManifestSchema = 1
	// elementNormalizerVersion is normalize.NORMALIZER_VERSION: part of a
	// pack's fingerprint, so a normaliser change re-syncs every pack.
	elementNormalizerVersion = 3
	elementDefaultColor      = "#7a64e0"
)

// elementAllowedLicenses is manifest.ALLOWED_LICENSES.
var elementAllowedLicenses = map[string]bool{
	"MIT": true, "ISC": true, "Apache-2.0": true, "OFL-1.1": true, "CC0-1.0": true,
	"BSD-2-Clause": true, "BSD-3-Clause": true, "Unlicense": true,
}

// elementNoticeLicenses need the copyright notice to travel with the artwork.
var elementNoticeLicenses = map[string]bool{
	"MIT": true, "ISC": true, "Apache-2.0": true, "OFL-1.1": true, "BSD-2-Clause": true, "BSD-3-Clause": true,
}

// elementAllowedHosts is manifest.ALLOWED_HOSTS.
var elementAllowedHosts = map[string]bool{
	"registry.npmjs.org": true, "opendoodles.s3-us-west-1.amazonaws.com": true,
	"cdn.prod.website-files.com": true,
}

var (
	elementKindValues  = []string{"shape", "line", "icon", "sticker", "illustration", "frame", "graphic"}
	elementStyleValues = []string{"outline", "filled", "duotone", "colour"}

	elementIDPattern        = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,59}$`)
	elementNpmNamePattern   = regexp.MustCompile(`^(?:@[a-z0-9~-][a-z0-9._~-]*/)?[a-z0-9~-][a-z0-9._~-]*$`)
	elementVersionPattern   = regexp.MustCompile(`^\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?$`)
	elementIntegrityPattern = regexp.MustCompile(`^sha512-[A-Za-z0-9+/]{86}==$`)
	elementSHA256Pattern    = regexp.MustCompile(`^[0-9a-f]{64}$`)
	elementStemPattern      = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,120}$`)
	elementColorPattern     = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)
)

// elementManifestError is manifest.ManifestError.
type elementManifestError struct{ message string }

func (e *elementManifestError) Error() string { return e.message }

func manifestErrorf(format string, args ...any) error {
	return &elementManifestError{message: fmt.Sprintf(format, args...)}
}

// elementSource is one of NpmSource | urlsSource | generatedSource.
type elementSource interface {
	label() string
	kind() string
}

// npmSource is manifest.NpmSource.
type npmSource struct {
	Package   string
	Version   string
	Integrity string
}

func (s npmSource) kind() string  { return "npm" }
func (s npmSource) label() string { return s.Package + "@" + s.Version }

// tarballURL is NpmSource.tarball_url.
func (s npmSource) tarballURL() string {
	base := s.Package
	if i := strings.LastIndex(base, "/"); i >= 0 {
		base = base[i+1:]
	}
	return "https://registry.npmjs.org/" + s.Package + "/-/" + base + "-" + s.Version + ".tgz"
}

// tarballName is NpmSource.tarball_name (what `npm pack` writes).
func (s npmSource) tarballName() string {
	return strings.ReplaceAll(strings.TrimPrefix(s.Package, "@"), "/", "-") + "-" + s.Version + ".tgz"
}

// urlFile is manifest.UrlFile.
type urlFile struct {
	Name   string
	URL    string
	SHA256 string
	Tags   []string
}

// urlsSource is manifest.UrlsSource.
type urlsSource struct {
	Index string
	Files []urlFile
}

func (s urlsSource) kind() string { return "urls" }
func (s urlsSource) label() string {
	return fmt.Sprintf("%s (%d files)", s.Index, len(s.Files))
}

// generatedSource is manifest.GeneratedSource: artwork generated from code in
// this repository (the pinned output is embedded — see D14).
type generatedSource struct {
	Generator string
	Set       string
}

func (s generatedSource) kind() string { return "generated" }
func (s generatedSource) label() string {
	return "generated:" + s.Generator + "/" + s.Set
}

// elementFilesSpec is manifest.FilesSpec.
type elementFilesSpec struct {
	Include []string
	Exclude []string
	Reader  string
}

// elementNamingSpec is manifest.NamingSpec.
type elementNamingSpec struct {
	StripPrefix string
	StripSuffix string
	Skip        string
	Brand       string
}

// elementTagsSpec is manifest.TagsSpec.
type elementTagsSpec struct {
	Format string
	File   string
}

// elementLicenseSpec is manifest.LicenseSpec.
type elementLicenseSpec struct {
	SPDX  string
	URL   string
	File  string
	Match string
}

// elementPackSpec is manifest.PackSpec.
type elementPackSpec struct {
	ID          string
	Name        string
	Kind        string
	Style       string
	Description string
	Homepage    string
	SortOrder   int
	Source      elementSource
	License     elementLicenseSpec
	Attribution string
	Files       elementFilesSpec
	Naming      elementNamingSpec
	Tags        elementTagsSpec
	Normalize   normalizeSpec
	Fingerprint string
}

// isGenerated reports whether the pack's artwork is generated locally.
func (p elementPackSpec) isGenerated() bool {
	_, ok := p.Source.(generatedSource)
	return ok
}

// sourceRef is PackSpec.source_ref.
func (p elementPackSpec) sourceRef() string { return p.Source.label() }

// hasBrand reports whether the pack carries trademarked logos.
func (p elementPackSpec) hasBrand() bool { return p.Naming.Brand != "" }

// fingerprintFor is PackSpec.fingerprint_for (brand logos change the sync).
func (p elementPackSpec) fingerprintFor(brandLogos bool) string {
	if !(brandLogos && p.hasBrand()) {
		return p.Fingerprint
	}
	sum := sha256.Sum256([]byte(p.Fingerprint + ":brand-logos"))
	return hex.EncodeToString(sum[:])
}

// patterns is PackSpec.patterns: artwork plus the tag and licence files.
func (p elementPackSpec) patterns() []string {
	out := append([]string{}, p.Files.Include...)
	if p.Tags.File != "" {
		out = append(out, p.Tags.File)
	}
	if p.License.File != "" {
		out = append(out, p.License.File)
	}
	return out
}

// elementManifest is manifest.Manifest.
type elementManifest struct {
	Packs []elementPackSpec
}

func (m *elementManifest) get(packID string) *elementPackSpec {
	for i := range m.Packs {
		if m.Packs[i].ID == packID {
			return &m.Packs[i]
		}
	}
	return nil
}

// selectPacks is Manifest.select: the named packs in manifest order, or all.
func (m *elementManifest) selectPacks(ids []string) ([]elementPackSpec, error) {
	if len(ids) == 0 {
		return append([]elementPackSpec{}, m.Packs...), nil
	}
	var unknown []string
	for _, id := range ids {
		if m.get(id) == nil {
			unknown = append(unknown, id)
		}
	}
	if len(unknown) > 0 {
		known := make([]string, len(m.Packs))
		for i, pack := range m.Packs {
			known[i] = pack.ID
		}
		return nil, manifestErrorf("Unknown pack id(s): %s. Known packs: %s.",
			strings.Join(unknown, ", "), strings.Join(known, ", "))
	}
	wanted := map[string]bool{}
	for _, id := range ids {
		wanted[id] = true
	}
	out := []elementPackSpec{}
	for _, pack := range m.Packs {
		if wanted[pack.ID] {
			out = append(out, pack)
		}
	}
	return out, nil
}

// loadElementManifest is load_manifest: read and validate the embedded
// manifest (the pinned copy of backend/data/element_packs.json).
func loadElementManifest() (*elementManifest, error) {
	var raw any
	dec := json.NewDecoder(strings.NewReader(string(elementPacksJSON)))
	dec.UseNumber()
	if err := dec.Decode(&raw); err != nil {
		return nil, manifestErrorf("element_packs.json is not valid JSON: %v", err)
	}
	return parseElementManifest(raw)
}

// packFingerprintText is the last fingerprint payload rendered (test/debug).
var packFingerprintText string

// parseElementManifest is parse_manifest.
func parseElementManifest(raw any) (*elementManifest, error) {
	obj, ok := raw.(map[string]any)
	if !ok {
		return nil, manifestErrorf("the manifest must be a JSON object")
	}
	if err := onlyKeys(obj, []string{"schema", "packs"}, "manifest"); err != nil {
		return nil, err
	}
	schema, _ := obj["schema"].(json.Number)
	if schema.String() != "1" {
		return nil, manifestErrorf("manifest: schema must be 1")
	}
	packsRaw, ok := obj["packs"].([]any)
	if !ok || len(packsRaw) == 0 {
		return nil, manifestErrorf("manifest: packs must be a non-empty list")
	}
	manifest := &elementManifest{}
	seen := map[string]bool{}
	for _, item := range packsRaw {
		pack, err := parseElementPack(item)
		if err != nil {
			return nil, err
		}
		if seen[pack.ID] {
			return nil, manifestErrorf("manifest: pack ids must be unique")
		}
		seen[pack.ID] = true
		manifest.Packs = append(manifest.Packs, *pack)
	}
	return manifest, nil
}

// licenseTerms is manifest.license_terms (SPDX expressions of AND/OR terms).
func licenseTerms(expression string) []string {
	var terms []string
	for _, part := range regexp.MustCompile(`\s+(?:AND|OR)\s+`).Split(strings.TrimSpace(expression), -1) {
		if part != "" {
			terms = append(terms, part)
		}
	}
	return terms
}

// isAllowedElementLicense is manifest.is_allowed_license.
func isAllowedElementLicense(expression string) bool {
	terms := licenseTerms(expression)
	if len(terms) == 0 {
		return false
	}
	for _, term := range terms {
		if !elementAllowedLicenses[term] {
			return false
		}
	}
	return true
}

func onlyKeys(raw map[string]any, allowed []string, where string) error {
	var extra []string
	for key := range raw {
		if !containsString(allowed, key) {
			extra = append(extra, key)
		}
	}
	if len(extra) > 0 {
		sort.Strings(extra)
		return manifestErrorf("%s: unknown key(s) %s", where, strings.Join(extra, ", "))
	}
	return nil
}

func manifestString(raw map[string]any, key, where string, required bool, fallback string) (string, error) {
	value, present := raw[key]
	if !present {
		if required {
			return "", manifestErrorf("%s: %s is required", where, key)
		}
		return fallback, nil
	}
	text, ok := value.(string)
	if !ok {
		return "", manifestErrorf("%s: %s must be a string", where, key)
	}
	if required && strings.TrimSpace(text) == "" {
		return "", manifestErrorf("%s: %s is required", where, key)
	}
	return strings.TrimSpace(text), nil
}

func manifestHTTPSURL(value, where string, hosts map[string]bool) (string, error) {
	parsed, err := urlParse(value)
	if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" || parsed.User != nil {
		return "", manifestErrorf("%s: %q is not an https URL", where, value)
	}
	if hosts != nil && !hosts[parsed.Hostname()] {
		return "", manifestErrorf("%s: host %q is not allowed", where, parsed.Hostname())
	}
	return value, nil
}

func manifestGlobList(raw map[string]any, key, where string) ([]string, error) {
	value, present := raw[key]
	if !present {
		return nil, nil
	}
	list, ok := value.([]any)
	if !ok {
		return nil, manifestErrorf("%s: %s must be a list of globs", where, key)
	}
	out := make([]string, 0, len(list))
	for _, item := range list {
		text, ok := item.(string)
		if !ok || text == "" {
			return nil, manifestErrorf("%s: %s must be a list of globs", where, key)
		}
		if strings.HasPrefix(text, "/") || containsString(strings.Split(text, "/"), "..") {
			return nil, manifestErrorf("%s: %s pattern %q must be relative", where, key, text)
		}
		out = append(out, text)
	}
	return out, nil
}

// loadElementPackIndex is manifest._load_index: the bundled per-file index for
// a urls-sourced pack.
func loadElementPackIndex(name, where string) ([]urlFile, error) {
	if !elementStemPattern.MatchString(name) || !strings.HasSuffix(name, ".json") {
		return nil, manifestErrorf("%s: index must be a .json file name", where)
	}
	data, err := elementPackFilesFS.ReadFile("data/element_pack_files/" + name)
	if err != nil {
		return nil, manifestErrorf("%s: index file %s not found", where, name)
	}
	var raw any
	if jerr := json.Unmarshal(data, &raw); jerr != nil {
		return nil, manifestErrorf("%s: %s is not valid JSON", where, name)
	}
	rows, ok := raw.([]any)
	if !ok || len(rows) == 0 {
		return nil, manifestErrorf("%s: %s must be a non-empty list", where, name)
	}
	var files []urlFile
	seen := map[string]bool{}
	for index, rowRaw := range rows {
		spot := fmt.Sprintf("%s: %s[%d]", where, name, index)
		row, ok := rowRaw.(map[string]any)
		if !ok {
			return nil, manifestErrorf("%s must be an object", spot)
		}
		if err := onlyKeys(row, []string{"name", "url", "sha256", "tags"}, spot); err != nil {
			return nil, err
		}
		fileName, err := manifestString(row, "name", spot, true, "")
		if err != nil {
			return nil, err
		}
		if !elementStemPattern.MatchString(fileName) || !strings.HasSuffix(fileName, ".svg") {
			return nil, manifestErrorf("%s: bad file name", spot)
		}
		if seen[fileName] {
			return nil, manifestErrorf("%s: duplicate name %s", spot, fileName)
		}
		seen[fileName] = true
		digest, err := manifestString(row, "sha256", spot, true, "")
		if err != nil {
			return nil, err
		}
		if !elementSHA256Pattern.MatchString(digest) {
			return nil, manifestErrorf("%s: sha256 must be 64 lower-case hex digits", spot)
		}
		rawURL, err := manifestString(row, "url", spot, true, "")
		if err != nil {
			return nil, err
		}
		cleanURL, err := manifestHTTPSURL(rawURL, spot, elementAllowedHosts)
		if err != nil {
			return nil, err
		}
		var tags []string
		if rawTags, present := row["tags"]; present {
			list, ok := rawTags.([]any)
			if !ok {
				return nil, manifestErrorf("%s: tags must be strings", spot)
			}
			for _, tag := range list {
				text, ok := tag.(string)
				if !ok {
					return nil, manifestErrorf("%s: tags must be strings", spot)
				}
				tags = append(tags, text)
			}
		}
		files = append(files, urlFile{Name: fileName, URL: cleanURL, SHA256: digest, Tags: tags})
	}
	return files, nil
}

// parseElementSource is manifest._source.
func parseElementSource(raw any, where string) (elementSource, error) {
	obj, ok := raw.(map[string]any)
	if !ok {
		return nil, manifestErrorf("%s: source must be an object", where)
	}
	kind, _ := obj["type"].(string)
	switch kind {
	case "npm":
		if err := onlyKeys(obj, []string{"type", "package", "version", "integrity"}, where+": source"); err != nil {
			return nil, err
		}
		pkg, err := manifestString(obj, "package", where, true, "")
		if err != nil {
			return nil, err
		}
		version, err := manifestString(obj, "version", where, true, "")
		if err != nil {
			return nil, err
		}
		integrity, err := manifestString(obj, "integrity", where, true, "")
		if err != nil {
			return nil, err
		}
		if !elementNpmNamePattern.MatchString(pkg) {
			return nil, manifestErrorf("%s: %q is not an npm package name", where, pkg)
		}
		if !elementVersionPattern.MatchString(version) {
			return nil, manifestErrorf("%s: version must be exact (e.g. 1.2.3), not a range", where)
		}
		if !elementIntegrityPattern.MatchString(integrity) {
			return nil, manifestErrorf("%s: integrity must be an npm sha512 value", where)
		}
		return npmSource{Package: pkg, Version: version, Integrity: integrity}, nil
	case "urls":
		if err := onlyKeys(obj, []string{"type", "index"}, where+": source"); err != nil {
			return nil, err
		}
		index, err := manifestString(obj, "index", where, true, "")
		if err != nil {
			return nil, err
		}
		files, ferr := loadElementPackIndex(index, where)
		if ferr != nil {
			return nil, ferr
		}
		return urlsSource{Index: index, Files: files}, nil
	case "generated":
		if err := onlyKeys(obj, []string{"type", "generator", "set"}, where+": source"); err != nil {
			return nil, err
		}
		generator, err := manifestString(obj, "generator", where, true, "")
		if err != nil {
			return nil, err
		}
		if generator != "graphics" {
			return nil, manifestErrorf("%s: unknown generator %q", where, generator)
		}
		set, err := manifestString(obj, "set", where, true, "")
		if err != nil {
			return nil, err
		}
		return generatedSource{Generator: generator, Set: set}, nil
	}
	return nil, manifestErrorf("%s: source.type must be npm, urls or generated", where)
}

// parseElementLicense is manifest._license.
func parseElementLicense(raw any, where string) (elementLicenseSpec, error) {
	var spec elementLicenseSpec
	obj, ok := raw.(map[string]any)
	if !ok {
		return spec, manifestErrorf("%s: license must be an object", where)
	}
	if err := onlyKeys(obj, []string{"spdx", "url", "file", "match"}, where+": license"); err != nil {
		return spec, err
	}
	spdx, err := manifestString(obj, "spdx", where, true, "")
	if err != nil {
		return spec, err
	}
	if !isAllowedElementLicense(spdx) {
		allowed := make([]string, 0, len(elementAllowedLicenses))
		for name := range elementAllowedLicenses {
			allowed = append(allowed, name)
		}
		sort.Strings(allowed)
		return spec, manifestErrorf("%s: licence %q is not allowed (allowed: %s)",
			where, spdx, strings.Join(allowed, ", "))
	}
	file, err := manifestString(obj, "file", where, false, "")
	if err != nil {
		return spec, err
	}
	match, err := manifestString(obj, "match", where, false, "")
	if err != nil {
		return spec, err
	}
	if (file != "") != (match != "") {
		return spec, manifestErrorf("%s: license.file and license.match go together", where)
	}
	if match != "" {
		if _, rerr := regexp.Compile(match); rerr != nil {
			return spec, manifestErrorf("%s: license.match is not a valid regular expression", where)
		}
	}
	rawURL, err := manifestString(obj, "url", where, true, "")
	if err != nil {
		return spec, err
	}
	cleanURL, err := manifestHTTPSURL(rawURL, where, nil)
	if err != nil {
		return spec, err
	}
	return elementLicenseSpec{SPDX: spdx, URL: cleanURL, File: file, Match: match}, nil
}

// parseElementNormalize is manifest._normalize.
func parseElementNormalize(raw any, where string) (normalizeSpec, error) {
	var spec normalizeSpec
	obj, ok := raw.(map[string]any)
	if !ok {
		return spec, manifestErrorf("%s: normalize must be an object", where)
	}
	if err := onlyKeys(obj, []string{"mode", "color", "size", "frame"}, where+": normalize"); err != nil {
		return spec, err
	}
	mode, err := manifestString(obj, "mode", where, true, "")
	if err != nil {
		return spec, err
	}
	if mode != "mono" && mode != "colour" {
		return spec, manifestErrorf("%s: normalize.mode must be mono or colour", where)
	}
	color, err := manifestString(obj, "color", where, false, elementDefaultColor)
	if err != nil {
		return spec, err
	}
	if !elementColorPattern.MatchString(color) {
		return spec, manifestErrorf("%s: normalize.color must be #rrggbb", where)
	}
	spec = normalizeSpec{Mode: mode, Color: strings.ToLower(color)}
	if sizeRaw, present := obj["size"]; present && sizeRaw != nil {
		number, ok := jsonNumberValue(sizeRaw)
		if !ok || number < 1 || number > 4096 {
			return spec, manifestErrorf("%s: normalize.size must be a number between 1 and 4096", where)
		}
		spec.Size = &number
	}
	if frameRaw, present := obj["frame"]; present {
		flag, ok := frameRaw.(bool)
		if !ok {
			return spec, manifestErrorf("%s: normalize.frame must be true or false", where)
		}
		spec.Frame = flag
	}
	return spec, nil
}

// jsonNumberValue reads a JSON number as a float64.
func jsonNumberValue(raw any) (float64, bool) {
	switch v := raw.(type) {
	case json.Number:
		f, err := v.Float64()
		return f, err == nil
	case float64:
		return v, true
	case int:
		return float64(v), true
	}
	return 0, false
}

// elementPackFingerprint is manifest._fingerprint: the manifest entry (and the
// normaliser version, and the urls index contents) hashed, so any change
// re-syncs the pack.
func elementPackFingerprint(raw map[string]any, source elementSource) string {
	payload := map[string]any{"pack": raw, "normalizer": elementNormalizerVersion}
	if src, ok := source.(urlsSource); ok {
		files := make([]any, 0, len(src.Files))
		for _, file := range src.Files {
			files = append(files, []any{file.Name, file.URL, file.SHA256, stringSliceToAny(file.Tags)})
		}
		payload["files"] = files
	}
	// Python: json.dumps(payload, sort_keys=True) then sha256 of the UTF-8 bytes.
	encoded := pythonSortedJSON(payload)
	packFingerprintText = string(encoded)
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

// parseElementPack is parse_pack.
func parseElementPack(raw any) (*elementPackSpec, error) {
	obj, ok := raw.(map[string]any)
	if !ok {
		return nil, manifestErrorf("every pack must be an object")
	}
	where, _ := obj["id"].(string)
	if where == "" {
		where = "?"
	}
	allowed := []string{"id", "name", "kind", "style", "description", "homepage", "sort_order",
		"source", "files", "naming", "tags", "normalize", "license", "attribution"}
	if err := onlyKeys(obj, allowed, where); err != nil {
		return nil, err
	}
	packID, err := manifestString(obj, "id", where, true, "")
	if err != nil {
		return nil, err
	}
	if !elementIDPattern.MatchString(packID) {
		return nil, manifestErrorf("%s: id must be lower-case letters, digits and hyphens", where)
	}
	kind, err := manifestString(obj, "kind", where, true, "")
	if err != nil {
		return nil, err
	}
	if !containsString(elementKindValues, kind) {
		return nil, manifestErrorf("%s: kind must be one of %s", where, strings.Join(elementKindValues, ", "))
	}
	style, err := manifestString(obj, "style", where, false, "")
	if err != nil {
		return nil, err
	}
	if style != "" && !containsString(elementStyleValues, style) {
		return nil, manifestErrorf("%s: style must be one of %s", where, strings.Join(elementStyleValues, ", "))
	}
	sortOrder := 0
	if rawSort, present := obj["sort_order"]; present {
		number, ok := jsonNumberValue(rawSort)
		if !ok || number < 0 || number != float64(int(number)) {
			return nil, manifestErrorf("%s: sort_order must be a non-negative integer", where)
		}
		sortOrder = int(number)
	}
	source, err := parseElementSource(obj["source"], where)
	if err != nil {
		return nil, err
	}
	licenseSpec, err := parseElementLicense(obj["license"], where)
	if err != nil {
		return nil, err
	}
	attribution, err := manifestString(obj, "attribution", where, false, "")
	if err != nil {
		return nil, err
	}
	for _, term := range licenseTerms(licenseSpec.SPDX) {
		if elementNoticeLicenses[term] && attribution == "" {
			return nil, manifestErrorf("%s: licence %s requires an attribution notice", where, licenseSpec.SPDX)
		}
	}
	homepage, err := manifestString(obj, "homepage", where, false, "")
	if err != nil {
		return nil, err
	}
	if homepage != "" {
		if _, herr := manifestHTTPSURL(homepage, where, nil); herr != nil {
			return nil, herr
		}
	}
	name, err := manifestString(obj, "name", where, true, "")
	if err != nil {
		return nil, err
	}
	description, err := manifestString(obj, "description", where, false, "")
	if err != nil {
		return nil, err
	}
	pack := &elementPackSpec{
		ID: packID, Name: name, Kind: kind, Style: style, Description: description, Homepage: homepage,
		SortOrder: sortOrder, Source: source, License: licenseSpec, Attribution: attribution,
		Normalize: normalizeSpec{Mode: "mono", Color: elementDefaultColor},
	}
	if pack.isGenerated() {
		for _, key := range []string{"files", "naming", "tags", "normalize"} {
			if _, present := obj[key]; present {
				return nil, manifestErrorf("%s: generated packs take no files/naming/tags/normalize", where)
			}
		}
		if licenseSpec.File != "" {
			return nil, manifestErrorf("%s: generated packs have no licence file to check", where)
		}
	} else {
		filesRaw, ok := obj["files"].(map[string]any)
		if !ok {
			return nil, manifestErrorf("%s: files must be an object", where)
		}
		if err := onlyKeys(filesRaw, []string{"include", "exclude", "reader"}, where+": files"); err != nil {
			return nil, err
		}
		reader, err := manifestString(filesRaw, "reader", where, false, "svg")
		if err != nil {
			return nil, err
		}
		if reader != "svg" && reader != "iconify-json" {
			return nil, manifestErrorf("%s: files.reader must be one of svg, iconify-json", where)
		}
		include, err := manifestGlobList(filesRaw, "include", where)
		if err != nil {
			return nil, err
		}
		if len(include) == 0 {
			return nil, manifestErrorf("%s: files.include needs at least one glob", where)
		}
		exclude, err := manifestGlobList(filesRaw, "exclude", where)
		if err != nil {
			return nil, err
		}
		pack.Files = elementFilesSpec{Include: include, Exclude: exclude, Reader: reader}
		naming := elementNamingSpec{}
		if namingRaw, present := obj["naming"]; present {
			namingObj, ok := namingRaw.(map[string]any)
			if !ok {
				return nil, manifestErrorf("%s: naming must be an object", where)
			}
			if err := onlyKeys(namingObj, []string{"strip_prefix", "strip_suffix", "skip", "brand"},
				where+": naming"); err != nil {
				return nil, err
			}
			naming.StripPrefix, err = manifestString(namingObj, "strip_prefix", where, false, "")
			if err != nil {
				return nil, err
			}
			naming.StripSuffix, err = manifestString(namingObj, "strip_suffix", where, false, "")
			if err != nil {
				return nil, err
			}
			naming.Skip, err = manifestString(namingObj, "skip", where, false, "")
			if err != nil {
				return nil, err
			}
			naming.Brand, err = manifestString(namingObj, "brand", where, false, "")
			if err != nil {
				return nil, err
			}
			for _, pattern := range []string{naming.Skip, naming.Brand} {
				if pattern == "" {
					continue
				}
				if _, rerr := compileElementPattern(pattern); rerr != nil {
					return nil, manifestErrorf("%s: naming.skip/brand is not a valid regular expression", where)
				}
			}
		}
		pack.Naming = naming
		tags := elementTagsSpec{Format: "none"}
		if tagsRaw, present := obj["tags"]; present {
			tagsObj, ok := tagsRaw.(map[string]any)
			if !ok {
				return nil, manifestErrorf("%s: tags must be an object", where)
			}
			if err := onlyKeys(tagsObj, []string{"format", "file"}, where+": tags"); err != nil {
				return nil, err
			}
			tags.Format, err = manifestString(tagsObj, "format", where, false, "none")
			if err != nil {
				return nil, err
			}
			if !containsString([]string{"none", "json-map", "tabler", "phosphor-js", "iconify-categories"},
				tags.Format) {
				return nil, manifestErrorf("%s: tags.format is not a known format", where)
			}
			tags.File, err = manifestString(tagsObj, "file", where, false, "")
			if err != nil {
				return nil, err
			}
			if (tags.Format == "none") != (tags.File == "") {
				return nil, manifestErrorf("%s: tags.file is needed exactly when tags.format is set", where)
			}
		}
		pack.Tags = tags
		if normalizeRaw, present := obj["normalize"]; present {
			spec, nerr := parseElementNormalize(normalizeRaw, where)
			if nerr != nil {
				return nil, nerr
			}
			pack.Normalize = spec
		}
		if _, ok := source.(npmSource); ok {
			if licenseSpec.File == "" {
				return nil, manifestErrorf("%s: npm packs must name a licence file to check", where)
			}
		} else if reader != "svg" {
			return nil, manifestErrorf("%s: url packs hold plain SVG files", where)
		}
	}
	pack.Fingerprint = elementPackFingerprint(obj, source)
	return pack, nil
}
