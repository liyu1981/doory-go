// Size-preset catalogues as JSON (apps/presets/importer.py): parsing with the
// field rules from validators.py, the upsert writer shared by the two
// commands, and the name-clash check.
//
// The same shape `GET /api/v1/size-presets/` answers with, so a served
// catalogue can be saved, edited and imported again.

package doory

import (
	"database/sql"
	_ "embed"
	"encoding/json"
	"fmt"
	"math/big"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	presetFormatVersion = 1
	presetMaxFileBytes  = 4 * 1024 * 1024
	presetMaxDepth      = 8
	presetMaxRows       = 5000
	presetMaxTags       = 20
	presetMaxAliases    = 20
	presetMaxMetaBytes  = 4096
	presetMaxDimension  = 30000 // designs.validators.MAX_DIMENSION
	presetMaxTagLength  = 32
)

var (
	presetSlugPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)
	groupSlugPattern  = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)
	iconNamePattern   = regexp.MustCompile(`^[a-z0-9-]{0,64}$`)
	presetUnits       = []string{"px", "mm", "cm", "in"}
)

// PresetImportError is importer.PresetImportError (a CommandError at the CLI).
type PresetImportError struct{ message string }

func (e *PresetImportError) Error() string { return e.message }

func presetImportErrorf(format string, args ...any) error {
	return &PresetImportError{message: fmt.Sprintf(format, args...)}
}

// presetGroupRow is one parsed group, flattened in file order.
type presetGroupRow struct {
	Slug      string
	Parent    string // "" for a top-level group
	Label     string
	Icon      string
	SortOrder int
	IsActive  bool
	// Unset holds the fields the file left out, which an existing row keeps.
	Unset map[string]bool
}

// presetRow is one parsed preset.
type presetRow struct {
	Slug      string
	Group     string
	Label     string
	Width     string // decimal text, as stored (no exponent, no trailing zeros)
	Height    string
	Unit      string
	Icon      string
	SortOrder int
	IsActive  bool
	IsQuick   bool
	Tags      []string
	Aliases   []string
	Meta      string // Python json.dumps text (what the JSONField stores)
}

// presetCatalogue is importer.Catalogue.
type presetCatalogue struct {
	Groups  []presetGroupRow
	Presets []presetRow
}

// PresetReport is importer.Report (its __str__ is the command's stdout tail).
type PresetReport struct {
	Created     int
	Updated     int
	Unchanged   int
	Deactivated int
}

func (r PresetReport) String() string {
	text := fmt.Sprintf("%d created, %d updated, %d unchanged", r.Created, r.Updated, r.Unchanged)
	if r.Deactivated > 0 {
		text += fmt.Sprintf(", %d deactivated", r.Deactivated)
	}
	return text
}

// presetBuiltinFile is importer.BUILTIN_FILE: backend/data/size_presets.json,
// embedded byte-identically (doory/data/size_presets.json, same sha256).
const presetBuiltinFile = "size_presets.json"

//go:embed data/size_presets.json
var embeddedSizePresets []byte

// readPresetCatalogue ports importer.read_file.
func (a *App) readPresetCatalogue(path string) (*presetCatalogue, error) {
	st, err := os.Stat(path)
	if err != nil {
		return nil, presetImportErrorf("%s: does not exist.", path)
	}
	if st.Size() > presetMaxFileBytes {
		return nil, presetImportErrorf("%s: the file is larger than %d MB.", path, presetMaxFileBytes/(1024*1024))
	}
	raw, rerr := os.ReadFile(path)
	if rerr != nil {
		return nil, presetImportErrorf("%s: %v", path, rerr)
	}
	node, uerr := parseJSONNode(raw)
	if uerr != nil {
		return nil, presetImportErrorf("%s: not valid JSON (%v).", path, uerr)
	}
	cat, perr := parsePresetCatalogueNode(node)
	if perr != nil {
		return nil, presetImportErrorf("%s: %v", path, perr)
	}
	return cat, nil
}

// builtinPresetCatalogue ports importer.builtin_catalogue: the embedded
// backend/data/size_presets.json.
func builtinPresetCatalogue() (*presetCatalogue, error) {
	node, err := parseJSONNode(embeddedSizePresets)
	if err != nil {
		return nil, presetImportErrorf("%s: not valid JSON (%v).", presetBuiltinFile, err)
	}
	cat, err := parsePresetCatalogueNode(node)
	if err != nil {
		return nil, presetImportErrorf("%s: %v", presetBuiltinFile, err)
	}
	return cat, nil
}

