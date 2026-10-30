#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <jpeglib.h>

__attribute__((export_name("doory_alloc")))
void *doory_alloc(unsigned int size) { return malloc(size); }

__attribute__((export_name("doory_free")))
void doory_free(void *ptr) { free(ptr); }

static uint64_t pack_result(const uint8_t *ptr, size_t len) {
  return ((uint64_t)len << 32) | (uint32_t)(uintptr_t)ptr;
}

/* Encode raw RGB pixels with libjpeg-turbo, mirroring Pillow's JPEG encoder
 * contract: quality via jpeg_set_quality(..., TRUE), optimize_coding and an
 * optional progressive scan script. */
__attribute__((export_name("doory_jpeg_encode")))
uint64_t doory_jpeg_encode(const uint8_t *rgb, int width, int height, int quality,
                           int optimize, int progressive) {
  if (width <= 0 || height <= 0 || rgb == NULL) return 0;

  struct jpeg_compress_struct cinfo;
  struct jpeg_error_mgr jerr;
  unsigned char *out = NULL;
  unsigned long out_len = 0;

  cinfo.err = jpeg_std_error(&jerr);
  jpeg_create_compress(&cinfo);
  jpeg_mem_dest(&cinfo, &out, &out_len);
  cinfo.image_width = width;
  cinfo.image_height = height;
  cinfo.input_components = 3;
  cinfo.in_color_space = JCS_RGB;
  jpeg_set_defaults(&cinfo);
  jpeg_set_quality(&cinfo, quality, TRUE);
  cinfo.optimize_coding = optimize ? TRUE : FALSE;
  if (progressive) jpeg_simple_progression(&cinfo);
  jpeg_start_compress(&cinfo, TRUE);
  while (cinfo.next_scanline < cinfo.image_height) {
    JSAMPROW row = (JSAMPROW)(rgb + (size_t)cinfo.next_scanline * width * 3);
    jpeg_write_scanlines(&cinfo, &row, 1);
  }
  jpeg_finish_compress(&cinfo);
  jpeg_destroy_compress(&cinfo);
  return pack_result(out, out_len);
}
