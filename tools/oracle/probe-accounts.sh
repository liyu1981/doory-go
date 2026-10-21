#!/usr/bin/env bash
# Probe the accounts surface of the pinned oracle (prod settings, plain-HTTP
# local opt-outs) and preserve raw transcripts as parity evidence for the
# accounts family (M2). Two oracle runs: the per-IP `auth` throttle scope is
# 20/min and the lockout probe alone needs 11.
set -uo pipefail

REPO="$(cd "$(dirname "$0")/../.." && pwd)"
OUT="$REPO/test/parity/probes/0002-accounts"
WORK="$REPO/.oracle/probe0002"
BASE="http://127.0.0.1:8015"
SHA="$(git -C "$REPO/.upstream/current" rev-parse HEAD)"

rm -rf "$WORK"; mkdir -p "$OUT" "$WORK/media" "$WORK/exports"
cat > "$OUT/meta.txt" << EOF
oracle_base=$BASE
upstream_commit=$SHA
probed_at=$(date -u +%Y-%m-%dT%H:%M:%SZ)
settings=doory.settings.prod (SECURE_SSL_REDIRECT=false, SECURE_COOKIES=false; EMAIL console)
note=raw evidence; never edit these files, only add new probe dirs
EOF
: > "$OUT/requests.txt"

boot() {
  env PYTHONDONTWRITEBYTECODE=1 DJANGO_SETTINGS_MODULE=doory.settings.prod \
    DJANGO_SECRET_KEY=parity-oracle-secret-key-0123456789 \
    SECURE_SSL_REDIRECT=false SECURE_COOKIES=false \
    DATABASE_URL="sqlite:///$WORK/oracle.sqlite3" \
    MEDIA_ROOT="$WORK/media" EXPORT_STORAGE_ROOT="$WORK/exports" \
    "$REPO/.oracle/venv/bin/python" "$REPO/.upstream/current/backend/manage.py" migrate --noinput > /dev/null 2>&1
  (cd "$REPO/.upstream/current/backend" && env PYTHONDONTWRITEBYTECODE=1 DJANGO_SETTINGS_MODULE=doory.settings.prod \
    DJANGO_SECRET_KEY=parity-oracle-secret-key-0123456789 \
    SECURE_SSL_REDIRECT=false SECURE_COOKIES=false \
    DATABASE_URL="sqlite:///$WORK/oracle.sqlite3" \
    MEDIA_ROOT="$WORK/media" EXPORT_STORAGE_ROOT="$WORK/exports" \
    "$REPO/.oracle/venv/bin/python" manage.py runserver 127.0.0.1:8015 --noreload > "$WORK/server.log" 2>&1 &)
  for _ in $(seq 1 60); do
    curl -sS -o /dev/null "$BASE/api/v1/health/" 2>/dev/null && return 0
    sleep 0.5
  done
  echo "oracle never became healthy" >&2; exit 1
}

die() { pkill -f "runserver 127.0.0.1:8015" 2>/dev/null; sleep 1; }

n=0
probe() { # probe <jar-name> <method> <path> [curl args...]
  local jar="$1" method="$2" path="$3"; shift 3
  n=$((n + 1))
  local slug
  slug="$(echo "${method}_${path}" | tr -cs 'A-Za-z0-9' '-' | sed 's/^-//; s/-$//')"
  echo "$method $path $*" >> "$OUT/requests.txt"
  curl -sS -c "$WORK/$jar.jar" -b "$WORK/$jar.jar" -X "$method" \
    -o "$OUT/$(printf '%02d' "$n")-${slug}.body" \
    -D "$OUT/$(printf '%02d' "$n")-${slug}.headers" \
    -w '%{http_code}\n' \
    "$BASE$path" "$@" > "$OUT/$(printf '%02d' "$n")-${slug}.status"
}
csrf() { awk '$6=="csrftoken"{print $7}' "$WORK/$1.jar"; }

boot
# ---- run A: register / login / me / logout / password change ----
probe anon GET /api/v1/auth/csrf/
probe anon POST /api/v1/auth/register/ -H 'Content-Type: application/json' \
  --data '{"email":"bob@example.com","password":"Tr0ub4dor&3xkcd9"}'
probe anon POST /api/v1/auth/register/ -H 'Content-Type: application/json' -H "X-CSRFToken: $(csrf anon)" \
  --data '{"email":"Alice@Example.com","password":"Tr0ub4dor&3xkcd9","display_name":"  Alice   X  "}'
