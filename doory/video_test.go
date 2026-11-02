package doory

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func makeTestClip(t *testing.T, audio bool, codec string) []byte {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	path := filepath.Join(t.TempDir(), "clip.mp4")
	args := []string{"-hide_banner", "-loglevel", "error", "-f", "lavfi",
		"-i", "testsrc=size=320x240:rate=25:duration=1"}
	if audio {
		args = append(args, "-f", "lavfi", "-i", "sine=frequency=440:duration=1")
	}
	args = append(args, "-c:v", codec, "-pix_fmt", "yuv420p")
	if audio {
		args = append(args, "-c:a", "aac")
	}
	args = append(args, "-movflags", "+faststart", "-y", path)
	if err := exec.Command("ffmpeg", args...).Run(); err != nil {
		t.Skipf("ffmpeg cannot make the fixture: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestVideoUploadStoresWebReadyClip(t *testing.T) {
	env := newAdminEnv(t)
	user := env.makeUser("video@example.com", false, false)
	env.loginAs(user)
	body := makeTestClip(t, true, "libx264")
	res, response := env.postFileWithCSRF("/api/v1/assets/", "file", "Beach.mp4", string(body))
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("video upload = %d: %s", res.StatusCode, response)
	}
	var payload struct {
		Kind       string `json:"kind"`
		Status     string `json:"status"`
		Mime       string `json:"mime"`
		URL        string `json:"url"`
		PosterURL  string `json:"poster_url"`
		ThumbURL   string `json:"thumb_url"`
		AudioURL   string `json:"audio_url"`
		Width      int    `json:"width"`
		Height     int    `json:"height"`
		DurationMS int    `json:"duration_ms"`
		Video      struct {
			FPS       float64        `json:"fps"`
			HasAudio  bool           `json:"has_audio"`
			Filmstrip map[string]any `json:"filmstrip"`
		} `json:"video"`
	}
	if err := json.Unmarshal([]byte(response), &payload); err != nil {
		t.Fatal(err)
	}
	var dbFile, dbThumb, dbPoster, dbStrip, dbAudio string
	_ = env.db.QueryRow(`SELECT file, thumbnail, poster, filmstrip, audio FROM assets_asset`).Scan(&dbFile, &dbThumb, &dbPoster, &dbStrip, &dbAudio)
	t.Logf("db file=%q thumb=%q poster=%q strip=%q audio=%q", dbFile, dbThumb, dbPoster, dbStrip, dbAudio)
	if payload.Kind != "video" || payload.Status != "ready" || payload.Mime != "video/mp4" ||
		payload.Width != 320 || payload.Height != 240 ||
		!strings.HasSuffix(payload.URL, ".mp4") || !strings.HasSuffix(payload.PosterURL, ".webp") ||
		!strings.HasSuffix(payload.ThumbURL, ".webp") || (payload.Video.HasAudio && !strings.HasSuffix(payload.AudioURL, ".m4a")) {
		t.Fatalf("video envelope = %+v", payload)
	}
	if payload.Video.Filmstrip == nil || payload.Video.Filmstrip["frames"] != float64(2) {
		t.Fatalf("filmstrip = %+v", payload.Video.Filmstrip)
	}
	// Stored clip is faststart (moov before mdat).
	var file string
	if err := env.db.QueryRow(`SELECT file FROM assets_asset`).Scan(&file); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(env.mediaPath(file))
	if err != nil {
		t.Fatal(err)
	}
	moov, mdat := bytes.Index(data, []byte("moov")), bytes.Index(data, []byte("mdat"))
	if moov < 0 || mdat < 0 || moov > mdat {
		t.Fatalf("clip is not faststart")
	}
}

func TestVideoUploadConvertsInBackground(t *testing.T) {
	env := newAdminEnv(t)
	user := env.makeUser("video-bg@example.com", false, false)
	env.loginAs(user)
	body := makeTestClip(t, false, "mpeg4")
	res, response := env.postFileWithCSRF("/api/v1/assets/", "file", "Talk.mp4", string(body))
	if res.StatusCode != http.StatusAccepted {
		t.Fatalf("video upload = %d: %s", res.StatusCode, response)
	}
	var payload struct{ ID, Status string }
	if err := json.Unmarshal([]byte(response), &payload); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		res, response = env.get("/api/v1/assets/" + dashedUUID(payload.ID) + "/")
		var got struct{ Status, URL, Error string }
		if err := json.Unmarshal([]byte(response), &got); err != nil {
			t.Fatal(err)
		}
		if got.Status == "ready" {
			if !strings.HasSuffix(got.URL, ".mp4") {
				t.Fatalf("ready URL = %q", got.URL)
			}
			return
		}
		if got.Status == "failed" {
			t.Fatalf("conversion failed: %s", got.Error)
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatal("conversion did not finish")
}

func TestStockVideoSearchUsesConfiguredProvider(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/videos/search" {
			http.NotFound(w, r)
			return
		}
		_ = r
		_, _ = w.Write([]byte(`{"total_results":1,"videos":[{"id":123,"width":1920,"height":1080,"duration":10,"image":"https://images.pexels.com/x.jpg","url":"https://www.pexels.com/video/123/","user":{"name":"Ada","url":"https://www.pexels.com/@ada"},"video_files":[{"link":"https://videos.pexels.com/x.mp4","width":1280,"height":720,"file_type":"video/mp4"}]}]}`))
	}))
	defer server.Close()
	oldBase := videoPexelsBase
	videoPexelsBase = server.URL
	t.Cleanup(func() { videoPexelsBase = oldBase })

	env := newAdminEnv(t)
	user := env.makeUser("video-stock@example.com", false, false)
	env.loginAs(user)
	env.app.Settings.PexelsVideoAPIKey = "test-key"
	res, body := env.get("/api/v1/stock/videos/?provider=pexels_video&q=sea")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("stock search = %d: %s", res.StatusCode, body)
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
		payload.Results[0].ID != "123" || payload.Results[0].Provider != "pexels_video" {
		t.Fatalf("stock payload = %+v", payload)
	}
}

