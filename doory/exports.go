// Server-side export endpoints (apps/exports/{views,serializers,services}.py).
//
// Probe lineage: probes/0009-exports (see its meta): disabled envelopes,
// validation messages, the PDF/image response headers and the delivery
// download flow. The happy render paths drive the SAME system Chromium on
// both sides (EXPORT_CHROMIUM_PATH); PDF BYTES carry per-render ids/times
// so scenarios compare PDF STRUCTURE (page count, boxes, catalog keys via
// internal/pdfscan), not bytes. pdfmeta's exact-box surgery is the family's
// remaining slice (boxes stay Chromium's; PORT_MAP note).
//
// Ports backend/apps/exports/views.py
// Ports backend/apps/exports/serializers.py
package doory

import (
	"bytes"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"math"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"doory-go/internal/pdfmeta"
	"doory-go/internal/svg"
)

// imageExportResult mirrors services.ImageResult (the fields filenames and
// headers are built from).
type imageExportResult struct {
	Data       []byte
	Format     string
	MIME       string
	Width      int
	Height     int
	PageID     string
	PageIndex  int
	PageWidth  float64
	PageHeight float64
	PageCount  int
	Preset     string
}

// pdfExport ports PdfExportView.
func (a *App) pdfExport(w http.ResponseWriter, r *http.Request) {
	if !a.Settings.ServerPDFEnabled {
		writeDRF(w, http.StatusNotFound, "server_pdf_disabled",
			"Server-side PDF export is not enabled on this server.")
		return
	}
	if r.ContentLength > 40*1024*1024+2*1024*1024 {
		writeDRF(w, http.StatusRequestEntityTooLarge, "payload_too_large", "The upload is too large.")
		return
	}
	// Empty/non-multipart requests behave like the oracle's: request.FILES
	// is empty and validate_page_files answers first (probe0009 step03).
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
		if err := r.ParseMultipartForm(64 * 1024 * 1024); err != nil {
			writeDRF(w, http.StatusBadRequest, "parse_error", "Malformed request body.")
			return
		}
	} else {
		r.MultipartForm = new(multipart.Form)
	}
	defer func() {
		if r.MultipartForm != nil {
			_ = r.MultipartForm.RemoveAll()
		}
	}()

	// validate_page_files: count + empty checks.
	files := r.MultipartForm.File["pages"]
	if len(files) == 0 {
		writeDRFValidation(w, []FieldError{SimpleField("pages", "Upload at least one SVG page.", "invalid")})
		return
	}
	if len(files) > 100 {
		writeDRFValidation(w, []FieldError{SimpleField("pages",
			"At most 100 pages can be exported at once.", "invalid")})
		return
	}
	var pages [][]byte
	for _, fh := range files {
		data, err := readUpload(fh)
		if err != nil {
			writeDRFValidation(w, []FieldError{SimpleField("pages",
				"A page file is empty.", "invalid")})
			return
		}
		pages = append(pages, data)
	}

	// PdfExportSerializer: scalar fields + sizes/fonts JSON checks.
	width, height, fields, ferr := a.validatePdfExport(r, len(pages))
	if ferr != nil {
		writeDRFValidation(w, []FieldError{*ferr})
		return
	}
	_ = width
	_ = height

	cleaned := make([]string, 0, len(pages))
	for i, data := range pages {
		out, err := svg.Sanitize(data, true)
		if err != nil {
			writeDRF(w, http.StatusBadRequest, "invalid_svg",
				fmt.Sprintf("Page %d is not a valid, safe SVG.", i+1))
			return
		}
		cleaned = append(cleaned, string(out))
	}

	fonts, ferr2 := a.loadExportFonts(fieldsFonts)
	if ferr2 != nil {
		writeDRF(w, ferr2.Status, ferr2.Code, ferr2.Message)
		return
	}

	geometries := make([]PageGeometry, 0, len(pages))
	if fields.sizes == nil {
		for range pages {
			geometries = append(geometries, NewPageGeometry(width, height, fields.bleedMM, fields.cropMarks))
		}
	} else {
		for _, size := range fields.sizes {
			geometries = append(geometries, NewPageGeometry(size[0], size[1], fields.bleedMM, fields.cropMarks))
		}
	}

	html := buildDocumentHTML(cleaned, geometries, fields.title, fonts, fields.lang)
	result, derr := a.renderPDF(html, geometries[0].pageWidth(), geometries[0].pageHeight())
	if derr != nil {
		writeDRF(w, derr.Status, derr.Code, derr.Message)
		return
	}
	// finalize_pdf: exact page boxes + /Lang + /Creator metadata (the
	// browser PDF is the same chromium output on both sides; this surgery
	// is what makes the boxes exact — pdfmeta.Apply).
	finalPDF := pdfmeta.Apply(result.Data, geometriesForPDF(geometries), fields.title, fields.lang)

	w.Header().Set("Content-Type", "application/pdf")
	w.Header().Set("Content-Disposition", contentDisposition(pdfFilename(fields.title), true))
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	body := finalPDF
	w.Header().Set("Content-Length", itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// geometriesForPDF maps layout geometry to the pdfmeta shape.
func geometriesForPDF(gs []PageGeometry) []pdfmeta.Geometry {
	out := make([]pdfmeta.Geometry, 0, len(gs))
	for _, g := range gs {
		out = append(out, pdfmeta.Geometry{
			PageWidth: g.pageWidth(), PageHeight: g.pageHeight(),
			Bleed: g.Bleed, Slug: g.Slug, HasMarks: g.hasMarks(),
		})
	}
	return out
}

// pdfFields is the validated scalar half of the multipart PDF request.
type pdfFields struct {
	width, height float64
	sizes         [][2]float64
	bleedMM       float64
	cropMarks     bool
	title         string
	lang          string
}

var fieldsFonts []fontSpec // per-request state; reset on entry (see validate)

// validatePdfExport ports PdfExportSerializer + the sizes/fonts JSON
// validation (messages probe-aligned where noted; DRF field names exact).
func (a *App) validatePdfExport(r *http.Request, pageCount int) (float64, float64, *pdfFields, *FieldError) {
	form := r.MultipartForm.Value
	fields := &pdfFields{lang: "en"}
	var errs []FieldError
	fieldsFonts = nil

	num := func(name string, low, high float64, required bool) (float64, error) {
		raw := form[name]
		if len(raw) == 0 || raw[0] == "" {
			if required {
				errs = append(errs, SimpleField(name, "This field is required.", "required"))
				return 0, fmt.Errorf("missing")
			}
			return 0, nil
		}
		v, err := parseFloatStrict(raw[0])
		if err != nil || v < low || v > high {
			if required {
				errs = append(errs, SimpleField(name,
					fmt.Sprintf("Ensure this value is between %g and %g.", low, high), "min_value"))
			} else {
				errs = append(errs, SimpleField(name,
					fmt.Sprintf("Ensure this value is between %g and %g.", low, high), "min_value"))
			}
			return 0, fmt.Errorf("range")
		}
		return v, nil
	}

	var err error
	fields.width, err = num("width", 1, 20000, true)
	if err != nil && len(errs) == 0 {
		return 0, 0, fields, nil
	}
	fields.height, err = num("height", 1, 20000, true)
	if err != nil && len(errs) == 0 {
		return 0, 0, fields, nil
	}
	fields.bleedMM, _ = num("bleed_mm", 0, 10, false)
	if raw := form["crop_marks"]; len(raw) > 0 && raw[0] != "" {
		fields.cropMarks = raw[0] == "true" || raw[0] == "1"
	}
	if raw := form["title"]; len(raw) > 0 {
		if len([]rune(raw[0])) > 200 {
			errs = append(errs, SimpleField("title",
				"Ensure this field has no more than 200 characters.", "max_length"))
		} else {
			fields.title = raw[0]
		}
	}
	if raw := form["lang"]; len(raw) > 0 && raw[0] != "" {
		if !validLangTag(raw[0]) {
			errs = append(errs, FieldError{Name: "lang",
				Messages: []string{"Enter a value matching the pattern ^[A-Za-z]{2,3}(?:-[A-Za-z0-9]{2,8}){0,3}$."},
				Codes:    []string{"invalid"}})
		} else {
			fields.lang = raw[0]
		}
	}
	if raw := form["sizes"]; len(raw) > 0 && strings.TrimSpace(raw[0]) != "" {
		var parsed []any
		if err := json.Unmarshal([]byte(raw[0]), &parsed); err != nil {
			errs = append(errs, SimpleField("sizes", "sizes must be a JSON array.", "invalid"))
		} else if len(parsed) != pageCount {
			errs = append(errs, SimpleField("sizes",
				fmt.Sprintf("sizes must list exactly one size per page (%d), got %d.", pageCount, len(parsed)),
				"invalid"))
		} else {
			for _, entry := range parsed {
				obj, ok := entry.(map[string]any)
				if !ok {
					errs = append(errs, SimpleField("sizes", "sizes must be a JSON array.", "invalid"))
					break
				}
				wv, wok := asNumber(obj["width"])
				hv, hok := asNumber(obj["height"])
				if !wok || !hok || wv < 1 || wv > 20000 || hv < 1 || hv > 20000 {
					errs = append(errs, SimpleField("sizes", "Invalid page size.", "invalid"))
					break
				}
				fields.sizes = append(fields.sizes, [2]float64{wv, hv})
			}
		}
	}
	if raw := form["fonts"]; len(raw) > 0 && strings.TrimSpace(raw[0]) != "" {
		var parsed []any
		if err := json.Unmarshal([]byte(raw[0]), &parsed); err != nil || len(parsed) > 32 {
			errs = append(errs, SimpleField("fonts",
				fmt.Sprintf("fonts must be a JSON array of at most %d entries.", 32), "invalid"))
		} else {
			for _, entry := range parsed {
				obj, ok := entry.(map[string]any)
				if !ok {
					errs = append(errs, SimpleField("fonts", "Invalid font.", "invalid"))
					break
				}
				spec := fontSpec{Style: "normal", Weight: 400}
				family, _ := obj["family"].(string)
				if !validFontFamily(family) {
					errs = append(errs, FieldError{Name: "fonts",
						Messages: []string{"Enter a value matching the pattern ^[\\w .\\-]{1,100}$."},
						Codes:    []string{"invalid"}})
					break
				}
				spec.Family = family
				url, _ := obj["url"].(string)
				if len([]rune(url)) > 500 || url == "" {
					errs = append(errs, FieldError{Name: "fonts",
						Messages: []string{"Ensure this field has no more than 500 characters."},
						Codes:    []string{"max_length"}})
					break
				}
				spec.URL = url
				if v, ok := asNumber(obj["weight"]); ok {
					if v < 1 || v > 1000 {
						errs = append(errs, FieldError{Name: "fonts",
							Messages: []string{"Ensure this value is between 1 and 1000."},
							Codes:    []string{"min_value"}})
						break
					}
					spec.Weight = int(v)
				}
				if style, _ := obj["style"].(string); style != "" {
					if style != "normal" && style != "italic" {
						errs = append(errs, FieldError{Name: "fonts",
							Messages: []string{fmt.Sprintf("%q is not a valid choice.", style)},
							Codes:    []string{"invalid_choice"}})
						break
					}
					spec.Style = style
				}
				fieldsFonts = append(fieldsFonts, spec)
			}
		}
	}
	if len(errs) > 0 {
		return fields.width, fields.height, fields, &errs[0]
	}
	return fields.width, fields.height, fields, nil
}

// validFontFamily: FONT_FAMILY_PATTERN ^[\w .\-]{1,100}$ (Python \w is
// unicode — letters/digits/underscore).
func validFontFamily(v string) bool {
	if v == "" || len([]rune(v)) > 100 {
		return false
	}
	for _, r := range v {
		switch {
		case r == ' ' || r == '.' || r == '-' || r == '_':
		case r > 127:
		case (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9'):
		default:
			return false
		}
	}
	return true
}

// validLangTag: LANGUAGE_PATTERN.
func validLangTag(v string) bool {
	if len(v) < 2 || len(v) > 35 {
		return false
	}
	parts := strings.Split(v, "-")
	for i, part := range parts {
		if i == 0 {
			if len(part) < 2 || len(part) > 3 || !onlyASCIIAlpha(part) {
				return false
			}
			continue
		}
		if len(part) < 2 || len(part) > 8 || !onlyAlnum(part) {
			return false
		}
	}
	return true
}

func onlyASCIIAlpha(s string) bool {
	for _, r := range s {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')) {
			return false
		}
	}
	return len(s) > 0
}

func onlyAlnum(s string) bool {
	for _, r := range s {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')) {
			return false
		}
	}
	return len(s) > 0
}

