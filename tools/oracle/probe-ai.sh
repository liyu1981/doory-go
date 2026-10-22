#!/usr/bin/env bash
# Probe the ai family against the pinned oracle. Two phases:
#   A: default env (AI_USER_PROVIDERS=off) — the gate, contract, empty catalog, task validation
#   B: AI_USER_PROVIDERS=everyone, AI_MAX_PROVIDERS_PER_USER=2, one env provider
#      (unreachable URL so the run path ends in a deterministic transport error)
# Raw transcripts: test/parity/probes/0010-ai/{phaseA,phaseB}/...
set -euo pipefail

REPO="$(cd "$(dirname "$0")/../.." && pwd)"
OUT="$REPO/test/parity/probes/0010-ai"
SHA="$(git -C "$REPO/.upstream/current" rev-parse HEAD)"
DATE="$(date -u +%Y-%m-%dT%H:%M:%SZ)"

phase() { # phase <dir> <port> [extra env...]
  local dir="$1" port="$2"; shift 2
  mkdir -p "$OUT/$dir"
  : > "$OUT/$dir/requests.txt"
  (
    export ORACLE_PORT="$port"
    export ORACLE_SETTINGS=doory.settings.prod
    export DJANGO_SECRET_KEY=parity-oracle-secret-key-0123456789
    export SECURE_SSL_REDIRECT=false SECURE_COOKIES=false
    export "$@"
    "$REPO/tools/oracle/serve.sh" > "$OUT/$dir/oracle.log" 2>&1 &
    echo $! > "$OUT/$dir/oracle.pid"
  )
  local i=0
  for _ in $(seq 1 60); do
    curl -s -o /dev/null "http://127.0.0.1:$port/api/v1/health/" && break
    sleep 0.5
  done

  local BASE="http://127.0.0.1:$port" J="$OUT/$dir/cookies" n=0
  curl -sS -c "$J" "$BASE/api/v1/auth/csrf/" -o /dev/null
  local csrf; csrf="$(awk '$6=="csrftoken"{print $7}' "$J")"
  curl -sS -b "$J" -c "$J" -X POST "$BASE/api/v1/auth/login/" \
    -H "X-CSRFToken: $csrf" -H "Referer: $BASE/" \
    -H 'Content-Type: application/json' \
    -d '{"email":"admin@doory.test","password":"admin-password-1"}' -o /dev/null

  probe() { # probe <method> <path> [curl args...]
    local method="$1" path="$2"; shift 2
    n=$((n + 1))
    local slug
    slug="$(echo "${method}_${path}" | tr -cs 'A-Za-z0-9' '-' | sed 's/^-//; s/-$//')"
    echo "$method $path $*" >> "$OUT/$dir/requests.txt"
    curl -sS -X "$method" -b "$J" -c "$J" \
      -o "$OUT/$dir/$(printf '%2d' "$n")-${slug}.body" \
      -D "$OUT/$dir/$(printf '%2d' "$n")-${slug}.headers" \
      -w '%{http_code}\n' \
      -H "X-CSRFToken: $csrf" -H "Referer: $BASE/" \
      "$BASE$path" "$@" > "$OUT/$dir/$(printf '%2d' "$n")-${slug}.status"
  }

  # share $csrf/$BASE with callers
  CSRF="$csrf" BASE="$BASE" N="$n" probe_all "$dir" || true
}

