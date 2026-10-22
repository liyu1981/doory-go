// Container loaders: sfnt (TTF/OTF), WOFF (per-table zlib) and WOFF2
// (brotli + transformed-directory rules) — all presenting the same lazy
// table view.
package fontparse

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"io"
	"unicode/utf16"
)

func utf16Decode(units []uint16) []rune { return utf16.Decode(units) }

// loadContainer parses the container and returns its table view.
func loadContainer(format Format, data []byte) (tables, error) {
	switch format {
	case TTF, OTF:
		return loadSfnt(data)
	case WOFF:
		return loadWOFF(data)
	case WOFF2:
		return loadWOFF2(data)
	}
	return nil, parseFailure()
}

// ---- sfnt core ----

type sfntTables struct {
	data  []byte
	dir   map[string][2]int // tag -> [offset, length]
	order []string
}

func parseSfntDirectory(data []byte, base int) (map[string][2]int, []string, error) {
	if base+12 > len(data) {
		return nil, nil, parseFailure()
	}
	numTables := int(binary.BigEndian.Uint16(data[base+4 : base+6]))
	if numTables < 1 || base+12+numTables*16 > len(data) {
		return nil, nil, parseFailure()
	}
	dir := map[string][2]int{}
	var order []string
	for i := 0; i < numTables; i++ {
		at := base + 12 + i*16
		tag := string(data[at : at+4])
		offset := int(binary.BigEndian.Uint32(data[at+8 : at+12]))
		length := int(binary.BigEndian.Uint32(data[at+12 : at+16]))
		if offset < 0 || offset+length > len(data) {
			return nil, nil, parseFailure()
		}
		dir[tag] = [2]int{offset, length}
		order = append(order, tag)
	}
	return dir, order, nil
}

func (s *sfntTables) has(tag string) bool { _, ok := s.dir[tag]; return ok }

func (s *sfntTables) table(tag string) ([]byte, error) {
	at, ok := s.dir[tag]
	if !ok {
		return nil, parseFailure()
	}
	return s.data[at[0] : at[0]+at[1]], nil
}

func loadSfnt(data []byte) (tables, error) {
	dir, order, err := parseSfntDirectory(data, 0)
	if err != nil {
		return nil, err
	}
	return &sfntTables{data: data, dir: dir, order: order}, nil
}

// ---- WOFF: per-table zlib (or raw when compLength == origLength) ----

type woffTables struct {
	sfntTables // tables hold ALREADY-DECOMPRESSED sfnt bytes
}

func loadWOFF(data []byte) (tables, error) {
	if len(data) < 44 {
		return nil, parseFailure()
	}
	numTables := int(binary.BigEndian.Uint16(data[12:14]))
	if numTables < 1 || 44+numTables*20 > len(data) {
		return nil, parseFailure()
	}
	decoded := map[string][]byte{}
	for i := 0; i < numTables; i++ {
		at := 44 + i*20
		tag := string(data[at : at+4])
		offset := int(binary.BigEndian.Uint32(data[at+4 : at+8]))
		compLength := int(binary.BigEndian.Uint32(data[at+8 : at+12]))
		origLength := int(binary.BigEndian.Uint32(data[at+12 : at+16]))
		if offset < 0 || compLength < 0 || offset+compLength > len(data) {
			return nil, parseFailure()
		}
		raw := data[offset : offset+compLength]
		if compLength == origLength {
			decoded[tag] = raw
			continue
		}
		zr, err := zlib.NewReader(bytes.NewReader(raw))
		if err != nil {
			return nil, parseFailure()
		}
		out, err := io.ReadAll(io.LimitReader(zr, int64(origLength)+1))
		zr.Close()
		if err != nil || len(out) != origLength {
			return nil, parseFailure()
		}
		decoded[tag] = out
	}
	return &directTables{m: decoded}, nil
}

// directTables serves already-decoded table bytes.
type directTables struct{ m map[string][]byte }

func (d *directTables) has(tag string) bool { _, ok := d.m[tag]; return ok }

func (d *directTables) table(tag string) ([]byte, error) {
	b, ok := d.m[tag]
	if !ok {
		return nil, parseFailure()
	}
	return b, nil
}
