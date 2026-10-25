// Template fields of a design document (apps/designs/doc/fields.py +
// kinds.py + the text-estimate builders they use).
//
// The rules match the editor's: a key is [a-z][a-z0-9_]{0,39} and names one
// value; it is used at most once per page; the same key on several pages is
// the same value there, so it must have the same type everywhere.
package doory

import (
	"encoding/json"
	"math"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

const (
	fieldLabelMax    = 80
	minFontSize      = 4    // builders.MIN_FONT_SIZE
	maxFontSize      = 999  // builders.MAX_FONT_SIZE
	fontSizeMult     = 1.13 // builders.FONT_SIZE_MULT
	averageGlyphWide = 0.52 // builders.AVERAGE_GLYPH_WIDTH
)

var fieldKeyPattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,39}$`)

var fieldTypes = map[string]bool{"text": true, "image": true, "color": true}

var (
	textObjectTypes  = map[string]bool{"textbox": true, "itext": true, "i-text": true, "text": true}
	lineObjectTypes  = map[string]bool{"doryline": true, "line": true}
	shapeObjectTypes = map[string]bool{
		"rect": true, "ellipse": true, "circle": true, "triangle": true,
		"polygon": true, "polyline": true, "path": true,
	}
	groupObjectTypes = map[string]bool{"group": true, "activeselection": true}
)

// objectKind ports doc/kinds.object_kind: qr, placeholder, element, video,
// image, text, line, shape, group or unknown (highest priority first).
func objectKind(obj map[string]any) string {
	if v, ok := obj["qr"]; ok && truthy(v) {
		return "qr"
	}
	if b, ok := obj["isPlaceholder"].(bool); ok && b {
		return "placeholder"
	}
	if _, ok := obj["elementId"].(string); ok {
		return "element"
	}
	kind := strings.ToLower(pyStr(obj["type"]))
	if kind == "image" {
		if _, ok := obj["videoSrc"].(string); ok {
			return "video"
		}
		return "image"
	}
	switch {
	case textObjectTypes[kind]:
		return "text"
	case lineObjectTypes[kind]:
		return "line"
	case shapeObjectTypes[kind]:
		return "shape"
	case groupObjectTypes[kind]:
		return "group"
	}
	return "unknown"
}

// truthy is Python truthiness for the values JSON can carry.
func truthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case string:
		return x != ""
	case float64:
		return x != 0
	case int64:
		return x != 0
	case []any:
		return len(x) > 0
	case map[string]any:
		return len(x) > 0
	}
	return true
}

// pyStr renders a JSON value the way Python's str() does (used for text
// values, which are normally strings already).
func pyStr(v any) string {
	switch x := v.(type) {
	case nil:
		return "None"
	case string:
		return x
	case bool:
		if x {
			return "True"
		}
		return "False"
	case float64:
		return pyNum(x)
	case int64:
		return strconv.FormatInt(x, 10)
	case json.Number:
		return string(x)
	}
	return ""
}

// pyNum renders a float like Python's str(): no trailing ".0" for whole
// numbers, shortest round-trip otherwise.
func pyNum(v float64) string {
	if v == math.Trunc(v) && math.Abs(v) < 1e16 {
		return strconv.FormatInt(int64(v), 10)
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}

// readFieldTag ports doc/fields.read_field: the tag when well formed, else
// nil (a malformed tag is ignored, like the editor does).
func readFieldTag(value any) map[string]any {
	obj, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	key, ok := obj["key"].(string)
	if !ok || !fieldKeyPattern.MatchString(key) {
		return nil
	}
	kind, _ := obj["type"].(string)
	if !fieldTypes[kind] {
		return nil
	}
	tag := map[string]any{"key": key, "type": kind}
	if label, ok := obj["label"].(string); ok && label != "" {
		tag["label"] = label
	}
	return tag
}

// readFit ports doc/fields.read_fit: the shrink-to-fit setting when well
// formed, else nil.
func readFit(value any) map[string]any {
	obj, ok := value.(map[string]any)
	if !ok || pyStr(obj["mode"]) != "shrink" {
		return nil
	}
	if minSize, ok := asNumber(obj["minFontSize"]); ok && minSize >= minFontSize {
		clamped := math.Min(float64(maxFontSize), minSize)
		return map[string]any{"mode": "shrink", "minFontSize": builderNum(clamped)}
	}
	return map[string]any{"mode": "shrink"}
}

// builderNum ports builders._num: whole floats as ints, others to three
// decimals.
func builderNum(value float64) any {
	rounded := math.Round(value*1000) / 1000
	if rounded == math.Trunc(rounded) {
		return int64(rounded)
	}
	return rounded
}

// estimateTextLines ports builders.estimate_text_lines: how many lines a
// textbox wraps to (an estimate; the editor measures real font metrics).
func estimateTextLines(text string, width, size float64, weight int) int {
	glyph := size * averageGlyphWide
	if weight >= 600 {
		glyph *= 1.06
	}
	perLine := int(math.Floor(width / math.Max(glyph, 0.1)))
	if perLine < 1 {
		perLine = 1
	}
	lines := 0
	for _, paragraph := range strings.Split(text, "\n") {
		length, count := 0, 1
		for _, word := range strings.Split(paragraph, " ") {
			needed := utf8.RuneCountInString(word)
			if length != 0 {
				needed = length + 1 + utf8.RuneCountInString(word)
			}
			if needed <= perLine {
				length = needed
				continue
			}
			if length != 0 {
				count++
			}
			length = utf8.RuneCountInString(word)
			for length > perLine {
				count++
				length -= perLine
			}
		}
		lines += count
	}
	if lines < 1 {
		return 1
	}
	return lines
}

// textHeight ports builders.text_height (Fabric's textbox height).
func textHeight(lines int, size, lineHeight float64) float64 {
	return size * fontSizeMult * (1 + float64(lines-1)*lineHeight)
}

// validFontSize ports builders.valid_font_size (1..999, finite).
func validFontSize(value any) (float64, bool) {
	v, ok := asNumber(value)
	if !ok || v < 1 || v > maxFontSize {
		return 0, false
	}
	return v, true
}

// posFloat ports builders._pos_float: a positive finite number, else default.
func posFloat(value any, def float64) float64 {
	if v, ok := asNumber(value); ok && v > 0 {
		return v
	}
	return def
}

// jsRound ports builders.js_round (Math.round: halves up).
func jsRound(value float64) int {
	return int(math.Floor(value + 0.5))
}

// fitFontSize ports builders.fit_font_size: the largest font size (1 px steps
// from `size` down to `minSize`) at which `text` wraps inside width x height,
// found by binary search.
func fitFontSize(text string, width, height, size float64, weight int, lineHeight float64, minSize *float64) any {
	target := math.Min(float64(maxFontSize), math.Max(float64(minFontSize), size))
	rawMin := float64(jsRound(target * 0.5))
	if rawMin < float64(minFontSize) {
		rawMin = float64(minFontSize)
	}
	if minSize != nil {
		rawMin = *minSize
	}
	floor := math.Min(target, math.Max(float64(minFontSize), math.Min(float64(maxFontSize), rawMin)))
	fits := func(candidate float64) bool {
		lines := estimateTextLines(text, width, candidate, weight)
		return textHeight(lines, candidate, lineHeight) <= height+1e-3
	}
	if fits(target) || target <= floor {
		return builderNum(target)
	}
	low, high := 1, int(math.Ceil(target-floor))
	for low < high {
		mid := (low + high) / 2
		if fits(math.Max(floor, target-float64(mid))) {
			high = mid
		} else {
			low = mid + 1
		}
	}
	return builderNum(math.Max(floor, target-float64(low)))
}

// textOverflows ports doc/fields.text_overflows: true when a shrink-to-fit
// text still does not fit inside its box at its minimum font size.
func textOverflows(obj map[string]any) bool {
	fit := readFit(obj["fit"])
	size, ok := validFontSize(obj["fontSize"])
	if fit == nil || !ok {
		return false
	}
	text := pyStr(obj["text"])
	width := posFloat(obj["width"], 200)
	leading := posFloat(obj["lineHeight"], 1.16)
	height := posFloat(obj["height"], textHeight(1, size, leading))
	weight := int(posFloat(obj["fontWeight"], 400))
	var minSize *float64
	if v, ok := asNumber(fit["minFontSize"]); ok {
		minSize = &v
	}
	fittedAny := fitFontSize(text, width, height, size, weight, leading, minSize)
	fitted, _ := asNumber(fittedAny)
	lines := estimateTextLines(text, width, fitted, weight)
	return textHeight(lines, fitted, leading) > height+1e-3
}

// fieldEntry is one entry of list_fields (the API schema's FieldSerializer).
type fieldEntry struct {
	Key       *string `json:"key"`
	Type      string  `json:"type"`
	Label     *string `json:"label"`
	Pages     []int   `json:"pages"`
	ObjectIDs []any   `json:"object_ids"`
	Current   any     `json:"current"`
	Fit       any     `json:"fit"`
	Overflow  bool    `json:"overflow"`
}

func newFieldEntry(key *string, fieldType string, label *string, obj map[string]any, page int) fieldEntry {
	entry := fieldEntry{
		Key: key, Type: fieldType, Label: label,
		Pages:     []int{page},
		ObjectIDs: []any{obj["id"]},
		Current:   currentValue(obj, fieldType),
	}
	if fieldType == "text" {
		entry.Fit = readFitOrNil(obj["fit"])
		entry.Overflow = textOverflows(obj)
	}
	return entry
}

func readFitOrNil(value any) any {
	if fit := readFit(value); fit != nil {
		return fit
	}
	return nil
}

// currentValue ports doc/fields._current.
func currentValue(obj map[string]any, fieldType string) any {
	switch fieldType {
	case "text":
		return pyStr(obj["text"])
	case "image":
		url := obj["docSrc"]
		if s, ok := url.(string); !ok || s == "" {
			url = obj["src"]
		}
		return map[string]any{"asset_id": obj["assetId"], "url": url}
	}
	if fill, ok := obj["fill"].(string); ok {
		return fill
	}
	return nil
}

// walkObjects ports doc/fields._walk: every object of a list, nested group
// members too, depth-first in document order.
func walkObjects(objects any) []map[string]any {
	out := []map[string]any{}
	root, ok := objects.([]any)
	if !ok {
		return out
	}
	stack := [][]any{root}
	index := []int{0}
	for len(stack) > 0 {
		top := len(stack) - 1
		if index[top] >= len(stack[top]) {
			stack = stack[:top]
			index = index[:top]
			continue
		}
		item := stack[top][index[top]]
		index[top]++
		obj, ok := item.(map[string]any)
		if !ok {
			continue
		}
		out = append(out, obj)
		if children, ok := obj["objects"].([]any); ok {
			stack = append(stack, children)
			index = append(index, 0)
		}
	}
	return out
}

// listFields ports doc/fields.list_fields: the fields of a design, one entry
// per key, in first-use order; untagged photo placeholders are listed as
// suggested image fields with key null.
func listFields(document map[string]any) []fieldEntry {
	out := []fieldEntry{}
	byKey := map[string]int{}
	pages, _ := document["pages"].([]any)
	for number, rawPage := range pages {
		page, ok := rawPage.(map[string]any)
		if !ok {
			continue
		}
		for _, obj := range walkObjects(page["objects"]) {
			tag := readFieldTag(obj["field"])
			if tag == nil {
				if objectKind(obj) == "placeholder" {
					out = append(out, newFieldEntry(nil, "image", nil, obj, number+1))
				}
				continue
			}
			key := tag["key"].(string)
			fieldType := tag["type"].(string)
			index, seen := byKey[key]
			if !seen {
				var label *string
				if l, ok := tag["label"].(string); ok {
					label = &l
				}
				byKey[key] = len(out)
				out = append(out, newFieldEntry(&key, fieldType, label, obj, number+1))
				continue
			}
			entry := &out[index]
			if entry.Type != fieldType || containsPage(entry.Pages, number+1) {
				continue
			}
			entry.ObjectIDs = append(entry.ObjectIDs, obj["id"])
			entry.Pages = append(entry.Pages, number+1)
			if fieldType == "text" {
				if entry.Fit == nil {
					entry.Fit = readFitOrNil(obj["fit"])
				}
				if textOverflows(obj) {
					entry.Overflow = true
				}
			}
		}
	}
	return out
}

func containsPage(values []int, want int) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
