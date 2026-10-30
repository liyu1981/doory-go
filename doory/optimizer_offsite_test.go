package doory

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCustomOffsiteOptimizerContract(t *testing.T) {
	img := image.NewNRGBA(image.Rect(0, 0, 64, 48))
	for y := 0; y < 48; y++ {
		for x := 0; x < 64; x++ {
			img.Set(x, y, color.NRGBA{uint8(x * 4), uint8(y * 5), uint8(x ^ y), 255})
		}
	}
	var input bytes.Buffer
	if err := png.Encode(&input, img); err != nil {
		t.Fatal(err)
	}
	small := image.NewNRGBA(image.Rect(0, 0, 64, 48))
	for y := 0; y < 48; y++ {
		for x := 0; x < 64; x++ {
			small.Set(x, y, color.NRGBA{20, 40, 60, 255})
		}
	}
	var output bytes.Buffer
	if err := png.Encode(&output, small); err != nil {
		t.Fatal(err)
	}
	var called bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		if r.Method != http.MethodPost || r.URL.Query().Get("format") != "keep" ||
			r.URL.Query().Get("metadata") != "none" {
			t.Errorf("unexpected custom request: %s %s", r.Method, r.URL.String())
		}
		if got := r.Header.Get("Content-Type"); got != "image/png" {
			t.Errorf("content type = %q", got)
		}
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("X-Format", "png")
		w.Header().Set("X-Width", "64")
		w.Header().Set("X-Height", "48")
		w.Header().Set("X-Output-Size", itoa(output.Len()))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(output.Bytes())
	}))
	defer server.Close()

	app := &App{Settings: &Settings{
		ImageOptimizers:         []string{"custom"},
		ImageOptimizerDefault:   "custom",
		ImageOptimizerCustomURL: server.URL,
		ImageOptimizerMaxBytes:  10 * 1024 * 1024,
		ImageOptimizerTimeout:   10,
		ExportImageMaxPixels:    40_000_000,
	}}
	result, derr := app.optimizeImage(input.Bytes(), optimizeRequest{
		Format: "png", Optimize: "custom", StripMetadata: true, Srgb: true,
	})
	if derr != nil {
		t.Fatalf("optimize: %+v", derr)
	}
	if !called || result.optimizer != "custom" || !result.optimized {
		t.Fatalf("result = %+v called=%v", result, called)
	}
	if !bytes.Equal(result.data, output.Bytes()) {
		t.Fatal("custom optimizer output was not kept")
	}
}

func TestKrakenDownloadURLMustStayOnKraken(t *testing.T) {
	for _, tc := range []struct {
		url string
		ok  bool
	}{
		{"https://dl.kraken.io/x.jpg", true},
		{"https://kraken.io/x.jpg", true},
		{"https://evil.example/x.jpg", false},
		{"https://dl.kraken.io.evil.example/x.jpg", false},
		{"http://dl.kraken.io/x.jpg", false},
		{"https://user:pass@dl.kraken.io/x.jpg", false},
	} {
		if got := optimizerKrakenDownload(tc.url); got != tc.ok {
			t.Errorf("optimizerKrakenDownload(%q) = %v, want %v", tc.url, got, tc.ok)
		}
	}
}
