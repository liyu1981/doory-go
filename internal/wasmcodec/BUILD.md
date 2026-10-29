# Embedded WASI image codecs

The release binary embeds two official upstream CLIs, compiled to
`wasm32-wasip1`:

| file | source | version | sha256 |
|---|---|---|---|
| `cjpeg-4.1.5.wasm` | mozilla/mozjpeg | v4.1.5 (`6c9f0897afa1c2738d7222a0a9ab49e8b536a267`) | `8733e8fd2ab463e8321090c8c7a00aa09acd5a7b9166a8dc5987e04b6f116784` |
| `oxipng-10.2.1.wasm` | oxipng/oxipng | v10.2.1 | `eb2a24583007758f18370bc2c4c25a7e5b5a9275af674e328e86a40198e68ef0` |

These are the **official CLI tools**, not custom exported APIs. They are started
as child processes by `doory mozjpeg` / `doory oxipng` through wazero/WASI, so
the Go optimizer keeps upstream’s temporary-directory + command + exit-code
model.

## Rebuilding cjpeg

Use wasi-sdk, not Emscripten: Emscripten’s standalone build did not emit
`path_open`, so the official `cjpeg` could not open its input file. The wasi-sdk
build imports the normal WASI filesystem calls.

```bash
git clone --depth 1 --branch v4.1.5 https://github.com/mozilla/mozjpeg /tmp/mozjpeg
docker run --rm -v /tmp/mozjpeg:/work -w /work \
  ghcr.io/webassembly/wasi-sdk:wasi-sdk-24 bash -lc '
    cmake -S /work -B /work/build-wasi \
      -DCMAKE_TOOLCHAIN_FILE=/opt/wasi-sdk/share/cmake/wasi-sdk.cmake \
      -DCMAKE_BUILD_TYPE=Release \
      -DENABLE_SHARED=0 -DPNG_SUPPORTED=0 -DWITH_TURBOJPEG=0 -DWITH_SIMD=0 \
      -DCMAKE_C_FLAGS=-O3
    cmake --build /work/build-wasi --target cjpeg-static -j4
  '
cp /tmp/mozjpeg/build-wasi/cjpeg-static cjpeg-4.1.5.wasm
```

## Rebuilding oxipng

```bash
git clone --depth 1 --branch v10.2.1 https://github.com/oxipng/oxipng /tmp/oxipng
cd /tmp/oxipng
rustup target add wasm32-wasip1
# libdeflate's C build needs a WASI sysroot (wasi-libc / wasi-sdk).
WASI_SYSROOT=/path/to/wasi-sysroot \
  cargo build --release --target wasm32-wasip1 \
  --no-default-features --features binary
cp target/wasm32-wasip1/release/oxipng.wasm oxipng-10.2.1.wasm
```

`--no-default-features --features binary` keeps the CLI, libdeflater and
single-threaded behavior; `-t/--threads` remains accepted by the CLI even
though the WASM build is single-threaded. It avoids the rayon/zopfli features,
which do not add anything for the upstream `-o 2` path.
