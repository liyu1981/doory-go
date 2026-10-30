// POST /api/v1/exports/optimize/ (apps/exports/views.py ImageOptimizeView +
// services.optimize_image): convert a PNG the browser drew to the format
// asked for and optimize it, for downloads from the editor.
//
// Ported: png / jpg / webp / auto conversion, the quality presets and the
// max_bytes search, transparency flattening, png palette reduction, the
// progressive JPG flag, and the response's X-Doory-* headers. Not ported:
// AVIF (no pure-Go encoder, D21 — the server advertises it away in /config/)
// and the external optimizers (they are only "available" when configured, and
// the availability/registry is already ported for /config/ and the checks;
// when one IS available the call is answered with a loud notPorted error
// rather than silently skipping the optimization).
package doory

import (
	"fmt"
	"image"
	"image/color"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"doory-go/internal/wasmimage"
)

const optimizeOverheadBytes = 1024 * 1024 // views.OPTIMIZE_OVERHEAD_BYTES

// qualityPresets mirrors services.QUALITY_PRESETS.
var qualityPresets = map[string]int{"small": 65, "balanced": 82, "best": 95, "original": 100}

// optimizeRequest is ImageOptimizeSerializer's validated form.
type optimizeRequest struct {
	Format        string
	Quality       int
	MaxBytes      *int64
	Transparent   bool
	PngColors     *int
	StripMetadata bool
	Srgb          bool
	Progressive   bool
	Optimize      string // "true", "false" or an optimizer id
}

// exportsOptimize ports ImageOptimizeView.post.
func (a *App) exportsOptimize(w http.ResponseWriter, r *http.Request) {
	limit := a.Settings.ImageOptimizerMaxBytes
	if r.ContentLength > int64(limit)+optimizeOverheadBytes {
		writeDRF(w, http.StatusRequestEntityTooLarge, "file_too_large",
			fmt.Sprintf("The image must be at most %d bytes.", limit))
		return
	}
	if err := r.ParseMultipartForm(int64(limit)); err != nil {
		// Not a multipart body at all: upstream sees no `file` and the view
		// raises its own field error (code "invalid", not "required").
		writeDRFValidation(w, []FieldError{SimpleField("file",
			"Send the image (a PNG) in the file field.", "invalid")})
		return
	}
	defer func() {
		if r.MultipartForm != nil {
			_ = r.MultipartForm.RemoveAll()
		}
	}()
	files := r.MultipartForm.File["file"]
	if len(files) == 0 || files[0].Size == 0 {
		writeDRFValidation(w, []FieldError{SimpleField("file",
			"Send the image (a PNG) in the file field.", "invalid")})
		return
	}
	if files[0].Size > int64(limit) {
		writeDRF(w, http.StatusRequestEntityTooLarge, "file_too_large",
			fmt.Sprintf("The image must be at most %d bytes.", limit))
		return
	}
	handle, err := files[0].Open()
	if err != nil {
		logerr(err, "exportsOptimize.open")
		writeDRF(w, http.StatusInternalServerError, "server_error", "Something went wrong on our side.")
		return
	}
	data, err := io.ReadAll(io.LimitReader(handle, int64(limit)+1))
	handle.Close()
	if err != nil || len(data) > limit {
		writeDRF(w, http.StatusRequestEntityTooLarge, "file_too_large",
			fmt.Sprintf("The image must be at most %d bytes.", limit))
		return
	}

	request, fields := a.validateOptimizeForm(r.MultipartForm.Value)
	if len(fields) > 0 {
		writeDRFValidation(w, fields)
		return
	}
	result, derr := a.optimizeImage(data, request)
	if derr != nil {
		writeDRF(w, derr.Status, derr.Code, derr.Message)
		return
	}

	w.Header().Set("Content-Type", result.mime)
	w.Header().Set("X-Doory-Format", result.format)
	w.Header().Set("X-Doory-Width", strconv.Itoa(result.width))
	w.Header().Set("X-Doory-Height", strconv.Itoa(result.height))
	if result.quality > 0 {
		w.Header().Set("X-Doory-Quality", strconv.Itoa(result.quality))
	}
	w.Header().Set("X-Doory-Bytes-Before", strconv.Itoa(result.bytesBefore))
	w.Header().Set("X-Doory-Optimizer", result.optimizer)
	w.Header().Set("X-Doory-Optimized", map[bool]string{true: "true", false: "false"}[result.optimized])
	w.Header().Set("X-Doory-Warnings", "[]")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Length", strconv.Itoa(len(result.data)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(result.data)
}

