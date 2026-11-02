// Package renderworker runs one export render like apps/exports/
// chromium_worker.py: a locked-down browser, the request policy, one work
// item (pdf or image) and the report JSON contract. Invoked as
// `doory render-worker <spec.json>` in its own process group — killed
// wholesale when the deadline passes (renderer.py's subprocess model).
package renderworker

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"doory-go/internal/chromium"
	"doory-go/internal/renderpolicy"
)

// Spec mirrors the python spec dict.
type Spec struct {
	Mode           string   `json:"mode"`
	HTMLPath       string   `json:"html_path,omitempty"`
	PDFPath        string   `json:"pdf_path,omitempty"`
	WidthPx        float64  `json:"width_px,omitempty"`
	HeightPx       float64  `json:"height_px,omitempty"`
	RenderURL      string   `json:"render_url,omitempty"`
	PayloadPath    string   `json:"payload_path,omitempty"`
	PNGPath        string   `json:"png_path,omitempty"`
	VideoPath      string   `json:"video_path,omitempty"`
	MaxVideoBytes  int      `json:"max_video_bytes,omitempty"`
	ReportPath     string   `json:"report_path"`
	ExecutablePath string   `json:"executable_path"`
	NoSandbox      bool     `json:"no_sandbox"`
	AllowedOrigins []string `json:"allowed_origins"`
	PathPrefixes   []string `json:"path_prefixes"`
	TimeoutSeconds float64  `json:"timeout_s"`
	MaxAssetBytes  int      `json:"max_asset_bytes"`
}

// Blocked mirrors BlockedRequest in the report.
type Blocked struct {
	URL    string `json:"url"`
	Reason string `json:"reason"`
}

// Report mirrors the python report dict.
type Report struct {
	Status       string    `json:"status"`
	Kind         string    `json:"kind,omitempty"`
	Message      string    `json:"message,omitempty"`
	Error        *repError `json:"error,omitempty"`
	Blocked      []Blocked `json:"blocked"`
	BlockedTotal int       `json:"blocked_total"`
	AllowedTotal int       `json:"allowed_total"`
	FontsReady   *bool     `json:"fonts_ready,omitempty"`
	Width        int       `json:"width,omitempty"`
	Height       int       `json:"height,omitempty"`
	PageID       string    `json:"page_id,omitempty"`
	PageIndex    int       `json:"page_index,omitempty"`
	Placeholders int       `json:"placeholders,omitempty"`
	MissingFonts []string  `json:"missing_fonts,omitempty"`
	Mime         string    `json:"mime,omitempty"`
	Bytes        int       `json:"bytes,omitempty"`
	FPS          int       `json:"fps,omitempty"`
	Frames       int       `json:"frames,omitempty"`
	DurationMS   int       `json:"duration_ms,omitempty"`
	Video        string    `json:"video,omitempty"`
	Audio        string    `json:"audio,omitempty"`
	Warnings     []string  `json:"warnings,omitempty"`
	PageIDs      []string  `json:"page_ids,omitempty"`
	PageIndexes  []int     `json:"page_indexes,omitempty"`
}

type repError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

const maxRecordedBlocked = 50

// Run executes the spec and returns the report.
func Run(specPath string) (*Report, error) {
	raw, err := os.ReadFile(specPath)
	if err != nil {
		return nil, err
	}
	var spec Spec
	if err := json.Unmarshal(raw, &spec); err != nil {
		return nil, err
	}
	report := &Report{Status: "ok", Blocked: []Blocked{}}
	if err := run(spec, report); err != nil {
		_ = err // run() fills the report's error kinds itself
	}
	return report, nil
}

type policyFetch struct {
	policy   *renderpolicy.Policy
	maxBytes int
}

