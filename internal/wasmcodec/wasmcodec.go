// Package wasmcodec runs the embedded upstream image-optimizer CLIs under
// wazero/WASI.
//
// The modules are the official command-line tools, not custom exported APIs:
//
//	cjpeg   4.1.5 (mozilla/mozjpeg, commit 6c9f0897...)
//	oxipng 10.2.1
//
// Building them for wasm32-wasip1 lets the Go side keep upstream's process
// model: a temporary work directory, a real CLI invocation, stdout/stderr and
// an exit code. The release binary embeds both .wasm files so no Rust,
// Emscripten or wasi-sdk toolchain is needed to build doory.
package wasmcodec

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
	"github.com/tetratelabs/wazero/sys"
)

// Ports backend/apps/exports/optimizers/builtin.py (the cjpeg/oxipng
// programs it invokes).

//go:embed cjpeg-4.1.5.wasm
var cjpegWasm []byte

//go:embed oxipng-10.2.1.wasm
var oxipngWasm []byte

// RunCjpeg runs the cjpeg CLI in workdir. args are the cjpeg arguments (not
// including argv[0]); it returns the process exit code.
func RunCjpeg(ctx context.Context, args []string, workdir string, stdout, stderr io.Writer) int {
	return run(ctx, "cjpeg", cjpegWasm, args, workdir, stdout, stderr)
}

// RunOxipng runs the oxipng CLI in workdir. args are the oxipng arguments.
func RunOxipng(ctx context.Context, args []string, workdir string, stdout, stderr io.Writer) int {
	return run(ctx, "oxipng", oxipngWasm, args, workdir, stdout, stderr)
}

func run(ctx context.Context, name string, wasm []byte, args []string, workdir string,
	stdout, stderr io.Writer) int {
	if stdout == nil {
		stdout = io.Discard
	}
	if stderr == nil {
		stderr = io.Discard
	}
	if workdir == "" {
		cwd, err := os.Getwd()
		if err != nil {
			fmt.Fprintf(stderr, "%s: cannot determine working directory: %v\n", name, err)
			return 1
		}
		workdir = cwd
	}
	workdir, err := filepath.Abs(workdir)
	if err != nil {
		fmt.Fprintf(stderr, "%s: invalid working directory: %v\n", name, err)
		return 1
	}

	config := wazero.NewRuntimeConfig().WithCloseOnContextDone(true)
	if cache := compilationCache(ctx); cache != nil {
		defer cache.Close(ctx)
		config = config.WithCompilationCache(cache)
	}
	runtime := wazero.NewRuntimeWithConfig(ctx, config)
	defer runtime.Close(ctx)
	wasi_snapshot_preview1.MustInstantiate(ctx, runtime)

	compiled, err := runtime.CompileModule(ctx, wasm)
	if err != nil {
		fmt.Fprintf(stderr, "%s: cannot load the embedded codec: %v\n", name, err)
		return 1
	}
	defer compiled.Close(ctx)

	moduleConfig := wazero.NewModuleConfig().
		WithName("").
		WithArgs(append([]string{name}, args...)...).
		WithFSConfig(wazero.NewFSConfig().WithDirMount(workdir, "/")).
		WithSysWalltime().
		WithSysNanotime().
		WithStdout(stdout).
		WithStderr(stderr)
	_, err = runtime.InstantiateModule(ctx, compiled, moduleConfig)
	if err == nil {
		return 0
	}
	var exit *sys.ExitError
	if errors.As(err, &exit) {
		return int(exit.ExitCode())
	}
	if ctx.Err() != nil {
		fmt.Fprintf(stderr, "%s: %v\n", name, ctx.Err())
		return 1
	}
	fmt.Fprintf(stderr, "%s: %v\n", name, err)
	return 1
}

// compilationCache keeps compiled WASM between short-lived worker processes.
// It is best-effort: a read-only or missing cache directory just means the
// module is compiled again.
func compilationCache(ctx context.Context) wazero.CompilationCache {
	dir := os.Getenv("DOORY_WASM_CACHE_DIR")
	if dir == "" {
		cacheRoot, err := os.UserCacheDir()
		if err != nil || cacheRoot == "" {
			return nil
		}
		dir = filepath.Join(cacheRoot, "doory", "wazero")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil
	}
	cache, err := wazero.NewCompilationCacheWithDir(dir)
	if err != nil {
		return nil
	}
	return cache
}