// validateOptimizeForm ports ImageOptimizeSerializer (declaration order).
func (a *App) validateOptimizeForm(values map[string][]string) (optimizeRequest, []FieldError) {
	get := func(name, def string) string {
		if v, ok := values[name]; ok && len(v) > 0 {
			return v[0]
		}
		return def
	}
	request := optimizeRequest{
		Format:        strings.ToLower(strings.TrimSpace(get("format", "png"))),
		StripMetadata: true,
		Srgb:          true,
		Optimize:      "true",
	}
	var fields []FieldError

	// format: png, jpg, jpeg, webp, avif, auto (default png).
	switch request.Format {
	case "png", "jpg", "jpeg", "webp", "avif", "auto":
	default:
		fields = append(fields, SimpleField("format",
			fmt.Sprintf("%q is not a valid choice.", request.Format), "invalid_choice"))
	}

	// quality: 1..100 or a preset name (default 90).
	rawQuality := strings.TrimSpace(get("quality", "90"))
	if preset, ok := qualityPresets[strings.ToLower(rawQuality)]; ok {
		request.Quality = preset
	} else if n, err := strconv.Atoi(rawQuality); err == nil && n >= 1 && n <= 100 {
		request.Quality = n
	} else {
		fields = append(fields, SimpleField("quality",
			"Give a whole number from 1 to 100, or one of small, balanced, best, original.", "invalid"))
	}

	// max_bytes: a whole number of bytes, 1 or more (optional).
	if raw, ok := values["max_bytes"]; ok && len(raw) > 0 && raw[0] != "" {
		if n, err := strconv.ParseInt(strings.TrimSpace(raw[0]), 10, 64); err == nil && n >= 1 {
			request.MaxBytes = &n
		} else {
			fields = append(fields, SimpleField("max_bytes",
				"A valid integer is required.", "invalid"))
		}
	}

	// transparent: a boolean (default false).
	if raw, ok := values["transparent"]; ok && len(raw) > 0 && raw[0] != "" {
		if v, err := parseDRFBool(raw[0]); err == nil {
			request.Transparent = v
		} else {
			fields = append(fields, SimpleField("transparent", "Must be a valid boolean.", "invalid"))
		}
	}

	// png_colors: "full" or 2..256 (default full).
	if raw, ok := values["png_colors"]; ok && len(raw) > 0 && raw[0] != "" {
		text := strings.TrimSpace(raw[0])
		if strings.EqualFold(text, "full") {
			request.PngColors = nil
		} else if n, err := strconv.Atoi(text); err == nil && n >= 2 && n <= 256 {
			request.PngColors = &n
		} else {
			fields = append(fields, SimpleField("png_colors",
				`Give "full" or a whole number of colours from 2 to 256.`, "invalid"))
		}
	}

	// strip_metadata / srgb / progressive booleans.
	for _, flag := range []struct {
		name string
		dst  *bool
	}{
		{"strip_metadata", &request.StripMetadata},
		{"srgb", &request.Srgb},
		{"progressive", &request.Progressive},
	} {
		if raw, ok := values[flag.name]; ok && len(raw) > 0 && raw[0] != "" {
			if v, err := parseDRFBool(raw[0]); err == nil {
				*flag.dst = v
			} else {
				fields = append(fields, SimpleField(flag.name, "Must be a valid boolean.", "invalid"))
			}
		}
	}

	// optimize: false, true or an optimizer id (default true).
	if raw, ok := values["optimize"]; ok && len(raw) > 0 && raw[0] != "" {
		text := strings.ToLower(strings.TrimSpace(raw[0]))
		switch text {
		case "true":
			request.Optimize = "true"
		case "false":
			request.Optimize = "false"
		default:
			if len(text) > 40 {
				fields = append(fields, SimpleField("optimize",
					"Give true, false or the id of an image optimizer.", "invalid"))
			} else {
				request.Optimize = text
			}
		}
	}
	return request, fields
}

