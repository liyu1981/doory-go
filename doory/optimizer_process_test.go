package doory

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"os/exec"
	"path/filepath"
	"testing"
)

// The built-in optimizer must really self-invoke the embedded WASI workers:
// build the release binary, point codecSelf at it, and run one PNG and one JPG
// through the same parent code the export endpoint uses.
func TestOptimizeBuiltinProcess(t *testing.T) {
	cwd, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(t.TempDir(), "doory")
	build := exec.Command("go", "build", "-o", exe, "./cmd/doory")
	build.Dir = filepath.Dir(cwd)
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build doory worker: %v\n%s", err, out)
	}
	oldSelf := codecSelf
	codecSelf = func() (string, error) { return exe, nil }
	t.Cleanup(func() { codecSelf = oldSelf })
	t.Setenv("DOORY_WASM_CACHE_DIR", t.TempDir())

	img := image.NewNRGBA(image.Rect(0, 0, 512, 512))
	for y := 0; y < 512; y++ {
		for x := 0; x < 512; x++ {
			img.Set(x, y, color.NRGBA{uint8(x / 2), uint8(y / 2), uint8((x * y) >> 8), 255})
		}
	}
	app := &App{Settings: &Settings{ImageOptimizerTimeout: 30}}

	var pngInput bytes.Buffer
	if err := png.Encode(&pngInput, img); err != nil {
		t.Fatal(err)
	}
	pngOutput, applied := app.optimizeBuiltin(pngInput.Bytes(), "png", 0, img,
		optimizeRequest{StripMetadata: true, Srgb: true})
	if !applied || len(pngOutput) >= pngInput.Len() {
		t.Fatalf("PNG worker: %d -> %d, applied=%v", pngInput.Len(), len(pngOutput), applied)
	}

	var jpgInput bytes.Buffer
	if err := jpeg.Encode(&jpgInput, img, &jpeg.Options{Quality: 60}); err != nil {
		t.Fatal(err)
	}
	jpgOutput, applied := app.optimizeBuiltin(jpgInput.Bytes(), "jpg", 60, img,
		optimizeRequest{StripMetadata: true, Srgb: true})
	if !applied || len(jpgOutput) >= jpgInput.Len() {
		t.Fatalf("JPG worker: %d -> %d, applied=%v", jpgInput.Len(), len(jpgOutput), applied)
	}
}
