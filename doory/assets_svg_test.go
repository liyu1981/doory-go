package doory

import (
	"bytes"
	"encoding/json"
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