// parseFloatStrict rejects NaN/Inf (FiniteFloatField: math.isfinite).
func parseFloatStrict(v string) (float64, error) {
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return 0, err
	}
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, fmt.Errorf("not finite")
	}
	return f, nil
}

// imageExport ports ImageExportView: renders one page of a SAVED design
// through the frontend's headless render page (server_image_export_enabled
// gates it; disabled by default — probe-environment enables it).
func (a *App) imageExport(w http.ResponseWriter, r *http.Request) {
	// The oracle looks the design up FIRST (get_object_or_404) and checks
	// the enabled switch inside export_image — an unknown design is
	// "Not found." even when exports are disabled (probed live by
	// scenario 14 step04).
	u := currentUser(r)
	d, ok := a.getDesignFor(w, r, u)
	if !ok {
		return
	}
	if !a.Settings.ServerImageExportEnabled {
		writeDRF(w, http.StatusNotFound, "server_image_export_disabled",
			"Server-side image export is not enabled on this server.")
		return
	}
	var body struct {
		Page          *json.Number `json:"page"`
		Format        string       `json:"format"`
		Quality       *int         `json:"quality"`
		Scale         *float64     `json:"scale"`
		Transparent   bool         `json:"transparent"`
		FilenameStyle string       `json:"filename_style"`
		FilenameTS    *bool        `json:"filename_timestamp"`
	}
	if err := jsonBody(r, &body); err != nil {
		writeDRF(w, http.StatusBadRequest, "parse_error", "Malformed request body.")
		return
	}
	var fields []FieldError

	format := strings.ToLower(body.Format)
	if format == "" {
		format = "png"
	}
	if format == "jpeg" {
		format = "jpg"
	}
	if format != "png" && format != "jpg" && format != "webp" {
		fields = append(fields, SimpleField("format",
			fmt.Sprintf("%q is not a valid choice.", body.Format), "invalid_choice"))
	}
	quality := 90
	if body.Quality != nil {
		quality = *body.Quality
		if quality > 100 {
			fields = append(fields, SimpleField("quality",
				"Ensure this value is less than or equal to 100.", "max_value"))
		} else if quality < 1 {
			fields = append(fields, SimpleField("quality",
				"Ensure this value is greater than or equal to 1.", "min_value"))
		}
	}
	scale := 1.0
	if body.Scale != nil {
		scale = *body.Scale
		if scale > 4 { // FiniteFloatField min/max: DRF's own messages
			fields = append(fields, SimpleField("scale",
				"Ensure this value is less than or equal to 4.", "max_value"))
		} else if scale < 0.1 {
			fields = append(fields, SimpleField("scale",
				"Ensure this value is greater than or equal to 0.1.", "min_value"))
		}
	}
	if body.FilenameStyle != "" && body.FilenameStyle != "simple" && body.FilenameStyle != "structured" {
		fields = append(fields, SimpleField("filename_style",
			fmt.Sprintf("%q is not a valid choice.", body.FilenameStyle), "invalid_choice"))
	}
	if len(fields) > 0 {
		writeDRFValidation(w, fields)
		return
	}
	transparent := body.Transparent && format != "jpg"

	// page_index: numbered from 1 (services.page_index).
	var pageIndex any
	if body.Page != nil {
		if i, ok := intVal(*body.Page); ok {
			if i < 1 {
				writeDRF(w, http.StatusBadRequest, "page_not_found", "Pages are numbered from 1.")
				return
			}
			pageIndex = i - 1 // 0-based into the document
		} else {
			pageIndex = body.Page.String() // a page id
		}
	}

	// document + size/pixel guards (services.export_image).
	doc, err := parseJSONValue([]byte(d.Document))
	if err != nil {
		writeDRF(w, http.StatusBadRequest, "invalid_document", "The design has no pages.")
		return
	}
	docMap, _ := doc.(map[string]any)
	if docMap == nil {
		writeDRF(w, http.StatusBadRequest, "invalid_document", "The design has no pages.")
		return
	}
	if len(encodeCompactJSON(docMap)) > a.Settings.ExportImageMaxDocBytes {
		writeDRF(w, http.StatusRequestEntityTooLarge, "payload_too_large",
			"The design is too large to export as an image on the server.")
		return
	}
	pageObj, index, perr := pickPage(docMap, pageIndex)
	if perr != nil {
		writeDRF(w, perr.Status, perr.Code, perr.Message)
		return
	}
	width, height, serr := pageSizeOf(docMap, pageObj)
	if serr != nil {
		writeDRF(w, serr.Status, serr.Code, serr.Message)
		return
	}
	if int64(float64(roundF(width*scale))*float64(roundF(height*scale))) > int64(a.Settings.ExportImageMaxPixels) {
		writeDRF(w, http.StatusBadRequest, "too_large",
			fmt.Sprintf("%d x %d px is more than this server exports (%d pixels); use a smaller scale.",
				roundF(width*scale), roundF(height*scale), a.Settings.ExportImageMaxPixels))
		return
	}

	// font_manifest(page): owner's uploaded fonts win, else the catalogue
	// (fonts API shape). Both queries are ports of the source.
	payload := map[string]any{
		"document": docMap, "page": index, "multiplier": scale,
		"transparent": transparent, "fonts": a.fontManifest(docMap, u.ID),
	}
	result, derr := a.renderImagePayload(payload)
	if derr != nil {
		writeDRF(w, derr.Status, derr.Code, derr.Message)
		return
	}
	converted, cerr := convertImage(result.Data, format, quality, transparent)
	if cerr != nil {
		writeDRF(w, 500, "export_failed", "The PDF could not be rendered.")
		return
	}
	preset, _ := docMap["meta"].(map[string]any)
	presetID, _ := preset["presetId"].(string)
	if presetID == "" {
		presetID = d.Preset
	}
	ires := &imageExportResult{
		Data: converted, Format: format, MIME: imageMIME(format),
		Width: result.Width, Height: result.Height,
		PageID: result.PageID, PageIndex: result.PageIndex,
		PageWidth: width, PageHeight: height,
		PageCount: len(asAnySlice(docMap["pages"])), Preset: presetID,
	}
	style := body.FilenameStyle
	if style == "" {
		style = a.Settings.ExportFilenameStyle
	}
	var unix *int
	if body.FilenameTS != nil && *body.FilenameTS {
		v := int(time.Now().Unix())
		unix = &v
	} else if body.FilenameTS == nil && a.Settings.ExportFilenameTS {
		v := int(time.Now().Unix())
		unix = &v
	}
	w.Header().Set("X-Doory-Placeholders", itoa(result.Placeholders))
	w.Header().Set("Content-Type", imageMIME(format))
	w.Header().Set("Content-Disposition", contentDisposition(a.pageFilename(d.Title, ires, style, unix), true))
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Length", itoa(len(converted)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(converted)
}

