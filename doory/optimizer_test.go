package doory

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"testing"

	"golang.org/x/image/webp"
)

func TestZopfliPNGRoundTrip(t *testing.T) {
	img := image.NewNRGBA(image.Rect(0, 0, 32, 16))
	for x := 0; x < 32; x++ {
		for y := 0; y < 16; y++ {
			img.Set(x, y, color.NRGBA{uint8(x * 8), uint8(y * 16), 90, 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	smaller, err := zopfliPNG(buf.Bytes())
	if err != nil {
		t.Fatalf("zopfliPNG: %v", err)
	}
	decoded, err := png.Decode(bytes.NewReader(smaller))
	if err != nil {
		t.Fatalf("recompressed PNG does not decode: %v", err)
	}
	if decoded.Bounds() != img.Bounds() {
		t.Fatalf("bounds changed: %v", decoded.Bounds())
	}
	wantR, wantG, wantB, _ := img.At(5, 7).RGBA()
	gotR, gotG, gotB, _ := decoded.At(5, 7).RGBA()
	if wantR != gotR || wantG != gotG || wantB != gotB {
		t.Fatal("pixels changed")
	}
}

func TestShrinkBuiltinNeverGrows(t *testing.T) {
	img := image.NewNRGBA(image.Rect(0, 0, 4, 4))
	webpData, err := encodeWebP(img)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := shrinkBuiltin(webpData, "webp", 90, img, false); ok {
		t.Fatal("webp must be left as it is")
	}
	if _, err := webp.Decode(bytes.NewReader(webpData)); err != nil {
		t.Fatalf("encoder output is not WebP: %v", err)
	}
}
