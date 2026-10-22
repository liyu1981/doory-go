// Package pdfmeta writes the exact print metadata pypdf writes in the
// oracle pipeline (apps/exports/pdfmeta.py): page boxes cut back from
// Chromium's rounded sheet to the exact page size (anchored top-left,
// TrimBox/BleedBox for bleed/marks) plus /Lang, /Creator and /Title.
//
// Chromium PDFs here are classic (xref table, no object streams): the
// surgery walks the xref offsets, rewrites the page/catalog/info
// dictionaries and regenerates a valid classic xref. Stream CONTENTS are
// copied byte-for-byte (their /Length stays valid; offsets are rebuilt).
//
// Ports backend/apps/exports/pdfmeta.py
package pdfmeta

import (
	"bytes"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

const ptPerPx = 0.75
const maxSheetExcessPT = 2.0
const minSheetExcessPT = -0.01

// Geometry mirrors layout.PageGeometry (CSS px) for one page.
type Geometry struct {
	PageWidth  float64
	PageHeight float64
	Bleed      float64
	Slug       float64
	HasMarks   bool
}

// Apply performs finalize_pdf's edits: exact Media/Trim/Bleed boxes,
// /Lang on the catalog, /Creator (+/Title) in the Info dict. Returns the
// rewritten PDF; on any structural surprise the input is returned
// unchanged (the python code logs and does the same: a PDF with Chromium's
// boxes is still a correct PDF).
func Apply(pdf []byte, geoms []Geometry, title, lang string) []byte {
	parsed, err := parse(pdf)
	if err != nil {
		return pdf
	}
	if len(parsed.pages) != len(geoms) {
		return pdf
	}
	for i, page := range parsed.pages {
		modified, ok := writeBoxes(parsed.objects[page], geoms[i])
		if !ok {
			return pdf
		}
		parsed.objects[page] = modified
	}
	// catalog: /Lang (replace an existing one, else add) — chromium sets
	// it from the HTML lang attribute; the requested lang wins either way.
	if body, ok := parsed.objects[parsed.root]; ok {
		text := string(body)
		if langRe.MatchString(text) {
			text = langRe.ReplaceAllString(text, "/Lang ("+pdfEscape(lang)+")")
		} else {
			inserted, ok := insertBeforeClose(text, "/Lang ("+pdfEscape(lang)+")\n")
			if !ok {
				return pdf
			}
			text = inserted
		}
		parsed.objects[parsed.root] = []byte(text)
	}
	// metadata: /Creator (+ /Title when given), other keys preserved
	if parsed.info > 0 {
		if body, ok := parsed.objects[parsed.info]; ok {
			text := string(body)
			text = replaceInfoString(text, "/Creator", "Doory")
			if title != "" {
				text = replaceInfoString(text, "/Title", title)
			}
			parsed.objects[parsed.info] = []byte(text)
		}
	}
	return rebuild(parsed)
}

var (
	langRe     = regexp.MustCompile(`/Lang\s*\([^)]*\)`)
	creatorRe  = regexp.MustCompile(`/Creator\s*\((?:\\.|[^\\)])*\)`)
	titleRe    = regexp.MustCompile(`/Title\s*\((?:\\.|[^\\)])*\)`)
	mediaBoxRe = regexp.MustCompile(`/MediaBox\s*\[\s*([-\d.]+)\s+([-\d.]+)\s+([-\d.]+)\s+([-\d.]+)\s*\]`)
)

// writeBoxes cuts the MediaBox to the exact page box (top-left anchored)
// and adds Trim/Bleed for bleed or marks; false when the sheet is not the
// requested size.
func writeBoxes(body []byte, g Geometry) ([]byte, bool) {
	text := string(body)
	nums := mediaBoxRe.FindStringSubmatch(text)
	if len(nums) != 5 {
		return nil, false
	}
	left, err1 := strconv.ParseFloat(nums[1], 64)
	top, err4 := strconv.ParseFloat(nums[4], 64)
	curW, err3 := strconv.ParseFloat(nums[3], 64) // minus left below
	bottom0, err2 := strconv.ParseFloat(nums[2], 64)
	if err1 != nil || err2 != nil || err3 != nil || err4 != nil {
		return nil, false
	}
	curW = curW - left
	curH := top - bottom0
	exactW := g.PageWidth * ptPerPx
	exactH := g.PageHeight * ptPerPx
	excessW := curW - exactW
	excessH := curH - exactH
	if excessW < minSheetExcessPT || excessW > maxSheetExcessPT ||
		excessH < minSheetExcessPT || excessH > maxSheetExcessPT {
		println("DEBUG excess", excessW, excessH, "cur", curW, curH, "exact", exactW, exactH)
		return nil, false
	}
	bottom := top - exactH
	media := fmt.Sprintf("/MediaBox [ %s %s %s %s ]", pf(left), pf(bottom), pf(left+exactW), pf(top))
	text = mediaBoxRe.ReplaceAllString(text, media)
	if g.Bleed > 0 || g.HasMarks {
		slugPt := g.Slug * ptPerPx
		marginPt := (g.Bleed + g.Slug) * ptPerPx
		bleedW := (g.PageWidth + 2*g.Bleed) * ptPerPx
		bleedH := (g.PageHeight + 2*g.Bleed) * ptPerPx
		bleed := fmt.Sprintf("/BleedBox [ %s %s %s %s ]", pf(left+slugPt), pf(top-slugPt-bleedH),
			pf(left+slugPt+bleedW), pf(top-slugPt))
		trim := fmt.Sprintf("/TrimBox [ %s %s %s %s ]", pf(left+marginPt), pf(top-marginPt-g.PageHeight*ptPerPx),
			pf(left+marginPt+g.PageWidth*ptPerPx), pf(top-marginPt))
		inserted, ok := insertBeforeClose(text, bleed+"\n"+trim+"\n")
		if !ok {
			return nil, false
		}
		text = inserted
	}
	return []byte(text), true
}

// pf formats a coordinate like pypdf WRITES numbers: rounded to 6
// decimals with trailing zeros stripped (empirically from the oracle
// output: 0.9599900000000048 -> 0.95999 and 1.0800019999999932 ->
// 1.080002 — both fit round-6 + strip).
func pf(v float64) string {
	s := strconv.FormatFloat(v, 'f', 6, 64)
	s = strings.TrimRight(s, "0")
	s = strings.TrimRight(s, ".")
	if s == "" || s == "-" {
		return "0"
	}
	return s
}

func pdfEscape(s string) string {
	out := make([]rune, 0, len(s))
	for _, r := range s {
		switch r {
		case '(', ')', '\\':
			out = append(out, '\\', r)
		default:
			out = append(out, r)
		}
	}
	return string(out)
}

// insertBeforeClose inserts a line before the dict's closing ">>" (last one
// in the object body).
func insertBeforeClose(text, line string) (string, bool) {
	idx := bytes.LastIndex([]byte(text), []byte(">>"))
	if idx < 0 {
		return "", false
	}
	return text[:idx] + line + text[idx:], true
}

// replaceInfoString sets/replaces a string entry in an Info dictionary.
func replaceInfoString(text, key, value string) string {
	re := regexp.MustCompile(regexp.QuoteMeta(key) + `\s*\((?:\\.|[^\\)])*\)`)
	replacement := key + " (" + pdfEscape(value) + ")"
	if re.MatchString(text) {
		return re.ReplaceAllString(text, replacement)
	}
	inserted, ok := insertBeforeClose(text, replacement+"\n")
	if !ok {
		return text
	}
	return inserted
}

// ---- parsing (classic xref) ----

type parsedPDF struct {
	objects map[int][]byte
	pages   []int // object numbers of /Type /Page (not /Pages)
	root    int
	info    int
	size    int
}

var objHeader = regexp.MustCompile(`(?m)^(\d+) 0 obj`)

func parse(data []byte) (*parsedPDF, error) {
	start := bytes.LastIndex(data, []byte("startxref"))
	if start < 0 {
		return nil, fmt.Errorf("no startxref")
	}
	offsetStr := trimSpaceBytes(data[start+len("startxref"):])
	offset, err := strconv.Atoi(firstToken(offsetStr))
	if err != nil {
		return nil, err
	}
	if offset < 0 || offset >= len(data) || !bytes.HasPrefix(data[offset:], []byte("xref")) {
		return nil, fmt.Errorf("not a classic xref")
	}
	p := &parsedPDF{objects: map[int][]byte{}}
	// xref subsections: "N M\n" then M 20-byte entries
	pos := offset + len("xref")
	for {
		pos = skipWhitespace(data, pos)
		if bytes.HasPrefix(data[pos:], []byte("trailer")) {
			break
		}
		lineEnd := bytes.IndexByte(data[pos:], '\n')
		if lineEnd < 0 {
			return nil, fmt.Errorf("bad xref subsection")
		}
		fields := strings.Fields(string(data[pos : pos+lineEnd]))
		if len(fields) != 2 {
			return nil, fmt.Errorf("bad xref subsection header")
		}
		first, _ := strconv.Atoi(fields[0])
		count, _ := strconv.Atoi(fields[1])
		pos += lineEnd + 1
		for i := 0; i < count; i++ {
			entry := data[pos : pos+20]
			pos += 20
			if entry[17] != 'n' {
				continue // free entry
			}
			objOffset, _ := strconv.Atoi(strings.TrimSpace(string(entry[:10])))
			num := first + i
			body := objectBody(data, objOffset)
			if body == nil {
				continue
			}
			p.objects[num] = body
		}
	}
	// trailer
	trailerAt := bytes.Index(data[pos:], []byte("trailer"))
	trailer := data[pos+trailerAt:]
	p.root = trailerRef(trailer, "/Root")
	p.info = trailerRef(trailer, "/Info")
	p.size = trailerSize(trailer)
	// page objects (dicts, not streams): text match on their bodies
	for num, body := range p.objects {
		if isPageDict(body) {
			p.pages = append(p.pages, num)
		}
	}
	sort.Ints(p.pages)
	return p, nil
}

func objectBody(data []byte, offset int) []byte {
	if offset <= 0 || offset >= len(data) {
		return nil
	}
	// skip "N 0 obj"
	nl := bytes.IndexByte(data[offset:], '\n')
	if nl < 0 {
		return nil
	}
	start := offset + nl + 1
	end := bytes.Index(data[start:], []byte("endobj"))
	if end < 0 {
		return nil
	}
	body := data[start : start+end]
	// trim the trailing newline pypdf-style writers leave
	return bytes.TrimRight(body, "\n")
}

func isPageDict(body []byte) bool {
	return bytes.Contains(body, []byte("/Type /Page\n")) || bytes.HasSuffix(trimSpaceBytes(body), []byte("/Type /Page"))
}

func trailerRef(trailer []byte, key string) int {
	re := regexp.MustCompile(regexp.QuoteMeta(key) + `\s+(\d+) 0 R`)
	m := re.FindSubmatch(trailer)
	if m == nil {
		return -1
	}
	n, _ := strconv.Atoi(string(m[1]))
	return n
}

func trailerSize(trailer []byte) int {
	re := regexp.MustCompile(`/Size\s+(\d+)`)
	m := re.FindSubmatch(trailer)
	if m == nil {
		return 0
	}
	n, _ := strconv.Atoi(string(m[1]))
	return n
}

func skipWhitespace(data []byte, pos int) int {
	for pos < len(data) && (data[pos] == ' ' || data[pos] == '\n' || data[pos] == '\r' || data[pos] == '\t') {
		pos++
	}
	return pos
}

func trimSpaceBytes(b []byte) []byte { return bytes.TrimSpace(b) }

func firstToken(b []byte) string {
	fields := strings.Fields(string(b))
	if len(fields) == 0 {
		return ""
	}
	return fields[0]
}

// rebuild writes a classic-xref PDF from the (possibly modified) objects.
func rebuild(p *parsedPDF) []byte {
	var buf bytes.Buffer
	buf.WriteString("%PDF-1.4\n%\xe2\xe3\xcf\xd3\n")
	offsets := map[int]int{}
	nums := make([]int, 0, len(p.objects))
	for n := range p.objects {
		nums = append(nums, n)
	}
	sort.Ints(nums)
	for _, n := range nums {
		offsets[n] = buf.Len()
		fmt.Fprintf(&buf, "%d 0 obj\n", n)
		buf.Write(p.objects[n])
		if !bytes.HasSuffix(p.objects[n], []byte("\n")) {
			buf.WriteByte('\n')
		}
		buf.WriteString("endobj\n")
	}
	xrefAt := buf.Len()
	maxObj := 0
	for n := range offsets {
		if n > maxObj {
			maxObj = n
		}
	}
	size := maxObj + 1
	if p.size > size {
		size = p.size
	}
	fmt.Fprintf(&buf, "xref\n0 %d\n", size)
	fmt.Fprintf(&buf, "%010d %05d f \n", 0, 65535)
	for i := 1; i < size; i++ {
		if off, ok := offsets[i]; ok {
			fmt.Fprintf(&buf, "%010d %05d n \n", off, 0)
		} else {
			fmt.Fprintf(&buf, "%010d %05d f \n", 0, 0)
		}
	}
	buf.WriteString("trailer\n<<\n")
	fmt.Fprintf(&buf, "/Size %d\n/Root %d 0 R\n", size, p.root)
	if p.info > 0 {
		fmt.Fprintf(&buf, "/Info %d 0 R\n", p.info)
	}
	fmt.Fprintf(&buf, ">>\nstartxref\n%d\n%%%%EOF\n", xrefAt)
	return buf.Bytes()
}