func imageMIME(format string) string {
	return map[string]string{"png": "image/png", "jpg": "image/jpeg", "webp": "image/webp"}[format]
}

// convertImage ports services._convert: PNG as drawn; JPG flattened on
// white. WEBP needs a pure-Go encoder — pending (PORT_MAP note); such a
// request answers the family's loud-pending error instead of diverging.
func convertImage(pngData []byte, format string, quality int, transparent bool) ([]byte, error) {
	switch format {
	case "png":
		return pngData, nil
	case "jpg":
		img, _, err := image.Decode(bytes.NewReader(pngData))
		if err != nil {
			return nil, err
		}
		var out bytes.Buffer
		if err := jpeg.Encode(&out, flattenWhite(img), &jpeg.Options{Quality: quality}); err != nil {
			return nil, err
		}
		return out.Bytes(), nil
	default:
		// No pure-Go WebP ENCODER exists; the request path is loud-pending
		// (PORT_MAP exports row) rather than silently different.
		return nil, fmt.Errorf("webp export pending (no pure-Go encoder)")
	}
}

// flattenWhite ports the JPG path of services._convert: composite over
// opaque white, drop alpha.
func flattenWhite(img image.Image) image.Image {
	bounds := img.Bounds()
	out := image.NewRGBA(bounds)
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			r, g, b, a := img.At(x, y).RGBA() // 16-bit precision channels
			if a != 0xffff {
				alpha := uint32(a)
				r = (r*alpha + 0xffff*(0xffff-alpha)) / 0xffff
				g = (g*alpha + 0xffff*(0xffff-alpha)) / 0xffff
				b = (b*alpha + 0xffff*(0xffff-alpha)) / 0xffff
			}
			out.Set(x, y, color.RGBA{uint8(r >> 8), uint8(g >> 8), uint8(b >> 8), 0xff})
		}
	}
	return out
}