func run(spec Spec, report *Report) error {
	if spec.Mode == "video" {
		spec.RenderURL = videoSecureURL(spec.RenderURL)
	}
	workdir := filepath.Dir(spec.ReportPath)
	deadline := time.Now().Add(time.Duration(spec.TimeoutSeconds * float64(time.Second)))
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()

	policy, err := renderpolicy.Build(spec.AllowedOrigins, spec.PathPrefixes, spec.RenderURL)
	if err != nil {
		report.Status, report.Kind = "error", "failed"
		report.Message = err.Error()[:min(len(err.Error()), 500)]
		return err
	}

	executable, err := chromium.FindExecutable(spec.ExecutablePath)
	if err != nil {
		report.Status, report.Kind = "error", "unavailable"
		report.Message = err.Error()
		return err
	}
	browser, err := chromium.Launch(ctx, executable, spec.NoSandbox, workdir)
	if err != nil {
		report.Status, report.Kind = "error", "unavailable"
		report.Message = truncate(err.Error())
		return err
	}
	defer browser.Close()

	if err := browser.AttachPage(ctx); err != nil {
		report.Status, report.Kind = "error", "failed"
		report.Message = truncate(err.Error())
		return err
	}
	// Fetch is enabled on the PAGE session AFTER attach — the navigation
	// must never race the interception setup (chromium resolves nothing:
	// LAUNCH_ARGS blackholes DNS, only fulfilled requests load).
	var inter fetchInterceptor
	inter.install(ctx, browser, policy, spec.MaxAssetBytes, report)

	switch spec.Mode {
	case "image":
		return runImage(ctx, browser, spec, report)
	case "video":
		return runVideo(ctx, browser, spec, report)
	default:
		return runPDF(ctx, browser, spec, report)
	}
}

func runPDF(ctx context.Context, browser *chromium.Browser, spec Spec, report *Report) error {
	html, err := os.ReadFile(spec.HTMLPath)
	if err != nil {
		report.Status, report.Kind = "error", "failed"
		report.Message = truncate(err.Error())
		return err
	}
	// pdf mode: JavaScript disabled for page content (playwright context
	// java_script_enabled=False); CDP disables it per-evaluation below.
	if err := browser.SetDocumentContent(ctx, string(html)); err != nil {
		report.Status, report.Kind = "error", "failed"
		report.Message = truncate(err.Error())
		return err
	}
	// WAIT_FOR_FONTS (the python worker passes it to page.evaluate WITH an
	// argument, which calls it; CDP evaluates expressions, so we invoke it
	// here with the same 10s cap).
	value, err := browser.Eval(ctx, `((timeoutMs) => Promise.race([
  document.fonts.ready.then(() => true),
  new Promise((resolve) => setTimeout(() => resolve(false), timeoutMs)),
]))(10000)`)
	if err == nil {
		ready := string(value) == "true"
		report.FontsReady = &ready
	}
	data, err := browser.PrintToPDF(ctx, spec.WidthPx, spec.HeightPx)
	if err != nil {
		report.Status, report.Kind = "error", "failed"
		report.Message = truncate(err.Error())
		return err
	}
	if err := os.WriteFile(spec.PDFPath, data, 0o644); err != nil {
		report.Status, report.Kind = "error", "failed"
		report.Message = truncate(err.Error())
		return err
	}
	return nil
}

// RENDER_READY / RENDER_IMAGE mirror the python worker's expressions.
// renderReady as an IIFE (playwright's wait_for_function calls the passed
// function; CDP evaluates expressions).
const renderReady = "(() => window.__dooryRenderReady === true && typeof window.__dooryRender === 'function')()"

const renderImage = `([input, timeoutMs]) => Promise.race([
  window.__dooryRender(input),
  new Promise((resolve) => setTimeout(
    () => resolve({ ok: false, error: { code: "timeout", message: "render deadline exceeded" } }), timeoutMs,
  )),
])`

