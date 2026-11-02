// Video exports: MP4, WebM and GIF made by the frontend render page
// (window.__dooryRenderVideo), streamed back through the render worker.
package doory

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"os"
	"strings"
	"time"
)

var videoExportFormats = map[string]string{"mp4": "video/mp4", "webm": "video/webm", "gif": "image/gif"}

var videoExportRules = map[string]struct {
	fps         []int
	resolutions []string
	maxDesign   float64
}{
	"mp4":  {fps: []int{24, 30, 60}, resolutions: []string{"design", "480", "720", "1080", "2160"}, maxDesign: 1920},
	"webm": {fps: []int{24, 30, 60}, resolutions: []string{"design", "480", "720", "1080", "2160"}, maxDesign: 1920},
	"gif":  {fps: []int{5, 10, 15, 20}, resolutions: []string{"240", "360", "480", "720"}, maxDesign: 720},
}

func videoMIME(format string) string { return videoExportFormats[format] }

func videoFilename(title, format string, style string, unix *int) string {
	if style == "structured" && unix != nil {
		return exportFilename(title, format)
	}
	return exportFilename(title, format)
}

// exportVideo ports services.export_video for the formats this server makes.
func (a *App) exportVideo(w http.ResponseWriter, r *http.Request, d *designRow, u *UserRow, body videoExportBody) {
	if !a.Settings.ServerVideoExportEnabled {
		writeDRF(w, http.StatusNotFound, "server_video_export_disabled",
			"Server-side video export is not enabled on this server.")
		return
	}
	format := strings.ToLower(body.Format)
	if videoExportFormats[format] == "" {
		writeDRF(w, http.StatusBadRequest, "invalid_format",
			"The format must be mp4, webm or gif.")
		return
	}
	docAny, err := parseJSONValue([]byte(d.Document))
	if err != nil {
		writeDRF(w, http.StatusBadRequest, "invalid_export", "The design has no pages.")
		return
	}
	doc, _ := docAny.(map[string]any)
	if doc == nil {
		writeDRF(w, http.StatusBadRequest, "invalid_export", "The design has no pages.")
		return
	}
	if len(encodeCompactJSON(doc)) > a.Settings.ExportImageMaxDocBytes {
		writeDRF(w, http.StatusRequestEntityTooLarge, "payload_too_large",
			"The design is too large to export on the server.")
		return
	}
	pages := asAnySlice(doc["pages"])
	if len(pages) == 0 {
		writeDRF(w, http.StatusBadRequest, "invalid_export", "The design has no pages.")
		return
	}
	indexes, derr := videoPageIndexes(doc, body.Pages)
	if derr != nil {
		writeDRF(w, derr.Status, derr.Code, derr.Message)
		return
	}
	first, _ := pages[indexes[0]].(map[string]any)
	firstW, firstH, sizeErr := pageSizeOf(doc, first)
	if sizeErr != nil {
		writeDRF(w, sizeErr.Status, sizeErr.Code, sizeErr.Message)
		return
	}
	rules := videoExportRules[format]
	fps := 30
	if format == "gif" {
		fps = 15
	}
	if body.FPS != nil {
		fps = *body.FPS
		ok := false
		for _, value := range rules.fps {
			if value == fps {
				ok = true
			}
		}
		if !ok {
			writeDRF(w, http.StatusBadRequest, "invalid_fps",
				fmt.Sprintf("%s frame rates are %s.", strings.ToUpper(format), joinInts(rules.fps)))
			return
		}
	}
	resolution := any("design")
	if body.Resolution != nil {
		resolution = body.Resolution
	}
	res := videoResolution(format, resolution, firstW, firstH, a.Settings.ExportVideoMaxResolution)
	if res.err != nil {
		writeDRF(w, http.StatusBadRequest, res.err.Code, res.err.Message)
		return
	}
	pageSeconds := 0.0
	if body.PageSeconds != nil {
		pageSeconds = *body.PageSeconds
		if pageSeconds < 1 || pageSeconds > 600 || math.IsNaN(pageSeconds) {
			writeDRF(w, http.StatusBadRequest, "invalid_page_seconds",
				"page_seconds must be between 1 and 600.")
			return
		}
	}
	held := videoHoldSeconds(pages, indexes, pageSeconds)
	if held > float64(a.Settings.ExportVideoMaxSeconds) {
		writeDRF(w, http.StatusBadRequest, "too_long",
			fmt.Sprintf("The video would last at least %g s; this server makes at most %d s.",
				held, a.Settings.ExportVideoMaxSeconds))
		return
	}
	loop := 0
	if body.Loop != nil {
		loop = *body.Loop
		if loop < 0 || loop > 65535 {
			writeDRF(w, http.StatusBadRequest, "invalid_loop",
				"loop must be a whole number from 0 (forever) to 65535.")
			return
		}
	}
	fit := body.Fit
	if fit == "" {
		fit = "fit"
	}
	if fit != "fit" && fit != "fill" {
		writeDRF(w, http.StatusBadRequest, "invalid_fit", "fit must be fit or fill.")
		return
	}
	quality := "balanced"
	if body.Quality != nil {
		if text, ok := body.Quality.(string); ok {
			switch strings.ToLower(strings.TrimSpace(text)) {
			case "small":
				quality = "small"
			case "balanced":
				quality = "balanced"
			case "best", "original":
				quality = "high"
			default:
				writeDRF(w, http.StatusBadRequest, "invalid", "Give a quality from 1 to 100 or a preset.")
				return
			}
		} else if value, ok := imageQualityValue(body.Quality); ok {
			switch {
			case value <= 70:
				quality = "small"
			case value <= 90:
				quality = "balanced"
			default:
				quality = "high"
			}
		} else {
			writeDRF(w, http.StatusBadRequest, "invalid", "Give a quality from 1 to 100 or a preset.")
			return
		}
	}
	sound := true
	if body.Sound != nil {
		sound = *body.Sound
	}
	dither := true
	if body.Dither != nil {
		dither = *body.Dither
	}
	if held > 10 {
		if ok, wait := a.chargeThrottle(u, "export"); !ok {
			writeDRFThrottledDetail(w, wait, throttledDefault)
			return
		}
	}
	payload := map[string]any{
		"document": doc, "pages": indexes, "format": format, "resolution": res.value,
		"fps": fps, "quality": quality, "fit": fit, "sound": sound, "loops": loop,
		"dither": dither, "pageSeconds": nil, "maxMs": a.Settings.ExportVideoMaxSeconds * 1000,
		"maxBytes": a.Settings.ExportVideoMaxBytes,
		"fonts":    a.fontManifest(doc, u.ID),
	}
	if pageSeconds > 0 {
		payload["pageSeconds"] = pageSeconds
	}
	if !a.acquireRenderSlot(u.ID) {
		writeDRF(w, exportBusy.Status, exportBusy.Code, exportBusy.Message)
		return
	}
	defer a.releaseRenderSlot(u.ID)
	result, rerr := a.runWorkerTimeout(videoExportFormats[format], float64(a.Settings.ExportVideoTimeoutSeconds),
		func(workdir string) (map[string]any, string, error) {
			payloadPath := workdir + "/payload.json"
			videoPath := workdir + "/output.video"
			data, _ := json.Marshal(payload)
			if err := osWriteFileText(payloadPath, data); err != nil {
				return nil, "", err
			}
			return map[string]any{"mode": "video", "render_url": a.Settings.ExportRenderPageURL,
				"payload_path": payloadPath, "video_path": videoPath,
				"max_video_bytes": a.Settings.ExportVideoMaxBytes}, videoPath, nil
		})
	if rerr != nil {
		writeDRF(w, rerr.Status, rerr.Code, rerr.Message)
		return
	}
	if result.Mime != videoExportFormats[format] || len(result.Data) == 0 {
		writeDRF(w, http.StatusInternalServerError, "export_failed", "The video could not be rendered.")
		return
	}
	style := body.FilenameStyle
	if style == "" {
		style = a.Settings.ExportFilenameStyle
	}
	var unix *int
	if body.FilenameTS != nil && *body.FilenameTS {
		v := int(time.Now().Unix())
		unix = &v
	}
	w.Header().Set("X-Doory-Placeholders", fmt.Sprint(result.Placeholders))
	w.Header().Set("Content-Type", videoExportFormats[format])
	w.Header().Set("Content-Disposition", contentDisposition(videoFilename(d.Title, format, style, unix), true))
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Length", fmt.Sprint(len(result.Data)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(result.Data)
}

