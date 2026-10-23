#!/usr/bin/env bash
# Probe the Doory MCP server (apps/mcp, served by doory.mcp_asgi:application
# via uvicorn; Streamable HTTP at /mcp, stateless JSON responses, bearer PAT)
# and preserve raw transcripts as parity evidence for the mcp family (M12).
set -euo pipefail

REPO="$(cd "$(dirname "$0")/../.." && pwd)"
OUT="$REPO/test/parity/probes/0013-mcp"
WORK="/tmp/mcpwork"
BASE="${MCP_BASE:-http://127.0.0.1:8017}"
SHA="$(git -C "$REPO/.upstream/current" rev-parse HEAD)"
DATE="$(date -u +%Y-%m-%dT%H:%M:%SZ)"

mkdir -p "$OUT"
cat > "$OUT/meta.txt" << EOF
oracle_base=$BASE
upstream_commit=$SHA
probed_at=$DATE
settings=doory.settings.prod (MCP_ENABLED=true, MCP_PATH=/mcp, MCP_CALLS_PER_MINUTE=120, MCP_MAX_REQUEST_BYTES=4194304)
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

rm -rf "$WORK"; mkdir -p "$WORK/media" "$WORK/exports"
export PYTHONDONTWRITEBYTECODE=1 DJANGO_SETTINGS_MODULE=doory.settings.prod
export DJANGO_SECRET_KEY=parity-oracle-secret-key-0123456789
export SECURE_SSL_REDIRECT=false SECURE_COOKIES=false
export DATABASE_URL="sqlite:///$WORK/oracle.sqlite3"
export MEDIA_ROOT="$WORK/media" EXPORT_STORAGE_ROOT="$WORK/exports"
cd "$REPO/.upstream/current/backend"
env PYTHONDONTWRITEBYTECODE=1 DJANGO_SETTINGS_MODULE=doory.settings.prod \
  DJANGO_SECRET_KEY=parity-oracle-secret-key-0123456789 SECURE_SSL_REDIRECT=false SECURE_COOKIES=false \
  DATABASE_URL="sqlite:///$WORK/oracle.sqlite3" MEDIA_ROOT="$WORK/media" EXPORT_STORAGE_ROOT="$WORK/exports" \
  "$REPO/.oracle/venv/bin/python" manage.py migrate --noinput > /dev/null 2>&1
nohup env PYTHONDONTWRITEBYTECODE=1 DJANGO_SETTINGS_MODULE=doory.settings.prod \
  DJANGO_SECRET_KEY=parity-oracle-secret-key-0123456789 SECURE_SSL_REDIRECT=false SECURE_COOKIES=false \
  DATABASE_URL="sqlite:///$WORK/oracle.sqlite3" MEDIA_ROOT="$WORK/media" EXPORT_STORAGE_ROOT="$WORK/exports" \
  MCP_ENABLED=true MCP_PATH=/mcp MCP_CALLS_PER_MINUTE=120 MCP_MAX_REQUEST_BYTES=4194304 \
  "$REPO/.oracle/venv/bin/python" -m uvicorn doory.mcp_asgi:application --host 127.0.0.1 --port 8017 \
  > "$WORK/mcp-server.log" 2>&1 &
echo $! > "$WORK/mcp.pid"
nohup env PYTHONDONTWRITEBYTECODE=1 DJANGO_SETTINGS_MODULE=doory.settings.prod \
  DJANGO_SECRET_KEY=parity-oracle-secret-key-0123456789 SECURE_SSL_REDIRECT=false SECURE_COOKIES=false \
  DATABASE_URL="sqlite:///$WORK/oracle.sqlite3" MEDIA_ROOT="$WORK/media" EXPORT_STORAGE_ROOT="$WORK/exports" \
  "$REPO/.oracle/venv/bin/python" manage.py runserver 127.0.0.1:8012 --noreload > "$WORK/django-server.log" 2>&1 &
echo $! > "$WORK/django.pid"
for _ in $(seq 1 150); do
  if curl -sS -o /dev/null http://127.0.0.1:8012/api/v1/health/ 2>/dev/null; then break; fi; sleep 1
done
for _ in $(seq 1 90); do
  if curl -sS -o /dev/null http://127.0.0.1:8017/mcp/health 2>/dev/null; then break; fi; sleep 1
done
sleep 2

# --- MCP bearer token (user + PAT via ORM) ---
export PYTHONPATH="$REPO/.upstream/current/backend"
TOKEN="$(DATABASE_URL="sqlite:///$WORK/oracle.sqlite3" DJANGO_SECRET_KEY=parity-oracle-secret-key-0123456789 PYTHONPATH="$REPO/.upstream/current/backend" DJANGO_SETTINGS_MODULE=doory.settings.prod /home/yli/doory-go/.oracle/venv/bin/python /tmp/mcp_mktoken7.py 2>/dev/null | sed -n 's/TOKEN: //p')"
export MCP_TOKEN="$TOKEN"

probe GET  /mcp/health
probe POST /mcp -H 'Content-Type: application/json' -H 'Accept: application/json, text/event-stream' -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"probe","version":"0"}}}'
probe POST /mcp -H 'Content-Type: application/json' -H 'Accept: application/json, text/event-stream' -H "Authorization: Bearer $TOKEN" -d '{"jsonrpc":"2.0","method":"notifications/initialized"}'
probe POST /mcp -H 'Content-Type: application/json' -H 'Accept: application/json, text/event-stream' -H "Authorization: Bearer $TOKEN" -d '{"jsonrpc":"2.0","id":2,"method":"tools/list"}'
probe POST /mcp -H 'Content-Type: application/json' -H 'Accept: application/json, text/event-stream' -H "Authorization: Bearer $TOKEN" -d '{"jsonrpc":"2.0","id":10,"method":"tools/call","params":{"name":"list_designs","arguments":{}}}'
probe POST /mcp -H 'Content-Type: application/json' -H 'Accept: application/json, text/event-stream' -H "Authorization: Bearer $TOKEN" -d '{"jsonrpc":"2.0","id":11,"method":"tools/call","params":{"name":"create_design","arguments":{"title":"M12 Probe"}}}'

kill "$(cat "$WORK/mcp.pid")" "$(cat "$WORK/django.pid")" 2>/dev/null || true
sleep 2
echo "probe: wrote $n transcripts to $OUT"
