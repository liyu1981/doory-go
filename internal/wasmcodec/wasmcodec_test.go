package wasmcodec

import (
	"bufio"
	"bytes"
	"context"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"testing"
)

func testImage() image.Image {
	img := image.NewNRGBA(image.Rect(0, 0, 64, 48))
	for y := 0; y < 48; y++ {
		for x := 0; x < 64; x++ {
			img.Set(x, y, color.NRGBA{uint8(x * 4), uint8(y * 5), uint8(x ^ y), 255})
		}
	}
	return img
}

func TestRunOxipng(t *testing.T) {
	t.Setenv("DOORY_WASM_CACHE_DIR", t.TempDir())
	dir := t.TempDir()
	img := testImage()
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, img); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "in.png"), encoded.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := RunOxipng(context.Background(),
		[]string{"-q", "-o", "2", "-t", "2", "--strip", "safe", "--out", "out.png", "in.png"},
		dir, &stdout, &stderr); code != 0 {
		t.Fatalf("oxipng exit %d: %s", code, stderr.String())
	}
	output, err := os.ReadFile(filepath.Join(dir, "out.png"))
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := png.Decode(bytes.NewReader(output))
	if err != nil {
		t.Fatalf("output is not PNG: %v", err)
	}
	if decoded.Bounds() != img.Bounds() {
		t.Fatalf("bounds = %v, want %v", decoded.Bounds(), img.Bounds())
	}
}

func TestRunCjpeg(t *testing.T) {
	t.Setenv("DOORY_WASM_CACHE_DIR", t.TempDir())
	dir := t.TempDir()
	img := testImage()
	file, err := os.Create(filepath.Join(dir, "in.ppm"))
	if err != nil {
		t.Fatal(err)
	}
	writer := bufio.NewWriter(file)
	bounds := img.Bounds()
	if _, err := writer.WriteString("P6\n64 48\n255\n"); err != nil {
		t.Fatal(err)
	}
	row := make([]byte, bounds.Dx()*3)
	for y := 0; y < 48; y++ {
		for x := 0; x < 64; x++ {
			r, g, b, _ := img.At(x, y).RGBA()
			row[x*3], row[x*3+1], row[x*3+2] = byte(r>>8), byte(g>>8), byte(b>>8)
		}
		if _, err := writer.Write(row); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := RunCjpeg(context.Background(),
		[]string{"-quality", "82", "-outfile", "out.jpg", "in.ppm"},
		dir, &stdout, &stderr); code != 0 {
		t.Fatalf("cjpeg exit %d: %s", code, stderr.String())
	}
	output, err := os.ReadFile(filepath.Join(dir, "out.jpg"))
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := jpeg.Decode(bytes.NewReader(output))
	if err != nil {
		t.Fatalf("output is not JPEG: %v", err)
	}
	if decoded.Bounds() != img.Bounds() {
		t.Fatalf("bounds = %v, want %v", decoded.Bounds(), img.Bounds())
	}
}
