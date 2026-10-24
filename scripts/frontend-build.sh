#!/usr/bin/env bash
# Rebuild the embedded frontend from the PINNED upstream frontend source.
#
# The mirror stays pristine: the tree is exported with `git archive` into a
# scratch directory, `npm ci && npm run build` runs there, and only the build
# output is copied into doory/webapp/ (which `go:embed` ships in the binary).
# Needs network (npm) and node; run `pnpm frontend:build` after an upstream leap.
set -euo pipefail

REPO="$(cd "$(dirname "$0")/.." && pwd)"
PIN_DIR="$REPO/.upstream/current"
SCRATCH="${DOORY_FRONTEND_SCRATCH:-$(mktemp -d -t doory-frontend-XXXXXX)}"

cleanup() {
  if [ "${DOORY_FRONTEND_SCRATCH:-}" = "" ]; then
    rm -rf "$SCRATCH"
  fi
}
trap cleanup EXIT

echo "frontend-build: exporting the pinned frontend to $SCRATCH"
git -C "$PIN_DIR" archive HEAD frontend | tar -x -C "$SCRATCH"

echo "frontend-build: installing dependencies (npm ci)"
(cd "$SCRATCH/frontend" && npm ci --no-audit --no-fund > /dev/null)

echo "frontend-build: building (tsc -b && vite build)"
(cd "$SCRATCH/frontend" && npm run build > /dev/null)

rm -rf "$REPO/doory/webapp"
mkdir -p "$REPO/doory/webapp"
cp -r "$SCRATCH/frontend/dist/." "$REPO/doory/webapp/"
echo "frontend-build: embedded tree refreshed ($(du -sh "$REPO/doory/webapp" | cut -f1))"
