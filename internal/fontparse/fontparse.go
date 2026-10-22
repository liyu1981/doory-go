// Package fontparse validates and describes font files by content — the Go
// port of apps/fonts/{inspector,probe_worker}.py + the WOFF/WOFF2 container
// handling that fontTools did implicitly.
//
// Never trusts the file name, extension or client MIME type:
//   - sniff: magic bytes (ttc rejected with the probed message)
//   - sfnt/WOFF/WOFF2 table directories; only the tables the catalog needs
//     are read (name, cmap, OS/2, head, post, maxp; fvar/glyf/CFF checks are
//     tag presence)
//   - WOFF: per-table zlib; WOFF2: brotli stream + table directory with
//     transform rules (glyf/loca transformed by default; our tables are raw)
//   - bounded: input capped by the caller (10 MB), WOFF2 decode budget
//     capped below (a crafted stream cannot decompress to gigabytes)
//
// Ports backend/apps/fonts/inspector.py
// Ports backend/apps/fonts/probe_worker.py
package fontparse

import (
	"bytes"
	"encoding/binary"
	"fmt"
)

// Format is the container format from the leading bytes.
type Format = string

const (
	TTF    Format = "ttf"
	OTF    Format = "otf"
	WOFF   Format = "woff"
	WOFF2  Format = "woff2"
	maxOut        = 64 << 20 // WOFF2 decode budget: crafted streams cannot balloon
)

// FontError is a user-safe rejection (inspector.FontError): the message is
// shown verbatim to the uploader.
type FontError struct{ Msg string }

func (e *FontError) Error() string { return e.Msg }

func fontError(format string, args ...any) *FontError {
	return &FontError{Msg: fmt.Sprintf(format, args...)}
}

// Info mirrors probe_worker's JSON result (what the catalog stores).
type Info struct {
	Family     string
	Subfamily  string
	Format     Format
	Weight     int
	Italic     bool
	IsVariable bool
	Scripts    []string
	Category   string
	Glyphs     int
}

// sniffFormat ports inspector.sniff_format.
func sniffFormat(data []byte) (Format, error) {
	if len(data) < 4 {
		return "", fontError("The file is not a TTF, OTF, WOFF or WOFF2 font.")
	}
	head := data[:4]
	if bytes.Equal(head, []byte("ttcf")) {
		return "", fontError("Font collections (.ttc/.otc) are not supported. Upload the individual fonts.")
	}
	switch {
	case bytes.Equal(head, []byte{0x00, 0x01, 0x00, 0x00}):
		return TTF, nil
	case bytes.Equal(head, []byte("true")):
		return TTF, nil
	case bytes.Equal(head, []byte("OTTO")):
		return OTF, nil
	case bytes.Equal(head, []byte("wOFF")):
		return WOFF, nil
	case bytes.Equal(head, []byte("wOF2")):
		return WOFF2, nil
	}
	return "", fontError("The file is not a TTF, OTF, WOFF or WOFF2 font.")
}

// tables abstracts the container: tag -> raw sfnt-format table bytes (lazy).
type tables interface {
	has(tag string) bool
	table(tag string) ([]byte, error)
}

// Inspect ports the combined sniff + probe flow: container parse, then the
// facts the catalog stores. Internal parse failures carry the probe's
// generic "could not be parsed" message (like probe_worker's catch-all).
func Inspect(data []byte) (*Info, error) {
	format, err := sniffFormat(data)
	if err != nil {
		return nil, err
	}
	dir, err := loadContainer(format, data)
	if err != nil {
		return nil, err
	}
	// probe(): vector outlines present?
	if !dir.has("glyf") && !dir.has("CFF ") && !dir.has("CFF2") {
		return nil, fontError("The font has no vector outlines.")
	}
	nameTable, err := dir.table("name")
	if err != nil {
		return nil, parseFailure()
	}
	family := clip(bestName(nameTable, 1))
	if family == "" {
		return nil, fontError("The font has no usable family name in its name table.")
	}
	cmap, err := loadBestCmap(dir)
	if err != nil || len(cmap) == 0 {
		return nil, fontError("The font has no character map.")
	}
	weight, italic, err := weightAndStyle(dir)
	if err != nil {
		return nil, parseFailure()
	}
	category, err := categoryHint(dir)
	if err != nil {
		return nil, parseFailure()
	}
	glyphs, err := numGlyphs(dir)
	if err != nil {
		return nil, parseFailure()
	}
	return &Info{
		Family:     family,
		Subfamily:  clip(bestName(nameTable, 2)),
		Format:     format,
		Weight:     weight,
		Italic:     italic,
		IsVariable: dir.has("fvar"),
		Scripts:    scriptsCovered(cmap),
		Category:   category,
		Glyphs:     glyphs,
	}, nil
}