func runImage(ctx context.Context, browser *chromium.Browser, spec Spec, report *Report) error {
	payload, err := os.ReadFile(spec.PayloadPath)
	if err != nil {
		report.Status, report.Kind = "error", "failed"
		report.Message = truncate(err.Error())
		return err
	}
	if err := browser.Navigate(ctx, spec.RenderURL); err != nil {
		report.Status, report.Kind = "error", "failed"
		report.Message = truncate(err.Error())
		return err
	}
	// wait_for_function(RENDER_READY)
	for {
		raw, err := browser.Eval(ctx, renderReady)
		if err == nil && string(raw) == "true" {
			break
		}
		if time.Now().After(deadlineOf(ctx)) {
			report.Status, report.Kind = "error", "timeout"
			report.Message = "render deadline exceeded"
			return fmt.Errorf("timeout")
		}
		time.Sleep(50 * time.Millisecond)
	}
	answer, err := browser.Eval(ctx, wrapRenderImage(payload))
	if err != nil {
		report.Status, report.Kind = "error", "failed"
		report.Message = truncate(err.Error())
		return err
	}
	var result struct {
		OK    bool `json:"ok"`
		Error *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
		PNG          string   `json:"png"`
		Width        int      `json:"width"`
		Height       int      `json:"height"`
		PageID       string   `json:"pageId"`
		PageIndex    int      `json:"pageIndex"`
		Placeholders int      `json:"placeholders"`
		MissingFonts []string `json:"missingFonts"`
	}
	if err := json.Unmarshal(answer, &result); err != nil {
		report.Status, report.Kind = "error", "failed"
		report.Message = "the render page sent no answer"
		return err
	}
	if !result.OK {
		code, message := "render_failed", ""
		if result.Error != nil {
			code, message = result.Error.Code, result.Error.Message
		}
		if code == "timeout" {
			report.Status, report.Kind = "error", "timeout"
			report.Message = message
			return fmt.Errorf("timeout")
		}
		report.Status, report.Kind = "error", "render"
		report.Error = &repError{Code: truncateCode(code), Message: truncate(message)}
		return fmt.Errorf("render: %s", code)
	}
	png, err := base64.StdEncoding.DecodeString(result.PNG)
	if err != nil {
		report.Status, report.Kind = "error", "failed"
		report.Message = "the render page sent no PNG"
		return err
	}
	if err := os.WriteFile(spec.PNGPath, png, 0o644); err != nil {
		report.Status, report.Kind = "error", "failed"
		report.Message = truncate(err.Error())
		return err
	}
	report.Width, report.Height = result.Width, result.Height
	report.PageID, report.PageIndex = result.PageID, result.PageIndex
	report.Placeholders = result.Placeholders
	for _, name := range result.MissingFonts {
		if len(report.MissingFonts) >= 64 {
			break
		}
		report.MissingFonts = append(report.MissingFonts, truncate(name))
	}
	return nil
}

// wrapRenderImage embeds the payload into the RENDER_IMAGE call.
func wrapRenderImage(payload []byte) string {
	return fmt.Sprintf(`Promise.race([
  window.__dooryRender(%s),
  new Promise((resolve) => setTimeout(
    () => resolve({ ok: false, error: { code: "timeout", message: "render deadline exceeded" } }), 30000,
  )),
])`, string(payload))
}

const videoSliceBytes = 4 * 1024 * 1024

const hasRenderVideo = "(() => typeof window.__dooryRenderVideo === 'function' && typeof window.__dooryVideoSlice === 'function')()"

func wrapRenderVideo(payload []byte, timeoutMS int) string {
	return fmt.Sprintf(`Promise.race([
  window.__dooryRenderVideo(%s),
  new Promise((resolve) => setTimeout(
    () => resolve({ ok: false, error: { code: "timeout", message: "render deadline exceeded" } }), %d,
  )),
])`, string(payload), timeoutMS)
}

func wrapVideoSlice(offset, length int) string {
	return fmt.Sprintf("window.__dooryVideoSlice(%d, %d)", offset, length)
}

// videoSecureURL keeps the render URL as configured. Loopback HTTP origins
// (the single-binary default) are already secure contexts; deployments on a
// non-loopback plain-HTTP host should expose the render page over localhost
// or HTTPS (the same constraint the frontend has for WebCodecs).
func videoSecureURL(raw string) string { return raw }

