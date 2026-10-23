// Server-side validation of design documents — a faithful port of
// apps/designs/validators.py (the safety net: bounded size and shape,
// refusal of client-local image references, iterative walk so nothing
// recurses the stack).
//
// Ports backend/apps/designs/validators.py
// Ports backend/apps/designs/exceptions.py (the three document errors)
package doory

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

const (
	supportedSchemaVersion = 1
	maxPages               = 100
	maxDimension           = 20_000
	maxObjectsPerPage      = 5_000
	maxDepth               = 64
	maxNodes               = 2_500_000
	maxDataURLChars        = 64 * 1024
	maxPresetLength        = 64
	maxNotesLength         = 5_000
	minPageDurationSec     = 1
	maxPageDurationSec     = 600
	maxAudioTracks         = 32
	maxAudioIDLength       = 64
	maxAudioNameLength     = 200
	maxAudioSrcLength      = 2_048
	maxDesignDocumentBytes = 12 * 1024 * 1024 // base.py MAX_DESIGN_DOCUMENT_BYTES
)

var (
	animationPreset     = regexp.MustCompile(`^[a-z0-9-]{1,40}$`)
	animationSlots      = []string{"enter", "exit", "emphasis"}
	animationEasings    = setOf("linear", "ease-in", "ease-out", "ease-in-out", "spring", "bounce")
	animationDirections = setOf("left", "right", "up", "down", "in", "out")
	animationTextUnits  = setOf("object", "line", "word", "letter")
	audioKinds          = setOf("music", "voiceover", "sfx", "upload")
	audioScopes         = setOf("design", "page")
	audioMSRanges       = map[string][2]float64{
		"startMs": {0, 86_400_000}, "trimStartMs": {0, 86_400_000},
		"trimEndMs": {0, 86_400_000}, "durationMs": {0, 86_400_000},
		"fadeInMs": {0, 60_000}, "fadeOutMs": {0, 60_000},
	}
	audioFlags           = []string{"loop", "muted", "duckUnderVoiceover"}
	localReference       = regexp.MustCompile(`(?i)^\s*(?:blob:|idb://)\S*\s*$`)
	animationDurationMS  = [2]float64{100, 10_000}
	animationDelayMS     = [2]float64{0, 60_000}
	transitionDurationMS = [2]float64{200, 3_000}
)

func setOf(items ...string) map[string]bool {
	m := map[string]bool{}
	for _, i := range items {
		m[i] = true
	}
	return m
}

// ---- document errors (apps/designs/exceptions.py codes/messages) ----

// DocumentError carries the API code; Message is the raised detail (the
// validator raises with specific text; the class defaults cover the paths
// that raise bare).
type DocumentError struct {
	Code    string
	Message string
	Status  int
}

func (e *DocumentError) Error() string { return e.Message }

func invalidDocument(format string, args ...any) *DocumentError {
	return &DocumentError{Code: "invalid_document", Status: 400,
		Message: fmt.Sprintf(format, args...)}
}

func inlineAssetNotAllowed() *DocumentError {
	return &DocumentError{Code: "inline_asset_not_allowed", Status: 400,
		Message: "Local (blob:, idb://) and oversized inline (data:) images must be uploaded as assets first."}
}

func documentTooLarge(format string, args ...any) *DocumentError {
	return &DocumentError{Code: "document_too_large", Status: 413,
		Message: fmt.Sprintf(format, args...)}
}

// DocumentInfo is validators.DocumentInfo: facts derived from a document.
type DocumentInfo struct {
	Width     int
	Height    int
	PageCount int
	Preset    string
}

func isInt(v any) bool {
	_, ok := v.(json.Number)
	if !ok {
		_, isInt := v.(int64)
		return isInt
	}
	// json.Number is an integer iff it parses without fraction/exponent
	// (Python: int is int; bool excluded by construction in Go).
	return !strings.ContainsAny(string(v.(json.Number)), ".eE")
}

