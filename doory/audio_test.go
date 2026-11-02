package doory

import (
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func makeAudioFixture(t *testing.T, ext string) []byte {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	path := filepath.Join(t.TempDir(), "clip."+ext)
	codec := map[string]string{"mp3": "libmp3lame", "ogg": "libvorbis", "m4a": "aac",
		"webm": "libopus", "wav": "pcm_s16le"}[ext]
	args := []string{"-hide_banner", "-loglevel", "error", "-f", "lavfi", "-i", "sine=frequency=440:duration=0.5"}
	if codec != "" {
		args = append(args, "-c:a", codec)
	}
	args = append(args, "-y", path)
	if err := exec.Command("ffmpeg", args...).Run(); err != nil {
		t.Skipf("ffmpeg cannot make %s: %v", ext, err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestParseAudioContainers(t *testing.T) {
	for _, tc := range []struct{ ext, mime, codec string }{
		{"wav", "audio/wav", "pcm"}, {"mp3", "audio/mpeg", "mp3"},
		{"ogg", "audio/ogg", "vorbis"}, {"m4a", "audio/mp4", "aac"},
		{"webm", "audio/webm", "opus"},
	} {
		info, err := parseAudio(makeAudioFixture(t, tc.ext))
		if err != nil {
			t.Fatalf("%s: %v", tc.ext, err)
		}
		if info.MIME != tc.mime || info.Codec != tc.codec || info.Ext() != map[string]string{"wav": "wav", "mp3": "mp3", "ogg": "ogg", "m4a": "m4a", "webm": "weba"}[tc.ext] {
			t.Fatalf("%s = %+v", tc.ext, info)
		}
	}
}

func TestAudioUploadStoresOriginalBytes(t *testing.T) {
	env := newAdminEnv(t)
	user := env.makeUser("audio@example.com", false, false)
	env.loginAs(user)
	data := makeAudioFixture(t, "wav")
	res, body := env.postFileWithCSRF("/api/v1/assets/", "file", "Voice.wav", string(data))
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("audio upload = %d: %s", res.StatusCode, body)
	}
	var payload struct {
		Kind          string `json:"kind"`
		Mime          string `json:"mime"`
		Size          int    `json:"size"`
		DurationMS    int    `json:"duration_ms"`
		URL           string `json:"url"`
		Width, Height int
	}
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Kind != "audio" || payload.Mime != "audio/wav" || payload.Size != len(data) ||
		payload.DurationMS != 500 || payload.Width != 0 || payload.Height != 0 {
		t.Fatalf("audio envelope = %+v", payload)
	}
	var file string
	if err := env.db.QueryRow(`SELECT file, mime FROM assets_asset`).Scan(&file, new(string)); err != nil {
		t.Fatal(err)
	}
	stored, err := os.ReadFile(env.mediaPath(file))
	if err != nil {
		t.Fatal(err)
	}
	if string(stored) != string(data) {
		t.Fatal("audio bytes were not stored as uploaded")
	}
}

func TestAudioUploadRejectsVideoAndGarbage(t *testing.T) {
	env := newAdminEnv(t)
	user := env.makeUser("audio-bad@example.com", false, false)
	env.loginAs(user)
	for _, data := range [][]byte{[]byte("<html></html>"), []byte("RIFF\x24\x00\x00\x00WAVEjunk"), []byte("OggS\x00\x00\x00")} {
		res, _ := env.postFileWithCSRF("/api/v1/assets/", "file", "bad.mp3", string(data))
		if res.StatusCode != http.StatusBadRequest {
			t.Fatalf("garbage audio = %d", res.StatusCode)
		}
	}
}