// fontManifest ports services.font_manifest: the OWNER's uploaded families
// the page names (they win, as in the editor) else the public catalogue's,
// in the fonts API's shape.
func (a *App) fontManifest(page map[string]any, ownerID string) []any {
	single := map[string]any{"pages": []any{page}}
	wanted := fontNamesInDocument(single)
	if len(wanted) == 0 {
		return []any{}
	}
	wantedFolded := map[string]bool{}
	for _, w := range wanted {
		wantedFolded[strings.ToLower(w)] = true
	}
	// owner's uploaded families first
	rows, err := a.DB.Query(
		`SELECT `+familyCols+` FROM fonts_fontfamily WHERE owner_id = ? ORDER BY family, id`, ownerID)
	if err != nil {
		return []any{}
	}
	taken := map[string]bool{}
	out := []any{}
	appendFamily := func(f fontFamilyRow) {
		variants, _ := a.variantsOf([]string{f.ID})
		out = append(out, f.envelope(variants[f.ID]))
	}
	for rows.Next() {
		f, err := scanFamily(rows)
		if err != nil {
			rows.Close()
			return out
		}
		if wantedFolded[strings.ToLower(f.Family)] {
			taken[strings.ToLower(f.Family)] = true
			appendFamily(f)
		}
	}
	rows.Close()
	// then the catalogue's (owner-less, published)
	rows, err = a.DB.Query(
		`SELECT ` + familyCols + ` FROM fonts_fontfamily WHERE owner_id IS NULL AND is_published = 1
		 ORDER BY family, id`)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		f, err := scanFamily(rows)
		if err != nil {
			return out
		}
		key := strings.ToLower(f.Family)
		if wantedFolded[key] && !taken[key] {
			appendFamily(f)
		}
	}
	return out
}

