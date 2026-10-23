// Export rendering: page geometry + the in-memory HTML document
// (apps/exports/layout.py), the render-slot semaphore, font embedding
// (apps/exports/fonts.py) and the worker subprocess model
// (apps/exports/renderer.py — subprocess with env allowlist, deadline,
// kill-group semantics; the worker itself is `doory render-worker`,
// internal/renderworker + internal/chromium).
//
// Ports backend/apps/exports/layout.py
// Ports backend/apps/exports/renderer.py (slots + worker model)
// Ports backend/apps/exports/fonts.py
package doory

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"doory-go/internal/svg"
)

const (
	pxPerMM             = 96 / 25.4
	markGapMM           = 1.0
	markLengthMM        = 5.0
	markStrokePX        = 0.5
	sheetAllowancePX    = 4.0 / 3.0
	maxWorkdirChars     = 60
	maxRenderAssetBytes = 10 * 1024 * 1024
	killGraceSeconds    = 3
)

// PageGeometry mirrors layout.PageGeometry (CSS px).
type PageGeometry struct {
	Width  float64
	Height float64
	Bleed  float64
	Slug   float64
}

// NewPageGeometry ports PageGeometry.create.
func NewPageGeometry(width, height, bleedMM float64, cropMarks bool) PageGeometry {
	slug := 0.0
	if cropMarks {
		slug = (markGapMM + markLengthMM) * pxPerMM
	}
	return PageGeometry{Width: width, Height: height, Bleed: bleedMM * pxPerMM, Slug: slug}
}

func (g PageGeometry) margin() float64     { return g.Bleed + g.Slug }
func (g PageGeometry) pageWidth() float64  { return g.Width + 2*g.margin() }
func (g PageGeometry) pageHeight() float64 { return g.Height + 2*g.margin() }
func (g PageGeometry) sheetWidth() float64 {
	return g.pageWidth() + sheetAllowancePX
}
func (g PageGeometry) sheetHeight() float64 {
	return g.pageHeight() + sheetAllowancePX
}
func (g PageGeometry) artWidth() float64  { return g.Width + 2*g.Bleed }
func (g PageGeometry) artHeight() float64 { return g.Height + 2*g.Bleed }
func (g PageGeometry) hasMarks() bool     { return g.Slug > 0 }

// px formats like layout.fmt.
func px(v float64) string { return svgPX(v) }

// svgPX delegates (shared formatting with the svg package).
func svgPX(v float64) string {
	s := fmt.Sprintf("%.4f", v)
	s = strings.TrimRight(s, "0")
	s = strings.TrimRight(s, ".")
	if s == "" || s == "-" {
		return "0"
	}
	return s
}

// CropMarksSVG ports crop_marks_svg: eight hairlines in the slug.
func CropMarksSVG(g PageGeometry) string {
	inner := g.Bleed + markGapMM*pxPerMM
	outer := inner + markLengthMM*pxPerMM
	left, top := g.margin(), g.margin()
	right, bottom := left+g.Width, top+g.Height
	var segments []string
	for _, corner := range []struct{ cx, sx, cy, sy float64 }{
		{left, -1, top, -1}, {right, 1, top, -1},
		{left, -1, bottom, 1}, {right, 1, bottom, 1},
	} {
		segments = append(segments,
			fmt.Sprintf("M%s %sL%s %s", px(corner.cx+corner.sx*inner), px(corner.cy),
				px(corner.cx+corner.sx*outer), px(corner.cy)))
		segments = append(segments,
			fmt.Sprintf("M%s %sL%s %s", px(corner.cx), px(corner.cy+corner.sy*inner),
				px(corner.cx), px(corner.cy+corner.sy*outer)))
	}
	width, height := px(g.pageWidth()), px(g.pageHeight())
	return `<svg class="marks" xmlns="http://www.w3.org/2000/svg" width="` + width +
		`" height="` + height + `" viewBox="0 0 ` + width + ` ` + height + `"><path d="` +
		strings.Join(segments, "") + `" fill="none" stroke="#000" stroke-width="` + px(markStrokePX) + `"/></svg>`
}

// EmbeddedFont mirrors layout.EmbeddedFont.
type EmbeddedFont struct {
	Family string
	Weight int
	Style  string
	MIME   string
	Data   []byte
}

