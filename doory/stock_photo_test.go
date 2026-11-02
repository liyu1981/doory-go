package doory

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestStockPhotoSearchUsesConfiguredProvider(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/search/photos" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"total":1,"results":[{"id":"photo-abcde","width":1200,"height":800,"urls":{"small":"https://images.unsplash.com/small.jpg","regular":"https://images.unsplash.com/regular.jpg"},"user":{"name":"Ada","links":{"html":"https://unsplash.com/@ada"}},"links":{"html":"https://unsplash.com/photos/photo-abcde"}}]}`))
	}))
	defer server.Close()
	old := unsplashAPIBase
	unsplashAPIBase = server.URL
	t.Cleanup(func() { unsplashAPIBase = old })

	env := newAdminEnv(t)
	user := env.makeUser("stock-photo@example.com", false, false)
	env.loginAs(user)
	env.app.Settings.UnsplashAccessKey = "test-key"
	res, body := env.get("/api/v1/stock/photos/?provider=unsplash&q=cat")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("stock photos = %d: %s", res.StatusCode, body)
	}
	var payload struct {
		Configured bool                            `json:"configured"`
		Total      int                             `json:"total"`
		Results    []struct{ ID, Provider string } `json:"results"`
	}
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		t.Fatal(err)
	}
	if !payload.Configured || payload.Total != 1 || len(payload.Results) != 1 ||
		payload.Results[0].ID != "photo-abcde" || payload.Results[0].Provider != "unsplash" {
		t.Fatalf("payload = %+v", payload)
	}
	if !strings.Contains(body, "https://images.unsplash.com/regular.jpg") {
		t.Fatalf("preview URL missing: %s", body)
	}
}

func TestStockPhotoImportStoresAsset(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/photos/12345" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"id":12345,"width":1200,"height":800,"src":{"medium":"https://images.pexels.com/m.jpg","large":"https://images.pexels.com/l.jpg","large2x":"https://images.pexels.com/l2.jpg"},"photographer":"Ada","photographer_url":"https://www.pexels.com/@ada","url":"https://www.pexels.com/photo/12345/"}`))
	}))
	defer server.Close()
	oldBase, oldDownload := pexelsAPIBase, stockPhotoDownload
	pexelsAPIBase = server.URL
	stockPhotoDownload = func(a *App, rawURL string, hosts map[string]bool, maxBytes int) ([]byte, error) {
		return pngUpload(t, 40, 30), nil
	}
	t.Cleanup(func() { pexelsAPIBase, stockPhotoDownload = oldBase, oldDownload })

	env := newAdminEnv(t)
	user := env.makeUser("photo-import@example.com", false, false)
	env.loginAs(user)
	env.app.Settings.PexelsAccessKey = "key"
	res, body := env.doJSON("/api/v1/stock/photos/import/", `{"provider":"pexels","id":"12345"}`)
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("photo import = %d: %s", res.StatusCode, body)
	}
	var payload struct {
		Kind, Mime    string
		Width, Height int
		Attribution   map[string]any `json:"attribution"`
	}
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Kind != "image" || payload.Mime != "image/png" || payload.Width != 40 || payload.Height != 30 ||
		payload.Attribution["author"] != "Ada" {
		t.Fatalf("photo import envelope = %+v", payload)
	}
}
