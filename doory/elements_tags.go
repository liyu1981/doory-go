// Element search text and tag handling (apps/elements/search.py clean_tags /
// fold / search_name / search_tags, plus the tag-file parsers of
// apps/elements/tagdata.py).
//
// Every element carries two denormalised, accent-folded, lower-case columns
// (search_name, search_tags) that the importer keeps in step with name and
// tags; queries match them with LIKE and rank with a CASE expression, so a
// search touches the database only twice regardless of catalogue size.

package doory

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"golang.org/x/text/unicode/norm"
)

const (
	elementMaxTags        = 30
	elementMaxTagLength   = 40
	elementMaxSearchWords = 8
)

var elementSpaces = regexp.MustCompile(`\s+`)

// foldElementText is search.fold: lower-case, accent-free, single-spaced.
func foldElementText(text string) string {
	decomposed := norm.NFKD.String(text)
	var stripped strings.Builder
	for _, r := range decomposed {
		if isCombiningMark(r) {
			continue
		}
		stripped.WriteRune(r)
	}
	return strings.TrimSpace(elementSpaces.ReplaceAllString(strings.ToLower(stripped.String()), " "))
}

// isCombiningMark reports a Unicode combining mark (unicodedata.combining).
func isCombiningMark(r rune) bool {
	// Combining Diacritical Marks and the common combining ranges.
	switch {
	case r >= 0x0300 && r <= 0x036F: // Combining Diacritical Marks
		return true
	case r >= 0x1AB0 && r <= 0x1AFF: // Combining Diacritical Marks Extended
		return true
	case r >= 0x1DC0 && r <= 0x1DFF: // Combining Diacritical Marks Supplement
		return true
	case r >= 0x20D0 && r <= 0x20FF: // Combining Diacritical Marks for Symbols
		return true
	case r >= 0xFE20 && r <= 0xFE2F: // Combining Half Marks
		return true
	}
	return false
}

// elementSearchName is search.search_name.
func elementSearchName(name string) string { return foldElementText(name) }

// elementSearchTags is search.search_tags: the folded tags, padded so ` tag `
// matches whole tags.
func elementSearchTags(tags []string) string {
	var folded []string
	seen := map[string]bool{}
	for _, tag := range tags {
		value := foldElementText(tag)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		folded = append(folded, value)
	}
	if len(folded) == 0 {
		return ""
	}
	return " " + strings.Join(folded, " ") + " "
}

// cleanElementTags is search.clean_tags: display tags, lower-cased,
// de-duplicated, short, at most `limit`, order kept.
func cleanElementTags(candidates []string, limit int) []string {
	if limit == 0 {
		limit = elementMaxTags
	}
	var out []string
	seen := map[string]bool{}
	for _, candidate := range candidates {
		tag := strings.ToLower(strings.TrimSpace(elementSpaces.ReplaceAllString(candidate, " ")))
		if tag == "" || len([]rune(tag)) > elementMaxTagLength || strings.HasPrefix(tag, "*") {
			continue
		}
		if seen[tag] {
			continue
		}
		seen[tag] = true
		out = append(out, tag)
		if len(out) >= limit {
			break
		}
	}
	return out
}

// ------------------------------------------------------------- tag files ----

// elementTagDataError is tagdata.TagDataError.
type elementTagDataError struct{ message string }

func (e *elementTagDataError) Error() string { return e.message }

func tagDataErrorf(format string, args ...any) error {
	return &elementTagDataError{message: fmt.Sprintf(format, args...)}
}

var (
	// Go's regexp has no backreferences: the enum body is matched literally on
	// the variable name (group 1) because the reference `(X || {})` repeats it.
	phosphorEnumPattern   = regexp.MustCompile(`(?s)(\w+) = /\* @__PURE__ \*/ \(\((\w+)\) => \((.*?)\)\)\(\w+ \|\| \{\}\)`)
	phosphorMemberPattern = regexp.MustCompile(`\w+\.(\w+) = "([^"]*)"`)
	phosphorIconPattern   = regexp.MustCompile(`(?s)\{\s*name:\s*"(?P<name>[^"]+)",\s*pascal_name:\s*"[^"]*",\s*` +
		`(?:alias:\s*\{\s*name:\s*"(?P<alias>[^"]+)"[^}]*\},\s*)?` +
		`categories:\s*\[(?P<categories>[^\]]*)\],[^\[\]]*?` +
		`tags:\s*\[(?P<tags>[^\]]*)\]`)
	phosphorStringPattern = regexp.MustCompile(`"((?:[^"\\]|\\.)*)"`)
)

// parseElementTags is tagdata.parse_tags: `{icon key: [tags]}` for the
// manifest's tag format.
func parseElementTags(format string, data []byte) (map[string][]string, error) {
	switch format {
	case "none":
		return map[string][]string{}, nil
	case "json-map":
		return parseTagJSONMap(data)
	case "tabler":
		return parseTagTabler(data)
	case "iconify-categories":
		return parseTagIconifyCategories(data)
	case "phosphor-js":
		return parseTagPhosphor(data)
	}
	return map[string][]string{}, nil
}

func decodeTagJSON(data []byte) (any, error) {
	var raw any
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.UseNumber()
	if err := dec.Decode(&raw); err != nil {
		return nil, tagDataErrorf("not valid JSON")
	}
	return raw, nil
}

// tagWords is tagdata._words: a scalar becomes one word.
func tagWords(value any) []string {
	switch v := value.(type) {
	case string:
		return []string{v}
	case json.Number:
		return []string{v.String()}
	case float64:
		return []string{strconv.FormatFloat(v, 'g', -1, 64)}
	}
	return nil
}