# --- phase A: default (user providers off) ---
phaseA() {
  local port=8021
  mkdir -p "$OUT/phaseA"; : > "$OUT/phaseA/requests.txt"
  (
    export ORACLE_PORT="$port" ORACLE_SETTINGS=doory.settings.prod
    export DJANGO_SECRET_KEY=parity-oracle-secret-key-0123456789
    export SECURE_SSL_REDIRECT=false SECURE_COOKIES=false
    "$REPO/tools/oracle/serve.sh" > "$OUT/phaseA/oracle.log" 2>&1 &
    echo $! > "$OUT/phaseA/oracle.pid"
  )
  for _ in $(seq 1 60); do curl -s -o /dev/null "http://127.0.0.1:$port/api/v1/health/" && break; sleep 0.5; done

  local BASE="http://127.0.0.1:$port" J="$OUT/phaseA/cookies" n=0
  curl -sS -c "$J" "$BASE/api/v1/auth/csrf/" -o /dev/null
  local csrf; csrf="$(awk '$6=="csrftoken"{print $7}' "$J")"
  curl -sS -b "$J" -c "$J" -X POST "$BASE/api/v1/auth/register/" -H "X-CSRFToken: $csrf" \
    -H "Referer: $BASE/" -H 'Content-Type: application/json' \
    -d '{"email":"probe-ai@example.com","password":"Tr0ub4dor&3xkcd9"}' -o /dev/null
  csrf="$(awk '$6=="csrftoken"{print $7}' "$J")"
  curl -sS -b "$J" -c "$J" -X POST "$BASE/api/v1/auth/login/" -H "X-CSRFToken: $csrf" \
    -H "Referer: $BASE/" -H 'Content-Type: application/json' \
    -d '{"email":"probe-ai@example.com","password":"Tr0ub4dor&3xkcd9"}' -o /dev/null
  csrf="$(awk '$6=="csrftoken"{print $7}' "$J")"

  p() { local method="$1" path="$2"; shift 2
    n=$((n+1)); local slug; slug="$(echo "${method}_${path}" | tr -cs 'A-Za-z0-9' '-' | sed 's/^-//; s/-$//')"
    echo "$method $path $*" >> "$OUT/phaseA/requests.txt"
    curl -sS -X "$method" -b "$J" -c "$J" \
      -o "$OUT/phaseA/$(printf '%02d' "$n")-${slug}.body" \
      -D "$OUT/phaseA/$(printf '%02d' "$n")-${slug}.headers" \
      -w '%{http_code}\n' -H "X-CSRFToken: $csrf" -H "Referer: $BASE/" \
      "$BASE$path" "$@" > "$OUT/phaseA/$(printf '%02d' "$n")-${slug}.status"; }

  p GET  /api/v1/ai/contract.yaml
  p GET  /api/v1/ai/models/
  p GET  /api/v1/ai/models/?task=bogus
  p GET  /api/v1/ai/providers/
  p POST /api/v1/ai/providers/ -H 'Content-Type: application/json' -d '{"name":"x"}'
  p POST /api/v1/ai/bogus/ -H 'Content-Type: application/json' -d '{}'
  p POST /api/v1/ai/background-removal/ -H 'Content-Type: application/json' -d '{"model":"device"}'
  p POST /api/v1/ai/background-removal/ -H 'Content-Type: application/json' \
     -d '{"asset_id":"00000000-0000-0000-0000-000000000000","model":"device"}'
  p POST /api/v1/ai/background-removal/ -H 'Content-Type: application/json' \
     -d '{"asset_id":"00000000-0000-0000-0000-000000000000","model":"nope:fast","options":"x"}'
  p POST /api/v1/assets/ -F "file=@$REPO/test/parity/testdata/assets/fixture.png"
  local aid; aid="$(OUT="$OUT" python3 -c "
import json,glob,os
fs=sorted(glob.glob(os.environ['OUT']+'/phaseA/*.body'))
d=json.load(open(fs[-1]))
print(d['id'])
")"
  p POST /api/v1/ai/background-removal/ -H 'Content-Type: application/json' -d "{\"asset_id\":\"$aid\",\"model\":\"device\"}"
  p POST /api/v1/ai/background-removal/ -H 'Content-Type: application/json' -d "{\"asset_id\":\"$aid\",\"model\":\"env-1:fast\"}"
  kill "$(cat "$OUT/phaseA/oracle.pid")" 2>/dev/null || true
}

