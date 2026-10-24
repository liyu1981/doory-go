// The embedded frontend (family 14, M14): the pinned Vite build is shipped in
// the binary and served with the rules upstream's nginx applies
// (deploy/nginx.conf + frontend/nginx/security-headers.conf):
//
//	/assets/      hashed build output: immutable, missing files are 404 (never the shell)
//	/index.html   no-cache (a release is picked up immediately)
//	other root    favicon, icons/, manifest, robots, og-image: 86400 s
//	everything    SPA fallback to index.html (no-cache) for client-side routes
//	every page    the security header set (CSP, nosniff, frame, referrer, permissions, COOP)
//
// The tree is rebuilt from the pinned upstream source with
// `pnpm frontend:build` (scripts/frontend-build.sh) — provenance in D16.
package doory

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"io/fs"
	"mime"
	"net/http"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"
)

//go:embed webapp
var webappFS embed.FS

// webappRoot is the embedded tree's root directory.
const webappRoot = "webapp"

// appSecurityHeaders is frontend/nginx/security-headers.conf, in its order.
// nginx sets every one of them on page responses; the values are part of the
// deployment contract (CSP workers/wasm for the editor, microphone for
// voiceovers, same-origin websockets for co-editing).
var appSecurityHeaders = [][2]string{
	{"Content-Security-Policy", "default-src 'self'; script-src 'self' 'wasm-unsafe-eval' " +
		"https://accounts.google.com/gsi/client; style-src 'self' 'unsafe-inline' " +
		"https://accounts.google.com/gsi/style; img-src 'self' data: blob: https:; " +
		"font-src 'self' data: blob:; connect-src 'self' blob: data: https://accounts.google.com/gsi/; " +
		"frame-src https://accounts.google.com/gsi/; worker-src 'self' blob:; media-src 'self' blob:; " +
		"object-src 'none'; base-uri 'self'; form-action 'self'; frame-ancestors 'none'"},
	{"X-Content-Type-Options", "nosniff"},
	{"X-Frame-Options", "DENY"},
	{"Referrer-Policy", "strict-origin-when-cross-origin"},
	{"Permissions-Policy", "accelerometer=(), bluetooth=(), browsing-topics=(), camera=(), geolocation=(), " +
		"gyroscope=(), magnetometer=(), microphone=(self), payment=(), serial=(), usb=()"},
	{"Cross-Origin-Opener-Policy", "same-origin-allow-popups"},
}

// webappCacheControl is the nginx cache policy per path class.
const (
	cacheImmutable  = "public, max-age=31536000, immutable"
	cacheNoCache    = "no-cache"
	cacheDaily      = "public, max-age=86400"
	cacheMedia      = "public, max-age=2592000, immutable"
	cacheMediaIndex = "no-cache"
)

// webappHandler serves the embedded frontend, /static (whitenoise semantics)
// and /media (nginx semantics). It owns every path that is not the API, the
// sockets, MCP or the sitemap.
type webappHandler struct {
	files  fs.FS
	static fs.FS
	media  *mediaHandler
	// fallback answers requests the app path set owns when the webapp cannot
	// (a missing /media file falls through to the Django stack, exactly as it
	// does in the oracle harness where nginx is not present).
	fallback http.Handler

	once    sync.Once
	entries map[string]*webappEntry
}

// webappEntry is one embedded file's metadata (raw bytes kept for clients that
// do not accept gzip, gzipped bytes for those that do).
type webappEntry struct {
	contentType string
	raw         []byte
	gzipped     []byte
	etag        string
}

func newWebappHandler(mediaRoot string, fallback http.Handler) *webappHandler {
	files, err := fs.Sub(webappFS, webappRoot)
	if err != nil {
		files = nil
	}
	return &webappHandler{
		files:    files,
		static:   staticFS(),
		media:    &mediaHandler{root: mediaRoot},
		fallback: fallback,
	}
}

// ServeHTTP dispatches on the path prefix.
func (h *webappHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case strings.HasPrefix(r.URL.Path, "/media/"):
		if !h.media.exists(r.URL.Path) && h.fallback != nil {
			h.fallback.ServeHTTP(w, r)
			return
		}
		h.media.ServeHTTP(w, r)
	case strings.HasPrefix(r.URL.Path, "/static/"):
		h.serveStatic(w, r)
	default:
		h.serveApp(w, r)
	}
}