type videoExportBody struct {
	Format        string          `json:"format"`
	Quality       any             `json:"quality"`
	Pages         json.RawMessage `json:"pages"`
	Resolution    any             `json:"resolution"`
	FPS           *int            `json:"fps"`
	Fit           string          `json:"fit"`
	Sound         *bool           `json:"sound"`
	Loop          *int            `json:"loop"`
	Dither        *bool           `json:"dither"`
	PageSeconds   *float64        `json:"page_seconds"`
	FilenameStyle string          `json:"filename_style"`
	FilenameTS    *bool           `json:"filename_timestamp"`
}

func videoPageIndexes(doc map[string]any, raw json.RawMessage) ([]int, *DocumentError) {
	pages := asAnySlice(doc["pages"])
	all := make([]int, len(pages))
	for i := range all {
		all[i] = i
	}
	if len(raw) == 0 || string(raw) == "null" || string(raw) == `"all"` {
		return all, nil
	}
	var refs []any
	if err := json.Unmarshal(raw, &refs); err != nil || len(refs) == 0 {
		return nil, &DocumentError{Code: "invalid", Status: 400,
			Message: "Give \"all\" or a list of page ids and page numbers counting from 1."}
	}
	var out []int
	seen := map[int]bool{}
	for _, ref := range refs {
		switch value := ref.(type) {
		case float64:
			index := int(value) - 1
			if index < 0 || index >= len(pages) {
				return nil, &DocumentError{Code: "page_not_found", Status: 400, Message: "Pages are numbered from 1."}
			}
			if !seen[index] {
				seen[index] = true
				out = append(out, index)
			}
		case string:
			found := -1
			for i, page := range pages {
				obj, _ := page.(map[string]any)
				if stringFromAny(obj["id"]) == value {
					found = i
					break
				}
			}
			if found < 0 {
				return nil, &DocumentError{Code: "page_not_found", Status: 400, Message: "The design has no page with that id."}
			}
			if !seen[found] {
				seen[found] = true
				out = append(out, found)
			}
		default:
			return nil, &DocumentError{Code: "invalid", Status: 400, Message: "Give page ids or page numbers counting from 1."}
		}
	}
	return out, nil
}

