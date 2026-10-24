// Readers that turn an upstream file layout into named SVG documents
// (apps/elements/readers.py): Iconify JSON icon sets become one standalone
// <svg> per icon.

package doory

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

const iconifySVGHead = `<svg xmlns="http://www.w3.org/2000/svg" xmlns:xlink="http://www.w3.org/1999/xlink"`

const iconifyMaxIcons = 50_000

// sourceIcon is readers.SourceIcon.
type sourceIcon struct {
	Name    string
	SVG     []byte
	Aliases []string
}

// AliasesAsWords turns alias names into tag hints (`alias-name` -> `alias name`).
func (i sourceIcon) AliasesAsWords() []string {
	out := make([]string, len(i.Aliases))
	for idx, alias := range i.Aliases {
		out[idx] = strings.ReplaceAll(alias, "-", " ")
	}
	return out
}

// readIconifyJSON is readers.read_iconify_json.
func readIconifyJSON(data []byte) ([]sourceIcon, error) {
	var raw map[string]any
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.UseNumber()
	if err := dec.Decode(&raw); err != nil {
		return nil, fmt.Errorf("not valid JSON")
	}
	icons, ok := raw["icons"].(map[string]any)
	if !ok || len(icons) == 0 || len(icons) > iconifyMaxIcons {
		return nil, fmt.Errorf("expected a non-empty 'icons' object")
	}
	dimension := func(value any, fallback float64) float64 {
		if number, ok := jsonNumberValue(value); ok && number > 0 {
			return number
		}
		return fallback
	}
	offset := func(value any) float64 {
		if number, ok := jsonNumberValue(value); ok {
			return number
		}
		return 0
	}
	width := dimension(raw["width"], 16)
	height := dimension(raw["height"], 16)
	aliases := map[string][]string{}
	if rawAliases, ok := raw["aliases"].(map[string]any); ok {
		for alias, value := range rawAliases {
			entry, ok := value.(map[string]any)
			if !ok {
				continue
			}
			parent, ok := entry["parent"].(string)
			if !ok || icons[parent] == nil {
				continue
			}
			aliases[parent] = append(aliases[parent], alias)
		}
	}
	names := make([]string, 0, len(icons))
	for name := range icons {
		names = append(names, name)
	}
	sort.Strings(names)
	var out []sourceIcon
	for _, name := range names {
		icon, ok := icons[name].(map[string]any)
		if !ok {
			continue
		}
		if hidden, _ := icon["hidden"].(bool); hidden {
			continue
		}
		body, ok := icon["body"].(string)
		if !ok {
			continue
		}
		w := dimension(icon["width"], width)
		h := dimension(icon["height"], height)
		left := offset(firstPresent(icon, raw, "left"))
		top := offset(firstPresent(icon, raw, "top"))
		// Python formats these with `%g` (shortest), not repr: 24.0 -> "24".
		svg := fmt.Sprintf(`%s viewBox="%s %s %s %s">%s</svg>`, iconifySVGHead,
			formatG(left), formatG(top), formatG(w), formatG(h), body)
		aliasList := append([]string{}, aliases[name]...)
		sort.Strings(aliasList)
		out = append(out, sourceIcon{Name: name, SVG: []byte(svg), Aliases: aliasList})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("the icon set has no visible icons")
	}
	return out, nil
}

// firstPresent returns icon[key] when set, else document[key] (readers.py's
// `icon.get(key, raw.get(key))`).
func firstPresent(icon, document map[string]any, key string) any {
	if value, ok := icon[key]; ok && value != nil {
		return value
	}
	return document[key]
}

// formatG is Python's "%g" (C's %g) for a float: shortest form, no
// trailing zeros.
func formatG(value float64) string {
	return strconv.FormatFloat(value, 'g', -1, 64)
}
