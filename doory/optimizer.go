// The built-in image optimizer (D22). Upstream runs mozjpeg's `cjpeg` and
// `oxipng` as child processes. The port does the same: the same binary starts
// the embedded WASI cjpeg/oxipng workers through the hidden `mozjpeg` and
// `oxipng` subcommands. The Go re-encode below is only the fallback used when
// the worker cannot be started (for example inside the Go test binary).
//
// Ports backend/apps/exports/optimizers/builtin.py
package doory

import (
	"bufio"
	"bytes"
	"compress/zlib"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"image"
	"image/jpeg"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// optimizerBuiltinFormats is what the embedded optimizer can shrink (the
// formats upstream's builtin handles when both tools are installed).
var optimizerBuiltinFormats = []string{"jpg", "png"}

// builtinOptimizerAvailable reports the embedded optimizer (always true,
// D22).
func builtinOptimizerAvailable() bool { return true }

// shrinkBuiltin is the pure-Go fallback, kept for tests and for environments
// where the worker process cannot be started.
func shrinkBuiltin(data []byte, format string, quality int, source image.Image, progressive bool) ([]byte, bool) {
	return shrinkBuiltinGo(data, format, quality, source, progressive)
}

// optimizeBuiltin ports builtin.Builtin.shrink through the embedded WASI
// processes, with the Go implementation as the fallback when the executable
// cannot be self-invoked.
func (a *App) optimizeBuiltin(data []byte, format string, quality int, source image.Image, request optimizeRequest) ([]byte, bool) {
	if format == "jpg" {
		// Upstream only re-encodes a JPG when the lossless source is
		// available and the export leaves metadata and colour profile out.
		if source == nil || quality <= 0 || quality > 90 || !request.StripMetadata || !request.Srgb {
			return data, false
		}
	}
	out, applied, err := a.runBuiltinWorker(data, format, quality, source, request)
	if err != nil {
		if errors.Is(err, errCodecWorkerUnavailable) {
			return shrinkBuiltinGo(data, format, quality, source, request.Progressive)
		}
		// A failed/timed-out optimizer leaves the file as it is (upstream
		// optimize() catches OptimizerError the same way).
		return data, false
	}
	return out, applied
}

var errCodecWorkerUnavailable = errors.New("codec worker unavailable")

// codecSelf is replaceable in tests; os.Executable is the same binary in
// production, so the parent opens two child processes just like upstream.
var codecSelf = os.Executable

func (a *App) runBuiltinWorker(data []byte, format string, quality int, source image.Image,
	request optimizeRequest) ([]byte, bool, error) {
	if format != "jpg" && format != "png" {
		return data, false, nil
	}
	exe, err := codecSelf()
	if err != nil {
		return nil, false, errCodecWorkerUnavailable
	}
	if strings.HasSuffix(filepath.Base(exe), ".test") {
		return nil, false, errCodecWorkerUnavailable
	}
	workdir, err := os.MkdirTemp("", "doory-optimize-")
	if err != nil {
		return nil, false, err
	}
	defer os.RemoveAll(workdir)

	var subcommand, outName string
	var args []string
	switch format {
	case "jpg":
		if err := writePPM(filepath.Join(workdir, "in.ppm"), flattenOnWhite(toRGBA(source))); err != nil {
			return nil, false, err
		}
		outName = "out.jpg"
		args = []string{"-quality", strconv.Itoa(quality), "-outfile", outName}
		if !request.Progressive {
			args = append(args, "-baseline")
		}
		args = append(args, "in.ppm")
		subcommand = "mozjpeg"
	case "png":
		if err := os.WriteFile(filepath.Join(workdir, "in.png"), data, 0o600); err != nil {
			return nil, false, err
		}
		outName = "out.png"
		args = []string{"-q", "-o", "2", "-t", "2"}
		if request.StripMetadata {
			args = append(args, "--strip", "safe")
		}
		args = append(args, "--out", outName, "in.png")
		subcommand = "oxipng"
	}
	timeout := time.Duration(a.Settings.ImageOptimizerTimeout) * time.Second
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	command := exec.CommandContext(ctx, exe, append([]string{subcommand}, args...)...)
	command.Dir = workdir
	command.Env = os.Environ()
	output, err := command.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		return nil, false, fmt.Errorf("%s timed out", subcommand)
	}
	if err != nil {
		return nil, false, fmt.Errorf("%s: %w: %s", subcommand, err, strings.TrimSpace(string(output)))
	}
	out, err := os.ReadFile(filepath.Join(workdir, outName))
	if err != nil {
		return nil, false, err
	}
	// upstream optimize() refuses a different format or size (same_image).
	inputW, inputH, inputFormat, err := decodeImageConfig(data)
	if err != nil {
		return nil, false, err
	}
	outputW, outputH, outputFormat, err := decodeImageConfig(out)
	if err != nil || outputFormat != inputFormat || outputW != inputW || outputH != inputH {
		return nil, false, fmt.Errorf("%s returned a different image", subcommand)
	}
	if len(out) >= len(data) {
		return data, false, nil
	}
	return out, true, nil
}

// writePPM writes P6 RGB bytes, the input cjpeg takes.
func writePPM(path string, img image.Image) error {
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	defer file.Close()
	bounds := img.Bounds()
	writer := bufio.NewWriterSize(file, 1<<20)
	if _, err := fmt.Fprintf(writer, "P6\n%d %d\n255\n", bounds.Dx(), bounds.Dy()); err != nil {
		return err
	}
	row := make([]byte, bounds.Dx()*3)
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			r, g, b, _ := img.At(x, y).RGBA()
			i := (x - bounds.Min.X) * 3
			row[i], row[i+1], row[i+2] = byte(r>>8), byte(g>>8), byte(b>>8)
		}
		if _, err := writer.Write(row); err != nil {
			return err
		}
	}
	return writer.Flush()
}

// shrinkBuiltinGo is the previous in-process implementation, used only as a
// fallback when the self-invoked WASI worker is unavailable.
func shrinkBuiltinGo(data []byte, format string, quality int, source image.Image, progressive bool) ([]byte, bool) {
	switch format {
	case "jpg":
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
	// Fallback path only: recompress the scanlines at the highest deflate
	// level (the embedded oxipng worker handles the normal path).
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
