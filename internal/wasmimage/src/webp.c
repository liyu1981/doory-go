#include <stdint.h>
#include <stdlib.h>
#include <string.h>
#include <webp/encode.h>

__attribute__((export_name("doory_alloc")))
void *doory_alloc(unsigned int size) { return malloc(size); }

__attribute__((export_name("doory_free")))
void doory_free(void *ptr) { free(ptr); }

__attribute__((export_name("doory_webp_free")))
void doory_webp_free(void *ptr) { WebPFree(ptr); }

static uint64_t pack_result(const uint8_t *ptr, size_t len) {
  return ((uint64_t)len << 32) | (uint32_t)(uintptr_t)ptr;
}

/* Encode raw RGB or RGBA pixels with libwebp using the same WebPConfig knobs
 * Pillow's WebPImagePlugin sets: quality, method, lossless, alpha_quality=100,
 * exact=0.  No EXIF/XMP/ICC is written by this entry point. */
__attribute__((export_name("doory_webp_encode")))
uint64_t doory_webp_encode(const uint8_t *data, int width, int height, int stride,
                           float quality, int method, int lossless, int rgb) {
  if (width <= 0 || height <= 0 || data == NULL) return 0;
  WebPConfig config;
  if (!WebPConfigInit(&config)) return 0;
  config.lossless = lossless ? 1 : 0;
  config.quality = quality;
  config.method = method;
  config.alpha_quality = 100;
  config.exact = 0;
  if (!WebPValidateConfig(&config)) return 0;

  WebPPicture picture;
  if (!WebPPictureInit(&picture)) return 0;
  picture.width = width;
  picture.height = height;
  picture.use_argb = 1;
  if (rgb) {
    if (!WebPPictureImportRGB(&picture, data, stride)) {
      WebPPictureFree(&picture);
      return 0;
    }
  } else if (!WebPPictureImportRGBA(&picture, data, stride)) {
    WebPPictureFree(&picture);
    return 0;
  }

  WebPMemoryWriter writer;
  WebPMemoryWriterInit(&writer);
  picture.writer = WebPMemoryWrite;
  picture.custom_ptr = &writer;
  if (!WebPEncode(&config, &picture)) {
    WebPPictureFree(&picture);
    WebPMemoryWriterClear(&writer);
    return 0;
  }
  WebPPictureFree(&picture);
  return pack_result(writer.mem, writer.size);
}
