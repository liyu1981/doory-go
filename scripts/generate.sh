#!/usr/bin/env bash
# Regenerate every compiler-derived artifact. Never hand-edit the outputs.
#   - ledger/routes-v*.json + ledger/interfaces-v*.json  (tools/oracle/introspect.py)
#   - docs/parity/PORT_MAP.md upstream column + sections  (test/parity/cmd/portmapgen)
set -euo pipefail

REPO="$(cd "$(dirname "$0")/.." && pwd)"
cd "$REPO"

if [ ! -x .oracle/venv/bin/python ]; then
  echo "generate: .oracle/venv missing — run 'pnpm oracle:setup' first (installs the pinned oracle deps)" >&2
  exit 1
fi

env PYTHONDONTWRITEBYTECODE=1 DJANGO_SETTINGS_MODULE=doory.settings.test \
  .oracle/venv/bin/python tools/oracle/introspect.py

go run ./test/parity/cmd/portmapgen