func asNumber(v any) (float64, bool) {
	switch x := v.(type) {
	case json.Number:
		f, err := x.Float64()
		return f, err == nil
	case int64:
		return float64(x), true
	case float64:
		return x, true
	}
	return 0, false
}

func intVal(v any) (int64, bool) {
	switch x := v.(type) {
	case json.Number:
		if !strings.ContainsAny(string(x), ".eE") {
			n, err := x.Int64()
			return n, err == nil
		}
	case int64:
		return x, true
	}
	return 0, false
}

func checkDimension(doc map[string]any, key string) (int64, error) {
	v, ok := doc[key]
	n, isN := intVal(v)
	if !ok || !isN || n < 1 || n > maxDimension {
		return 0, invalidDocument("`%s` must be an integer between 1 and %d.", key, maxDimension)
	}
	return n, nil
}

// checkNumber ports _check_number (range + type; NaN/Inf can't appear —
// Go's parser rejects them; Python's float range test refuses them).
func checkNumber(value any, low, high float64, where, key string) error {
	f, ok := asNumber(value)
	if !ok || f < low || f > high {
		return invalidDocument("%s `%s` must be a number between %g and %g.", where, key, low, high)
	}
	return nil
}

// checkMotion ports _check_motion: short-slug preset, allowed enums,
// in-range settings; null counts as unset.
func checkMotion(value any, where string, durations [2]float64, transition bool) error {
	obj, ok := value.(map[string]any)
	if !ok {
		return invalidDocument("%s must be an object.", where)
	}
	preset, _ := obj["preset"].(string)
	if !animationPreset.MatchString(preset) {
		return invalidDocument("%s `preset` must be 1 to 40 characters of a-z, 0-9 and hyphens.", where)
	}
	for _, pair := range []struct {
		key     string
		allowed map[string]bool
	}{{"easing", animationEasings}, {"direction", animationDirections}} {
		if v, present := obj[pair.key]; present && v != nil {
			s, isStr := v.(string)
			if !isStr || !pair.allowed[s] {
				return invalidDocument("%s `%s` is not one of %s.", where, pair.key, sortedJoin(pair.allowed))
			}
		}
	}
	if v, present := obj["durationMs"]; present && v != nil {
		if err := checkNumber(v, durations[0], durations[1], where, "durationMs"); err != nil {
			return err
		}
	}
	if transition {
		return nil
	}
	if v, present := obj["delayMs"]; present && v != nil {
		if err := checkNumber(v, animationDelayMS[0], animationDelayMS[1], where, "delayMs"); err != nil {
			return err
		}
	}
	if v, present := obj["intensity"]; present && v != nil {
		if err := checkNumber(v, 0, 1, where, "intensity"); err != nil {
			return err
		}
	}
	if v, present := obj["by"]; present && v != nil {
		s, isStr := v.(string)
		if !isStr || !animationTextUnits[s] {
			return invalidDocument("%s `by` is not one of %s.", where, sortedJoin(animationTextUnits))
		}
	}
	if v, present := obj["loop"]; present && v != nil {
		if _, isBool := v.(bool); !isBool {
			return invalidDocument("%s `loop` must be true or false.", where)
		}
	}
	return nil
}

func sortedJoin(set map[string]bool) string {
	items := make([]string, 0, len(set))
	for k := range set {
		items = append(items, k)
	}
	sortStrings(items)
	return strings.Join(items, ", ")
}

func checkPresentation(page map[string]any, index int) error {
	if notes, present := page["notes"]; present && notes != nil {
		s, isStr := notes.(string)
		if !isStr || len([]rune(s)) > maxNotesLength {
			return invalidDocument("Page %d `notes` must be text of at most %d characters.", index+1, maxNotesLength)
		}
	}
	duration, present := page["durationSec"]
	if !present || duration == nil {
		return nil
	}
	f, isNum := asNumber(duration)
	if !isNum || f < minPageDurationSec || f > maxPageDurationSec {
		return invalidDocument("Page %d `durationSec` must be a number between %d and %d.",
			index+1, minPageDurationSec, maxPageDurationSec)
	}
	return nil
}

