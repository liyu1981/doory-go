// Image decoding/encoding shared by the endpoints that make pictures
// (design thumbnails, /exports/optimize/ and, once ported, the asset video
// pipeline).
//
// Decoding accepts what the ported stack can read (PNG, JPEG, GIF, WebP —
// AVIF is loud-pending, D21). WebP and JPEG encoding use the embedded
// libwebp 1.6.0 and libjpeg-turbo 3.1.0 modules, matching the libraries
// Pillow 13 links; PNG stays on the standard library.
package doory

import (
	"bytes"
	"errors"
	"image"
	"image/color"
	"image/gif"
	"image/png"

	"doory-go/internal/wasmimage"
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
	// CatmullRom is the closest stdlib filter to Lanczos. Resampling still
	// differs from Pillow's Lanczos at the pixel level (D15).
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
	switch format {
	case "png":
		var buf bytes.Buffer
		if err := png.Encode(&buf, img); err != nil {
			return nil, err
		}
		return buf.Bytes(), nil
	case "jpg", "jpeg":
		return encodeJPEG(flattenOnWhite(img), 90, false)
	case "webp":
		return encodeWebP(img)
	default:
		return nil, errors.New("unsupported format")
	}
}

// encodeWebP makes a WebP file at Pillow's make_thumbnail defaults:
// quality 82, method 4, lossy.
func encodeWebP(img image.Image) ([]byte, error) { return encodeWebPQuality(img, 82, false) }

// encodeWebPQuality encodes WebP with Pillow's WebPImagePlugin settings:
// method 4, alpha_quality 100, exact 0.
func encodeWebPQuality(img image.Image, quality float32, lossless bool) ([]byte, error) {
	pixels, width, height, alpha := webpPixels(img)
	return wasmimage.EncodeWebP(pixels, width, height, quality, 4, lossless, alpha)
}

// webpPixels returns RGB when the image is fully opaque and RGBA otherwise.
func webpPixels(img image.Image) ([]byte, int, int, bool) {
	b := img.Bounds()
	width, height := b.Dx(), b.Dy()
	if imageHasAlpha(img) {
		return rgbaPixels(img), width, height, true
	}
	return rgbPixels(img), width, height, false
}

// imageHasAlpha reports whether any pixel is not fully opaque.
func imageHasAlpha(img image.Image) bool {
	switch src := img.(type) {
	case *image.NRGBA:
		for y := 0; y < src.Rect.Dy(); y++ {
			row := src.Pix[y*src.Stride : y*src.Stride+src.Rect.Dx()*4]
			for i := 3; i < len(row); i += 4 {
				if row[i] != 0xff {
					return true
				}
			}
		}
		return false
	case *image.RGBA:
		for y := 0; y < src.Rect.Dy(); y++ {
			row := src.Pix[y*src.Stride : y*src.Stride+src.Rect.Dx()*4]
			for i := 3; i < len(row); i += 4 {
				if row[i] != 0xff {
					return true
				}
			}
		}
		return false
	case *image.Gray, *image.Gray16, *image.YCbCr:
		return false
	}
	b := img.Bounds()
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			if _, _, _, a := img.At(x, y).RGBA(); a != 0xffff {
				return true
			}
		}
	}
	return false
}

// rgbaPixels copies non-premultiplied RGBA bytes.
func rgbaPixels(img image.Image) []byte {
	b := img.Bounds()
	width, height := b.Dx(), b.Dy()
	out := make([]byte, width*height*4)
	if src, ok := img.(*image.NRGBA); ok {
		for y := 0; y < height; y++ {
			srcRow := src.Pix[(b.Min.Y+y-src.Rect.Min.Y)*src.Stride:]
			copy(out[y*width*4:(y+1)*width*4], srcRow[(b.Min.X-src.Rect.Min.X)*4:(b.Min.X-src.Rect.Min.X)*4+width*4])
		}
		return out
	}
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			c := color.NRGBAModel.Convert(img.At(b.Min.X+x, b.Min.Y+y)).(color.NRGBA)
			i := (y*width + x) * 4
			out[i], out[i+1], out[i+2], out[i+3] = c.R, c.G, c.B, c.A
		}
	}
	return out
}

// rgbPixels copies non-premultiplied RGB bytes (alpha is dropped).
func rgbPixels(img image.Image) []byte {
	b := img.Bounds()
	width, height := b.Dx(), b.Dy()
	out := make([]byte, width*height*3)
	if src, ok := img.(*image.NRGBA); ok {
		for y := 0; y < height; y++ {
			for x := 0; x < width; x++ {
				i := ((b.Min.Y+y-src.Rect.Min.Y)*src.Stride + (b.Min.X+x-src.Rect.Min.X)*4)
				o := (y*width + x) * 3
				out[o], out[o+1], out[o+2] = src.Pix[i], src.Pix[i+1], src.Pix[i+2]
			}
		}
		return out
	}
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			c := color.NRGBAModel.Convert(img.At(b.Min.X+x, b.Min.Y+y)).(color.NRGBA)
			i := (y*width + x) * 3
			out[i], out[i+1], out[i+2] = c.R, c.G, c.B
		}
	}
	return out
}

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
