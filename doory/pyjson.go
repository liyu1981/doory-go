// Python-compatible JSON storage text: Django's JSONField stores
// json.dumps(value) — key order preserved, separators ", " and ": ", no
// ASCII escaping. Postgres/SQLite then hand that text back verbatim, so a
// value written by `import_size_presets` must serialize exactly like the
// oracle's (the cli-driver's database digest compares the stored text).

package doory

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// jsonNode is an order-preserving JSON value.
type jsonNode struct {
	kind string // "object" | "array" | "string" | "number" | "bool" | "null"
	keys []string
	vals []*jsonNode
	text string
	b    bool
}

// parseJSONNode decodes one JSON document keeping object key order.
func parseJSONNode(raw []byte) (*jsonNode, error) {
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	node, err := readJSONNode(dec)
	if err != nil {
		return nil, err
	}
	if dec.More() {
		return nil, fmt.Errorf("trailing JSON")
	}
	return node, nil
}

func readJSONNode(dec *json.Decoder) (*jsonNode, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			node := &jsonNode{kind: "object"}
			for dec.More() {
				keyTok, err := dec.Token()
				if err != nil {
					return nil, err
				}
				key, _ := keyTok.(string)
				value, err := readJSONNode(dec)
				if err != nil {
					return nil, err
				}
				node.keys = append(node.keys, key)
				node.vals = append(node.vals, value)
			}
			if _, err := dec.Token(); err != nil { // closing }
				return nil, err
			}
			return node, nil
		case '[':
			node := &jsonNode{kind: "array"}
			for dec.More() {
				value, err := readJSONNode(dec)
				if err != nil {
					return nil, err
				}
				node.vals = append(node.vals, value)
			}
			if _, err := dec.Token(); err != nil { // closing ]
				return nil, err
			}
			return node, nil
		}
		return nil, fmt.Errorf("unexpected delimiter")
	case string:
		return &jsonNode{kind: "string", text: t}, nil
	case json.Number:
		return &jsonNode{kind: "number", text: t.String()}, nil
	case float64:
		return &jsonNode{kind: "number", text: strconv.FormatFloat(t, 'g', -1, 64)}, nil
	case bool:
		return &jsonNode{kind: "bool", b: t}, nil
	case nil:
		return &jsonNode{kind: "null"}, nil
	}
	return nil, fmt.Errorf("unexpected token")
}

// pyDumps renders the value the way Python's json.dumps does by default
// (ensure_ascii=False, separators (", ", ": ")).
func pyDumps(node *jsonNode) string {
	var b strings.Builder
	writePyJSON(&b, node)
	return b.String()
}

func writePyJSON(b *strings.Builder, node *jsonNode) {
	if node == nil {
		b.WriteString("null")
		return
	}
	switch node.kind {
	case "object":
		b.WriteByte('{')
		for i, key := range node.keys {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(pyJSONString(key))
			b.WriteString(": ")
			writePyJSON(b, node.vals[i])
		}
		b.WriteByte('}')
	case "array":
		b.WriteByte('[')
		for i, value := range node.vals {
			if i > 0 {
				b.WriteString(", ")
			}
			writePyJSON(b, value)
		}
		b.WriteByte(']')
	case "string":
		b.WriteString(pyJSONString(node.text))
	case "number":
		b.WriteString(pyJSONNumber(node.text))
	case "bool":
		if node.b {
			b.WriteString("true")
		} else {
			b.WriteString("false")
		}
	default:
		b.WriteString("null")
	}
}

// pyJSONString quotes a string with Python's default escaping: only the
// mandatory escapes, unicode left as-is.
func pyJSONString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		default:
			if r < 0x20 {
				fmt.Fprintf(&b, `\u%04x`, r)
				continue
			}
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// pyJSONNumber keeps the JSON literal (both languages print the literal the
// source file carried for these catalogues).
func pyJSONNumber(text string) string { return text }

// pyNodeRepr renders a node the way Python's repr() shows the parsed value in
// importer messages (numbers as literals, strings single-quoted).
func pyNodeRepr(node *jsonNode) string {
	if node == nil {
		return "None"
	}
	switch node.kind {
	case "string":
		return "'" + strings.ReplaceAll(node.text, "'", "\\'") + "'"
	case "number", "bool":
		if node.kind == "bool" {
			if node.b {
				return "True"
			}
			return "False"
		}
		return node.text
	case "null":
		return "None"
	}
	return pyDumps(node)
}