func checkAnimation(page map[string]any, index int) error {
	if transition, present := page["transition"]; present && transition != nil {
		if err := checkMotion(transition, fmt.Sprintf("Page %d `transition`", index+1),
			transitionDurationMS, true); err != nil {
			return err
		}
	}
	objects, _ := page["objects"].([]any)
	for pos, item := range objects {
		obj, ok := item.(map[string]any)
		if !ok {
			continue // structure pass already rejected non-objects
		}
		animation, present := obj["animation"]
		if !present || animation == nil {
			continue
		}
		where := fmt.Sprintf("Page %d object %d `animation`", index+1, pos+1)
		anim, ok := animation.(map[string]any)
		if !ok {
			return invalidDocument("%s must be an object.", where)
		}
		for _, slot := range animationSlots {
			if v, has := anim[slot]; has && v != nil {
				if err := checkMotion(v, where+"."+slot, animationDurationMS, false); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func checkAudioSrc(src any, where string) error {
	s, isStr := src.(string)
	if !isStr || s == "" || len([]rune(s)) > maxAudioSrcLength {
		return invalidDocument("%s `src` must be a link of at most %d characters.", where, maxAudioSrcLength)
	}
	if localReference.MatchString(s) {
		return nil // left to the document walk (inline_asset_not_allowed)
	}
	if !(strings.HasPrefix(s, "https://") ||
		(strings.HasPrefix(s, "/") && !strings.HasPrefix(s, "//"))) {
		return invalidDocument("%s `src` must be an https:// address or a path on this site.", where)
	}
	return nil
}

func checkAudioTrack(track any, index int, seen map[string]bool) error {
	where := fmt.Sprintf("Audio track %d", index+1)
	obj, ok := track.(map[string]any)
	if !ok {
		return invalidDocument("%s must be an object.", where)
	}
	id, isStr := obj["id"].(string)
	if !isStr || len([]rune(id)) < 1 || len([]rune(id)) > maxAudioIDLength || seen[id] {
		return invalidDocument("%s needs a unique string `id` of at most %d characters.", where, maxAudioIDLength)
	}
	seen[id] = true
	kind, isStr := obj["kind"].(string)
	if !isStr || !audioKinds[kind] {
		return invalidDocument("%s `kind` is not one of %s.", where, sortedJoin(audioKinds))
	}
	if name, present := obj["name"]; present && name != nil {
		s, isStr := name.(string)
		if !isStr || len([]rune(s)) > maxAudioNameLength {
			return invalidDocument("%s `name` must be text of at most %d characters.", where, maxAudioNameLength)
		}
	}
	if err := checkAudioSrc(obj["src"], where); err != nil {
		return err
	}
	if assetID, present := obj["assetId"]; present && assetID != nil {
		s, isStr := assetID.(string)
		if !isStr || len([]rune(s)) > maxAudioIDLength {
			return invalidDocument("%s `assetId` must be a string of at most %d characters.", where, maxAudioIDLength)
		}
	}
	scope, ok := obj["scope"].(map[string]any)
	scopeType, _ := scope["type"].(string)
	if !ok || !audioScopes[scopeType] {
		return invalidDocument("%s `scope` must be {type: 'design'} or {type: 'page', pageId}.", where)
	}
	if scopeType == "page" {
		pid, isStr := scope["pageId"].(string)
		if !isStr || len([]rune(pid)) < 1 || len([]rune(pid)) > maxAudioIDLength {
			return invalidDocument("%s `scope.pageId` must be a page id.", where)
		}
	}
	for key, rng := range audioMSRanges {
		if v, present := obj[key]; present && v != nil {
			if err := checkNumber(v, rng[0], rng[1], where, key); err != nil {
				return err
			}
		}
	}
	if v, present := obj["volume"]; present && v != nil {
		if err := checkNumber(v, 0, 1, where, "volume"); err != nil {
			return err
		}
	}
	for _, key := range audioFlags {
		if v, present := obj[key]; present && v != nil {
			if _, isBool := v.(bool); !isBool {
				return invalidDocument("%s `%s` must be true or false.", where, key)
			}
		}
	}
	start, hasStart := obj["trimStartMs"]
	end, hasEnd := obj["trimEndMs"]
	if hasStart && hasEnd && start != nil && end != nil {
		fs, okS := asNumber(start)
		fe, okE := asNumber(end)
		if okS && okE && fe < fs {
			return invalidDocument("%s `trimEndMs` cannot be before `trimStartMs`.", where)
		}
	}
	return nil
}

func checkAudioTracks(doc map[string]any) error {
	tracks, present := doc["audioTracks"]
	if !present || tracks == nil {
		return nil
	}
	list, ok := tracks.([]any)
	if !ok || len(list) > maxAudioTracks {
		return invalidDocument("`audioTracks` must be a list of at most %d tracks.", maxAudioTracks)
	}
	seen := map[string]bool{}
	for i, t := range list {
		if err := checkAudioTrack(t, i, seen); err != nil {
			return err
		}
	}
	return nil
}

// checkStructure ports _check_structure; returns the derived facts.
func checkStructure(doc any) (DocumentInfo, error) {
	obj, ok := doc.(map[string]any)
	if !ok {
		return DocumentInfo{}, invalidDocument("The document must be a JSON object.")
	}
	version, isInt := intVal(obj["schemaVersion"])
	if !isInt || version < 1 {
		return DocumentInfo{}, invalidDocument("`schemaVersion` must be an integer of at least 1.")
	}
	if version > supportedSchemaVersion {
		return DocumentInfo{}, invalidDocument("Schema version %d is newer than this server supports.", version)
	}
	width, err := checkDimension(obj, "width")
	if err != nil {
		return DocumentInfo{}, err
	}
	height, err := checkDimension(obj, "height")
	if err != nil {
		return DocumentInfo{}, err
	}
	pages, ok := obj["pages"].([]any)
	if !ok || len(pages) < 1 || len(pages) > maxPages {
		return DocumentInfo{}, invalidDocument("`pages` must be a list of 1 to %d pages.", maxPages)
	}
	for index, raw := range pages {
		page, ok := raw.(map[string]any)
		if !ok {
			return DocumentInfo{}, invalidDocument("Page %d must be an object.", index+1)
		}
		pid, isStr := page["id"].(string)
		if !isStr || pid == "" {
			return DocumentInfo{}, invalidDocument("Page %d needs a string `id`.", index+1)
		}
		for _, key := range []string{"width", "height"} {
			if v, present := page[key]; present {
				n, isN := intVal(v)
				if !isN || n < 1 || n > maxDimension {
					return DocumentInfo{}, invalidDocument("Page %d `%s` must be an integer between 1 and %d.",
						index+1, key, maxDimension)
				}
			}
		}
		objects, ok := page["objects"].([]any)
		if !ok || len(objects) > maxObjectsPerPage {
			return DocumentInfo{}, invalidDocument("Page %d `objects` must be a list of at most %d objects.",
				index+1, maxObjectsPerPage)
		}
		for _, item := range objects {
			if _, isObj := item.(map[string]any); !isObj {
				return DocumentInfo{}, invalidDocument("Every object on page %d must be a JSON object.", index+1)
			}
		}
		if err := checkPresentation(page, index); err != nil {
			return DocumentInfo{}, err
		}
		if err := checkAnimation(page, index); err != nil {
			return DocumentInfo{}, err
		}
	}
	if err := checkAudioTracks(obj); err != nil {
		return DocumentInfo{}, err
	}
	meta, present := obj["meta"]
	if present && meta != nil {
		if _, ok := meta.(map[string]any); !ok {
			return DocumentInfo{}, invalidDocument("`meta` must be an object.")
		}
	}
	preset := ""
	if metaMap, ok := obj["meta"].(map[string]any); ok {
		if p, isStr := metaMap["presetId"].(string); isStr && len([]rune(p)) <= maxPresetLength {
			preset = p
		}
	}
	return DocumentInfo{Width: int(width), Height: int(height), PageCount: len(pages), Preset: preset}, nil
}

// checkText ports _check_text: NUL, UTF-8, local references, oversized data
// URLs (Python's isascii/encode gate is a no-op for Go strings, which are
// always valid UTF-8 after decoding — invalid input never parses).
func checkText(text string) error {
	if strings.ContainsRune(text, 0) {
		return invalidDocument("The document contains a NUL character.")
	}
	if !utf8.ValidString(text) {
		return invalidDocument("The document contains invalid Unicode text.")
	}
	head := strings.TrimLeft(text[:min(32, len(text))], " \t\n\r")
	head = strings.ToLower(head)
	if strings.HasPrefix(head, "blob:") || strings.HasPrefix(head, "idb://") {
		if localReference.MatchString(text) {
			return inlineAssetNotAllowed()
		}
	}
	if len(text) > maxDataURLChars && (strings.HasPrefix(head, "data:") || strings.Contains(text, ";base64,")) {
		return inlineAssetNotAllowed()
	}
	return nil
}

// walkDocument ports _walk: iterative depth-limited traversal with node
// budget, text checks on keys and values.
func walkDocument(root any) error {
	type entry struct {
		value any
		depth int
	}
	stack := []entry{{root, 0}}
	nodes := 0
	for len(stack) > 0 {
		e := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		nodes++
		if nodes > maxNodes {
			return invalidDocument("The document is too complex.")
		}
		switch v := e.value.(type) {
		case string:
			if err := checkText(v); err != nil {
				return err
			}
		case float64:
			// Go parses only finite floats (NaN/Inf literals are parse
			// errors) — Python rejects them in the range tests instead.
		case map[string]any:
			if e.depth >= maxDepth {
				return invalidDocument("The document is nested deeper than %d levels.", maxDepth)
			}
			for key, child := range v {
				if err := checkText(key); err != nil {
					return err
				}
				stack = append(stack, entry{child, e.depth + 1})
			}
		case []any:
			if e.depth >= maxDepth {
				return invalidDocument("The document is nested deeper than %d levels.", maxDepth)
			}
			for _, child := range v {
				stack = append(stack, entry{child, e.depth + 1})
			}
		}
	}
	return nil
}

// validateDocument ports validate_document: structure, walk, size bound
// (400 invalid_document / inline_asset_not_allowed, 413 document_too_large).
// Every failure path raises *DocumentError, so the assertion is total.
func validateDocument(doc any, maxBytes int) *DocumentError {
	if _, err := checkStructure(doc); err != nil {
		return err.(*DocumentError)
	}
	if err := walkDocument(doc); err != nil {
		return err.(*DocumentError)
	}
	encoded := len(compactValue(doc))
	if encoded > maxBytes {
		return documentTooLarge("The design document is larger than %g MB.", float64(maxBytes)/(1024*1024))
	}
	return nil
}

// describeDocument ports describe_document (facts from an already-validated
// document).
func describeDocument(doc any) (DocumentInfo, error) { return checkStructure(doc) }

// compactValue renders like Python json.dumps(..., ensure_ascii=False,
// separators=(",",":")) — Go Marshal without HTML escaping; number literals
// pass through json.Number unmodified.
func compactValue(v any) []byte {
	return encodeCompactJSON(v)
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
