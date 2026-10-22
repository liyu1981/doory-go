// Package pdfscan extracts the user-visible STRUCTURE of a PDF for parity
// comparison: page count (the /Count of the page tree), per-page MediaBox,
// TrimBox/BleedBox values, /Lang, /Creator and /Title. Binary PDF BYTES
// carry per-render ids and timestamps (Chromium embeds them), so scenarios
// compare this canonical form instead (PLAN.md M9 note; pdfmeta's exact-
// box surgery is the family's remaining slice — boxes are Chromium's
// until then, and equal on both sides because both print the same HTML
// with the same browser).
package pdfscan

import (
	"bytes"
	"compress/zlib"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
)

// Structure is the canonical, comparable shape.
type Structure struct {
	Pages   int        `json:"pages"`
	Count   string     `json:"count"`
	Lang    string     `json:"lang,omitempty"`
	Creator string     `json:"creator,omitempty"`
	Title   string     `json:"title,omitempty"`
	Boxes   []BoxEntry `json:"boxes"`
}

// BoxEntry is one page's boxes (normalized to canonical numbers).
type BoxEntry struct {
	Media [4]float64  `json:"media"`
	Trim  *[4]float64 `json:"trim,omitempty"`
	Bleed *[4]float64 `json:"bleed,omitempty"`
}

var (
	pageCountRe = regexp.MustCompile(`/Count\s+(\d+)`)
	langRe      = regexp.MustCompile(`/Lang\s*\(([^)]*)\)`)
	creatorRe   = regexp.MustCompile(`/Creator\s*\(([^)]*)\)`)
	titleRe     = regexp.MustCompile(`/Title\s*\(([^)]*)\)`)
	mediaBoxRe  = regexp.MustCompile(`/MediaBox\s*\[\s*([-\d.]+)\s+([-\d.]+)\s+([-\d.]+)\s+([-\d.]+)\s*\]`)
	trimBoxRe   = regexp.MustCompile(`/TrimBox\s*\[\s*([-\d.]+)\s+([-\d.]+)\s+([-\d.]+)\s+([-\d.]+)\s*\]`)
	bleedBoxRe  = regexp.MustCompile(`/BleedBox\s*\[\s*([-\d.]+)\s+([-\d.]+)\s+([-\d.]+)\s+([-\d.]+)\s*\]`)
)

// Scan extracts the structure from a PDF (raw + Flate-decompressed text).
func Scan(data []byte) (*Structure, error) {
	hay := append([]byte{}, data...)
	for _, stream := range flateStreams(data) {
		hay = append(hay, stream...)
	}
	structure := &Structure{Boxes: []BoxEntry{}}
	if m := pageCountRe.FindSubmatch(hay); m != nil {
		structure.Count = string(m[1])
		structure.Pages, _ = strconv.Atoi(structure.Count)
	}
	if m := langRe.FindSubmatch(hay); m != nil {
		structure.Lang = unescapePDFString(string(m[1]))
	}
	if m := creatorRe.FindSubmatch(hay); m != nil {
		// pypdf re-escapes ":" as octal in its own output; values must be
		// compared unescaped.
		structure.Creator = unescapePDFString(string(m[1]))
	}
	if m := titleRe.FindSubmatch(hay); m != nil {
		structure.Title = unescapePDFString(string(m[1]))
	}
	for _, m := range mediaBoxRe.FindAllSubmatch(hay, -1) {
		entry := BoxEntry{Media: parseBox(m)}
		if tm := trimBoxRe.Find(hay); tm != nil {
			tm2 := trimBoxRe.FindSubmatch(tm)
			box := parseBox(tm2)
			entry.Trim = &box
		}
		if bm := bleedBoxRe.Find(hay); bm != nil {
			bm2 := bleedBoxRe.FindSubmatch(bm)
			box := parseBox(bm2)
			entry.Bleed = &box
		}
		structure.Boxes = append(structure.Boxes, entry)
	}
	if structure.Pages == 0 {
		return nil, fmt.Errorf("no pages found")
	}
	return structure, nil
}

func parseBox(m [][]byte) [4]float64 {
	var box [4]float64
	for i := 0; i < 4 && i+1 < len(m); i++ {
		box[i], _ = strconv.ParseFloat(string(m[i+1]), 64)
	}
	return box
}

// unescapePDFString resolves PDF string escapes (\072 octal, \( \) \\).
func unescapePDFString(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' || i+1 >= len(s) {
			b.WriteByte(s[i])
			continue
		}
		next := s[i+1]
		if next >= '0' && next <= '7' {
			j := i + 1
			for j < len(s) && j <= i+3 && s[j] >= '0' && s[j] <= '7' {
				j++
			}
			v, _ := strconv.ParseUint(s[i+1:j], 8, 8)
			b.WriteByte(byte(v))
			i = j - 1
			continue
		}
		b.WriteByte(next)
		i++
	}
	return b.String()
}

// flateStreams finds and decompresses FlateDecode streams (best-effort).
func flateStreams(data []byte) [][]byte {
	var out [][]byte
	for {
		start := bytes.Index(data, []byte("stream"))
		if start < 0 {
			return out
		}
		body := start + len("stream")
		if body < len(data) && data[body] == '\r' {
			body++
		}
		if body < len(data) && data[body] == '\n' {
			body++
		}
		end := bytes.Index(data[body:], []byte("endstream"))
		if end < 0 {
			return out
		}
		chunk := data[body : body+end]
		zr, err := zlib.NewReader(bytes.NewReader(chunk))
		if err == nil {
			decompressed, rerr := io.ReadAll(zr)
			zr.Close()
			if rerr == nil {
				out = append(out, decompressed)
			}
		}
		data = data[body+end:]
	}
}

// Canonical renders the structure for the comparator (numbers via %g).
func (s *Structure) Canonical() string {
	b, _ := json.Marshal(s)
	return string(b)
}