// parseTagJSONMap is tagdata.parse_json_map (`{"name": ["tag", ...]}`).
func parseTagJSONMap(data []byte) (map[string][]string, error) {
	raw, err := decodeTagJSON(data)
	if err != nil {
		return nil, err
	}
	obj, ok := raw.(map[string]any)
	if !ok {
		return nil, tagDataErrorf("expected an object mapping icon names to tag lists")
	}
	out := map[string][]string{}
	for name, value := range obj {
		list, ok := value.([]any)
		if !ok {
			continue
		}
		var candidates []string
		for _, item := range list {
			candidates = append(candidates, tagWords(item)...)
		}
		out[name] = cleanElementTags(candidates, elementMaxTags)
	}
	return out, nil
}

// parseTagTabler is tagdata.parse_tabler.
func parseTagTabler(data []byte) (map[string][]string, error) {
	raw, err := decodeTagJSON(data)
	if err != nil {
		return nil, err
	}
	obj, ok := raw.(map[string]any)
	if !ok {
		return nil, tagDataErrorf("expected an object keyed by icon name")
	}
	out := map[string][]string{}
	for name, value := range obj {
		entry, ok := value.(map[string]any)
		if !ok {
			continue
		}
		var candidates []string
		if tags, ok := entry["tags"].([]any); ok {
			for _, item := range tags {
				candidates = append(candidates, tagWords(item)...)
			}
		}
		candidates = append(candidates, tagWords(entry["category"])...)
		out[name] = cleanElementTags(candidates, elementMaxTags)
	}
	return out, nil
}

// parseTagIconifyCategories is tagdata.parse_iconify_categories.
func parseTagIconifyCategories(data []byte) (map[string][]string, error) {
	raw, err := decodeTagJSON(data)
	if err != nil {
		return nil, err
	}
	obj, _ := raw.(map[string]any)
	categories, ok := obj["categories"].(map[string]any)
	if !ok {
		return nil, tagDataErrorf("expected a 'categories' object")
	}
	out := map[string][]string{}
	for category, names := range categories {
		list, ok := names.([]any)
		if !ok {
			continue
		}
		for _, name := range list {
			text, ok := name.(string)
			if !ok {
				continue
			}
			out[text] = append(out[text], category)
		}
	}
	for name, tags := range out {
		out[name] = cleanElementTags(tags, elementMaxTags)
	}
	return out, nil
}

// parseTagPhosphor is tagdata.parse_phosphor (the icon table inside
// @phosphor-icons/core's generated dist/index.mjs).
func parseTagPhosphor(data []byte) (map[string][]string, error) {
	source := string(data)
	enums := map[string]map[string]string{}
	for _, match := range phosphorEnumPattern.FindAllStringSubmatch(source, -1) {
		variable, body := match[1], match[3]
		members := map[string]string{}
		for _, member := range phosphorMemberPattern.FindAllStringSubmatch(body, -1) {
			members[member[1]] = member[2]
		}
		enums[variable] = members
	}
	out := map[string][]string{}
	nameIdx := phosphorIconPattern.SubexpIndex("name")
	aliasIdx := phosphorIconPattern.SubexpIndex("alias")
	categoriesIdx := phosphorIconPattern.SubexpIndex("categories")
	tagsIdx := phosphorIconPattern.SubexpIndex("tags")
	for _, match := range phosphorIconPattern.FindAllStringSubmatch(source, -1) {
		var tags []string
		for _, raw := range phosphorStringPattern.FindAllStringSubmatch(match[tagsIdx], -1) {
			decoded, err := strconv.Unquote(`"` + raw[1] + `"`)
			if err == nil {
				tags = append(tags, decoded)
			}
		}
		var categories []string
		for _, reference := range strings.Split(match[categoriesIdx], ",") {
			variable, member, _ := strings.Cut(strings.TrimSpace(reference), ".")
			if value := enums[variable][member]; value != "" {
				categories = append(categories, value)
			}
		}
		var alias []string
		if match[aliasIdx] != "" {
			alias = append(alias, strings.ReplaceAll(match[aliasIdx], "-", " "))
		}
		out[match[nameIdx]] = cleanElementTags(append(append(tags, categories...), alias...), elementMaxTags)
	}
	if len(out) == 0 {
		return nil, tagDataErrorf("no icons found")
	}
	return out, nil
}

// elementPattern is a compiled regexp with the one lookbehind form the pinned
// manifest uses: Go's RE2 has no lookbehind, so `(?<!literal)` is evaluated
// after matching (see match).
type elementPattern struct {
	re       *regexp.Regexp
	negative []string
}

// compileElementPattern compiles a manifest regexp, lifting `(?<!literal)`
// assertions out of the pattern.
func compileElementPattern(pattern string) (*elementPattern, error) {
	const marker = "(?<!"
	var negatives []string
	rest := pattern
	for {
		start := strings.Index(rest, marker)
		if start < 0 {
			break
		}
		end := strings.Index(rest[start:], ")")
		if end < 0 {
			return nil, fmt.Errorf("unbalanced lookbehind")
		}
		negatives = append(negatives, rest[start+len(marker):start+end])
		rest = rest[:start] + rest[start+end+1:]
	}
	compiled, err := regexp.Compile(rest)
	if err != nil {
		return nil, err
	}
	return &elementPattern{re: compiled, negative: negatives}, nil
}

// match reports whether the pattern matches anywhere (re.search) with every
// lifted negative lookbehind satisfied at the match position.
func (p *elementPattern) match(value string) bool {
	for _, loc := range p.re.FindAllStringIndex(value, -1) {
		start := loc[0]
		ok := true
		for _, literal := range p.negative {
			if start-len(literal) >= 0 && value[start-len(literal):start] == literal {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}