// optimizedImage is OptimizedImage (the pieces the response needs).
type optimizedImage struct {
	data        []byte
	format      string
	mime        string
	width       int
	height      int
	quality     int
	bytesBefore int
	optimizer   string
	optimized   bool
}

// optimizeImage ports services.optimize_image for the formats this server
// makes (png/jpg/webp; AVIF is D21).
func (a *App) optimizeImage(png []byte, request optimizeRequest) (optimizedImage, *DocumentError) {
	format := request.Format
	if format == "jpeg" {
		format = "jpg"
	}
	if format != "auto" && !stringInList(imageFormatsSupported, format) {
		return optimizedImage{}, &DocumentError{
			Code: "format_unavailable", Status: http.StatusBadRequest,
			Message: fmt.Sprintf("This server can't make %s images.", strings.ToUpper(format)),
		}
	}
	// The optimizer choice happens before the image is touched: an id this
	// server doesn't offer is an error; `true` without one is a no-op.
	optimizer := ""
	if request.Optimize != "false" {
		if request.Optimize == "true" {
			if def := a.imageOptimizeConfig().Default; def != nil {
				optimizer = *def
			}
		} else {
			offered := []string{}
			for _, candidate := range a.imageOptimizeConfig().Optimizers {
				offered = append(offered, candidate.ID)
			}
			if !stringInList(offered, request.Optimize) {
				return optimizedImage{}, &DocumentError{
					Code: "optimizer_unavailable", Status: http.StatusBadRequest,
					Message: fmt.Sprintf("This server has no image optimizer %q (it offers: %s).",
						request.Optimize, strings.Join(offered, ", ")),
				}
			}
			optimizer = request.Optimize
		}
	}
	unreadable := &DocumentError{
		Code: "invalid_image", Status: http.StatusBadRequest,
		Message: "The file is not a readable PNG image.",
	}
	width, height, detected, err := decodeImageConfig(png)
	if err != nil {
		return optimizedImage{}, unreadable
	}
	if strings.ToLower(detected) != "png" {
		return optimizedImage{}, &DocumentError{
			Code: "invalid_image", Status: http.StatusBadRequest, Message: "Send the image as a PNG.",
		}
	}
	maxPixels := int64(a.Settings.ExportImageMaxPixels)
	if int64(width)*int64(height) > maxPixels {
		return optimizedImage{}, &DocumentError{
			Code: "too_large", Status: http.StatusBadRequest,
			Message: fmt.Sprintf("%d x %d px is more than this server converts (%d pixels).",
				width, height, maxPixels),
		}
	}
	img, _, err := decodeImageData(png)
	if err != nil {
		return optimizedImage{}, unreadable
	}
	// A decoder can be more permissive than the header check; keep the
	// reported size from the header (the oracle uses Pillow's probe).
	formats := []string{format}
	if format == "auto" {
		formats = append([]string{}, imageFormatsSupported...)
		if request.Transparent {
			formats = withoutString(formats, "jpg")
		}
	}
	var best *optimizedImage
	for _, candidate := range formats {
		converted, cerr := convertOptimize(img, candidate, request)
		if cerr != nil {
			return optimizedImage{}, cerr
		}
		if best == nil || len(converted.data) < len(best.data) {
			copy := converted
			best = &copy
		}
	}
	if best == nil {
		return optimizedImage{}, unreadable
	}
	best.width, best.height = width, height
	best.bytesBefore = len(best.data)
	if optimizer == "builtin" {
		// The built-in optimizer (D22, embedded WASI cjpeg/oxipng): never
		// larger, and the headers say whether its file was kept.
		shrunk, applied := a.optimizeBuiltin(best.data, best.format, best.quality, img, request)
		if applied {
			best.data = shrunk
			best.optimized = true
		}
		best.optimizer = "builtin"
	} else if optimizer != "" {
		// TinyPNG / Kraken.io / custom: same contract as upstream's
		// optimize(): a failure, timeout or non-smaller answer leaves the
		// converted file as it was.
		if len(best.data) <= a.Settings.ImageOptimizerMaxBytes {
			var quality *int
			if best.quality > 0 {
				value := best.quality
				quality = &value
			}
			job := offsiteImageJob{
				data: best.data, format: best.format, mime: best.mime,
				width: width, height: height, quality: quality,
				stripMeta: request.StripMetadata, srgb: request.Srgb,
			}
			if shrunk, err := a.optimizeOffsite(job, optimizer); err == nil && len(shrunk) < len(best.data) {
				outW, outH, outFormat, cerr := decodeImageConfig(shrunk)
				if cerr == nil && outFormat == best.format && outW == width && outH == height {
					best.data = shrunk
					best.optimized = true
				}
			}
		}
		best.optimizer = optimizer
	}
	return *best, nil
}

