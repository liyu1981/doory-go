package renderworker

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"doory-go/internal/chromium"
)

func TestRunVideoReadsTheRenderedFileInSlices(t *testing.T) {
	if _, err := chromium.FindExecutable(""); err != nil {
		t.Skip("chromium not installed")
	}
	page := `<!doctype html><html><body><script>
window.__dooryRender = () => ({ ok: true });
window.__dooryRenderReady = true;
window.__dooryRenderVideo = async () => ({ ok: true, mime: "video/mp4", bytes: 5, width: 320, height: 240,
  fps: 30, frames: 1, durationMs: 33, video: "h264", audio: null, warnings: [], pageIds: ["p1"],
  pageIndexes: [0], placeholders: 0, missingFonts: [] });
window.__dooryVideoSlice = (offset, length) => {
  const text = "hello";
  return { ok: true, data: btoa(text.slice(offset, offset + length)) };
};
</script></body></html>`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/render.html" {
			_, _ = w.Write([]byte(page))
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	dir := t.TempDir()
	payload := filepath.Join(dir, "payload.json")
	if err := os.WriteFile(payload, []byte(`{"pages":[0]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	spec := map[string]any{
		"mode": "video", "render_url": server.URL + "/render.html",
		"payload_path": payload, "video_path": filepath.Join(dir, "out.mp4"),
		"report_path": filepath.Join(dir, "report.json"), "max_video_bytes": 100,
		"executable_path": "", "no_sandbox": true, "allowed_origins": []string{},
		"path_prefixes": []string{"/media/", "/static/"}, "timeout_s": 30, "max_asset_bytes": 1024,
	}
	raw, _ := json.Marshal(spec)
	specPath := filepath.Join(dir, "spec.json")
	if err := os.WriteFile(specPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	report, err := Run(specPath)
	if err != nil || report == nil || report.Status != "ok" {
		t.Fatalf("run = %+v, %v", report, err)
	}
	if report.Mime != "video/mp4" || report.Bytes != 5 || report.Width != 320 || report.FPS != 30 {
		t.Fatalf("report = %+v", report)
	}
	data, err := os.ReadFile(filepath.Join(dir, "out.mp4"))
	if err != nil || string(data) != "hello" {
		t.Fatalf("video = %q, %v", data, err)
	}
	_ = time.Now()
}