// pickPage ports services._pick_page.
func pickPage(doc map[string]any, page any) (map[string]any, int, *DocumentError) {
	pages := asAnySlice(doc["pages"])
	if len(pages) == 0 {
		return nil, 0, &DocumentError{Code: "invalid_document", Status: 400,
			Message: "The design has no pages."}
	}
	index := 0
	switch v := page.(type) {
	case nil:
		index = 0
	case int64:
		index = int(v)
	case string:
		found := -1
		for i, raw := range pages {
			if obj, ok := raw.(map[string]any); ok {
				if id, _ := obj["id"].(string); id == v {
					found = i
				}
			}
		}
		if found < 0 {
			return nil, 0, &DocumentError{Code: "page_not_found", Status: 400,
				Message: fmt.Sprintf("The design has no page %q.", v)}
		}
		index = found
	default:
		return nil, 0, &DocumentError{Code: "page_not_found", Status: 400, Message: "The design has no page."}
	}
	if index < 0 || index >= len(pages) {
		return nil, 0, &DocumentError{Code: "page_not_found", Status: 400,
			Message: fmt.Sprintf("The design has no page %v.", page)}
	}
	obj, ok := pages[index].(map[string]any)
	if !ok {
		return nil, 0, &DocumentError{Code: "page_not_found", Status: 400, Message: "The design has no page."}
	}
	return obj, index, nil
}

