#!/usr/bin/env bash
# Probe the live oracle and preserve raw transcripts as parity evidence.
#
# Usage:  tools/oracle/probe.sh <probe-dir-name>
# Requires the oracle on 127.0.0.1:8012 (tools/oracle/serve.sh).
# Output: test/parity/probes/<probe-dir-name>/{meta.txt,requests.txt,<N>-<slug>.{status,headers,body}}
set -euo pipefail

REPO="$(cd "$(dirname "$0")/../.." && pwd)"
PROBE_NAME="${1:?usage: probe.sh <probe-dir-name>}"
OUT="$REPO/test/parity/probes/$PROBE_NAME"
BASE="${ORACLE_BASE:-http://127.0.0.1:8012}"
SHA="$(git -C "$REPO/.upstream/current" rev-parse HEAD)"
DATE="$(date -u +%Y-%m-%dT%H:%M:%SZ)"

mkdir -p "$OUT"
cat > "$OUT/meta.txt" << EOF
oracle_base=$BASE
upstream_commit=$SHA
probed_at=$DATE
settings=doory.settings.test (SQLite, throttles off, locmem email)
note=raw evidence; never edit these files, only add new probe dirs
EOF

: > "$OUT/requests.txt"
n=0
probe() { # probe <method> <path> [curl args...]
  local method="$1" path="$2"; shift 2
  n=$((n + 1))
  local slug
  slug="$(echo "${method}_${path}" | tr -cs 'A-Za-z0-9' '-' | sed 's/^-//; s/-$//')"
  echo "$method $path $*" >> "$OUT/requests.txt"
  curl -sS -X "$method" \
    -o "$OUT/$(printf '%02d' "$n")-${slug}.body" \
    -D "$OUT/$(printf '%02d' "$n")-${slug}.headers" \
    -w '%{http_code}\n' \
    "$BASE$path" "$@" > "$OUT/$(printf '%02d' "$n")-${slug}.status"
}

# --- kernel family probes (M0) ---
probe GET  /api/v1/health/
probe GET  /api/v1/config/
probe GET  /api/v1/auth/csrf/
probe GET  /api/v1/nope/                          # handler404 shape
probe POST /api/v1/health/                        # 405 shape
probe GET  /api/v1/designs/                       # unauthenticated DRF shape
probe OPTIONS /api/v1/designs/ -H 'Origin: http://example.com' -H 'Access-Control-Request-Method: GET'
probe GET  /api/v1/schema/                        # drf-spectacular contract
probe GET  /sitemap.xml

echo "probe: wrote $n transcripts to $OUT"