// convertOptimize makes one format at the requested quality, searching down
// when max_bytes asks for a smaller file.
func convertOptimize(img image.Image, format string, request optimizeRequest) (optimizedImage, *DocumentError) {
	encode := func(quality int) ([]byte, *DocumentError) {
		var prepared image.Image = img
		if format == "jpg" || (format == "webp" && !request.Transparent) {
			prepared = flattenOnWhite(toRGBA(img))
		}
		switch format {
		case "png":
			prepared = toRGBA(prepared)
			if request.PngColors != nil {
				prepared = quantize(prepared, *request.PngColors)
			}
			data, err := encodeImage(prepared, "png")
			if err != nil {
				return nil, &DocumentError{Code: "server_error", Status: http.StatusInternalServerError,
					Message: "Something went wrong on our side."}
			}
			return data, nil
		case "jpg":
			data, err := encodeJPEG(prepared, quality, request.Progressive)
			if err != nil {
				return nil, &DocumentError{Code: "server_error", Status: http.StatusInternalServerError,
					Message: "Something went wrong on our side."}
			}
			return data, nil
		case "webp":
			data, err := encodeWebPQuality(prepared, float32(quality), false)
			if err != nil {
				return nil, &DocumentError{Code: "server_error", Status: http.StatusInternalServerError,
					Message: "Something went wrong on our side."}
			}
			return data, nil
		}
		return nil, &DocumentError{Code: "invalid_format", Status: http.StatusBadRequest,
			Message: "The format must be png, jpg, webp, avif or auto."}
	}
	data, derr := encode(request.Quality)
	if derr != nil {
		return optimizedImage{}, derr
	}
	quality := request.Quality
	if format == "png" {
		quality = 0 // PNG reports no quality
	}
	if request.MaxBytes != nil && int64(len(data)) > *request.MaxBytes {
		// Step the quality down until the file fits (the oracle's search),
		// never below 1.
		for q := request.Quality - 5; q >= 1; q -= 5 {
			candidate, err := encode(q)
			if err != nil {
				break
			}
			data, quality = candidate, q
			if format == "png" {
				quality = 0
			}
			if int64(len(data)) <= *request.MaxBytes {
				break
			}
		}
	}
	mime := map[string]string{"png": "image/png", "jpg": "image/jpeg", "webp": "image/webp"}[format]
	return optimizedImage{
		data: data, format: format, mime: mime, quality: quality,
		bytesBefore: len(data), optimizer: "", optimized: false,
	}, nil
}

