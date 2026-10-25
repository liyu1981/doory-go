// Image decoding/encoding shared by the endpoints that make pictures
// (design thumbnails, /exports/optimize/ and, once ported, the asset video
// pipeline).
//
// Decoding accepts what the ported stack can read (PNG, JPEG, GIF, WebP —
// AVIF is loud-pending, D21). Encoding is WebP via a pure-Go lossless encoder
// (D20: valid WebP, identical pixels, bytes unlike Pillow's) plus PNG/JPEG
// from the standard library.
package doory

import (
	"bytes"
	"errors"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"

	"github.com/HugoSmits86/nativewebp"
	xdraw "golang.org/x/image/draw"
	_ "golang.org/x/image/webp" // register the WebP decoder for image.Decode
)

// imageFormatsSupported are the formats this server can read and write
// (used by /exports/optimize/ and /config/).
var imageFormatsSupported = []string{"png", "jpg", "webp"}

// decodeImageData decodes an upload, returning the image and its format
// ("png", "jpeg", "gif", "webp"). Unsupported content is an error.
func decodeImageData(data []byte) (image.Image, string, error) {
	if len(data) == 0 {
		return nil, "", errors.New("empty")
	}
	img, format, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, "", err
	}
	return img, format, nil
}

// decodeImageConfig reads only the header (dimension checks before a full
// decode, like Pillow's probe).
func decodeImageConfig(data []byte) (int, int, string, error) {
	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return 0, 0, "", err
	}
	return cfg.Width, cfg.Height, format, nil
}

// fitThumbnail scales `src` down (never up) so its longest side is at most
// `maxSide`, preserving the aspect ratio — Pillow's Image.thumbnail(size).
func fitThumbnail(src image.Image, maxSide int) image.Image {
	b := src.Bounds()
	width, height := b.Dx(), b.Dy()
	if width <= maxSide && height <= maxSide {
		return toRGBA(src)
	}
	scale := float64(maxSide) / float64(width)
	if height > width {
		scale = float64(maxSide) / float64(height)
	}
	targetW := int(float64(width)*scale + 0.5)
	targetH := int(float64(height)*scale + 0.5)
	if targetW < 1 {
		targetW = 1
	}
	if targetH < 1 {
		targetH = 1
	}
	dst := image.NewNRGBA(image.Rect(0, 0, targetW, targetH))
	// CatmullRom is the closest stdlib filter to Lanczos; pixels are compared
	// structurally, never byte-wise (D15/D20).
	xdraw.CatmullRom.Scale(dst, dst.Bounds(), src, b, xdraw.Src, nil)
	return dst
}

// toRGBA copies any image into an NRGBA (alpha preserved).
func toRGBA(src image.Image) image.Image {
	if rgba, ok := src.(*image.NRGBA); ok {
		return rgba
	}
	b := src.Bounds()
	dst := image.NewNRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	xdraw.Draw(dst, dst.Bounds(), src, b.Min, xdraw.Src)
	return dst
}

// encodeImage encodes an image in one of the supported formats.
func encodeImage(img image.Image, format string) ([]byte, error) {
	var buf bytes.Buffer
	switch format {
	case "png":
		if err := png.Encode(&buf, img); err != nil {
			return nil, err
		}
	case "jpg", "jpeg":
		if err := jpeg.Encode(&buf, flattenOnWhite(img), &jpeg.Options{Quality: 90}); err != nil {
			return nil, err
		}
	case "webp":
		if err := nativewebp.Encode(&buf, img, nil); err != nil {
			return nil, err
		}
	default:
		return nil, errors.New("unsupported format")
	}
	return buf.Bytes(), nil
}

// encodeWebP makes a WebP file (lossless; D20).
func encodeWebP(img image.Image) ([]byte, error) { return encodeImage(img, "webp") }

// flattenOnWhite ports the JPG flattening the oracle does for opaque formats.
func flattenOnWhite(src image.Image) image.Image {
	b := src.Bounds()
	dst := image.NewRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	white := color.RGBA{255, 255, 255, 255}
	for y := 0; y < b.Dy(); y++ {
		for x := 0; x < b.Dx(); x++ {
			r, g, bl, a := src.At(b.Min.X+x, b.Min.Y+y).RGBA()
			if a == 0xffff {
				dst.SetRGBA(x, y, color.RGBA{uint8(r >> 8), uint8(g >> 8), uint8(bl >> 8), 255})
				continue
			}
			af := float64(a) / 65535.0
			dst.SetRGBA(x, y, color.RGBA{
				uint8(float64(r>>8)*af + float64(white.R)*(1-af)),
				uint8(float64(g>>8)*af + float64(white.G)*(1-af)),
				uint8(float64(bl>>8)*af + float64(white.B)*(1-af)),
				255,
			})
		}
	}
	return dst
}

// decodeGIFFirstFrame picks the first frame of a GIF (Pillow's img.seek(0)).
func decodeGIFFirstFrame(data []byte) (image.Image, error) {
	g, err := gif.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	return g, nil
}
