// Package wasmimage encodes raw pixels with the embedded libwebp and
// libjpeg-turbo builds.
//
// The modules export a tiny C ABI (see src/webp.c and src/jpeg.c) so the Go
// side can hand over an RGB/RGBA pixel buffer without a temporary file or a
// child process.  This keeps the release build CGO-free and cross-compilable
// while using the same libraries Pillow links: libwebp 1.6.0 and
// libjpeg-turbo 3.1.0 (JPEG API 6.2).
package wasmimage

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"sync"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
)

//go:embed webp-1.6.0.wasm
var webpWasm []byte

//go:embed jpeg-turbo-3.1.0.wasm
var jpegWasm []byte

// ErrEncode reports an encoder failure (invalid dimensions, OOM, or a
// libjpeg/libwebp error).
var ErrEncode = errors.New("wasm image encoder failed")

type codec struct {
	name string
	wasm []byte

	once     sync.Once
	runtime  wazero.Runtime
	compiled wazero.CompiledModule
	initErr  error
}

var (
	webpCodec = &codec{name: "libwebp", wasm: webpWasm}
	jpegCodec = &codec{name: "libjpeg-turbo", wasm: jpegWasm}
)

func (c *codec) init() error {
	c.once.Do(func() {
		ctx := context.Background()
		c.runtime = wazero.NewRuntimeWithConfig(ctx, wazero.NewRuntimeConfig())
		wasi_snapshot_preview1.MustInstantiate(ctx, c.runtime)
		c.compiled, c.initErr = c.runtime.CompileModule(ctx, c.wasm)
	})
	return c.initErr
}

// EncodeWebP encodes RGB (hasAlpha false) or RGBA pixels as WebP. The
// settings mirror Pillow's WebPImagePlugin defaults: method 4,
// alpha_quality 100, exact 0.
func EncodeWebP(pixels []byte, width, height int, quality float32, method int, lossless, hasAlpha bool) ([]byte, error) {
	if width <= 0 || height <= 0 {
		return nil, fmt.Errorf("%w: invalid dimensions %dx%d", ErrEncode, width, height)
	}
	channels := 1
	if hasAlpha {
		channels = 4
	} else {
		channels = 3
	}
	if len(pixels) < width*height*channels {
		return nil, fmt.Errorf("%w: pixel buffer is too short", ErrEncode)
	}
	return webpCodec.encode(pixels, width, height, channels, func(ctx context.Context, inst api.Module, ptr uint32) (uint64, error) {
		encode := inst.ExportedFunction("doory_webp_encode")
		if encode == nil {
			return 0, fmt.Errorf("%w: missing doory_webp_encode", ErrEncode)
		}
		results, err := encode.Call(ctx, uint64(ptr), uint64(width), uint64(height), uint64(width*channels),
			api.EncodeF32(quality), uint64(method), boolToUint64(lossless), boolToUint64(!hasAlpha))
		if err != nil || len(results) == 0 {
			return 0, err
		}
		return results[0], nil
	}, "doory_webp_free")
}

// EncodeJPEG encodes RGB pixels as JPEG, mirroring Pillow's JPEG encoder
// contract: quality via jpeg_set_quality(..., TRUE), optimize_coding and an
// optional progressive scan script.
func EncodeJPEG(pixels []byte, width, height, quality int, optimize, progressive bool) ([]byte, error) {
	if width <= 0 || height <= 0 {
		return nil, fmt.Errorf("%w: invalid dimensions %dx%d", ErrEncode, width, height)
	}
	if len(pixels) < width*height*3 {
		return nil, fmt.Errorf("%w: pixel buffer is too short", ErrEncode)
	}
	return jpegCodec.encode(pixels, width, height, 3, func(ctx context.Context, inst api.Module, ptr uint32) (uint64, error) {
		encode := inst.ExportedFunction("doory_jpeg_encode")
		if encode == nil {
			return 0, fmt.Errorf("%w: missing doory_jpeg_encode", ErrEncode)
		}
		results, err := encode.Call(ctx, uint64(ptr), uint64(width), uint64(height), uint64(quality),
			boolToUint64(optimize), boolToUint64(progressive))
		if err != nil || len(results) == 0 {
			return 0, err
		}
		return results[0], nil
	}, "doory_free")
}

func (c *codec) encode(pixels []byte, width, height, channels int, call func(context.Context, api.Module, uint32) (uint64, error), freeName string) ([]byte, error) {
	if err := c.init(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrEncode, err)
	}
	ctx := context.Background()
	inst, err := c.runtime.InstantiateModule(ctx, c.compiled, wazero.NewModuleConfig().WithName(""))
	if err != nil {
		return nil, fmt.Errorf("%w: instantiate %s: %v", ErrEncode, c.name, err)
	}
	defer inst.Close(ctx)

	alloc := inst.ExportedFunction("doory_alloc")
	free := inst.ExportedFunction("doory_free")
	if alloc == nil || free == nil {
		return nil, fmt.Errorf("%w: missing allocator in %s", ErrEncode, c.name)
	}
	memory := inst.Memory()
	if memory == nil {
		return nil, fmt.Errorf("%w: missing memory in %s", ErrEncode, c.name)
	}
	results, err := alloc.Call(ctx, uint64(len(pixels)))
	if err != nil {
		return nil, fmt.Errorf("%w: alloc: %v", ErrEncode, err)
	}
	inputPtr := uint32(results[0])
	if inputPtr == 0 {
		return nil, fmt.Errorf("%w: out of memory", ErrEncode)
	}
	defer free.Call(ctx, uint64(inputPtr)) //nolint:errcheck
	if !memory.Write(inputPtr, pixels) {
		return nil, fmt.Errorf("%w: cannot write pixel buffer", ErrEncode)
	}

	packed, err := call(ctx, inst, inputPtr)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrEncode, err)
	}
	if packed == 0 {
		return nil, ErrEncode
	}
	length := uint32(packed >> 32)
	outputPtr := uint32(packed)
	if length == 0 || outputPtr == 0 {
		return nil, ErrEncode
	}
	output, ok := memory.Read(outputPtr, length)
	if !ok {
		return nil, fmt.Errorf("%w: cannot read encoded bytes", ErrEncode)
	}
	out := append([]byte(nil), output...)
	if release := inst.ExportedFunction(freeName); release != nil {
		_, _ = release.Call(ctx, uint64(outputPtr))
	}
	return out, nil
}

func boolToUint64(value bool) uint64 {
	if value {
		return 1
	}
	return 0
}