// encodeJPEG encodes with the requested quality through libjpeg-turbo,
// mirroring Pillow's optimize=True and optional progressive mode.
func encodeJPEG(img image.Image, quality int, progressive bool) ([]byte, error) {
	pixels, width, height := rgbPixels(flattenOnWhite(img)), img.Bounds().Dx(), img.Bounds().Dy()
	return wasmimage.EncodeJPEG(pixels, width, height, quality, true, progressive)
}

// withoutString drops one value (the auto format list without jpg for a
// transparent export).
func withoutString(values []string, drop string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		if value != drop {
			out = append(out, value)
		}
	}
	return out
}

func stringInList(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// quantize reduces an image to at most `colors` colours by median cut
// (png_colors; Pillow's img.quantize). Pixels keep their alpha.
func quantize(src image.Image, colors int) image.Image {
	b := src.Bounds()
	type sample struct {
		r, g, bl, a uint8
	}
	samples := make([]sample, 0, b.Dx()*b.Dy())
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			r, g, bl, a := src.At(x, y).RGBA()
			samples = append(samples, sample{uint8(r >> 8), uint8(g >> 8), uint8(bl >> 8), uint8(a >> 8)})
		}
	}
	type box struct {
		from, to int
	}
	boxes := []box{{0, len(samples)}}
	channel := func(s sample, axis int) int {
		switch axis {
		case 0:
			return int(s.r)
		case 1:
			return int(s.g)
		case 2:
			return int(s.bl)
		}
		return int(s.a)
	}
	for len(boxes) < colors {
		// Split the box with the widest channel range at its median.
		bestIndex, bestAxis, bestRange := -1, 0, -1
		for i, bx := range boxes {
			if bx.to-bx.from < 2 {
				continue
			}
			for axis := 0; axis < 4; axis++ {
				low, high := 255, 0
				for _, s := range samples[bx.from:bx.to] {
					v := channel(s, axis)
					if v < low {
						low = v
					}
					if v > high {
						high = v
					}
				}
				if high-low > bestRange {
					bestIndex, bestAxis, bestRange = i, axis, high-low
				}
			}
		}
		if bestIndex < 0 {
			break
		}
		bx := boxes[bestIndex]
		sort.SliceStable(samples[bx.from:bx.to], func(i, j int) bool {
			return channel(samples[bx.from+i], bestAxis) < channel(samples[bx.from+j], bestAxis)
		})
		mid := (bx.from + bx.to) / 2
		boxes[bestIndex] = box{bx.from, mid}
		boxes = append(boxes, box{mid, bx.to})
	}
	palette := make(color.Palette, 0, len(boxes))
	lookup := make([]int, len(samples))
	// Each box contributes its average colour; every sample maps to the box
	// containing it (boxes cover contiguous ranges after the splits).
	starts := make([]int, len(boxes))
	for i, bx := range boxes {
		starts[i] = bx.from
		var r, g, bl, a uint64
		for _, s := range samples[bx.from:bx.to] {
			r += uint64(s.r)
			g += uint64(s.g)
			bl += uint64(s.bl)
			a += uint64(s.a)
		}
		n := uint64(bx.to - bx.from)
		if n == 0 {
			n = 1
		}
		palette = append(palette, color.NRGBA{uint8(r / n), uint8(g / n), uint8(bl / n), uint8(a / n)})
		for idx := bx.from; idx < bx.to; idx++ {
			lookup[idx] = i
		}
	}
	out := image.NewPaletted(image.Rect(0, 0, b.Dx(), b.Dy()), palette)
	for i, idx := range lookup {
		out.Pix[i] = uint8(idx)
	}
	return out
}