// pageSizeOf ports services._page_size (page size or design size).
func pageSizeOf(doc, page map[string]any) (float64, float64, *DocumentError) {
	width := numberOr(page["width"], doc["width"])
	height := numberOr(page["height"], doc["height"])
	if width <= 0 || height <= 0 {
		return 0, 0, &DocumentError{Code: "invalid_document", Status: 400,
			Message: "The page has no size."}
	}
	return width, height, nil
}

func numberOr(a, b any) float64 {
	if v, ok := asNumber(a); ok {
		return v
	}
	v, _ := asNumber(b)
	return v
}

func roundF(v float64) int {
	if v >= 0 {
		return int(v + 0.5)
	}
	return int(v - 0.5)
}

func asAnySlice(v any) []any {
	s, _ := v.([]any)
	return s
}

// imageDownload ports ImageDownloadView: the signed link is the only way
// in; forged/expired/gone ->404 without saying why.
func (a *App) imageDownload(w http.ResponseWriter, r *http.Request) {
	name, mime, ok := a.openLink(routeParam(r, "token"))
	if !ok {
		// Http404 in a DRF view -> NotFound default (compact envelope).
		writeDRF(w, http.StatusNotFound, "not_found", "Not found.")
		return
	}
	data, err := os.ReadFile(filepath.Join(a.exportStorageRoot(), name))
	if err != nil {
		writeDRF(w, http.StatusNotFound, "not_found", "Not found.")
		return
	}
	w.Header().Set("Content-Type", mime)
	w.Header().Set("Content-Disposition", contentDisposition(name, false))
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Length", itoa(len(data)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}