// Style renders the catalog's style string (italic/normal).
func (i *Info) Style() string {
	if i.Italic {
		return "italic"
	}
	return "normal"
}

func parseFailure() *FontError {
	return fontError("The file could not be parsed as a font.")
}

// clip mirrors probe_worker.clip (name strings: at most 200 chars).
func clip(v string) string {
	if v == "" {
		return ""
	}
	runes := []rune(v)
	if len(runes) > 200 {
		return string(runes[:200])
	}
	return v
}

// scriptProbes mirrors probe_worker.SCRIPT_PROBES (range bounds as code
// points; share threshold of covered probe codes).
var scriptProbes = []struct {
	name  string
	from  rune
	to    rune
	step  rune
	share float64
}{
	{"latin", 0x41, 0x5B, 1, 0.95},
	{"latin-ext", 0x100, 0x180, 1, 0.5},
	{"cyrillic", 0x410, 0x450, 1, 0.9},
	{"greek", 0x3B1, 0x3CA, 1, 0.9},
	{"devanagari", 0x905, 0x93A, 1, 0.7},
	{"bengali", 0x985, 0x9BA, 1, 0.6},
	{"arabic", 0x621, 0x64B, 1, 0.7},
	{"hebrew", 0x5D0, 0x5EB, 1, 0.9},
	{"thai", 0xE01, 0xE3B, 1, 0.8},
	{"japanese", 0x3041, 0x3097, 1, 0.9},
	{"korean", 0xAC00, 0xD7A4, 97, 0.8},
	{"chinese", 0x4E00, 0x9FA6, 389, 0.8},
}

func scriptsCovered(cmap map[rune]bool) []string {
	var out []string
	for _, p := range scriptProbes {
		total := 0
		covered := 0
		for c := p.from; c < p.to; c += p.step {
			total++
			if cmap[c] {
				covered++
			}
		}
		if float64(covered) >= p.share*float64(total) {
			out = append(out, p.name)
		}
	}
	return out // nil renders as JSON null? probe emits [] — caller normalizes
}

// weightAndStyle ports probe_worker.weight_and_style (round-half-to-EVEN to
// hundreds, like Python's round(x/100)*100).
func weightAndStyle(dir tables) (int, bool, error) {
	os2, _ := dir.table("OS/2")
	head, _ := dir.table("head")
	post, _ := dir.table("post")

	weight := 0
	if len(os2) >= 6 {
		weight = int(binary.BigEndian.Uint16(os2[4:6]))
	}
	macStyle := 0
	if len(head) >= 46 {
		macStyle = int(binary.BigEndian.Uint16(head[44:46]))
	}
	if weight < 1 || weight > 1000 {
		if macStyle&1 != 0 {
			weight = 700
		} else {
			weight = 400
		}
	}
	weight = roundHundredEven(weight)
	if weight < 100 {
		weight = 100
	}
	if weight > 900 {
		weight = 900
	}

	italic := false
	if len(os2) >= 64 {
		fsSelection := binary.BigEndian.Uint16(os2[62:64])
		italic = fsSelection&1 != 0
	}
	if !italic && macStyle&2 != 0 {
		italic = true
	}
	if !italic && len(post) >= 8 && binary.BigEndian.Uint32(post[4:8]) != 0 {
		italic = true // italicAngle (FIXED 16.16 at offset 4) nonzero
	}
	return weight, italic, nil
}

// roundHundredEven: Python round(weight/100)*100 — ties round to EVEN.
func roundHundredEven(w int) int {
	quotient := w / 100
	remainder := w % 100
	switch {
	case remainder > 50:
		return (quotient + 1) * 100
	case remainder < 50:
		return quotient * 100
	default: // exactly .5: banker's rounding (450 -> 400, 550 -> 600)
		if quotient%2 == 1 {
			return (quotient + 1) * 100
		}
		return quotient * 100
	}
}

