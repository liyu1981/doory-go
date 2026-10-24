// Golden tests for the embedded-frontend serving rules: they encode
// deploy/nginx.conf + frontend/nginx/security-headers.conf (the deployment
// contract upstream serves with nginx; the Django oracle does not own these
// paths, see D7/D16).
package doory

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func webappTestHandler(t *testing.T) (*webappHandler, string) {
	t.Helper()
	media := t.TempDir()
	if err := os.MkdirAll(filepath.Join(media, "audio"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(media, "audio", "index.json"), []byte(`{"schema":1}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(media, "a.svg"), []byte(`<svg xmlns="http://www.w3.org/2000/svg"/>`), 0o644); err != nil {
		t.Fatal(err)
	}
	return newWebappHandler(media, nil), media
}

func serveWebapp(t *testing.T, h *webappHandler, method, path string, headers map[string]string) *http.Response {
	t.Helper()
	request := httptest.NewRequest(method, path, nil)
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, request)
	return recorder.Result()
}

func TestWebappServesIndexAndRoutes(t *testing.T) {
	h, _ := webappTestHandler(t)
	for _, path := range []string{"/", "/index.html", "/some/client/route"} {
		response := serveWebapp(t, h, http.MethodGet, path, nil)
		if response.StatusCode != 200 {
			t.Fatalf("%s: status %d", path, response.StatusCode)
		}
		if got := response.Header.Get("Cache-Control"); got != "no-cache" {
			t.Fatalf("%s: cache-control %q", path, got)
		}
		// The security header set rides on every page response.
		if !strings.Contains(response.Header.Get("Content-Security-Policy"), "wasm-unsafe-eval") {
			t.Fatalf("%s: missing the app CSP", path)
		}
		if response.Header.Get("X-Frame-Options") != "DENY" ||
			response.Header.Get("X-Content-Type-Options") != "nosniff" ||
			response.Header.Get("Cross-Origin-Opener-Policy") != "same-origin-allow-popups" {
			t.Fatalf("%s: incomplete security headers: %v", path, response.Header)
		}
		if !strings.Contains(response.Header.Get("Content-Type"), "text/html") {
			t.Fatalf("%s: content-type %q", path, response.Header.Get("Content-Type"))
		}
	}
}

func TestWebappHashedAssets(t *testing.T) {
	h, _ := webappTestHandler(t)
	// Every /assets file in the embedded build is immutable; a missing one is
	// a 404, never the SPA shell.
	asset := ""
	entries, err := osReadDirEmbedded("webapp/assets")
	if err == nil && len(entries) > 0 {
		for _, name := range entries {
			if strings.HasSuffix(name, ".js") {
				asset = "assets/" + name
				break
			}
		}
	}
	if asset == "" {
		t.Skip("no embedded JS asset to probe")
	}
	response := serveWebapp(t, h, http.MethodGet, "/"+asset, nil)
	if response.StatusCode != 200 {
		t.Fatalf("%s: status %d", asset, response.StatusCode)
	}
	if got := response.Header.Get("Cache-Control"); got != "public, max-age=31536000, immutable" {
		t.Fatalf("asset cache-control %q", got)
	}
	missing := serveWebapp(t, h, http.MethodGet, "/assets/does-not-exist-abc123.js", nil)
	if missing.StatusCode != 404 {
		t.Fatalf("missing asset status %d, want 404 (never the shell)", missing.StatusCode)
	}
}

func TestWebappCompressionNegotiation(t *testing.T) {
	h, _ := webappTestHandler(t)
	_, err := osReadDirEmbedded("webapp/assets")
	if err != nil {
		t.Skip("no embedded assets")
	}
	var jsAsset string
	names, _ := osReadDirEmbedded("webapp/assets")
	for _, name := range names {
		if strings.HasSuffix(name, ".js") {
			jsAsset = "assets/" + name
			break
		}
	}
	if jsAsset == "" {
		t.Skip("no JS asset")
	}
	plain := serveWebapp(t, h, http.MethodGet, "/"+jsAsset, nil)
	if plain.Header.Get("Content-Encoding") != "" {
		t.Fatalf("plain response must not be gzip (header %q)", plain.Header.Get("Content-Encoding"))
	}
	gzipped := serveWebapp(t, h, http.MethodGet, "/"+jsAsset, map[string]string{"Accept-Encoding": "gzip"})
	if gzipped.Header.Get("Content-Encoding") != "gzip" {
		t.Fatalf("gzip response not compressed")
	}
	if gzipped.Header.Get("Vary") != "Accept-Encoding" {
		t.Fatalf("vary %q", gzipped.Header.Get("Vary"))
	}
	etag := gzipped.Header.Get("ETag")
	notModified := serveWebapp(t, h, http.MethodGet, "/"+jsAsset, map[string]string{"If-None-Match": etag})
	if notModified.StatusCode != http.StatusNotModified {
		t.Fatalf("If-None-Match status %d, want 304", notModified.StatusCode)
	}
}

func TestWebappMediaRules(t *testing.T) {
	h, _ := webappTestHandler(t)
	svg := serveWebapp(t, h, http.MethodGet, "/media/a.svg", nil)
	if svg.StatusCode != 200 {
		t.Fatalf("media svg status %d", svg.StatusCode)
	}
	if svg.Header.Get("Cache-Control") != "public, max-age=2592000, immutable" ||
		svg.Header.Get("X-Content-Type-Options") != "nosniff" ||
		svg.Header.Get("Content-Disposition") != "attachment" ||
		!strings.Contains(svg.Header.Get("Content-Security-Policy"), "sandbox") {
		t.Fatalf("media headers: %v", svg.Header)
	}
	index := serveWebapp(t, h, http.MethodGet, "/media/audio/index.json", nil)
	if index.Header.Get("Cache-Control") != "no-cache" ||
		index.Header.Get("Content-Security-Policy") != "default-src 'none'; sandbox" {
		t.Fatalf("audio catalogue headers: %v", index.Header)
	}
	for _, path := range []string{"/media/missing.png", "/media/../etc/passwd", "/static/anything.css"} {
		response := serveWebapp(t, h, http.MethodGet, path, nil)
		if response.StatusCode != 404 {
			t.Fatalf("%s: status %d, want 404", path, response.StatusCode)
		}
	}
	// A missing media file falls through to the app stack (Django's 404 with
	// its middleware headers), exactly as the oracle harness behaves.
	fallbackCalls := 0
	h.fallback = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fallbackCalls++
		w.Header().Set("X-Fallback", "1")
		w.WriteHeader(http.StatusNotFound)
	})
	missing := serveWebapp(t, h, http.MethodGet, "/media/missing.png", nil)
	if fallbackCalls != 1 || missing.Header.Get("X-Fallback") != "1" {
		t.Fatalf("missing media must fall through to the stack (calls=%d, header=%q)",
			fallbackCalls, missing.Header.Get("X-Fallback"))
	}
}

func TestManifestHashed(t *testing.T) {
	for name, want := range map[string]bool{
		"admin/css/base.6f8e2f2a4c4d.css": true,
		"admin/css/base.css":              false,
		"a-deadbeef.js":                   false, // "a-deadbeef" is not hex-only
		"styles.abcdef12.css":             true,
	} {
		if got := manifestHashed(name); got != want {
			t.Errorf("manifestHashed(%q) = %v, want %v", name, got, want)
		}
	}
}

// osReadDirEmbedded lists one embedded directory (test helper).
func osReadDirEmbedded(name string) ([]string, error) {
	entries, err := webappFS.ReadDir(name)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names, nil
}
