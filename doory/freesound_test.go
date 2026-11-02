package doory

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestFreesoundSearchUsesConfiguredProvider(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/search/text/" {
			http.NotFound(w, r)
			return
		}
		if got := r.Header.Get("Authorization"); got != "Token test-key" {
			t.Errorf("Authorization = %q", got)
		}
		_, _ = w.Write([]byte(`{"count":1,"next":null,"results":[{"id":42,"name":"Rain","username":"Ada","license":"http://creativecommons.org/publicdomain/zero/1.0/","duration":1.5,"category":"Soundscapes","subcategory":"Nature","url":"https://freesound.org/s/42/","tags":["rain"],"previews":{"preview-lq-mp3":"https://cdn.freesound.org/preview.mp3","preview-hq-ogg":"https://cdn.freesound.org/sound.ogg"}}]}`))
	}))
	defer server.Close()
	old := freesoundAPIBase
	freesoundAPIBase = server.URL
	t.Cleanup(func() { freesoundAPIBase = old })

	env := newAdminEnv(t)
	user := env.makeUser("freesound@example.com", false, false)
	env.loginAs(user)
	env.app.Settings.FreesoundAccessKey = "test-key"
	res, body := env.get("/api/v1/stock/audio/?type=ambience")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("audio search = %d: %s", res.StatusCode, body)
	}
	var payload struct {
		Configured bool `json:"configured"`
		Total      int  `json:"total"`
		Results    []struct {
			ID, Title, License string
			CreditRequired     bool `json:"credit_required"`
		} `json:"results"`
	}
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		t.Fatal(err)
	}
	if !payload.Configured || payload.Total != 1 || len(payload.Results) != 1 ||
		payload.Results[0].ID != "42" || payload.Results[0].Title != "Rain" ||
		payload.Results[0].License != "CC0 1.0" || payload.Results[0].CreditRequired {
		t.Fatalf("payload = %+v", payload)
	}
	if !strings.Contains(body, `"provider":"freesound"`) {
		t.Fatalf("missing provider: %s", body)
	}
}

func TestFreesoundImportStoresAudio(t *testing.T) {
	audio := makeAudioFixture(t, "wav")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/sounds/42/" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"id":42,"name":"Rain","username":"Ada","license":"http://creativecommons.org/publicdomain/zero/1.0/","duration":0.5,"category":"Soundscapes","subcategory":"Nature","url":"https://freesound.org/s/42/","tags":["rain"],"previews":{"preview-lq-mp3":"https://cdn.freesound.org/preview.mp3","preview-hq-ogg":"https://cdn.freesound.org/sound.ogg"}}`))
	}))
	defer server.Close()
	oldBase, oldDownload := freesoundAPIBase, freesoundDownload
	freesoundAPIBase = server.URL
	freesoundDownload = func(a *App, rawURL string, hosts map[string]bool, maxBytes int) ([]byte, error) { return audio, nil }
	t.Cleanup(func() { freesoundAPIBase, freesoundDownload = oldBase, oldDownload })

	env := newAdminEnv(t)
	user := env.makeUser("freesound-import@example.com", false, false)
	env.loginAs(user)
	env.app.Settings.FreesoundAccessKey = "test-key"
	res, body := env.doJSON("/api/v1/stock/audio/import/", `{"id":"42"}`)
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("freesound import = %d: %s", res.StatusCode, body)
	}
	var payload struct {
		Kind, Mime string
		DurationMS int            `json:"duration_ms"`
		Credit     map[string]any `json:"credit"`
	}
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Kind != "audio" || payload.Mime != "audio/wav" || payload.DurationMS != 500 ||
		payload.Credit["id"] != "42" || payload.Credit["required"] != false {
		t.Fatalf("freesound envelope = %+v", payload)
	}
}