probe anon POST /api/v1/auth/register/ -H 'Content-Type: application/json' -H "X-CSRFToken: $(csrf anon)" \
  --data '{"email":"alice@example.com","password":"Tr0ub4dor&3xkcd9"}'
probe anon POST /api/v1/auth/register/ -H 'Content-Type: application/json' -H "X-CSRFToken: $(csrf anon)" \
  --data '{"email":"carol@example.com","password":"short"}'
probe anon POST /api/v1/auth/register/ -H 'Content-Type: application/json' -H "X-CSRFToken: $(csrf anon)" \
  --data '{"email":"dave@example.com","password":"12345678"}'
probe anon POST /api/v1/auth/register/ -H 'Content-Type: application/json' -H "X-CSRFToken: $(csrf anon)" \
  --data '{"email":"erin@example.com","password":"password"}'
probe anon POST /api/v1/auth/register/ -H 'Content-Type: application/json' -H "X-CSRFToken: $(csrf anon)" \
  --data '{"email":"frank@example.com","password":"frank-frank-1"}'
probe anon POST /api/v1/auth/register/ -H 'Content-Type: application/json' -H "X-CSRFToken: $(csrf anon)" \
  --data '{"email":"not-an-email","password":"Tr0ub4dor&3xkcd9"}'
probe anon POST /api/v1/auth/login/ -H 'Content-Type: application/json' \
  --data '{"email":"alice@example.com","password":"wrongpass123"}'
probe anon POST /api/v1/auth/login/ -H 'Content-Type: application/json' -H "X-CSRFToken: $(csrf anon)" \
  --data '{"email":"alice@example.com","password":"wrongpass123"}'
probe alice POST /api/v1/auth/login/ -H 'Content-Type: application/json' -H "X-CSRFToken: $(csrf anon)" \
  --data '{"email":"ALICE@example.com","password":"Tr0ub4dor&3xkcd9"}'
probe alice GET /api/v1/auth/me/
probe anon GET /api/v1/auth/me/
probe alice POST /api/v1/auth/logout/ -H "X-CSRFToken: $(csrf alice)"
probe alice GET /api/v1/auth/me/
probe anon POST /api/v1/auth/logout/ -H "X-CSRFToken: $(csrf anon)"
# two sessions: A keeps, B dies on password change (session auth hash)
probe sessA POST /api/v1/auth/login/ -H 'Content-Type: application/json' -H "X-CSRFToken: $(csrf anon)" \
  --data '{"email":"alice@example.com","password":"Tr0ub4dor&3xkcd9"}'
probe sessB POST /api/v1/auth/login/ -H 'Content-Type: application/json' -H "X-CSRFToken: $(csrf anon)" \
  --data '{"email":"alice@example.com","password":"Tr0ub4dor&3xkcd9"}'
probe sessA POST /api/v1/auth/password/change/ -H 'Content-Type: application/json' -H "X-CSRFToken: $(csrf sessA)" \
  --data '{"old_password":"wrong","new_password":"N3w&Secure-pass9x"}'
probe sessA POST /api/v1/auth/password/change/ -H 'Content-Type: application/json' -H "X-CSRFToken: $(csrf sessA)" \
  --data '{"old_password":"Tr0ub4dor&3xkcd9","new_password":"N3w&Secure-pass9x"}'
probe sessA GET /api/v1/auth/me/
probe sessB GET /api/v1/auth/me/
die

# password hash evidence from run A's DB
rm -f "$WORK/oracle.sqlite3-wal" "$WORK/oracle.sqlite3-shm" 2>/dev/null
sqlite3 "$WORK/oracle.sqlite3" "SELECT email||'|'||password FROM accounts_user ORDER BY email;" \
  > "$OUT/password-hashes.txt" 2>/dev/null || echo "(sqlite3 unavailable)" > "$OUT/password-hashes.txt"

boot
# ---- run B: account lockout (10 failures -> 429) ----
probe anon GET /api/v1/auth/csrf/
for i in $(seq 1 11); do
  probe anon POST /api/v1/auth/login/ -H 'Content-Type: application/json' -H "X-CSRFToken: $(csrf anon)" \
    --data '{"email":"lockme@example.com","password":"guess-attempt"}'
done
die

echo "probe: wrote $n transcripts to $OUT"
