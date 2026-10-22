// cmap parsing: coverage lookup for the script probes (fontTools
// getBestCmap equivalent with its platform preference: (3,10) > (3,1) >
// (0,*) unicode subtables). Formats 0, 4, 6 and 12 are supported — the
// formats real uploads and the fixtures carry.
package fontparse

import "encoding/binary"

// loadBestCmap selects the subtable like fontTools' getBestCmap and inverts
// it into a coverage set.
func loadBestCmap(dir tables) (map[rune]bool, error) {
	raw, err := dir.table("cmap")
	if err != nil || len(raw) < 4 {
		return nil, parseFailure()
	}
	numTables := int(binary.BigEndian.Uint16(raw[2:4]))
	type subtable struct {
		platform, encoding int
		offset             int
	}
	var subs []subtable
	for i := 0; i < numTables; i++ {
		at := 4 + i*8
		if at+8 > len(raw) {
			return nil, parseFailure()
		}
		subs = append(subs, subtable{
			platform: int(binary.BigEndian.Uint16(raw[at : at+2])),
			encoding: int(binary.BigEndian.Uint16(raw[at+2 : at+4])),
			offset:   int(binary.BigEndian.Uint32(raw[at+4 : at+8])),
		})
	}
	preference := func(s subtable) int {
		switch {
		case s.platform == 3 && s.encoding == 10:
			return 0
		case s.platform == 3 && s.encoding == 1:
			return 1
		case s.platform == 0:
			return 2 + s.encoding // lower encoding variants first
		default:
			return 99
		}
	}
	best := -1
	for i, s := range subs {
		p := preference(s)
		if p < 99 && (best < 0 || p < preference(subs[best])) {
			best = i
		}
	}
	if best < 0 {
		return nil, nil
	}
	return parseCmapSubtable(raw, subs[best].offset)
}

func parseCmapSubtable(raw []byte, offset int) (map[rune]bool, error) {
	if offset+2 > len(raw) {
		return nil, parseFailure()
	}
	format := binary.BigEndian.Uint16(raw[offset : offset+2])
	out := map[rune]bool{}
	switch format {
	case 0:
		if offset+262 > len(raw) {
			return nil, parseFailure()
		}
		for code, glyph := range raw[offset+6 : offset+262] {
			if glyph != 0 {
				out[rune(code)] = true
			}
		}
	case 4:
		if offset+14 > len(raw) {
			return nil, parseFailure()
		}
		segCountX2 := int(binary.BigEndian.Uint16(raw[offset+6 : offset+8]))
		segCount := segCountX2 / 2
		endAt := offset + 14
		startAt := endAt + segCountX2 + 2 // +2 reservedPad
		deltaAt := startAt + segCountX2
		rangeOffsetAt := deltaAt + segCountX2
		if rangeOffsetAt+segCountX2 > len(raw) {
			return nil, parseFailure()
		}
		for s := 0; s < segCount; s++ {
			end := int(binary.BigEndian.Uint16(raw[endAt+s*2:]))
			start := int(binary.BigEndian.Uint16(raw[startAt+s*2:]))
			delta := int(int16(binary.BigEndian.Uint16(raw[deltaAt+s*2:])))
			rangeOff := int(binary.BigEndian.Uint16(raw[rangeOffsetAt+s*2:]))
			if start > end || start == 0xFFFF {
				continue
			}
			for c := start; c <= end && c < 0x10000; c++ {
				var glyph int
				if rangeOff == 0 {
					glyph = (c + delta) & 0xFFFF
				} else {
					// glyphIndexAddress = rangeOffsetAt + s*2 + rangeOff + (c-start)*2
					ga := rangeOffsetAt + s*2 + rangeOff + (c-start)*2
					if ga+2 > len(raw) {
						continue
					}
					glyph = int(binary.BigEndian.Uint16(raw[ga : ga+2]))
					if glyph != 0 {
						glyph = (glyph + delta) & 0xFFFF
					}
				}
				if glyph != 0 {
					out[rune(c)] = true
				}
			}
		}
	case 6:
		if offset+10 > len(raw) {
			return nil, parseFailure()
		}
		first := int(binary.BigEndian.Uint16(raw[offset+6 : offset+8]))
		count := int(binary.BigEndian.Uint16(raw[offset+8 : offset+10]))
		for i := 0; i < count; i++ {
			at := offset + 10 + i*2
			if at+2 > len(raw) {
				break
			}
			if binary.BigEndian.Uint16(raw[at:]) != 0 {
				out[rune(first+i)] = true
			}
		}
	case 12:
		if offset+16 > len(raw) {
			return nil, parseFailure()
		}
		nGroups := int(binary.BigEndian.Uint32(raw[offset+12 : offset+16]))
		for g := 0; g < nGroups; g++ {
			at := offset + 16 + g*12
			if at+12 > len(raw) {
				break
			}
			start := binary.BigEndian.Uint32(raw[at : at+4])
			end := binary.BigEndian.Uint32(raw[at+4 : at+8])
			for c := start; c <= end && c <= 0x10FFFF; c++ {
				out[rune(c)] = true
			}
		}
	default:
		return nil, nil // unsupported subtable: treat as no coverage
	}
	return out, nil
}
