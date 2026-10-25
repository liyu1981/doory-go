#!/usr/bin/env bash
# Build the single binary from a plain clone of this repository.
#
# This is the ONLY step a release build needs: no Node/pnpm, no Python, no
# oracle venv, no `.upstream/` mirror and no parity harness. Everything the
# binary serves is committed in the tree (doory/webapp, doory/data,
# doory/migrations), so `go build` is the whole pipeline.
#
# A fresh environment is handled: the script checks the Go toolchain, warms a
# cold module cache itself (and can build strictly offline), and never touches
# the upstream mirror.
#
#   ./scripts/build.sh                    # -> dist/doory (host arch)
#   ./scripts/build.sh -o /tmp/doory      # -> /tmp/doory
#   ./scripts/build.sh --offline          # no network: fail fast if modules are missing
#   ./scripts/build.sh --verbose          # show the go command
#   GOOS=linux GOARCH=arm64 ./scripts/build.sh -o dist/doory-arm64   # cross-compile
set -euo pipefail

REPO="$(cd "$(dirname "$0")/.." && pwd)"
cd "$REPO"

usage() {
  sed -n '2,20p' "$0" | sed 's/^# \{0,1\}//'
  exit "${1:-0}"
}

OUT="$REPO/dist/doory"
OFFLINE=0
VERBOSE=0
while [ $# -gt 0 ]; do
  case "$1" in
    -o|--output) OUT="${2:-}"; shift 2 ;;
    --offline)   OFFLINE=1; shift ;;
    --verbose|-v) VERBOSE=1; shift ;;
    -h|--help)   usage 0 ;;
    *) echo "build: unknown argument: $1 (see --help)" >&2; exit 2 ;;
  esac
done
[ -n "$OUT" ] || { echo "build: -o needs a path" >&2; exit 2; }

# ---- 1. Toolchain -----------------------------------------------------------------------------
# Only the Go toolchain is required. Everything else (Node, Python, uv, the
# oracle, git) belongs to the development/parity gates.
if ! command -v go >/dev/null 2>&1; then
  cat >&2 <<'MSG'
build: the Go toolchain is not installed (no `go` on PATH).
  Install Go >= 1.27: https://go.dev/dl/  (or your package manager: apt/dnf/brew install golang)
  Then re-run ./scripts/build.sh — nothing else is needed to build the binary.
MSG
  exit 127
fi

REQUIRED_GO="$(sed -n 's/^go \([0-9][0-9.]*\).*/\1/p' go.mod | head -1)"
CURRENT_GO="$(go env GOVERSION 2>/dev/null | sed 's/^go//')"
printf 'build: Go %s (go.mod wants >= %s), target %s/%s\n' \
  "${CURRENT_GO:-unknown}" "${REQUIRED_GO:-unknown}" "$(go env GOOS)" "$(go env GOARCH)"

# Go >= 1.21 downloads the toolchain named in go.mod by itself (GOTOOLCHAIN=auto).
if [ -n "$REQUIRED_GO" ] && [ -n "$CURRENT_GO" ]; then
  if [ "$(printf '%s\n%s\n' "$REQUIRED_GO" "$CURRENT_GO" | sort -V | head -1)" != "$REQUIRED_GO" ]; then
    echo "build: your Go is older than go.mod's $REQUIRED_GO; letting the toolchain switch (GOTOOLCHAIN=$(go env GOTOOLCHAIN)) fetch it" >&2
  fi
fi

# The frontend bundle and the content fixtures are what make the binary
# self-contained; a partial checkout (sparse/submodule-less) is the only way
# they can be missing.
for asset in doory/webapp/index.html doory/data/element_packs.json doory/data/size_presets.json doory/data/fonts; do
  if [ ! -e "$asset" ]; then
    echo "build: missing embedded asset $asset — this looks like a partial checkout; re-clone the repository" >&2
    exit 1
  fi
done
# NOT needed for the build (says so, so nobody goes looking for it):
#   .upstream/current  — parity oracle mirror (git-ignored, pnpm upstream-mirror)
#   .oracle/venv       — Django oracle venv
#   node_modules/      — frontend sources are already built into doory/webapp

# ---- 2. Modules (a cold cache is normal on a new machine) -------------------------------------
if [ "$OFFLINE" = "1" ]; then
  export GOFLAGS="-mod=readonly ${GOFLAGS:-}"
  export GOPROXY=off
  echo "build: offline mode (GOPROXY=off): every module in go.sum must already be cached"
else
  # Warm the cache up front: a no-op when it is already warm (a few ms), and on
  # a new machine it makes the download explicit instead of looking like a hang.
  # `go mod download` (no arguments) fetches exactly what building needs.
  echo "build: ensuring modules from go.sum are in the cache ..."
  go mod download
fi

build() {
  local log
  log="$(mktemp)"
  set +e
  if [ "$VERBOSE" = "1" ]; then
    CGO_ENABLED="${CGO_ENABLED:-0}" go build -trimpath -buildvcs=false -ldflags="-s -w" -o "$OUT" ./cmd/doory
    rc=$?
  else
    CGO_ENABLED="${CGO_ENABLED:-0}" go build -trimpath -buildvcs=false -ldflags="-s -w" -o "$OUT" ./cmd/doory 2>"$log"
    rc=$?
  fi
  set -e
  BUILD_LOG="$log"
  return $rc
}

mkdir -p "$(dirname "$OUT")"
if ! build; then
  # The usual fresh-machine failure: the module cache is empty. Download and
  # retry once, unless the caller asked for a strictly offline build.
  if [ "$OFFLINE" = "1" ]; then
    echo "build: cannot build offline — the module cache is incomplete:" >&2
    sed 's/^/  /' "$BUILD_LOG" >&2
    echo '  Run ./scripts/build.sh once with network (or: go mod download all) and retry.' >&2
    rm -f "$BUILD_LOG"
    exit 1
  fi
  echo "build: build failed; downloading the full module graph and retrying ..."
  go mod download all
  if ! build; then
    echo "build: failed:" >&2
    sed 's/^/  /' "$BUILD_LOG" >&2
    rm -f "$BUILD_LOG"
    exit 1
  fi
fi

SIZE="$(wc -c < "$OUT" | tr -d ' ')"
echo "build: $OUT ($SIZE bytes, $(go env GOOS)/$(go env GOARCH))"
echo "build: first run: '$OUT entrypoint :8111' migrates the database and installs the embedded content"
rm -f "${BUILD_LOG:-/dev/null}"