// pyDumpsASCII renders the value the way Django's JSONField STORES it:
// json.dumps default (ensure_ascii=True, separators (", ", ": ")).
func pyDumpsASCII(node *jsonNode) string {
	var b strings.Builder
	writePyJSONASCII(&b, node)
	return b.String()
}

func writePyJSONASCII(b *strings.Builder, node *jsonNode) {
	if node == nil {
		b.WriteString("null")
		return
	}
	switch node.kind {
	case "object":
		b.WriteByte('{')
		for i, key := range node.keys {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(pyJSONStringASCII(key))
			b.WriteString(": ")
			writePyJSONASCII(b, node.vals[i])
		}
		b.WriteByte('}')
	case "array":
		b.WriteByte('[')
		for i, value := range node.vals {
			if i > 0 {
				b.WriteString(", ")
			}
			writePyJSONASCII(b, value)
		}
		b.WriteByte(']')
	case "string":
		b.WriteString(pyJSONStringASCII(node.text))
	case "number":
		b.WriteString(node.text)
	case "bool":
		if node.b {
			b.WriteString("true")
		} else {
			b.WriteString("false")
		}
	default:
		b.WriteString("null")
	}
}

// pyJSONStringASCII is Python's escaping with ensure_ascii=True.
func pyJSONStringASCII(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		default:
			if r < 0x20 || r > 0x7e {
				if r > 0xFFFF {
					r1, r2 := utf16Surrogates(r)
					fmt.Fprintf(&b, `\u%04x\u%04x`, r1, r2)
					continue
				}
				fmt.Fprintf(&b, `\u%04x`, r)
				continue
			}
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

func utf16Surrogates(r rune) (rune, rune) {
	v := r - 0x10000
	return 0xD800 + (v >> 10), 0xDC00 + (v & 0x3FF)
}

// pythonStoredJSON is what a Django JSONField column holds for a value the
// API received as raw JSON: Python json.dumps text (order kept, numeric
// literals kept, ", "/": " separators, non-ASCII escaped). API responses are
// re-canonicalized from it (compactStoredJSON), so the wire bytes do not
// change — only the stored text matches the oracle.
func pythonStoredJSON(raw []byte) []byte {
	node, err := parseJSONNode(raw)
	if err != nil {
		return []byte(raw)
	}
	return []byte(pyDumpsASCII(node))
}

// pythonSortedJSON renders a decoded JSON value the way Python's
// json.dumps(value, sort_keys=True) does for a fingerprint payload: sorted
// keys, default separators (", ", ": "), ensure_ascii escaping.
func pythonSortedJSON(value any) []byte {
	var b strings.Builder
	writePySortedJSON(&b, value)
	return []byte(b.String())
}

func writePySortedJSON(b *strings.Builder, value any) {
	switch v := value.(type) {
	case nil:
		b.WriteString("null")
	case map[string]any:
		keys := make([]string, 0, len(v))
		for key := range v {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		b.WriteByte('{')
		for i, key := range keys {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(pyJSONStringASCII(key))
			b.WriteString(": ")
			writePySortedJSON(b, v[key])
		}
		b.WriteByte('}')
	case []any:
		b.WriteByte('[')
		for i, item := range v {
			if i > 0 {
				b.WriteString(", ")
			}
			writePySortedJSON(b, item)
		}
		b.WriteByte(']')
	case string:
		b.WriteString(pyJSONStringASCII(v))
	case bool:
		if v {
			b.WriteString("true")
		} else {
			b.WriteString("false")
		}
	case json.Number:
		b.WriteString(v.String())
	case float64:
		if v == float64(int64(v)) {
			fmt.Fprintf(b, "%d", int64(v))
			return
		}
		b.WriteString(strconv.FormatFloat(v, 'g', -1, 64))
	case int:
		fmt.Fprintf(b, "%d", v)
	case int64:
		fmt.Fprintf(b, "%d", v)
	default:
		b.WriteString("null")
	}
}
