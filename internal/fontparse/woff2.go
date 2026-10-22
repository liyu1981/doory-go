// WOFF2: brotli stream + the transformed table directory.
//
// Layout (W3C WOFF2): 48-byte header (signature 'wOF2', flavor, length,
// numTables, reserved, totalSfntSize, totalCompressedSize, version fields,
// meta/priv blocks), then per-table directory entries:
//
//	UInt8 flags: bits0-5 = KnownTags index (63 -> 4-byte custom tag follows),
//	           bits6-7 = transform version (0 default, 1 explicit)
//	UIntBase128 origLength
//	UIntBase128 transformLength — present iff the transform APPLIES
//	           (glyf/loca at version 0 — fontTools' output; other tables at
//	           version 1)
//
// followed by the brotli-compressed concatenation of the STORED table bytes.
// The catalog only reads name/cmap/OS/2/head/post/maxp — all stored RAW for
// fontTools-produced files — so transformed glyf/loca are skipped entirely;
// fvar/glyf/CFF presence comes from tags alone.
package fontparse

import (
	"bytes"
	"encoding/binary"
	"io"

	"github.com/andybalholm/brotli"
)

const woff2HeaderLen = 48

// knownTags is the WOFF2 KnownTags list (index -> tag).
var knownTags = []string{
	"cmap", "head", "hhea", "hmtx", "maxp", "name", "OS/2", "post",
	"cvt ", "fpgm", "glyf", "loca", "prep", "CFF ", "VORG", "EBDT",
	"EBLC", "gasp", "hdmx", "kern", "LTSH", "PCLT", "VDMX", "vhea",
	"vmtx", "BASE", "GDEF", "GPOS", "GSUB", "EBSC", "JSTF", "MATH",
	"CBDT", "CBLC", "COLR", "CPAL", "SVG ", "sbix", "acnt", "avar",
	"bdat", "bloc", "bsln", "cvar", "fdsc", "feat", "fmtx", "fvar",
	"gvar", "hsty", "just", "lcar", "mort", "morx", "opbd", "prop",
	"trak", "Zapf", "Silf", "Glat", "Gloc", "Feat", "Sill",
}

type woff2Tables struct {
	m    map[string][]byte // raw (sfnt-format) bytes where stored raw
	tags map[string]bool
}

func (w *woff2Tables) has(tag string) bool { return w.tags[tag] }

func (w *woff2Tables) table(tag string) ([]byte, error) {
	b, ok := w.m[tag]
	if !ok {
		return nil, parseFailure()
	}
	return b, nil
}

// readBase128 consumes a UIntBase128 (continuation bit7; at most 5 bytes).
func readBase128(b []byte, at int) (val, next int, ok bool) {
	v := 0
	for i := 0; i < 5 && at < len(b); i++ {
		byteVal := b[at]
		at++
		if i == 0 && byteVal == 0x80 {
			return 0, at, false // leading zero forbidden
		}
		v = (v << 7) | int(byteVal&0x7F)
		if byteVal&0x80 == 0 {
			return v, at, true
		}
	}
	return 0, at, false
}

func loadWOFF2(data []byte) (tables, error) {
	if len(data) < woff2HeaderLen || !bytes.Equal(data[0:4], []byte("wOF2")) {
		return nil, parseFailure()
	}
	numTables := int(binary.BigEndian.Uint16(data[12:14]))
	// Header: sig(0) flavor(4) length(8) numTables(12) reserved(14)
	// totalSfntSize(16) totalCompressedSize(20) — fontTools woff2.py struct.
	totalCompressed := int(binary.BigEndian.Uint32(data[20:24]))
	if numTables < 1 || numTables > 64*1024 {
		return nil, parseFailure()
	}
	at := woff2HeaderLen
	type entry struct {
		tag          string
		transform    int
		origLength   int
		transformLen int // -1 when absent
	}
	var entries []entry
	for i := 0; i < numTables; i++ {
		if at >= len(data) {
			return nil, parseFailure()
		}
		flags := data[at]
		at++
		tagIdx := int(flags & 0x3F)
		transform := int(flags >> 6)
		var tag string
		if tagIdx == 63 {
			if at+4 > len(data) {
				return nil, parseFailure()
			}
			tag = string(data[at : at+4])
			at += 4
		} else {
			if tagIdx >= len(knownTags) {
				return nil, parseFailure()
			}
			tag = knownTags[tagIdx]
		}
		orig, next, ok := readBase128(data, at)
		if !ok {
			return nil, parseFailure()
		}
		at = next
		e := entry{tag: tag, transform: transform, origLength: orig, transformLen: -1}
		// transformLength present iff the transform applies: glyf/loca at
		// default version 0 (their transform IS the default), other tables
		// at explicit version 1.
		applies := (tag == "glyf" || tag == "loca") && transform == 0 ||
			tag != "glyf" && tag != "loca" && transform == 1
		if applies {
			tlen, next, ok := readBase128(data, at)
			if !ok {
				return nil, parseFailure()
			}
			at = next
			e.transformLen = tlen
		} else if transform != 0 && !(tag == "glyf" || tag == "loca") {
			// reserved/unknown transform for a plain table
			if !(tag == "glyf" || tag == "loca") && transform > 1 {
				return nil, parseFailure()
			}
		}
		entries = append(entries, e)
	}
	if at+totalCompressed > len(data) {
		return nil, parseFailure()
	}
	// Brotli-decode the stored stream with a hard output budget (crafted
	// inputs must not balloon; probe_worker's RLIMIT_AS equivalent).
	decoded, err := io.ReadAll(io.LimitReader(
		brotli.NewReader(bytes.NewReader(data[at:at+totalCompressed])), maxOut+1))
	if err != nil || len(decoded) > maxOut {
		return nil, fontError("The font is too large to read.")
	}
	// Slice the stored tables in directory order.
	m := map[string][]byte{}
	tags := map[string]bool{}
	cursor := 0
	for _, e := range entries {
		tags[e.tag] = true
		size := e.origLength
		if e.transformLen >= 0 {
			size = e.transformLen
		}
		if cursor+size > len(decoded) {
			return nil, parseFailure()
		}
		stored := decoded[cursor : cursor+size]
		cursor += size
		// Raw tables are sfnt-format bytes; transformed ones (glyf/loca for
		// fontTools output) are never read by the catalog — presence tags
		// still cover the fvar/glyf/CFF checks.
		if e.transformLen < 0 {
			m[e.tag] = stored
		}
	}
	return &woff2Tables{m: m, tags: tags}, nil
}