func TestVideoExportValidation(t *testing.T) {
	env := newAdminEnv(t)
	user := env.makeUser("video-export@example.com", false, false)
	env.loginAs(user)
	design := env.makeDesign(user, "Video design", "", false)
	env.app.Settings.ServerImageExportEnabled = true
	env.app.Settings.ServerVideoExportEnabled = false
	res, body := env.doJSON("/api/v1/designs/"+dashedUUID(design)+"/export/image/", `{"format":"mp4"}`)
	if res.StatusCode != http.StatusNotFound || !strings.Contains(body, "server_video_export_disabled") {
		t.Fatalf("disabled video export = %d: %s", res.StatusCode, body)
	}
	env.app.Settings.ServerVideoExportEnabled = true
	res, body = env.doJSON("/api/v1/designs/"+dashedUUID(design)+"/export/image/", `{"format":"mp4","resolution":9999}`)
	if res.StatusCode != http.StatusBadRequest || !strings.Contains(body, "invalid_resolution") {
		t.Fatalf("invalid video resolution = %d: %s", res.StatusCode, body)
	}
	res, body = env.doJSON("/api/v1/designs/"+dashedUUID(design)+"/export/image/", `{"format":"mp4","fps":7}`)
	if res.StatusCode != http.StatusBadRequest || !strings.Contains(body, "invalid_fps") {
		t.Fatalf("invalid video fps = %d: %s", res.StatusCode, body)
	}
}

func TestSweepVideoJobsCommand(t *testing.T) {
	env := newAdminEnv(t)
	user := env.makeUser("sweep@example.com", false, false)
	id := newUUID4Hex()
	now := env.now()
	env.exec(`INSERT INTO assets_asset
		(id, created_at, updated_at, kind, name, width, height, size, mime, tags, meta, file, thumbnail,
		 poster, filmstrip, audio, status, error, owner_id)
		VALUES (?, ?, ?, 'video', 'Old', 0, 0, 10, 'video/mp4', '[]', '{}', '', '', '', '', '', 'processing', '', ?)`,
		id, now, formatDjangoDateTime(time.Now().Add(-2*time.Hour)), user.ID)
	var stdout, stderr bytes.Buffer
	code := RunCLI(env.app, []string{"sweep_video_jobs"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("sweep exit %d: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Marked 1 interrupted video conversion(s) as failed.") {
		t.Fatalf("sweep output = %q", stdout.String())
	}
	if env.count(`SELECT COUNT(*) FROM assets_asset WHERE id = ? AND status = 'failed' AND size = 0 AND error = ?`,
		id, videoInterrupted) != 1 {
		t.Fatalf("processing row was not failed by the sweep")
	}
}

func TestStockVideoImportStoresClip(t *testing.T) {
	clip := makeTestClip(t, false, "libx264")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/videos/videos/123" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"id":123,"width":320,"height":240,"duration":1,"image":"https://images.pexels.com/x.jpg","url":"https://www.pexels.com/video/123/","user":{"name":"Ada","url":"https://www.pexels.com/@ada"},"video_files":[{"link":"https://videos.pexels.com/x.mp4","width":320,"height":240,"file_type":"video/mp4"}]}`))
	}))
	defer server.Close()
	oldBase, oldDownload := videoPexelsBase, videoStockDownload
	videoPexelsBase = server.URL
	videoStockDownload = func(a *App, rawURL string, hosts map[string]bool, maxBytes int) ([]byte, error) { return clip, nil }
	t.Cleanup(func() { videoPexelsBase, videoStockDownload = oldBase, oldDownload })

	env := newAdminEnv(t)
	user := env.makeUser("video-import@example.com", false, false)
	env.loginAs(user)
	env.app.Settings.PexelsVideoAPIKey = "key"
	res, body := env.doJSON("/api/v1/stock/videos/import/", `{"provider":"pexels_video","id":"123"}`)
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("video import = %d: %s", res.StatusCode, body)
	}
	var payload struct {
		Kind, Status, Mime string
		Video              struct{ Filmstrip map[string]any } `json:"video"`
	}
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Kind != "video" || payload.Status != "ready" || payload.Mime != "video/mp4" || payload.Video.Filmstrip == nil {
		t.Fatalf("video import envelope = %+v", payload)
	}
}
