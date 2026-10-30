package wasmimage

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"testing"

	_ "golang.org/x/image/webp"
)

func testPixels(alpha bool) ([]byte, int, int) {
	const width, height = 64, 48
	channels := 3
	if alpha {
		channels = 4
	}
	pixels := make([]byte, width*height*channels)
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			i := (y*width + x) * channels
			pixels[i] = byte(x * 4)
			pixels[i+1] = byte(y * 5)
			pixels[i+2] = byte(x ^ y)
			if alpha {
				pixels[i+3] = byte(128 + (x+y)%128)
			}
		}
	}
	return pixels, width, height
}

func TestEncodeWebP(t *testing.T) {
	pixels, width, height := testPixels(false)
	out, err := EncodeWebP(pixels, width, height, 82, 4, false, false)
	if err != nil {
		t.Fatal(err)
	}
	img, _, err := image.Decode(bytes.NewReader(out))
	if err != nil {
		t.Fatalf("output is not an image: %v", err)
	}
	if img.Bounds().Dx() != width || img.Bounds().Dy() != height {
		t.Fatalf("bounds = %v", img.Bounds())
	}
}

func TestEncodeWebPAlpha(t *testing.T) {
	pixels, width, height := testPixels(true)
	out, err := EncodeWebP(pixels, width, height, 82, 4, false, true)
	if err != nil {
		t.Fatal(err)
	}
	img, _, err := image.Decode(bytes.NewReader(out))
	if err != nil {
		t.Fatalf("output is not an image: %v", err)
	}
	if _, _, _, a := img.At(0, 0).RGBA(); a != 0x8080 {
		t.Fatalf("alpha = %#x, want 0x8080", a)
	}
}

func TestEncodeJPEG(t *testing.T) {
	pixels, width, height := testPixels(false)
	out, err := EncodeJPEG(pixels, width, height, 90, true, false)
	if err != nil {
		t.Fatal(err)
	}
	img, err := jpeg.Decode(bytes.NewReader(out))
	if err != nil {
		t.Fatalf("output is not JPEG: %v", err)
	}
	if img.Bounds().Dx() != width || img.Bounds().Dy() != height {
		t.Fatalf("bounds = %v", img.Bounds())
	}
}

func TestRGBDataIsNotMutated(t *testing.T) {
	src := image.NewNRGBA(image.Rect(0, 0, 2, 2))
	src.SetNRGBA(0, 0, color.NRGBA{R: 1, G: 2, B: 3, A: 4})
	pixels, width, height := testPixels(false)
	before := append([]byte(nil), pixels...)
	if _, err := EncodeJPEG(pixels, width, height, 80, false, false); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(pixels, before) {
		t.Fatal("encoder mutated the input buffer")
	}
}

// The fixtures are Pillow 13.0.0 (libwebp 1.6.0 / libjpeg-turbo 3.1.0)
// output for testPixels, so these are byte-for-byte parity tests, not merely
// decode checks.
func TestEncodersMatchPillowBytes(t *testing.T) {
	rgb, width, height := testPixels(false)
	rgba, _, _ := testPixels(true)
	cases := []struct {
		name string
		got  func() ([]byte, error)
		file string
	}{
		{"webp", func() ([]byte, error) {
			return EncodeWebP(rgb, width, height, 82, 4, false, false)
		}, "testdata/pillow-webp-q82.webp"},
		{"webp-alpha", func() ([]byte, error) {
			return EncodeWebP(rgba, width, height, 82, 4, false, true)
		}, "testdata/pillow-webp-alpha-q82.webp"},
		{"jpeg", func() ([]byte, error) {
			return EncodeJPEG(rgb, width, height, 90, true, false)
		}, "testdata/pillow-jpeg-q90.jpg"},
		{"jpeg-progressive", func() ([]byte, error) {
			return EncodeJPEG(rgb, width, height, 90, true, true)
		}, "testdata/pillow-jpeg-progressive-q90.jpg"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.got()
			if err != nil {
				t.Fatal(err)
			}
			want, err := os.ReadFile(tc.file)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("encoded %d bytes, Pillow wrote %d; first difference follows\n%x\n%x",
					len(got), len(want), got, want)
			}
		})
	}
}
