// The built-in image optimizer, implemented in process (D22): upstream shells
// out to mozjpeg's `cjpeg` and `oxipng`; the port re-encodes JPEG with the
// standard library and re-deflates PNG scanlines at the highest level, which
// is the same contract (never larger, metadata kept, fail-safe) without native
// binaries in the binary.
package doory

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"hash/crc32"
	"image"
	"image/jpeg"
	"io"
)

// optimizerBuiltinFormats is what the in-process optimizer can shrink (the
// formats upstream's builtin handles when both tools are installed).
var optimizerBuiltinFormats = []string{"jpg", "png"}

// builtinOptimizerAvailable reports the in-process optimizer (always true,
// D22).
func builtinOptimizerAvailable() bool { return true }

// shrinkBuiltin ports optimizers/builtin.py for the formats the port makes:
// JPG re-encoded at the same quality (below mozjpeg's ceiling), PNG
// recompressed losslessly. Returns the (possibly unchanged) file and whether
// a smaller one was kept.
func shrinkBuiltin(data []byte, format string, quality int, source image.Image, progressive bool) ([]byte, bool) {
	switch format {
	case "jpg":
		// mozjpeg only gains below quality 91; above it Pillow's file stands.
		if quality <= 0 || quality > 90 {
			return data, false
		}
		flat := flattenOnWhite(toRGBA(source))
		out, err := encodeJPEG(flat, quality, progressive)
		if err != nil || len(out) >= len(data) {
			return data, false
		}
		return out, true
	case "png":
		out, err := zopfliPNG(data)
		if err != nil || len(out) >= len(data) {
			return data, false
		}
		return out, true
	}
	return data, false
}

// zopfliPNG recompresses a PNG's IDAT stream with Zopfli (lossless: pixels and
// every ancillary chunk are untouched; oxipng's -o 2 equivalent without filter
// trials).
func zopfliPNG(data []byte) ([]byte, error) {
	const signature = "\x89PNG\r\n\x1a\n"
	if len(data) < len(signature) || string(data[:8]) != signature {
		return nil, errNotPNG
	}
	var idat bytes.Buffer
	out := bytes.NewBuffer(make([]byte, 0, len(data)))
	out.WriteString(signature)
	pos := 8
	for pos+8 <= len(data) {
		length := int(binary.BigEndian.Uint32(data[pos : pos+4]))
		kind := string(data[pos+4 : pos+8])
		end := pos + 8 + length + 4
		if end > len(data) {
			return nil, errNotPNG
		}
		body := data[pos+8 : pos+8+length]
		switch kind {
		case "IDAT":
			idat.Write(body)
		case "IEND":
			// Flush the recompressed stream before the end chunk.
			if err := writeZopfliIDAT(out, idat.Bytes()); err != nil {
				return nil, err
			}
			out.Write(data[pos:end])
			return out.Bytes(), nil
		default:
			out.Write(data[pos:end])
		}
		pos = end
	}
	return nil, errNotPNG
}

// writeZopfliIDAT decompresses `zlibData`, recompresses it with Zopfli and
// writes it as one IDAT chunk (zlib-wrapped, as PNG requires).
func writeZopfliIDAT(out *bytes.Buffer, zlibData []byte) error {
	if len(zlibData) == 0 {
		return nil
	}
	reader, err := zlib.NewReader(bytes.NewReader(zlibData))
	if err != nil {
		return err
	}
	raw, err := io.ReadAll(reader)
	if err != nil {
		return err
	}
	// Recompress the scanlines at the highest deflate level (zopfli's Go
	// port emits a broken stored-block stream for the zlib wrapper, so the
	// standard library does the deflate work — D22).
	var recompressed bytes.Buffer
	writer, err := zlib.NewWriterLevel(&recompressed, zlib.BestCompression)
	if err != nil {
		return err
	}
	if _, err := writer.Write(raw); err != nil {
		return err
	}
	if err := writer.Close(); err != nil {
		return err
	}

	// One IDAT chunk: length, type, body, CRC.
	body := recompressed.Bytes()
	var length [4]byte
	binary.BigEndian.PutUint32(length[:], uint32(len(body)))
	out.Write(length[:])
	out.WriteString("IDAT")
	out.Write(body)
	out.Write(crc32PNG(body))
	return nil
}

// crc32PNG is the chunk CRC (over type + body); callers pass the body and the
// type is written just before it.
func crc32PNG(body []byte) []byte {
	crc := crc32.ChecksumIEEE(append([]byte("IDAT"), body...))
	var out [4]byte
	binary.BigEndian.PutUint32(out[:], crc)
	return out[:]
}

var errNotPNG = errNotPNGType{}

type errNotPNGType struct{}

func (errNotPNGType) Error() string { return "not a PNG" }

// decodeJPEGSource decodes the JPEG bytes a conversion made (used by the JPG
// re-encode path).
func decodeJPEGSource(data []byte) (image.Image, error) {
	return jpeg.Decode(bytes.NewReader(data))
}
