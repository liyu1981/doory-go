package doory

import (
	"bytes"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func (e *adminEnv) postFileWithCSRF(path, field, filename, body string) (*http.Response, string) {
	e.t.Helper()
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	part, err := writer.CreateFormFile(field, filename)
	if err != nil {
		e.t.Fatal(err)
	}
	_, _ = part.Write([]byte(body))
	_ = writer.Close()
	req, err := http.NewRequest(http.MethodPost, e.url(path), &buf)
	if err != nil {
		e.t.Fatal(err)
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("X-CSRFToken", e.csrf)
	res, err := e.client.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	data, _ := io.ReadAll(res.Body)
	_ = res.Body.Close()
	return res, string(data)
}

func TestSVGAssetUploadIsSanitisedAndStored(t *testing.T) {
	env := newAdminEnv(t)
	user := env.makeUser("svg@example.com", false, false)
	env.loginAs(user)
	svgBody := `<svg xmlns="http://www.w3.org/2000/svg" width="200" height="100" onload="alert(1)">` +
		`<script>alert(1)</script><rect width="200" height="100" fill="#ff0000"/></svg>`
	res, body := env.postFileWithCSRF("/api/v1/assets/", "file", "badge.svg", svgBody)
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("svg upload = %d %s", res.StatusCode, body)
	}
	var payload struct {
		ID       string `json:"id"`
		Kind     string `json:"kind"`
		Width    int    `json:"width"`
		Height   int    `json:"height"`
		Mime     string `json:"mime"`
		ThumbURL string `json:"thumb_url"`
	}
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Kind != "svg" || payload.Width != 200 || payload.Height != 100 ||
		payload.Mime != "image/svg+xml" || !strings.HasSuffix(payload.ThumbURL, ".svg") {
		t.Fatalf("svg envelope = %+v", payload)
	}
	var name string
	if err := env.db.QueryRow(`SELECT file FROM assets_asset WHERE id = ?`, hexUUID(payload.ID)).Scan(&name); err != nil {
		t.Fatal(err)
	}
	stored, err := os.ReadFile(filepath.Join(env.settings.MediaRoot, filepath.FromSlash(name)))
	if err != nil {
		t.Fatal(err)
	}
	text := string(stored)
	if strings.Contains(text, "<script") || strings.Contains(text, "onload") {
		t.Fatalf("stored SVG still has unsafe content: %s", text)
	}
	if !strings.Contains(text, `width="200"`) || !strings.Contains(text, `height="100"`) {
		t.Fatalf("stored SVG lost its intrinsic size: %s", text)
	}

	res, body = env.postFileWithCSRF("/api/v1/assets/", "file", "bad.svg", `<svg><script>alert(1)</script>`)
	if res.StatusCode != http.StatusBadRequest || !strings.Contains(body, "not a valid, safe SVG") {
		t.Fatalf("unsafe svg = %d %s", res.StatusCode, body)
	}
}

func TestRasterAssetThumbnailIsWebP(t *testing.T) {
	env := newAdminEnv(t)
	user := env.makeUser("thumb@example.com", false, false)
	env.loginAs(user)
	src := image.NewNRGBA(image.Rect(0, 0, 1000, 500))
	for y := 0; y < 500; y++ {
		for x := 0; x < 1000; x++ {
			src.SetNRGBA(x, y, color.NRGBA{R: uint8(x), G: uint8(y), B: uint8(x ^ y), A: 255})
		}
	}
	var pngBytes bytes.Buffer
	if err := png.Encode(&pngBytes, src); err != nil {
		t.Fatal(err)
	}
	res, body := env.postFileWithCSRF("/api/v1/assets/", "file", "photo.png", pngBytes.String())
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("png upload = %d %s", res.StatusCode, body)
	}
	var payload struct {
		ID       string `json:"id"`
		Kind     string `json:"kind"`
		ThumbURL string `json:"thumb_url"`
	}
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Kind != "image" || !strings.HasSuffix(payload.ThumbURL, ".webp") {
		t.Fatalf("png envelope = %+v", payload)
	}
	var name string
	if err := env.db.QueryRow(`SELECT thumbnail FROM assets_asset WHERE id = ?`, hexUUID(payload.ID)).Scan(&name); err != nil {
		t.Fatal(err)
	}
	stored, err := os.ReadFile(filepath.Join(env.settings.MediaRoot, filepath.FromSlash(name)))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(stored, []byte("RIFF")) || !bytes.Contains(stored[:12], []byte("WEBP")) {
		t.Fatalf("thumbnail is not WebP: %q", stored[:12])
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(stored))
	if err != nil || format != "webp" || cfg.Width != 480 || cfg.Height != 240 {
		t.Fatalf("thumbnail config = %dx%d %s err=%v, want 480x240 webp", cfg.Width, cfg.Height, format, err)
	}
}