// serveApp is nginx's server{} block for the SPA.
func (h *webappHandler) serveApp(w http.ResponseWriter, r *http.Request) {
	if h.files == nil {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD, OPTIONS")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	requestPath := path.Clean("/" + strings.TrimPrefix(r.URL.Path, "/"))
	name := strings.TrimPrefix(requestPath, "/")
	cache := cacheDaily
	switch {
	case name == "" || name == "index.html":
		name, cache = "index.html", cacheNoCache
	case strings.HasPrefix(name, "assets/"):
		cache = cacheImmutable
		if !h.exists(name) {
			// A missing hashed file is a 404, never the SPA shell.
			http.NotFound(w, r)
			return
		}
	case name == "presenter-view.html" || name == "render.html":
		cache = cacheNoCache
		if !h.exists(name) {
			http.NotFound(w, r)
			return
		}
	case strings.HasPrefix(name, "icons/") || strings.HasSuffix(name, ".webmanifest") ||
		name == "robots.txt" || name == "og-image.png":
		if !h.exists(name) {
			http.NotFound(w, r)
			return
		}
	default:
		// Every other path is a client-side route.
		name, cache = "index.html", cacheNoCache
	}
	h.serveFile(w, r, name, cache, true)
}

// exists reports whether the embedded tree holds the file.
func (h *webappHandler) exists(name string) bool {
	info, err := fs.Stat(h.files, name)
	return err == nil && !info.IsDir()
}

// serveFile writes one embedded file with its cache class and (for pages) the
// app security headers. Compression follows Accept-Encoding (gzip).
func (h *webappHandler) serveFile(w http.ResponseWriter, r *http.Request, name, cache string,
	pageHeaders bool,
) {
	entry, err := h.entry(name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if pageHeaders {
		for _, header := range appSecurityHeaders {
			w.Header().Set(header[0], header[1])
		}
	}
	w.Header().Set("Cache-Control", cache)
	w.Header().Set("Content-Type", entry.contentType)
	w.Header().Set("ETag", entry.etag)
	w.Header().Add("Vary", "Accept-Encoding")
	if match := r.Header.Get("If-None-Match"); match != "" && etagMatches(match, entry.etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	// nginx compresses by type and only above gzip_min_length (512); already
	// compressed payloads (wasm, images, fonts) are sent as they are.
	body := entry.raw
	if acceptsGzip(r) && gzipEligible(name, entry.contentType, len(entry.raw)) {
		body = entry.gzipped
		w.Header().Set("Content-Encoding", "gzip")
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	if _, err := w.Write(body); err != nil {
		return
	}
}

// entry reads and caches one file's content type, gzipped bytes and ETag.
func (h *webappHandler) entry(name string) (*webappEntry, error) {
	h.once.Do(func() { h.entries = map[string]*webappEntry{} })
	if entry, ok := h.entries[name]; ok {
		return entry, nil
	}
	data, err := fs.ReadFile(h.files, name)
	if err != nil {
		return nil, err
	}
	var compressed bytes.Buffer
	writer := gzip.NewWriter(&compressed)
	if _, err := writer.Write(data); err == nil {
		_ = writer.Close()
	}
	sum := sha256.Sum256(data)
	entry := &webappEntry{
		contentType: contentTypeFor(name),
		raw:         data,
		gzipped:     compressed.Bytes(),
		etag:        `"` + hex.EncodeToString(sum[:16]) + `"`,
	}
	h.entries[name] = entry
	return entry, nil
}

// contentTypeFor is mime.TypeByExtension with the small set of overrides the
// app needs (widget manifests, wasm, webmanifest).
func contentTypeFor(name string) string {
	switch path.Ext(name) {
	case ".wasm":
		return "application/wasm"
	case ".webmanifest":
		return "application/manifest+json"
	case ".mjs", ".js":
		return "text/javascript; charset=utf-8"
	case ".css":
		return "text/css; charset=utf-8"
	case ".html":
		return "text/html; charset=utf-8"
	case ".json":
		return "application/json"
	case ".svg":
		return "image/svg+xml"
	case ".woff2":
		return "font/woff2"
	}
	if guessed := mime.TypeByExtension(path.Ext(name)); guessed != "" {
		return guessed
	}
	return "application/octet-stream"
}

// gzipTypes is nginx's gzip_types from deploy/nginx.conf.
var gzipTypes = map[string]bool{
	"text/plain": true, "text/css": true, "text/javascript": true, "application/javascript": true,
	"application/json": true, "application/manifest+json": true, "application/xml": true,
	"application/wasm": true, "image/svg+xml": true, "font/ttf": true, "font/otf": true,
}

// gzipEligible applies nginx's gzip_min_length (512) and gzip_types.
func gzipEligible(name, contentType string, size int) bool {
	if size < 512 {
		return false
	}
	base := contentType
	if i := strings.IndexByte(base, ';'); i >= 0 {
		base = base[:i]
	}
	return gzipTypes[base]
}

// acceptsGzip reports a gzip-capable client.
func acceptsGzip(r *http.Request) bool {
	for _, part := range strings.Split(r.Header.Get("Accept-Encoding"), ",") {
		encoding := strings.TrimSpace(strings.SplitN(part, ";", 2)[0])
		if encoding == "gzip" {
			return true
		}
	}
	return false
}

// etagMatches implements the If-None-Match weak comparison.
func etagMatches(header, etag string) bool {
	for _, candidate := range strings.Split(header, ",") {
		trimmed := strings.TrimSpace(candidate)
		if trimmed == etag || strings.TrimPrefix(trimmed, "W/") == etag || trimmed == "*" {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------- static ---

// staticFS is the embedded Django-style static tree (admin/assets). Empty in
// the port (D2 keeps the admin minimal), so `/static/` answers 404 like an
// uncollected oracle.
func staticFS() fs.FS { return nil }

// serveStatic applies whitenoise's semantics: content-type by extension,
// Accept-Encoding negotiation, ETag/If-None-Match, and immutable caching for
// manifest-hashed names (a 32-byte hash before the extension).
func (h *webappHandler) serveStatic(w http.ResponseWriter, r *http.Request) {
	if h.static == nil {
		http.NotFound(w, r)
		return
	}
	name := strings.TrimPrefix(path.Clean("/"+strings.TrimPrefix(r.URL.Path, "/")), "/")
	data, err := fs.ReadFile(h.static, name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	sum := sha256.Sum256(data)
	etag := `"` + hex.EncodeToString(sum[:16]) + `"`
	w.Header().Set("ETag", etag)
	w.Header().Add("Vary", "Accept-Encoding")
	w.Header().Set("Content-Type", contentTypeFor(name))
	if manifestHashed(name) {
		w.Header().Set("Cache-Control", cacheImmutable)
	} else {
		w.Header().Set("Cache-Control", "max-age=60")
	}
	if match := r.Header.Get("If-None-Match"); match != "" && etagMatches(match, etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	body := data
	if acceptsGzip(r) {
		var compressed bytes.Buffer
		writer := gzip.NewWriter(&compressed)
		if _, werr := writer.Write(data); werr == nil && writer.Close() == nil {
			body = compressed.Bytes()
			w.Header().Set("Content-Encoding", "gzip")
		}
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	_, _ = w.Write(body)
}

// manifestHashed reports a whitenoise-manifest name (`<stem>.<hash>.<ext>`):
// the second-to-last dot segment is a hex hash.
func manifestHashed(name string) bool {
	parts := strings.Split(path.Base(name), ".")
	if len(parts) < 3 {
		return false
	}
	candidate := parts[len(parts)-2]
	if len(candidate) < 8 {
		return false
	}
	for _, r := range candidate {
		isHex := (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')
		if !isHex {
			return false
		}
	}
	return true
}

// ----------------------------------------------------------------- media ---

// mediaHandler serves uploaded files from MEDIA_ROOT the way nginx does:
// immutable caching (names are unguessable and never change), nosniff, the
// sandbox CSP that neutralises a directly-opened SVG, and a no-cache
// catalogue at audio/index.json.
type mediaHandler struct {
	root string
}

// exists reports whether the media path names an existing file.
func (h *mediaHandler) exists(requestPath string) bool {
	name := strings.TrimPrefix(path.Clean("/"+strings.TrimPrefix(requestPath, "/media/")), "/")
	if name == "." || name == "" || strings.HasPrefix(name, "..") {
		return false
	}
	info, err := os.Stat(path.Join(h.root, name))
	return err == nil && !info.IsDir()
}

func (h *mediaHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD, OPTIONS")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	name := strings.TrimPrefix(path.Clean("/"+strings.TrimPrefix(r.URL.Path, "/media/")), "/")
	if name == "." || name == "" || strings.HasPrefix(name, "..") {
		http.NotFound(w, r)
		return
	}
	full := path.Join(h.root, name)
	file, err := openMediaFile(full)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer file.Close()
	info, statErr := file.Stat()
	if statErr != nil || info.IsDir() {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if strings.HasSuffix(name, ".svg") {
		w.Header().Set("Content-Disposition", "attachment")
	}
	if name == "audio/index.json" {
		// nginx's exact-match block for the audio catalogue.
		w.Header().Set("Cache-Control", cacheMediaIndex)
		w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
	} else {
		w.Header().Set("Cache-Control", cacheMedia)
		w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; sandbox")
	}
	if contentType := contentTypeFor(name); contentType != "application/octet-stream" {
		w.Header().Set("Content-Type", contentType)
	}
	http.ServeContent(w, r, name, info.ModTime(), file)
}

// openMediaFile opens a file under MEDIA_ROOT (no directory traversal: the
// cleaned name is already relative).
func openMediaFile(full string) (*os.File, error) {
	return os.Open(full)
}