// categoryHint ports probe_worker.category_hint (post.isFixedPitch +
// PANOSE family/serif style).
func categoryHint(dir tables) (string, error) {
	post, _ := dir.table("post")
	// fontTools postFormat: version(0) italicAngle(4) underlinePosition(8)
	// underlineThickness(10) isFixedPitch(12) — probe evidence: underline
	// -100 = 0xff9c0032 at [8:12], isFixedPitch 0 at [12:16].
	if len(post) >= 16 && binary.BigEndian.Uint32(post[12:16]) != 0 {
		return "monospace", nil
	}
	os2, _ := dir.table("OS/2")
	var panose [10]byte
	if len(os2) >= 42 {
		copy(panose[:], os2[32:42])
	}
	// PANOSE: [0]=bFamilyType, [1]=bSerifStyle (probe: family 2, style 11).
	switch panose[0] {
	case 3:
		return "handwriting", nil
	case 4:
		return "display", nil
	case 2:
		if panose[1] >= 3 && panose[1] <= 10 {
			return "serif", nil
		}
	}
	return "sans-serif", nil
}

func numGlyphs(dir tables) (int, error) {
	maxp, err := dir.table("maxp")
	if err != nil || len(maxp) < 6 {
		return 0, parseFailure()
	}
	return int(binary.BigEndian.Uint16(maxp[4:6])), nil
}

// bestName ports fontTools' getBestFamilyName/getBestSubFamilyName name-ID
// lookup with the (platform, encoding, language) preference list: Windows
// Unicode English first, then generic Unicode, then Mac Roman.
func bestName(nameTable []byte, nameID int) string {
	if len(nameTable) < 6 {
		return ""
	}
	count := int(binary.BigEndian.Uint16(nameTable[2:4]))
	storage := int(binary.BigEndian.Uint16(nameTable[4:6]))
	type rec struct{ platform, encoding, language, id, length, offset int }
	var records []rec
	for i := 0; i < count; i++ {
		at := 6 + i*12
		if at+12 > len(nameTable) {
			break
		}
		records = append(records, rec{
			platform: int(binary.BigEndian.Uint16(nameTable[at : at+2])),
			encoding: int(binary.BigEndian.Uint16(nameTable[at+2 : at+4])),
			language: int(binary.BigEndian.Uint16(nameTable[at+4 : at+6])),
			id:       int(binary.BigEndian.Uint16(nameTable[at+6 : at+8])),
			length:   int(binary.BigEndian.Uint16(nameTable[at+8 : at+10])),
			offset:   int(binary.BigEndian.Uint16(nameTable[at+10 : at+12])),
		})
	}
	preferences := []struct{ platform, encoding, language int }{
		{3, 1, 0x400}, {3, 1, 1033}, {3, 1, 0},
		{3, 10, 0x400}, {3, 10, 1033},
		{1, 0, 0},
		{0, 3, 0}, {0, 4, 0}, {0, 6, 0}, {0, 0, 0},
	}
	pick := func() string {
		for _, pref := range preferences {
			for _, r := range records {
				if r.id == nameID && r.platform == pref.platform &&
					r.encoding == pref.encoding && r.language == pref.language {
					if s := decodeName(nameTable, storage, r.platform, r.offset, r.length); s != "" {
						return s
					}
				}
			}
		}
		return ""
	}
	return pick()
}

func decodeName(table []byte, storage, platform, offset, length int) string {
	at := storage + offset
	if at < 0 || at+length > len(table) {
		return ""
	}
	raw := table[at : at+length]
	if platform == 1 {
		// Mac Roman approximates as Latin-1 for the byte range we keep.
		out := make([]rune, len(raw))
		for i, b := range raw {
			out[i] = rune(b)
		}
		return string(out)
	}
	// Unicode platforms store UTF-16BE (probe fixtures and real fonts).
	if len(raw)%2 != 0 {
		raw = raw[:len(raw)-1]
	}
	out := make([]uint16, len(raw)/2)
	for i := range out {
		out[i] = binary.BigEndian.Uint16(raw[i*2:])
	}
	return string(utf16Decode(out))
}