// parsePresetCatalogueNode ports importer.parse over an order-preserving
// node (the file's key order survives into the stored JSON text).
func parsePresetCatalogueNode(doc *jsonNode) (*presetCatalogue, error) {
	if doc == nil || doc.kind != "object" {
		return nil, presetImportErrorf(`Expected an object with a "groups" list.`)
	}
	groupsRaw := nodeField(doc, "groups")
	if groupsRaw == nil || groupsRaw.kind != "array" {
		return nil, presetImportErrorf(`Expected an object with a "groups" list.`)
	}
	if versionNode := nodeField(doc, "version"); versionNode != nil {
		if versionNode.kind != "number" || versionNode.text != "1" {
			return nil, presetImportErrorf("Unsupported version %s (expected %d).",
				pyNodeRepr(versionNode), presetFormatVersion)
		}
	}
	var unknown []string
	for _, key := range doc.keys {
		if key != "version" && key != "groups" {
			unknown = append(unknown, key)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return nil, presetImportErrorf("Unknown key(s): %s.", strings.Join(unknown, ", "))
	}
	cat := &presetCatalogue{}
	if err := parsePresetGroups(groupsRaw.vals, "", "groups", 1, cat); err != nil {
		return nil, err
	}
	if err := checkPresetUnique(cat); err != nil {
		return nil, err
	}
	if len(cat.Presets) > presetMaxRows {
		return nil, presetImportErrorf("At most %d presets can be imported at once.", presetMaxRows)
	}
	return cat, nil
}

var presetGroupKeys = map[string]bool{
	"slug": true, "label": true, "icon": true, "order": true, "is_active": true,
	"children": true, "presets": true,
}

var presetKeys = map[string]bool{
	"slug": true, "label": true, "width": true, "height": true, "unit": true, "icon": true,
	"order": true, "is_active": true, "is_quick": true, "tags": true, "aliases": true, "meta": true,
	// derived by the API; accepted (and ignored) so a served catalogue imports as it is
	"width_px": true, "height_px": true,
}

func parsePresetGroups(items []*jsonNode, parent, where string, depth int, into *presetCatalogue) error {
	if depth > presetMaxDepth {
		return presetImportErrorf("%s: groups can be nested at most %d levels deep.", where, presetMaxDepth)
	}
	for index, item := range items {
		at := fmt.Sprintf("%s[%d]", where, index)
		if item.kind != "object" {
			return presetImportErrorf("%s must be an object.", at)
		}
		if err := noUnknownNodeKeys(item, presetGroupKeys, at); err != nil {
			return err
		}
		slug := nodeStringValue(item, "slug")
		if !groupSlugPattern.MatchString(slug) {
			return presetImportErrorf("%s.slug must be 1 to 64 lower-case letters, digits and hyphens.", at)
		}
		at = fmt.Sprintf("%s (%s)", at, slug)
		label, err := presetLabel(item, at, 80)
		if err != nil {
			return err
		}
		icon, err := presetIcon(item, at)
		if err != nil {
			return err
		}
		order, err := presetOrder(item, index, at)
		if err != nil {
			return err
		}
		active, err := presetFlag(item, "is_active", true, at)
		if err != nil {
			return err
		}
		unset := map[string]bool{}
		if nodeField(item, "icon") == nil {
			unset["icon"] = true
		}
		if nodeField(item, "order") == nil {
			unset["sort_order"] = true
		}
		into.Groups = append(into.Groups, presetGroupRow{
			Slug: slug, Parent: parent, Label: label, Icon: icon,
			SortOrder: order, IsActive: active, Unset: unset,
		})
		presets, ok := nodeList(item, "presets")
		if !ok {
			return presetImportErrorf("%s.presets must be a list.", at)
		}
		for i, rawPreset := range presets {
			row, perr := parsePresetRow(rawPreset, slug, i, at+".presets")
			if perr != nil {
				return perr
			}
			into.Presets = append(into.Presets, row)
		}
		children, ok := nodeList(item, "children")
		if !ok {
			return presetImportErrorf("%s.children must be a list.", at)
		}
		if len(children) > 0 {
			if err := parsePresetGroups(children, slug, at+".children", depth+1, into); err != nil {
				return err
			}
		}
	}
	return nil
}

func parsePresetRow(item *jsonNode, group string, index int, where string) (presetRow, error) {
	at := fmt.Sprintf("%s[%d]", where, index)
	if item.kind != "object" {
		return presetRow{}, presetImportErrorf("%s must be an object.", at)
	}
	if err := noUnknownNodeKeys(item, presetKeys, at); err != nil {
		return presetRow{}, err
	}
	slug := nodeStringValue(item, "slug")
	if !presetSlugPattern.MatchString(slug) {
		return presetRow{}, presetImportErrorf("%s.slug must be 1 to 64 letters, digits, dots, hyphens or underscores.", at)
	}
	at = fmt.Sprintf("%s (%s)", at, slug)
	unit := "px"
	if rawUnit := nodeField(item, "unit"); rawUnit != nil {
		if rawUnit.kind != "string" {
			return presetRow{}, presetImportErrorf("%s: The unit must be one of %s.", at, strings.Join(presetUnits, ", "))
		}
		unit = rawUnit.text
	}
	width, height, err := checkPresetSize(nodeField(item, "width"), nodeField(item, "height"), unit)
	if err != nil {
		return presetRow{}, presetImportErrorf("%s: %v", at, err)
	}
	tags, terr := normalizePresetTags(nodeField(item, "tags"))
	if terr != nil {
		return presetRow{}, presetImportErrorf("%s: %v", at, terr)
	}
	aliases, aerr := normalizePresetAliases(nodeField(item, "aliases"))
	if aerr != nil {
		return presetRow{}, presetImportErrorf("%s: %v", at, aerr)
	}
	meta, merr := checkPresetMeta(nodeField(item, "meta"))
	if merr != nil {
		return presetRow{}, presetImportErrorf("%s: %v", at, merr)
	}
	if containsString(aliases, slug) {
		return presetRow{}, presetImportErrorf("%s: an alias cannot repeat the preset's own slug.", at)
	}
	label, lerr := presetLabel(item, at, 120)
	if lerr != nil {
		return presetRow{}, lerr
	}
	icon, ierr := presetIcon(item, at)
	if ierr != nil {
		return presetRow{}, ierr
	}
	order, oerr := presetOrder(item, index, at)
	if oerr != nil {
		return presetRow{}, oerr
	}
	active, ferr := presetFlag(item, "is_active", true, at)
	if ferr != nil {
		return presetRow{}, ferr
	}
	quick, qerr := presetFlag(item, "is_quick", false, at)
	if qerr != nil {
		return presetRow{}, qerr
	}
	return presetRow{
		Slug: slug, Group: group, Label: label, Width: width, Height: height, Unit: unit,
		Icon: icon, SortOrder: order, IsActive: active, IsQuick: quick,
		Tags: tags, Aliases: aliases, Meta: meta,
	}, nil
}

func noUnknownNodeKeys(item *jsonNode, allowed map[string]bool, at string) error {
	var unknown []string
	for _, key := range item.keys {
		if !allowed[key] {
			unknown = append(unknown, key)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return presetImportErrorf("%s: unknown key(s) %s.", at, strings.Join(unknown, ", "))
	}
	return nil
}

func presetLabel(item *jsonNode, at string, limit int) (string, error) {
	node := nodeField(item, "label")
	label := ""
	if node != nil && node.kind == "string" {
		label = strings.TrimSpace(node.text)
	}
	if label == "" {
		return "", presetImportErrorf("%s.label must be text of 1 to %d characters.", at, limit)
	}
	if len([]rune(label)) > limit {
		return "", presetImportErrorf("%s.label must be text of 1 to %d characters.", at, limit)
	}
	return label, nil
}

func presetIcon(item *jsonNode, at string) (string, error) {
	icon := ""
	if node := nodeField(item, "icon"); node != nil {
		if node.kind != "string" || !iconNamePattern.MatchString(node.text) {
			return "", presetImportErrorf("%s.icon must be a Lucide icon name such as `file-text`.", at)
		}
		icon = node.text
	}
	return icon, nil
}

func presetOrder(item *jsonNode, index int, at string) (int, error) {
	if node := nodeField(item, "order"); node != nil {
		if node.kind != "number" || !isWholeNumber(node.text) {
			return 0, presetImportErrorf("%s.order must be a whole number from 0 to 1000000.", at)
		}
		num, _ := strconv.Atoi(node.text)
		if num < 0 || num > 1_000_000 {
			return 0, presetImportErrorf("%s.order must be a whole number from 0 to 1000000.", at)
		}
		return num, nil
	}
	return (index + 1) * 10, nil
}

// isWholeNumber reports whether a JSON number literal is an integer.
func isWholeNumber(text string) bool {
	if strings.ContainsAny(text, ".eE") {
		f, err := strconv.ParseFloat(text, 64)
		return err == nil && f == float64(int64(f))
	}
	_, err := strconv.ParseInt(text, 10, 64)
	return err == nil
}

func presetFlag(item *jsonNode, key string, fallback bool, at string) (bool, error) {
	node := nodeField(item, key)
	if node == nil {
		return fallback, nil
	}
	if node.kind != "bool" {
		return false, presetImportErrorf("%s.%s must be true or false.", at, key)
	}
	return node.b, nil
}

// nodeList returns a list value (an absent key is an empty list).
func nodeList(item *jsonNode, key string) ([]*jsonNode, bool) {
	node := nodeField(item, key)
	if node == nil {
		return nil, true
	}
	if node.kind != "array" {
		return nil, false
	}
	return node.vals, true
}

// nodeStringValue reads a string member ("" when absent or not a string).
func nodeStringValue(item *jsonNode, key string) string {
	node := nodeField(item, key)
	if node == nil || node.kind != "string" {
		return ""
	}
	return node.text
}

func checkPresetUnique(cat *presetCatalogue) error {
	seenGroups := map[string]bool{}
	for _, g := range cat.Groups {
		if seenGroups[g.Slug] {
			return presetImportErrorf("The group slug %q appears more than once.", g.Slug)
		}
		seenGroups[g.Slug] = true
	}
	seenPresets := map[string]bool{}
	for _, p := range cat.Presets {
		if seenPresets[p.Slug] {
			return presetImportErrorf("The preset slug %q appears more than once.", p.Slug)
		}
		seenPresets[p.Slug] = true
	}
	return nil
}

// decimalText normalizes a JSON number/string to the stored decimal form
// (no exponent, no trailing zeros) and reports it as an exact rational.
func decimalText(raw *jsonNode) (string, *big.Rat, bool) {
	if raw == nil {
		return "", nil, false
	}
	var text string
	switch raw.kind {
	case "number":
		text = raw.text
	case "string":
		text = strings.TrimSpace(raw.text)
	default:
		return "", nil, false
	}
	r, ok := new(big.Rat).SetString(text)
	if !ok {
		return "", nil, false
	}
	return text, r, true
}

// trimFloat renders a float the way Python's Decimal(str(x)) would for the
// catalogue values (no exponent, no trailing zeros).
func trimFloat(v float64) string {
	text := strconvFormatFloat(v)
	if strings.Contains(text, ".") {
		text = strings.TrimRight(text, "0")
		text = strings.TrimRight(text, ".")
	}
	return text
}

// checkPresetSize ports validators.check_size; returns the stored decimals.
func checkPresetSize(widthRaw, heightRaw *jsonNode, unit string) (string, string, error) {
	if !containsString(presetUnits, unit) {
		return "", "", fmt.Errorf("The unit must be one of %s.", strings.Join(presetUnits, ", "))
	}
	sides := make([]string, 2)
	for i, raw := range []*jsonNode{widthRaw, heightRaw} {
		name := "width"
		if i == 1 {
			name = "height"
		}
		text, rat, ok := decimalText(raw)
		if !ok {
			return "", "", fmt.Errorf("The %s must be a number.", name)
		}
		if rat.Sign() <= 0 {
			return "", "", fmt.Errorf("The %s must be greater than 0.", name)
		}
		if unit == "px" && !rat.IsInt() {
			return "", "", fmt.Errorf("The %s must be a whole number of pixels.", name)
		}
		if !hasAtMostThreeDecimals(text) {
			return "", "", fmt.Errorf("The %s can have at most 3 decimals.", name)
		}
		px := toPx(text, unit)
		if px < 1 || px > presetMaxDimension {
			return "", "", fmt.Errorf("The %s must come to between 1 and %d px.", name, presetMaxDimension)
		}
		sides[i] = canonicalDecimal(text)
	}
	return sides[0], sides[1], nil
}

// hasAtMostThreeDecimals checks the textual decimal places (Python quantizes
// Decimal("0.001")).
func hasAtMostThreeDecimals(text string) bool {
	intPart := text
	if dot := strings.IndexByte(text, '.'); dot >= 0 {
		intPart = text[:dot]
		if len(text)-dot-1 > 3 {
			return false
		}
	}
	// A value with more than 3 decimals but trailing zeros is still exact
	// after quantize only when the excess digits are zeros.
	if dot := strings.IndexByte(text, '.'); dot >= 0 {
		frac := text[dot+1:]
		if len(frac) > 3 {
			return strings.Trim(frac[3:], "0") == ""
		}
	}
	_ = intPart
	return true
}

// canonicalDecimal strips trailing zeros of the fraction (what Django stores).
func canonicalDecimal(text string) string {
	text = strings.TrimSpace(text)
	if dot := strings.IndexByte(text, '.'); dot >= 0 {
		frac := strings.TrimRight(text[dot+1:], "0")
		if frac == "" {
			return text[:dot]
		}
		return text[:dot] + "." + frac
	}
	return text
}

// normalizePresetLabels ports validators.normalize_labels.
func normalizePresetLabels(raw *jsonNode, what string, limit int, pattern *regexp.Regexp) ([]string, error) {
	items := []string{}
	if raw == nil {
		return items, nil
	}
	if raw.kind == "string" {
		if raw.text == "" {
			return items, nil
		}
		items = strings.Split(raw.text, ",")
	} else if raw.kind == "array" {
		for _, entry := range raw.vals {
			if entry.kind != "string" {
				return nil, fmt.Errorf("%s must be a list of strings.", capitalize(what))
			}
			items = append(items, entry.text)
		}
	} else {
		return nil, fmt.Errorf("%s must be a list of strings.", capitalize(what))
	}
	seen := []string{}
	index := map[string]bool{}
	for _, item := range items {
		cleaned := strings.Join(strings.Fields(item), " ")
		if cleaned == "" {
			continue
		}
		if pattern != nil && !pattern.MatchString(cleaned) {
			return nil, fmt.Errorf("%q is not a valid %s.", cleaned, singular(what))
		}
		if pattern == nil && len([]rune(cleaned)) > presetMaxTagLength {
			return nil, fmt.Errorf("A %s is longer than %d characters.", singular(what), presetMaxTagLength)
		}
		if index[cleaned] {
			continue
		}
		index[cleaned] = true
		seen = append(seen, cleaned)
	}
	if len(seen) > limit {
		return nil, fmt.Errorf("Use at most %d %s.", limit, what)
	}
	return seen, nil
}

func normalizePresetTags(raw *jsonNode) ([]string, error) {
	tags, err := normalizePresetLabels(raw, "tags", presetMaxTags, nil)
	if err != nil {
		return nil, err
	}
	for i, tag := range tags {
		tags[i] = strings.ToLower(tag)
	}
	return tags, nil
}

func normalizePresetAliases(raw *jsonNode) ([]string, error) {
	return normalizePresetLabels(raw, "aliases", presetMaxAliases, presetSlugPattern)
}

// checkPresetMeta ports validators.check_meta. The value is validated and
// returned as the Python json.dumps text stored in the JSONField.
func checkPresetMeta(node *jsonNode) (string, error) {
	if node == nil {
		return "{}", nil
	}
	if node.kind == "string" && node.text == "" {
		return "{}", nil
	}
	if node.kind != "object" {
		return "", fmt.Errorf("Meta must be a JSON object.")
	}
	text := pyDumps(node)
	if len(text) > presetMaxMetaBytes {
		return "", fmt.Errorf("Meta is larger than %d bytes.", presetMaxMetaBytes)
	}
	zoneNode := nodeField(node, "safe_zone")
	if zoneNode != nil && zoneNode.kind != "null" {
		if zoneNode.kind != "object" {
			return "", fmt.Errorf("`safe_zone` must be an object with some of: top, bottom, left, right.")
		}
		for i, side := range zoneNode.keys {
			if side != "top" && side != "bottom" && side != "left" && side != "right" {
				return "", fmt.Errorf("`safe_zone` must be an object with some of: top, bottom, left, right.")
			}
			value := zoneNode.vals[i]
			if value.kind != "number" {
				return "", fmt.Errorf("`safe_zone.%s` must be a fraction of the page from 0 to below 0.5.", side)
			}
			num, perr := strconv.ParseFloat(value.text, 64)
			if perr != nil || num < 0 || num >= 0.5 {
				return "", fmt.Errorf("`safe_zone.%s` must be a fraction of the page from 0 to below 0.5.", side)
			}
		}
	}
	return text, nil
}

// nodeField returns the value of an object key, or nil.
func nodeField(node *jsonNode, key string) *jsonNode {
	if node == nil || node.kind != "object" {
		return nil
	}
	for i, k := range node.keys {
		if k == key {
			return node.vals[i]
		}
	}
	return nil
}

// pyJSONStringList renders a list of strings as Python json.dumps does.
func pyJSONStringList(values []string) string {
	if len(values) == 0 {
		return "[]"
	}
	quoted := make([]string, len(values))
	for i, v := range values {
		quoted[i] = pyJSONString(v)
	}
	return "[" + strings.Join(quoted, ", ") + "]"
}

// pyRepr renders a JSON value the way Python's repr() does in these messages.
func pyRepr(v any) string {
	switch x := v.(type) {
	case nil:
		return "None"
	case float64:
		if x == float64(int64(x)) {
			return strconv.FormatInt(int64(x), 10)
		}
		return strconv.FormatFloat(x, 'g', -1, 64)
	case string:
		return "'" + strings.ReplaceAll(x, "'", "\\'") + "'"
	case bool:
		if x {
			return "True"
		}
		return "False"
	}
	return fmt.Sprintf("%v", v)
}

// strconvFormatFloat renders a JSON number without an exponent when the value
// is in the range these catalogues use (Python repr parity).
func strconvFormatFloat(v float64) string {
	return strconv.FormatFloat(v, 'f', -1, 64)
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

func singular(what string) string {
	if strings.HasSuffix(what, "s") {
		return strings.TrimSuffix(what, "s")
	}
	return what
}

// writePresetCatalogue ports importer.write: upsert by slug. The caller owns
// the transaction (a failed name-clash check leaves nothing half-written).
func (a *App) writePresetCatalogue(cat *presetCatalogue, mode string, overwrite bool) (PresetReport, error) {
	return a.writePresetCatalogueTx(cat, mode, overwrite, false)
}

// writePresetCatalogueTx is writePresetCatalogue with an optional rollback
// (import_size_presets --dry-run: validate and report, change nothing).
func (a *App) writePresetCatalogueTx(cat *presetCatalogue, mode string, overwrite, dryRun bool,
) (PresetReport, error) {
	var report PresetReport
	if mode != "extend" && mode != "replace" {
		return report, presetImportErrorf("Unknown mode %q.", mode)
	}
	tx, err := a.DB.Begin()
	if err != nil {
		return report, err
	}
	defer func() { _ = tx.Rollback() }()
	now := formatDjangoDateTime(time.Now())
	groupIDs := map[string]string{}
	for _, g := range cat.Groups {
		var parentID any
		if g.Parent != "" {
			parentID = groupIDs[g.Parent]
		}
		id, created, changed, uerr := a.upsertPresetGroup(tx, g, parentID, overwrite, now)
		if uerr != nil {
			return report, uerr
		}
		groupIDs[g.Slug] = id
		switch {
		case created:
			report.Created++
		case changed:
			report.Updated++
		default:
			report.Unchanged++
		}
	}
	for _, p := range cat.Presets {
		created, changed, uerr := a.upsertPresetRow(tx, p, groupIDs[p.Group], overwrite, now)
		if uerr != nil {
			return report, uerr
		}
		switch {
		case created:
			report.Created++
		case changed:
			report.Updated++
		default:
			report.Unchanged++
		}
	}
	if mode == "replace" {
		n, derr := a.deactivateMissingPresets(tx, cat, now)
		if derr != nil {
			return report, derr
		}
		report.Deactivated += n
	}
	clash, cerr := a.findPresetNameClash(tx)
	if cerr != nil {
		return report, cerr
	}
	if clash != "" {
		return report, presetImportErrorf("%s", clash)
	}
	if dryRun {
		return report, nil // deferred Rollback discards everything
	}
	if err := tx.Commit(); err != nil {
		return report, err
	}
	return report, nil
}

// upsertPresetGroup ports importer._upsert for a group row; `keep` fields (an
// existing group keeps its icon/order when the file omits them).
func (a *App) upsertPresetGroup(tx *sql.Tx, g presetGroupRow, parentID any, overwrite bool,
	now string,
) (id string, created, changed bool, err error) {
	var cur struct {
		id        string
		label     string
		icon      string
		sortOrder int
		isActive  bool
		parent    sql.NullString
	}
	row := tx.QueryRow(`SELECT id, label, icon, sort_order, is_active, parent_id
	      FROM presets_sizepresetgroup WHERE slug = ?`, g.Slug)
	scanErr := row.Scan(&cur.id, &cur.label, &cur.icon, &cur.sortOrder, &cur.isActive, &cur.parent)
	if scanErr == sql.ErrNoRows {
		id = newUUID4Hex()
		if _, ierr := tx.Exec(`INSERT INTO presets_sizepresetgroup
		     (id, created_at, updated_at, slug, label, icon, sort_order, is_active, parent_id)
		     VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			id, now, now, g.Slug, g.Label, g.Icon, g.SortOrder, boolToInt(g.IsActive), parentID); ierr != nil {
			return "", false, false, ierr
		}
		return id, true, false, nil
	}
	if scanErr != nil {
		return "", false, false, scanErr
	}
	if !overwrite {
		return cur.id, false, false, nil
	}
	differs := cur.label != g.Label || cur.isActive != g.IsActive ||
		nullStringEquals(cur.parent, parentID) == false
	if !g.Unset["icon"] && cur.icon != g.Icon {
		differs = true
	}
	if !g.Unset["sort_order"] && cur.sortOrder != g.SortOrder {
		differs = true
	}
	if !differs {
		return cur.id, false, false, nil
	}
	icon, order := cur.icon, cur.sortOrder
	if !g.Unset["icon"] {
		icon = g.Icon
	}
	if !g.Unset["sort_order"] {
		order = g.SortOrder
	}
	if _, uerr := tx.Exec(`UPDATE presets_sizepresetgroup SET label = ?, icon = ?, sort_order = ?,
	     is_active = ?, parent_id = ?, updated_at = ? WHERE id = ?`,
		g.Label, icon, order, boolToInt(g.IsActive), parentID, now, cur.id); uerr != nil {
		return "", false, false, uerr
	}
	return cur.id, false, true, nil
}

// upsertPresetRow ports importer._upsert for a preset (no kept fields).
func (a *App) upsertPresetRow(tx *sql.Tx, p presetRow, groupID string, overwrite bool, now string,
) (created, changed bool, err error) {
	var cur struct {
		id        string
		label     string
		width     string
		height    string
		unit      string
		icon      string
		sortOrder int
		isActive  bool
		isQuick   bool
		tags      string
		aliases   string
		meta      string
		groupID   string
	}
	row := tx.QueryRow(`SELECT id, label, width, height, unit, icon, sort_order, is_active, is_quick,
	      tags, aliases, meta, group_id FROM presets_sizepreset WHERE slug = ?`, p.Slug)
	scanErr := row.Scan(&cur.id, &cur.label, &cur.width, &cur.height, &cur.unit, &cur.icon, &cur.sortOrder,
		&cur.isActive, &cur.isQuick, &cur.tags, &cur.aliases, &cur.meta, &cur.groupID)
	if scanErr == sql.ErrNoRows {
		id := newUUID4Hex()
		if _, ierr := tx.Exec(`INSERT INTO presets_sizepreset
		     (id, created_at, updated_at, slug, label, width, height, unit, icon, sort_order,
		      is_active, is_quick, tags, aliases, meta, group_id)
		     VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			id, now, now, p.Slug, p.Label, p.Width, p.Height, p.Unit, p.Icon, p.SortOrder,
			boolToInt(p.IsActive), boolToInt(p.IsQuick),
			pyJSONStringList(p.Tags), pyJSONStringList(p.Aliases), p.Meta,
			groupID); ierr != nil {
			return false, false, ierr
		}
		return true, false, nil
	}
	if scanErr != nil {
		return false, false, scanErr
	}
	if !overwrite {
		return false, false, nil
	}
	tags, aliases := pyJSONStringList(p.Tags), pyJSONStringList(p.Aliases)
	differs := cur.label != p.Label || cur.width != p.Width || cur.height != p.Height ||
		cur.unit != p.Unit || cur.icon != p.Icon || cur.sortOrder != p.SortOrder ||
		cur.isActive != p.IsActive || cur.isQuick != p.IsQuick ||
		cur.tags != tags || cur.aliases != aliases || cur.meta != p.Meta ||
		cur.groupID != groupID
	if !differs {
		return false, false, nil
	}
	if _, uerr := tx.Exec(`UPDATE presets_sizepreset SET label = ?, width = ?, height = ?, unit = ?,
	     icon = ?, sort_order = ?, is_active = ?, is_quick = ?, tags = ?, aliases = ?, meta = ?,
	     group_id = ?, updated_at = ? WHERE id = ?`,
		p.Label, p.Width, p.Height, p.Unit, p.Icon, p.SortOrder, boolToInt(p.IsActive), boolToInt(p.IsQuick),
		tags, aliases, p.Meta, groupID, now, cur.id); uerr != nil {
		return false, false, uerr
	}
	return false, true, nil
}

// deactivateMissingPresets ports the replace-mode tail: presets and groups the
// catalogue does not name stop being active.
func (a *App) deactivateMissingPresets(tx *sql.Tx, cat *presetCatalogue, now string) (int, error) {
	total := 0
	presetSlugs := make([]any, 0, len(cat.Presets))
	for _, p := range cat.Presets {
		presetSlugs = append(presetSlugs, p.Slug)
	}
	n, err := deactivateMissing(tx, "presets_sizepreset", presetSlugs, now)
	if err != nil {
		return total, err
	}
	total += n
	groupSlugs := make([]any, 0, len(cat.Groups))
	for _, g := range cat.Groups {
		groupSlugs = append(groupSlugs, g.Slug)
	}
	n, err = deactivateMissing(tx, "presets_sizepresetgroup", groupSlugs, now)
	if err != nil {
		return total, err
	}
	return total + n, nil
}

func deactivateMissing(tx *sql.Tx, table string, slugs []any, now string) (int, error) {
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(slugs)), ",")
	// updated_at is the FIRST placeholder, then the kept slugs.
	args := append([]any{now}, slugs...)
	query := `UPDATE ` + table + ` SET is_active = 0, updated_at = ? WHERE is_active = 1`
	if placeholders != "" {
		query += ` AND slug NOT IN (` + placeholders + `)`
	}
	res, err := tx.Exec(query, args...)
	if err != nil {
		return 0, err
	}
	affected, _ := res.RowsAffected()
	return int(affected), nil
}

// findPresetNameClash ports importer.find_name_clash.
func (a *App) findPresetNameClash(tx *sql.Tx) (string, error) {
	rows, err := tx.Query(`SELECT slug, aliases FROM presets_sizepreset WHERE is_active = 1`)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	owner := map[string]string{}
	order := []struct {
		slug    string
		aliases []string
	}{}
	for rows.Next() {
		var slug, aliases string
		if err := rows.Scan(&slug, &aliases); err != nil {
			return "", err
		}
		order = append(order, struct {
			slug    string
			aliases []string
		}{slug, parseJSONStringList(aliases)})
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	for _, row := range order {
		names := append([]string{row.slug}, row.aliases...)
		for _, name := range names {
			if existing, ok := owner[name]; ok && existing != row.slug {
				return fmt.Sprintf("%q would mean both %q and %q: slugs and aliases must differ.",
					name, existing, row.slug), nil
			}
			owner[name] = row.slug
		}
	}
	return "", nil
}

func parseJSONStringList(text string) []string {
	var out []string
	if text == "" {
		return out
	}
	_ = json.Unmarshal([]byte(text), &out)
	return out
}

func boolToInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

func nullStringEquals(v sql.NullString, want any) bool {
	if want == nil {
		return !v.Valid
	}
	s, ok := want.(string)
	return ok && v.Valid && v.String == s
}