type videoResolutionResult struct {
	value any
	err   *DocumentError
}

func videoResolution(format string, raw any, width, height float64, limit int) videoResolutionResult {
	rules := videoExportRules[format]
	value := raw
	if value == nil {
		value = "design"
	}
	if text, ok := value.(string); ok && text != "design" {
		if n, err := strconvAtoi(text); err == nil {
			value = n
		}
	}
	valid := false
	switch v := value.(type) {
	case string:
		valid = v == "design"
	case float64:
		for _, allowed := range rules.resolutions {
			if allowed != "design" && allowed == fmt.Sprint(int(v)) {
				valid = true
			}
		}
	case int:
		for _, allowed := range rules.resolutions {
			if allowed != "design" && allowed == fmt.Sprint(v) {
				valid = true
			}
		}
	}
	if !valid {
		return videoResolutionResult{err: &DocumentError{Code: "invalid_resolution", Status: 400,
			Message: fmt.Sprintf("%s resolutions are %s.", strings.ToUpper(format), strings.Join(rules.resolutions, ", "))}}
	}
	if value == "design" {
		short := math.Min(width, height) * math.Min(1, rules.maxDesign/math.Max(width, height))
		if int(math.Round(short)) > limit {
			return videoResolutionResult{err: &DocumentError{Code: "invalid_resolution", Status: 400,
				Message: fmt.Sprintf("At the design's size this video would be %d px on its shorter side; this server makes at most %d. Give a resolution of %d or less.", int(math.Round(short)), limit, limit)}}
		}
		return videoResolutionResult{value: "design"}
	}
	n, _ := strconvAtoi(fmt.Sprint(value))
	if n > limit {
		return videoResolutionResult{err: &DocumentError{Code: "invalid_resolution", Status: 400,
			Message: fmt.Sprintf("This server makes videos of at most %d px on the shorter side.", limit)}}
	}
	return videoResolutionResult{value: n}
}

func videoHoldSeconds(pages []any, indexes []int, pageSeconds float64) float64 {
	total := 0.0
	for _, index := range indexes {
		if pageSeconds > 0 {
			total += pageSeconds
			continue
		}
		page, _ := pages[index].(map[string]any)
		if value, ok := page["durationSec"].(float64); ok && value >= 1 && value <= 600 {
			total += value
		} else {
			total += 5
		}
	}
	return total
}

func joinInts(values []int) string {
	parts := make([]string, len(values))
	for i, value := range values {
		parts[i] = fmt.Sprint(value)
	}
	return strings.Join(parts, ", ")
}

func strconvAtoi(text string) (int, error) {
	n := 0
	for _, r := range text {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("not a number")
		}
		n = n*10 + int(r-'0')
	}
	return n, nil
}

func osWriteFileText(path string, data []byte) error { return os.WriteFile(path, data, 0o600) }