// fontFaceCSS ports font_face_css (@font-face data: inlining).
func fontFaceCSS(fonts []EmbeddedFont) string {
	var b strings.Builder
	for _, f := range fonts {
		family := fontFamilyUnsafe(f.Family)
		payload := base64.StdEncoding.EncodeToString(f.Data)
		fmt.Fprintf(&b, `@font-face{font-family:"%s";font-weight:%d;font-style:%s;src:url("data:%s;base64,%s")}`,
			family, f.Weight, f.Style, f.MIME, payload)
	}
	return b.String()
}

var familyUnsafe = map[rune]bool{}

func fontFamilyUnsafe(name string) string {
	var b strings.Builder
	for _, r := range name {
		if r < 128 && ((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') ||
			r == '_' || r == '.' || r == '-' || r == ' ') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// buildDocumentHTML ports layout.build_document_html: one section per page,
// declarative shadow roots, named pages for per-page sizes, first-page
// @page default, sheet allowance.
func buildDocumentHTML(svgs []string, geometries []PageGeometry, title string, fonts []EmbeddedFont, lang string) string {
	if len(geometries) == 0 {
		geometries = make([]PageGeometry, len(svgs))
	}
	first := geometries[0]
	names := map[PageGeometry]string{}
	var order []PageGeometry
	for _, g := range geometries {
		if _, ok := names[g]; !ok {
			names[g] = fmt.Sprintf("p%d", len(names))
			order = append(order, g)
		}
	}
	named := len(names) > 1
	var css strings.Builder
	fmt.Fprintf(&css, "@page{size:%spx %spx;margin:0}", px(first.sheetWidth()), px(first.sheetHeight()))
	css.WriteString("html,body{margin:0;padding:0;background:#fff}")
	css.WriteString(".page{position:relative;overflow:hidden;break-after:page;break-inside:avoid}")
	css.WriteString(".page:last-child{break-after:auto}")
	css.WriteString(".art{position:absolute;overflow:hidden}")
	css.WriteString(".marks{position:absolute;left:0;top:0;display:block}")
	for _, g := range order {
		name := names[g]
		if named {
			fmt.Fprintf(&css, "@page %s{size:%spx %spx;margin:0}", name, px(g.sheetWidth()), px(g.sheetHeight()))
		}
		selector := "." + name
		pageName := ""
		if named {
			pageName = "page:" + name + ";"
		}
		fmt.Fprintf(&css, "%s{%swidth:%spx;height:%spx}", selector, pageName, px(g.pageWidth()), px(g.pageHeight()))
		fmt.Fprintf(&css, "%s .art{left:%spx;top:%spx;width:%spx;height:%spx}",
			selector, px(g.Slug), px(g.Slug), px(g.artWidth()), px(g.artHeight()))
	}
	var body strings.Builder
	for i, s := range svgs {
		g := geometries[i]
		fmt.Fprintf(&body, `<section class="page %s"><div class="art"><template shadowrootmode="open">`, names[g])
		body.WriteString("<style>:host{display:block}svg{display:block}</style>")
		body.WriteString(s)
		body.WriteString("</template></div>")
		if g.hasMarks() {
			body.WriteString(CropMarksSVG(g))
		}
		body.WriteString("</section>")
	}
	return fmt.Sprintf(`<!doctype html><html lang="%s"><head><meta charset="utf-8"><title>%s</title><style>%s%s</style></head><body>%s</body></html>`,
		html.EscapeString(lang), html.EscapeString(title), fontFaceCSS(fonts), css.String(), body.String())
}

// ---- render slots + worker model (renderer.py) ----

var (
	renderSlots   chan struct{}
	renderSlotsMu sync.Mutex
	busyOwners    = map[string]bool{}
)

// exportBusy is ExportBusy:503 with Retry-After 5.
var exportBusy = &DocumentError{Code: "export_busy", Status: 503,
	Message: "The export service is busy. Try again in a few seconds."}

// acquireRenderSlot ports render_slot: EXPORT_MAX_CONCURRENCY slots; one
// render per owner at a time. Returns false when busy.
func (a *App) acquireRenderSlot(owner string) bool {
	renderSlotsMu.Lock()
	defer renderSlotsMu.Unlock()
	if renderSlots == nil || cap(renderSlots) != a.Settings.ExportMaxConcurrency {
		renderSlots = make(chan struct{}, maxInt(1, a.Settings.ExportMaxConcurrency))
	}
	if owner != "" && busyOwners[owner] {
		return false
	}
	select {
	case renderSlots <- struct{}{}:
		if owner != "" {
			busyOwners[owner] = true
		}
		return true
	default:
		return false
	}
}

func (a *App) releaseRenderSlot(owner string) {
	renderSlotsMu.Lock()
	defer renderSlotsMu.Unlock()
	delete(busyOwners, owner)
	if renderSlots != nil {
		<-renderSlots
	}
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// renderResult mirrors renderer.RenderResult (report + output file).
type renderResult struct {
	Data         []byte
	AllowedTotal int
	BlockedTotal int
	Width        int
	Height       int
	PageID       string
	PageIndex    int
	Placeholders int
	MissingFonts []string
}

// workerReport mirrors the worker's report JSON contract.
type workerReport struct {
	Status  string `json:"status"`
	Kind    string `json:"kind"`
	Message string `json:"message"`
	Error   *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
	Blocked []struct {
		URL    string `json:"url"`
		Reason string `json:"reason"`
	} `json:"blocked"`
	BlockedTotal int      `json:"blocked_total"`
	AllowedTotal int      `json:"allowed_total"`
	FontsReady   *bool    `json:"fonts_ready"`
	Width        int      `json:"width"`
	Height       int      `json:"height"`
	PageID       string   `json:"page_id"`
	PageIndex    int      `json:"page_index"`
	Placeholders int      `json:"placeholders"`
	MissingFonts []string `json:"missing_fonts"`
}

// runWorker ports renderer._run_worker: spec + subprocess + deadline +
// report mapping. `prepare` writes the spec fields unique to the mode.
func (a *App) runWorker(mime string, prepare func(workdir string) (map[string]any, string, error)) (*renderResult, *DocumentError) {
	timeout := time.Duration(a.Settings.ExportTimeoutSeconds * float64(time.Second))
	root := a.Settings.ExportWorkDir
	if root != "" {
		if err := os.MkdirAll(root, 0o755); err != nil {
			return nil, &DocumentError{Code: "export_unavailable", Status: 503,
				Message: "The PDF renderer is not available on this server."}
		}
	}
	wd, err := os.MkdirTemp(root, "doory-export-")
	if err != nil {
		return nil, &DocumentError{Code: "export_unavailable", Status: 503,
			Message: "The PDF renderer is not available on this server."}
	}
	defer os.RemoveAll(wd)
	if len(wd) > maxWorkdirChars {
		// Chromium keeps a unix socket below TMPDIR; long paths break it.
		return nil, &DocumentError{Code: "export_unavailable", Status: 503,
			Message: "The PDF renderer is not available on this server."}
	}
	special, outputPath, perr := prepare(wd)
	if perr != nil {
		return nil, &DocumentError{Code: "export_failed", Status: 500, Message: "The PDF could not be rendered."}
	}
	reportPath := filepath.Join(wd, "report.json")
	spec := map[string]any{
		"report_path":     reportPath,
		"executable_path": a.Settings.ExportChromiumPath,
		"no_sandbox":      a.Settings.ExportChromiumNoSandbox,
		"allowed_origins": a.Settings.ExportAllowedOrigins,
		"path_prefixes":   []string{"/media/", "/static/"},
		"timeout_s":       a.Settings.ExportTimeoutSeconds,
		"max_asset_bytes": maxRenderAssetBytes,
	}
	for k, v := range special {
		spec[k] = v
	}
	specJSON, _ := json.Marshal(spec)
	if err := os.WriteFile(filepath.Join(wd, "spec.json"), specJSON, 0o644); err != nil {
		return nil, &DocumentError{Code: "export_failed", Status: 500, Message: "The PDF could not be rendered."}
	}

	self, err := os.Executable()
	if err != nil {
		return nil, &DocumentError{Code: "export_unavailable", Status: 503,
			Message: "The PDF renderer is not available on this server."}
	}
	cmd := exec.Command(self, "render-worker", filepath.Join(wd, "spec.json"))
	// env allowlist like renderer._worker_env: no application secrets.
	cmd.Env = allowlistedEnv(wd)
	logFile, _ := os.Create(filepath.Join(wd, "worker.log"))
	if logFile != nil {
		cmd.Stdout = logFile
		cmd.Stderr = logFile
		defer logFile.Close()
	}
	cmd.SysProcAttr = sysProcAttrSetsid()
	if err := cmd.Start(); err != nil {
		return nil, &DocumentError{Code: "export_unavailable", Status: 503,
			Message: "The PDF renderer is not available on this server."}
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-time.After(timeout + killGraceSeconds*time.Second):
		killProcessGroup(cmd)
		return nil, &DocumentError{Code: "export_timeout", Status: 504,
			Message: "The export took too long and was cancelled."}
	case <-done:
	}

	rawReport, err := os.ReadFile(reportPath)
	if err != nil {
		return nil, &DocumentError{Code: "export_failed", Status: 500, Message: "The PDF could not be rendered."}
	}
	var report workerReport
	if err := json.Unmarshal(rawReport, &report); err != nil {
		return nil, &DocumentError{Code: "export_failed", Status: 500, Message: "The PDF could not be rendered."}
	}
	if report.Status != "ok" {
		switch report.Kind {
		case "timeout":
			return nil, &DocumentError{Code: "export_timeout", Status: 504,
				Message: "The export took too long and was cancelled."}
		case "unavailable":
			return nil, &DocumentError{Code: "export_unavailable", Status: 503,
				Message: "The PDF renderer is not available on this server."}
		case "render":
			if report.Error != nil && renderPageInputErrors[report.Error.Code] {
				return nil, &DocumentError{Code: report.Error.Code, Status: 400,
					Message: orDefault(report.Error.Message, "The design cannot be drawn.")}
			}
		}
		return nil, &DocumentError{Code: "export_failed", Status: 500, Message: "The PDF could not be rendered."}
	}
	data, err := os.ReadFile(outputPath)
	if err != nil {
		return nil, &DocumentError{Code: "export_failed", Status: 500, Message: "The PDF could not be rendered."}
	}
	return &renderResult{
		Data: data, AllowedTotal: report.AllowedTotal, BlockedTotal: report.BlockedTotal,
		Width: report.Width, Height: report.Height, PageID: report.PageID,
		PageIndex: report.PageIndex, Placeholders: report.Placeholders,
		MissingFonts: report.MissingFonts,
	}, nil
}

// renderPageInputErrors: worker render errors that describe the request.
var renderPageInputErrors = map[string]bool{
	"invalid_document": true, "page_not_found": true, "invalid_options": true,
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// renderPDF ports services.export_pdf's render half: html in, PDF out.
func (a *App) renderPDF(html string, widthPx, heightPx float64) (*renderResult, *DocumentError) {
	return a.runWorker("application/pdf", func(workdir string) (map[string]any, string, error) {
		htmlPath := filepath.Join(workdir, "input.html")
		pdfPath := filepath.Join(workdir, "output.pdf")
		if err := os.WriteFile(htmlPath, []byte(html), 0o644); err != nil {
			return nil, "", err
		}
		return map[string]any{
			"html_path": htmlPath, "pdf_path": pdfPath,
			"width_px": widthPx, "height_px": heightPx,
		}, pdfPath, nil
	})
}

// renderImagePayload ports services.export_image's render half (the worker
// drives the frontend render page; the payload reaches it through the
// RENDER_IMAGE evaluation).
func (a *App) renderImagePayload(payload map[string]any) (*renderResult, *DocumentError) {
	if a.Settings.ExportRenderPageURL == "" {
		return nil, &DocumentError{Code: "export_unavailable", Status: 503,
			Message: "The PDF renderer is not available on this server."}
	}
	return a.runWorker("image/png", func(workdir string) (map[string]any, string, error) {
		payloadPath := filepath.Join(workdir, "payload.json")
		pngPath := filepath.Join(workdir, "output.png")
		data, err := json.Marshal(payload)
		if err != nil {
			return nil, "", err
		}
		if err := os.WriteFile(payloadPath, data, 0o644); err != nil {
			return nil, "", err
		}
		return map[string]any{
			"mode": "image", "render_url": a.Settings.ExportRenderPageURL,
			"payload_path": payloadPath, "png_path": pngPath,
		}, pngPath, nil
	})
}

// preparePages ports services.sanitize_pages: every page through the
// shared SVG sanitiser (allow_links=True for pages); one unusable page
// rejects the request naming it.
func preparePages(pages [][]byte) ([]string, *DocumentError) {
	out := make([]string, 0, len(pages))
	for i, data := range pages {
		clean, err := svg.Sanitize(data, true)
		if err != nil {
			return nil, &DocumentError{Code: "invalid_svg", Status: 400,
				Message: fmt.Sprintf("Page %d is not a valid, safe SVG.", i+1)}
		}
		out = append(out, string(clean))
	}
	return out, nil
}

// loadExportFonts ports apps/exports/fonts.py load_fonts: /media/ URLs the
// server inlines from its own storage (sniffed content; size caps).
func (a *App) loadExportFonts(specs []fontSpec) ([]EmbeddedFont, *DocumentError) {
	if len(specs) > 32 {
		return nil, &DocumentError{Code: "invalid_font", Status: 400,
			Message: "At most 32 font files can be embedded."}
	}
	cache := map[string][]byte{}
	var total int
	out := []EmbeddedFont{}
	for _, spec := range specs {
		name, err := a.mediaStorageName(spec.URL)
		if err != nil {
			return nil, err
		}
		data, ok := cache[name]
		if !ok {
			raw, rerr := os.ReadFile(filepath.Join(a.Settings.MediaRoot, name))
			if rerr != nil || len(raw) == 0 {
				return nil, &DocumentError{Code: "invalid_font", Status: 400,
					Message: "A requested font file does not exist."}
			}
			if len(raw) > 8*1024*1024 {
				return nil, &DocumentError{Code: "invalid_font", Status: 400,
					Message: "A requested font file is too large."}
			}
			mime := fontMIME(raw)
			if mime == "" {
				return nil, &DocumentError{Code: "invalid_font", Status: 400,
					Message: "A requested font file is not a font."}
			}
			data = raw
			cache[name] = raw
			total += len(raw)
			if total > 24*1024*1024 {
				return nil, &DocumentError{Code: "invalid_font", Status: 400,
					Message: "The selected fonts are too large to embed."}
			}
		}
		mime := fontMIME(data)
		out = append(out, EmbeddedFont{Family: spec.Family, Weight: spec.Weight, Style: spec.Style,
			MIME: mime, Data: data})
	}
	return out, nil
}

type fontSpec struct {
	Family string
	Weight int
	Style  string
	URL    string
}

// fontMIME ports fonts.font_mime (content sniffing).
func fontMIME(head []byte) string {
	signatures := []struct {
		magic string
		mime  string
	}{
		{"wOF2", "font/woff2"}, {"wOFF", "font/woff"}, {"OTTO", "font/otf"},
		{"true", "font/ttf"},
	}
	for _, s := range signatures {
		if len(head) >= len(s.magic) && string(head[:len(s.magic)]) == s.magic {
			return s.mime
		}
	}
	if len(head) >= 4 && head[0] == 0x00 && head[1] == 0x01 && head[2] == 0x00 && head[3] == 0x00 {
		return "font/ttf"
	}
	return ""
}

// mediaStorageName ports fonts.media_storage_name: /media/... -> storage
// name (relative to MEDIA_ROOT), refusing traversal and non-media origins.
func (a *App) mediaStorageName(rawURL string) (string, *DocumentError) {
	if strings.Contains(rawURL, "://") {
		if !strings.HasPrefix(rawURL, originOf(a.Settings.SiteURL)+mediaURL) &&
			!allowedExportOrigin(rawURL, a.Settings.ExportAllowedOrigins) {
			return "", &DocumentError{Code: "invalid_font", Status: 400,
				Message: "Fonts must be served from this site's media storage."}
		}
	}
	if !strings.HasPrefix(rawURL, mediaURL) {
		return "", &DocumentError{Code: "invalid_font", Status: 400,
			Message: "Fonts must be served from this site's media storage."}
	}
	name := strings.TrimPrefix(rawURL, mediaURL)
	if name == "" || strings.Contains(name, "\\") || strings.Contains(name, "\x00") ||
		filepath.Clean(name) != name || strings.HasPrefix(name, "/") || strings.HasPrefix(name, "..") {
		return "", &DocumentError{Code: "invalid_font", Status: 400, Message: "Invalid font path."}
	}
	return name, nil
}

func allowedExportOrigin(rawURL string, allowed []string) bool {
	for _, o := range allowed {
		if strings.HasPrefix(rawURL, strings.TrimSuffix(o, "/")+"/") {
			return true
		}
	}
	return false
}

// envAllowlist mirrors renderer._worker_env: no application secrets reach
// the worker.
func allowlistedEnv(workdir string) []string {
	allow := map[string]bool{
		"PATH": true, "HOME": true, "LANG": true, "LC_ALL": true,
		"PLAYWRIGHT_BROWSERS_PATH": true, "PLAYWRIGHT_NODEJS_PATH": true,
		"FONTCONFIG_PATH": true, "FONTCONFIG_FILE": true,
		"XDG_CACHE_HOME": true, "XDG_CONFIG_HOME": true, "XDG_DATA_HOME": true,
		"TMPDIR": true,
	}
	var out []string
	for _, kv := range os.Environ() {
		key := kv[:strings.IndexByte(kv, '=')]
		if allow[key] {
			out = append(out, kv)
		}
	}
	out = append(out, "TMPDIR="+workdir)
	return out
}