func runVideo(ctx context.Context, browser *chromium.Browser, spec Spec, report *Report) error {
	payload, err := os.ReadFile(spec.PayloadPath)
	if err != nil {
		report.Status, report.Kind = "error", "failed"
		report.Message = truncate(err.Error())
		return err
	}
	if err := browser.Navigate(ctx, spec.RenderURL); err != nil {
		report.Status, report.Kind = "error", "failed"
		report.Message = truncate(err.Error())
		return err
	}
	for {
		raw, err := browser.Eval(ctx, renderReady)
		if err == nil && string(raw) == "true" {
			break
		}
		if time.Now().After(deadlineOf(ctx)) {
			report.Status, report.Kind = "error", "timeout"
			report.Message = "render deadline exceeded"
			return fmt.Errorf("timeout")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if raw, err := browser.Eval(ctx, hasRenderVideo); err != nil || string(raw) != "true" {
		report.Status, report.Kind = "error", "unavailable"
		report.Message = "the render page cannot make videos yet"
		return fmt.Errorf("no video renderer")
	}
	left := int(time.Until(deadlineOf(ctx)) / time.Millisecond)
	if left <= 0 {
		report.Status, report.Kind = "error", "timeout"
		return fmt.Errorf("timeout")
	}
	answer, err := browser.Eval(ctx, wrapRenderVideo(payload, left))
	if err != nil {
		report.Status, report.Kind = "error", "failed"
		report.Message = truncate(err.Error())
		return err
	}
	var result struct {
		OK           bool                            `json:"ok"`
		Error        *struct{ Code, Message string } `json:"error"`
		Mime         string                          `json:"mime"`
		Bytes        int                             `json:"bytes"`
		Width        int                             `json:"width"`
		Height       int                             `json:"height"`
		FPS          int                             `json:"fps"`
		Frames       int                             `json:"frames"`
		DurationMS   int                             `json:"durationMs"`
		Video        string                          `json:"video"`
		Audio        string                          `json:"audio"`
		Warnings     []string                        `json:"warnings"`
		PageIDs      []string                        `json:"pageIds"`
		PageIndexes  []int                           `json:"pageIndexes"`
		Placeholders int                             `json:"placeholders"`
		MissingFonts []string                        `json:"missingFonts"`
	}
	if err := json.Unmarshal(answer, &result); err != nil {
		report.Status, report.Kind = "error", "failed"
		report.Message = "the render page sent no answer"
		return err
	}
	if !result.OK {
		code, message := "render_failed", ""
		if result.Error != nil {
			code, message = result.Error.Code, result.Error.Message
		}
		if code == "timeout" {
			report.Status, report.Kind = "error", "timeout"
			report.Message = message
			return fmt.Errorf("timeout")
		}
		report.Status, report.Kind = "error", "render"
		report.Error = &repError{Code: truncateCode(code), Message: truncate(message)}
		return fmt.Errorf("render: %s", code)
	}
	if result.Bytes <= 0 || (spec.MaxVideoBytes > 0 && result.Bytes > spec.MaxVideoBytes) {
		report.Status, report.Kind = "error", "failed"
		report.Message = fmt.Sprintf("the render page made a video of %d bytes", result.Bytes)
		return fmt.Errorf("bad size")
	}
	out, err := os.Create(spec.VideoPath)
	if err != nil {
		report.Status, report.Kind = "error", "failed"
		report.Message = truncate(err.Error())
		return err
	}
	defer out.Close()
	written := 0
	for written < result.Bytes {
		if time.Now().After(deadlineOf(ctx)) {
			report.Status, report.Kind = "error", "timeout"
			return fmt.Errorf("timeout")
		}
		raw, err := browser.Eval(ctx, wrapVideoSlice(written, videoSliceBytes))
		if err != nil {
			report.Status, report.Kind = "error", "failed"
			report.Message = truncate(err.Error())
			return err
		}
		var part struct {
			OK    bool                            `json:"ok"`
			Data  string                          `json:"data"`
			Error *struct{ Code, Message string } `json:"error"`
		}
		if err := json.Unmarshal(raw, &part); err != nil {
			report.Status, report.Kind = "error", "failed"
			return err
		}
		if !part.OK {
			report.Status, report.Kind = "error", "failed"
			report.Message = "the render page could not read the video"
			return fmt.Errorf("slice")
		}
		data, derr := base64.StdEncoding.DecodeString(part.Data)
		if derr != nil || len(data) == 0 {
			break
		}
		if _, werr := out.Write(data); werr != nil {
			report.Status, report.Kind = "error", "failed"
			report.Message = truncate(werr.Error())
			return werr
		}
		written += len(data)
	}
	if written != result.Bytes {
		report.Status, report.Kind = "error", "failed"
		report.Message = fmt.Sprintf("the render page sent %d of %d bytes", written, result.Bytes)
		return fmt.Errorf("short video")
	}
	report.Mime = truncateCode(result.Mime)
	report.Bytes, report.Width, report.Height = result.Bytes, result.Width, result.Height
	report.FPS, report.Frames, report.DurationMS = result.FPS, result.Frames, result.DurationMS
	report.Video, report.Audio = truncateCode(result.Video), truncateCode(result.Audio)
	report.Warnings = result.Warnings
	report.PageIDs, report.PageIndexes = result.PageIDs, result.PageIndexes
	report.Placeholders, report.MissingFonts = result.Placeholders, result.MissingFonts
	return nil
}

func deadlineOf(ctx context.Context) time.Time {
	if d, ok := ctx.Deadline(); ok {
		return d
	}
	return time.Now().Add(time.Hour)
}

func truncate(s string) string {
	if len(s) > 500 {
		return s[:500]
	}
	return s
}

func truncateCode(s string) string {
	if len(s) > 64 {
		return s[:64]
	}
	return s
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