# --- phase B: user providers everyone + one env provider (unreachable) ---
phaseB() {
  local port=8022
  mkdir -p "$OUT/phaseB"; : > "$OUT/phaseB/requests.txt"
  (
    export ORACLE_PORT="$port" ORACLE_SETTINGS=doory.settings.prod
    export DJANGO_SECRET_KEY=parity-oracle-secret-key-0123456789
    export SECURE_SSL_REDIRECT=false SECURE_COOKIES=false
    export AI_USER_PROVIDERS=everyone AI_MAX_PROVIDERS_PER_USER=2
    export AI_PROVIDER_1_URL=http://127.0.0.1:9
    export AI_PROVIDER_1_LABEL="Local server"
    export AI_PROVIDER_1_TASKS=background-removal
    export AI_PROVIDER_1_MODELS=fast:Fast,best-model:Best
    "$REPO/tools/oracle/serve.sh" > "$OUT/phaseB/oracle.log" 2>&1 &
    echo $! > "$OUT/phaseB/oracle.pid"
  )
  for _ in $(seq 1 60); do curl -s -o /dev/null "http://127.0.0.1:$port/api/v1/health/" && break; sleep 0.5; done

  local BASE="http://127.0.0.1:$port" J="$OUT/phaseB/cookies" n=0
  curl -sS -c "$J" "$BASE/api/v1/auth/csrf/" -o /dev/null
  local csrf; csrf="$(awk '$6=="csrftoken"{print $7}' "$J")"
  curl -sS -b "$J" -c "$J" -X POST "$BASE/api/v1/auth/register/" -H "X-CSRFToken: $csrf" \
    -H "Referer: $BASE/" -H 'Content-Type: application/json' \
    -d '{"email":"probe-ai@example.com","password":"Tr0ub4dor&3xkcd9"}' -o /dev/null
  csrf="$(awk '$6=="csrftoken"{print $7}' "$J")"
  curl -sS -b "$J" -c "$J" -X POST "$BASE/api/v1/auth/login/" -H "X-CSRFToken: $csrf" \
    -H "Referer: $BASE/" -H 'Content-Type: application/json' \
    -d '{"email":"probe-ai@example.com","password":"Tr0ub4dor&3xkcd9"}' -o /dev/null
  csrf="$(awk '$6=="csrftoken"{print $7}' "$J")"

  p() { local method="$1" path="$2"; shift 2
    n=$((n+1)); local slug; slug="$(echo "${method}_${path}" | tr -cs 'A-Za-z0-9' '-' | sed 's/^-//; s/-$//')"
    echo "$method $path $*" >> "$OUT/phaseB/requests.txt"
    curl -sS -X "$method" -b "$J" -c "$J" \
      -o "$OUT/phaseB/$(printf '%02d' "$n")-${slug}.body" \
      -D "$OUT/phaseB/$(printf '%02d' "$n")-${slug}.headers" \
      -w '%{http_code}\n' -H "X-CSRFToken: $csrf" -H "Referer: $BASE/" \
      "$BASE$path" "$@" > "$OUT/phaseB/$(printf '%02d' "$n")-${slug}.status"; }

  p GET  /api/v1/ai/models/
  p GET  /api/v1/ai/models/?task=background-removal
  p GET  /api/v1/ai/providers/
  p POST /api/v1/ai/providers/ -H 'Content-Type: application/json' -d '{"name":"PlainHttp","base_url":"http://10.0.0.5","style":"doory","tasks":["background-removal"]}'
  p POST /api/v1/ai/providers/ -H 'Content-Type: application/json' -d '{"name":"Mine","base_url":"https://ai.example.com","style":"doory","tasks":["background-removal"],"secret":"sk-oracle-secret-1","auth_header":"X-Api-Key"}'
  p POST /api/v1/ai/providers/ -H 'Content-Type: application/json' -d '{"name":"Bad","base_url":"ftp://x","tasks":["background-removal"]}'
  p POST /api/v1/ai/providers/ -H 'Content-Type: application/json' -d '{"name":"Line","base_url":"https://ai.example.com","tasks":["background-removal"],"secret":"a\nb"}'
  p POST /api/v1/ai/providers/ -H 'Content-Type: application/json' -d '{"name":"T","base_url":"https://ai.example.com","tasks":["nope"]}'
  p POST /api/v1/ai/providers/ -H 'Content-Type: application/json' -d '{"name":"TO","base_url":"https://ai.example.com","tasks":["background-removal"],"timeout_seconds":0}'
  local pid; pid="$(OUT="$OUT" python3 -c "
import json,glob,os
for f in sorted(glob.glob(os.environ['OUT']+'/phaseB/*.body')):
    try: d=json.load(open(f))
    except Exception: continue
    if isinstance(d,dict) and d.get('name')=='Mine': print(d['id']); break
")"
  echo "provider=$pid" >> "$OUT/phaseB/requests.txt"
  p GET  "/api/v1/ai/providers/$pid/"
  p PATCH "/api/v1/ai/providers/$pid/" -H 'Content-Type: application/json' -d '{"base_url":"https://other.example.com"}'
  p PATCH "/api/v1/ai/providers/$pid/" -H 'Content-Type: application/json' -d '{"secret":""}'
  p PATCH "/api/v1/ai/providers/$pid/" -H 'Content-Type: application/json' -d '{"base_url":"https://third.example.com/"}'
  p PUT  "/api/v1/ai/providers/$pid/models/" -H 'Content-Type: application/json' \
     -d '{"models":[{"task":"background-removal","remote_name":"fast","label":"Fast","enabled":true},{"task":"background-removal","remote_name":"slow","enabled":false}]}'
  p PUT  "/api/v1/ai/providers/$pid/models/" -H 'Content-Type: application/json' \
     -d '{"models":[{"task":"background-removal","remote_name":"fast"},{"task":"bogus","remote_name":"x"}]}'
  p POST /api/v1/ai/providers/ -H 'Content-Type: application/json' -d '{"name":"Second","base_url":"https://two.example.com","tasks":["background-removal"]}'
  p POST /api/v1/ai/providers/ -H 'Content-Type: application/json' -d '{"name":"Third","base_url":"https://three.example.com","tasks":["background-removal"]}'
  p POST /api/v1/ai/background-removal/ -H 'Content-Type: application/json' \
     -d '{"asset_id":"00000000-0000-0000-0000-000000000000","model":"device"}'
  p POST /api/v1/ai/background-removal/ -H 'Content-Type: application/json' \
     -d '{"asset_id":"00000000-0000-0000-0000-000000000000","model":"env-1:fast"}'
  p GET  /api/v1/ai/providers/00000000-0000-0000-0000-000000000000/
  p POST /api/v1/assets/ -F "file=@$REPO/test/parity/testdata/assets/fixture.png"
  local aid; aid="$(OUT="$OUT" python3 -c "
import json,glob,os
fs=sorted(glob.glob(os.environ['OUT']+'/phaseB/*.body'))
d=json.load(open(fs[-1]))
print(d['id'])
")"
  p POST /api/v1/ai/background-removal/ -H 'Content-Type: application/json' -d "{\"asset_id\":\"$aid\",\"model\":\"device\"}"
  p POST /api/v1/ai/background-removal/ -H 'Content-Type: application/json' -d "{\"asset_id\":\"$aid\",\"model\":\"env-1:missing\"}"
  p POST /api/v1/ai/background-removal/ -H 'Content-Type: application/json' -d "{\"asset_id\":\"$aid\",\"model\":\"env-1:fast\"}"
  kill "$(cat "$OUT/phaseB/oracle.pid")" 2>/dev/null || true
}

mkdir -p "$OUT"
cat > "$OUT/meta.txt" << EOF
probe=0010-ai
upstream_commit=$SHA
probed_at=$DATE
settings=doory.settings.prod (DJANGO_SECRET_KEY=parity-oracle-secret-key-0123456789)
phaseA=defaults (AI_USER_PROVIDERS=off)
phaseB=AI_USER_PROVIDERS=everyone, AI_MAX_PROVIDERS_PER_USER=2, AI_PROVIDER_1_URL=http://127.0.0.1:9 (unreachable)
note=raw evidence; never edit these files, only add new probe dirs
EOF

phaseA
phaseB
echo "probe: wrote transcripts to $OUT"
