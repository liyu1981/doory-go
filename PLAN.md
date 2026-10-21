# Doory backend → Go: faithful rewrite plan (PiG methodology)

Goal: rebuild doory's Django backend (https://github.com/shaongitbd/doory) in Go with
**behavioral parity against a pinned upstream**, then ship the existing React frontend
embedded in the same binary. Methodology is the one used in `~/PiG` (Pi → PiG TypeScript→Go
port), whose research summary is `~/code_notes/pig/pig-pi-parity-methodology.md`. The core
idea: the pinned upstream is the **behavior oracle**, denominators are **compiler-derived**,
evidence is reviewed, differences are **fixed or numbered**, and green is necessary but not
sufficient.

## 0. The oracle (pinned)

- Mirror: `.upstream/current/` — doory @ `2eb2557c1eed3aa256a78f1874abfb5a1315849c`
  ("High-contrast mode (#120)", 2026-10-20). Refresh with `pnpm upstream-mirror`.
- What we are porting (inventory as of the pin):
  - **16 Django apps**, `backend/apps/{core,accounts,apitokens,designs,assets,gallery,
    elements,fonts,brand,sharing,exports,collab,presets,mcp,ai}` + project `backend/doory/`.
    ~39k LOC production Python, ~35k LOC tests, 74 serializers, 66 view classes/functions,
    ~50 hand-written routes + DRF `SimpleRouter` routes, 17 migrations, 32 indexes/
    constraints.
  - **Three deployed entry points sharing one codebase/DB**: WSGI (API+admin, `/api/v1/`,
    `/admin/`), ASGI (WebSockets `/ws/designs/<uuid>/`, channels + Redis channel layer),
    MCP ASGI (`/mcp`, bearer personal-access tokens).
  - **Auth**: custom `User`, DB-backed sessions + CSRF (`auth/csrf/`), HMAC single-use
    tokens (Django `PasswordResetTokenGenerator` semantics) for reset/email-verify, Google
    ID-token sign-in (`google-auth`), per-account password lockout, throttled auth scopes,
    personal access tokens with scopes (MCP).
  - **Realtime collab**: Yjs CRDT via `pycrdt` (yrs native engine) on dedicated 1 GB-stack
    threads; `YState` snapshot + `YUpdate` append log with compaction; room registry,
    presence, token-bucket limits, origin validation, `collab_maintenance`.
  - **Server-side export**: Playwright headless Chromium renders the frontend's
    `render.html`; PDF assembly with `pypdf`; image pages via multipart upload; signed
    download tokens.
  - **AI image providers**: operator + user providers, Fernet+HKDF encrypted secrets at
    rest (`reencrypt_ai_secrets`), outbound network policy with DNS/pinned-address rules,
    style pipelines (json_b64, multipart, mask, custom), quotas, checked-in
    `apps/ai/contract/openapi.yaml`.
  - **Media/fonts/elements**: upload quota + S3-or-local storage, stock photo/audio
    providers (Unsplash/Pexels/Pixabay/Freesound), font catalogue + user fonts
    (fonttools/brotli/woff2), element packs (~27.5k icons), templates/sitemap,
    size presets, brand kits.
  - **Ops surface**: 15 management commands (`seed_*`, `sync_*`, `purge_*`, `import_*`,
    `collab_maintenance`, `reencrypt_ai_secrets`), `.env` contract (~21 root vars, fully
    enumerated in `docs/self-hosting/configuration.md` — 252 rows to honor), docker
    compose (postgres:16 + api + realtime + mcp + web), nginx deploy configs.
  - **Frontend** (to embed, not rewrite): React 19 + Vite, 853 TS/TSX files / ~102k LOC,
    builds `index.html`, `presenter-view.html`, `render.html`; dev proxy forwards
    `/api`, `/ws`, `/media`, `/static`, `/admin` to :8000.
- Non-goals for "faithful": byte-identical CRDT update blobs (CRDTs are convergent —
  compare final document JSON and client-observable frames instead); byte-identical
  exported PDFs (compare structure via `pypdf`-equivalent inspection); Django admin HTML
  pixel parity (see D2).
- **Testing scope (owner decision):** parity proves **backend behavior only** — the
  HTTP/WS/MCP API contracts, DB state, and side effects must match the oracle. The
  embedded frontend gets no behavioral validation during the port; it is smoke-tested
  after integration and validated **case by case** as issues surface. Scenarios never
  drive the React app.

## 1. Methodology (PiG rules, restated for an HTTP/DB oracle)

1. **Pin the oracle; probe before porting.** Every porting loop starts by running the pinned
   Django code and preserving raw output (HTTP transcript, WS frame log, DB digest, CLI
   stdout) as evidence under `test/parity/probes/`. No Go code before a failing
   comparator exists ("make the current failure red, or mutation-prove an already
   matching path").
2. **Port observed semantics, not Python syntax.** DRF pagination/filtering/error-JSON
   ordering, Django session/CSRF cookie behavior, timezone/UTC handling, UUID formats,
   slug/path converters, throttling cache semantics, template rendering — these are the
   contract. Naming mirrors upstream 1:1 (`apps/accounts/views.py` →
   `apps/accounts/views.go`, same route names, same JSON keys) to keep the next upstream
   sync cheap.
3. **Denominators are compiler-derived.** A generated inventory (from Django introspection
   of the pinned commit + the file tree) defines the review queue; it never proves parity.
   Drift gates fail when source, scenarios, and reports fall out of sync.
4. **Behavioral proof = paired scenarios.** Each scenario runs Django-oracle and Go side
   side in isolated environments (own DB, media root, scrubbed env, fake upstream
   providers) and compares with the strongest comparator that can hold.
5. **Comparator ladder** (strongest first): binary/escaped output equal → exact body equal →
   structural JSON equal with typed `json_aliases` (uuid/timestamp/token/path/…) →
   normalized equal (every `normalize_replace` needs a per-rule reason) → substring.
   Never weaken a comparator to get green.
6. **Quality tiers.** boot-only / registration-only / smoke scenarios stay visible but do
   not count as verification; only exercised-path scenarios do.
7. **Differences are fixed or numbered.** Every deliberate difference gets a global `D<N>`
   id in `docs/parity/DIVERGENCES.md` with `SCRUTINIZED:approved`, a **remove-when**
   condition, `// doory divergence (D<N>):` markers at call sites, and coverage. A
   divergence-guard ratchet (CI scan for swallowed errors, invented timeouts/literals,
   bare `recover()`, TODO/FIXME, status-code substring classification) makes unrecorded
   workarounds fail; the baseline only shrinks.
8. **Test files get dispositions.** Every upstream test file (there are many; ~35k LOC)
   is ported, partially ported, or explicitly pending in a machine-checked ledger;
   "pending" needs an owner and a missing-case reference.
9. **Gate ladder** (targets live in `package.json`, run with pnpm — no Makefile).
   `pnpm generate` (never hand-edit inventories) → `pnpm check` (build,
   vet, lint, unit, race, drift gates, divergence guards, lint-scenarios, port-map drift,
   coverage drift, parity-fast) → `pnpm verify` (full parity + contracts) →
   `pnpm parity` (parallel pairs) → `pnpm coverage` (honest report embedded in
   `AGENTS.md` between markers; stale = fail).
10. **Green is necessary, not sufficient.** A parity run that finds zero bugs over many
    loops is a stated smell: re-probe the oracle, add adversarial cases.

## 2. Repo layout (PiG-shaped)

```
doory-go/
├── .upstream/current/            # frozen mirror of the pin (already cloned)
├── AGENTS.md                     # working agreement + embedded coverage report block
├── PLAN.md                       # this file
├── package.json                  # gate ladder: pnpm scripts (pnpm run help lists targets)
├── go.mod
├── cmd/doory/                    # single binary: serve / migrate / seed / admin / reencrypt …
├── internal/dooryversion/        # sole pin: UpstreamRepo/UpstreamCommit/UpstreamReleaseLine
├── internal/ledgergen/           # Django-introspection compilers (models, routes, serializers, env, tests)
├── apps/<name>/                  # ports, one package per Django app; // Ports backend/apps/<name>/<file>.py
├── doory/                        # settings/env contract, middleware stack, URL table, entry-point wiring
├── web/                          # embed.FS of the built frontend (generated)
├── docs/parity/PORT_MAP.md       # file denominator: upstream file → Go file(s) + status
├── docs/parity/DIVERGENCES.md    # D<N> ledger + DIVERGENCE-IDS.txt
├── ledger/                       # compiler-derived: interfaces-v<sha>.json, test-mapping-v<sha>.json,
│                                 # env-contract-v<sha>.json, migrations-v<sha>.json, routes-v<sha>.json
├── test/parity/
│   ├── families.toml             # behavior families declare scope BEFORE scenarios exist
│   ├── scenarios/<family>/*.toml # paired oracle/Go scenarios
│   ├── runner/                   # dual-server scheduler, isolation, comparators
│   ├── drivers/                  # http / ws / mcp / cli / schema drivers
│   ├── unit-evidence/*.json      # upstream path + Go test names + compiling mutation
│   ├── probes/                   # raw preserved upstream evidence
│   └── cmd/{coverage,familygaps,lint-scenario,…}/
└── automation/ci/{divguard,portmapdrift,coveragedrift,…}/
```

Version pin (`internal/dooryversion/dooryversion.go`) is the only place the commit lives;
`test/upstream-parity/mirror_version_test.go` fails if `.upstream/current` doesn't match.

## 3. Compiler-derived ledgers (Phase 0 tooling)

| Ledger | Derived from | Forces |
|---|---|---|
| `ledger/routes-v*.json` | Django URL resolver walk (`manage.py` probe script) | every route (incl. router-generated + WS + MCP) mapped to a Go handler + scenario coverage |
| `ledger/interfaces-v*.json` | model/field/validator/meta introspection, serializer field introspection, DRF settings, throttle rates | every model field & serializer field mapped; schema digest parity |
| `ledger/env-contract-v*.json` | `docs/self-hosting/configuration.md` ∩ `doory/settings/*.py` env reads | every env row honored by Go with same default, or divergence |
| `ledger/migrations-v*.json` | Django migrations → schema digest per app, per dialect (SQLite + Postgres) | Go schema produces the same table/column/index/constraint digest on both dialects |
| `ledger/test-mapping-v*.json` | pytest test-file inventory | per-file disposition: ported / partial / pending(owner, missing-case) |
| `ledger/commands-v*.json` | management-command inventory | each command has a Go subcommand + cli scenario |

`pnpm port-map-drift` rejects Go source without a PORT_MAP row and rows whose upstream file
vanished; explicit `// Ports backend/...` comments require a reviewed row.

## 4. Parity harness (the heart of the port)

The oracle is a server + database, not a CLI, so the harness has these drivers:

- **http-driver**: seeds both sides from one canonical fixture export (built by Django
  factories, dumped once to JSON with typed fields, replayed into SQLite in-memory or
  Postgres for both sides), replays identical request sequences (cookies, CSRF,
  multipart, headers),
  records transcripts. Comparators use typed aliases: `id`→uuid-shape, `timestamp`→
  ISO-8601-shape, `token`→shape-only, `path`→basename, `etag`→shape. Cookie jars compared
  by name/attrs, not value, unless the scenario asserts interop (see D5).
- **schema-driver**: canonical schema digest (tables, columns, types, defaults, indexes,
  constraints, FKs) from Django migrations vs Go migration output, **computed once per
  dialect** (SQLite and Postgres) so type mapping (uuid/json/timestamptz/bool/blob) stays
  honest.
- **ws-driver**: drives `/ws/designs/<uuid>/` with a scripted Y-protocol client on both
  sides; asserts frame transcripts (sync step1/2, update, awareness/presence frames,
  error/close codes, rate-limit refusals) and final **document JSON** equality after
  applying the same update script to the persisted `YState`. Update blobs stay opaque.
- **mcp-driver**: JSON-RPC transcripts against `/mcp` with personal access tokens
  (initialize/tools/list/call/resources/prompts) and 401/WWW-Authenticate shapes.
- **cli-driver**: management commands → stdout digest + DB-state digest + media-tree
  digest (network syncs served by recorded fixture HTTP).
- **contract-driver**: drf-spectacular OpenAPI output (structural diff, not bytes) and
  the checked-in `apps/ai/contract/openapi.yaml` (exact, it's a committed contract file).

Hermeticity rules (mirror PiG): per-scenario temp dirs for MEDIA/EXPORT roots, scrubbed
ambient env, no real provider keys, httptest fixtures for Unsplash/Pexels/Pixabay/
Freesound/Google/smtp, parallel scheduler groups, cleanup kills only its own resources.

**Dialect matrix (implements D4).** Every behavioral scenario runs on **SQLite** by
default — in-memory (`file::memory:?cache=shared` or a per-scenario dsn) unless the
scenario needs cross-connection or on-disk persistence (collab multi-connection,
media-adjacent state), in which case a per-scenario temp-file DB is used. Scenarios whose
behavior must also hold on Postgres declare `dialects = ["sqlite", "postgres"]`; the
Postgres lane runs against a **local docker** `postgres:16-alpine` instance started by
the runner (`docker run` per `pnpm parity` invocation, unique DB name per scenario,
truncated between runs). The pinned Django oracle runs the **same scenario on the same
dialect** (`DATABASE_URL` accepts both; the Phase-0 inventory confirmed the upstream
model layer is dialect-portable — no ArrayField/GinIndex/HStore/RawSQL). The Go port is
written dialect-neutral through `database/sql`; behavior that exists on only one dialect
must be a scenario-declared exception with a Divergence ID, never a silent skip. Known
SQLite hazards, each owed at least one paired scenario: JSON1 query semantics, datetime
string canonicalization (Django stores ISO text on SQLite vs `timestamptz` on PG),
boolean/int encoding, UUID text format, and WAL + `busy_timeout` behavior under the
single-writer collab append/compaction path.

Scenario TOML records, in comments: the upstream probe (command + raw output path), and
any bug found in the loop. Quality tiers are enforced by `coverage` tooling; weak
evidence never counts.

## 5. Porting order (families → phases)

Families are declared in `test/parity/families.toml` **first**, in dependency order:

| # | Family / phase | Scope (upstream) | Key hazards |
|---|---|---|---|
| 1 | kernel | `doory/settings/*`, middleware stack (security headers, whitenoise, CORS, CSRF, throttling, `PrivateApiCacheMiddleware`, `PermissionsPolicyMiddleware`, `MediaSecurityHeadersMiddleware`, `RealtimeGuardMiddleware`), pagination, DRF error-JSON shape, content negotiation, `/health/`, `/config/`, `/schema/`, `/docs/` | DRF error body ordering & field-error nesting; throttle cache semantics; CORS preflight |
| 2 | accounts | register/login/logout/me, password change/reset, email verify/resend, Google sign-in, lockout, CSRF endpoint, emails (async toggle), admin-user interop | HMAC token semantics (state-bound, single-use), uidb64 codecs, lockout vs throttle interplay, PBKDF2 params (D6) |
| 3 | apitokens + mcp-auth | personal access tokens, scopes | token hashing at rest, scope checks |
| 4 | designs + presets | Design/Folder CRUD, trash/purge, versions list/detail/restore, revision semantics, document validators, size presets | JSONField validation errors, UUID route converters, soft-delete semantics |
| 5 | elements + gallery + brand | element packs (~27.5k), templates/categories/use, sitemap.xml, brand kits | large seed data digests; sitemap XML exactness |
| 6 | fonts | catalogue, user fonts upload/delete, woff2/ttf parsing metadata | fonttools → Go `sfnt`/woff2 libs; brotli; metadata field parity |
| 7 | assets | uploads, quota, stock photos/audio (search/import/preview), remote fetch with pinned-address + private-network policy | DNS-pinning & SSRF policy byte-for-byte; urllib3 decompression bounds; Pillow ops inventory |
| 8 | sharing | share links (roles/passwords), comments, `shared/<token>/` public views | permission matrix; public serializer shape (`public_name` etc.) |
| 9 | exports | PDF (Playwright + pypdf), image (multipart pages + render.html), delivery tokens, purge | playwright-go vs playwright-python; PDF structure parity; size caps |
| 10 | ai | providers/models admin API, per-task proxy, styles pipeline, secrets (Fernet+HKDF), quotas, transport policy, `reencrypt_ai_secrets`, contract.yaml | crypto compat with existing encrypted rows; SSRF policy; style multipart/json_b64/mask byte semantics |
| 11 | collab | consumer handshake/auth/limits, room registry, engine (`yrs` isolation), store (YState/YUpdate/compaction), presence/notify, channel layer (Redis), `collab_maintenance`, `RealtimeGuard` | Y engine in Go (S0); compaction invariants (`version`, `flushed_version`, `base_revision`); reconnect/sync semantics |
| 12 | mcp | server, tools/*, resources, prompts, bearer auth, resolver | JSON-RPC framing over HTTP; token scope enforcement |
| 13 | commands + content | all 15 management commands, `backend/data/*.json` fixtures, docker entrypoint, healthcheck | seeded-content byte digests |
| 14 | single-binary release | `embed.FS` frontend (`index.html`, `presenter-view.html`, `render.html`, hashed assets), `/static` (precompressed, whitenoise-manifest equivalent), `/media` local or S3, dev-mode proxy parity with `frontend/vite.config.ts`, `docker` image replacing api+realtime+mcp+mcp services, docs updates | `render.html` export path must hit the embedded origin; CORS not needed same-origin but must still behave |

Admin site: see D2 — a gated sub-phase inside #14.

## 6. Divergences opened on day one (each gets remove-when + coverage)

- **D1 — three deploy units → one process.** Default: one listener serves HTTP + WS +
  MCP (paths `/api/v1`, `/ws`, `/mcp`, `/`, `/static`, `/media`). `--ws-addr`/`--mcp-addr`
  flags restore the nginx three-upstream topology for drop-in compose/nginx parity.
  Remove-when: upstream changes its topology; keep flags tested.
- **D2 — Django admin + django-unfold.** Not ported as Django HTML. Interim: minimal Go
  admin covering the operations self-host docs actually require (`createsuperuser`,
  template/content management, AI provider confirm-actions, user/asset moderation),
  rendered from the same models. Remove-when: admin surface fully inventoried and
  scenario-covered (spike S3 produces the inventory first; the honest alternative —
  "admin unsupported" — fails self-hosting parity and needs an explicit owner decision).
- **D3 — CRDT engine.** pycrdt(yrs) native in Python → a Go yrs binding, yrs-WASM, or a
  minimal Y-protocol implementation (spike S0 decides). Observable contract: client
  protocol frames + final document JSON + stored-snapshot round-trip, never blob bytes.
- **D4 — swappable database (headline divergence, owner-requested; permanent).**
  Upstream deploys Postgres in prod (docker-compose `postgres:16`) and defaults to
  SQLite for dev. This port **defaults to SQLite** — zero-config single binary:
  `DATABASE_URL` unset → embedded SQLite file (WAL, busy_timeout, JSON1) in the data
  dir — and works against Postgres via `DATABASE_URL=postgres://…` (pgx) with no code
  change. The parity harness runs every scenario on SQLite and the dialect-declared
  subset additionally on local docker Postgres (§4 dialect matrix); schema/type mapping
  is ledgered per dialect (`ledger/migrations-v*.json` carries both digests). The
  collab write path carries a single-writer stress scenario on both dialects.
  Remove-when: n/a (additive, intentional). Re-verify on every upstream leap that
  upstream has not adopted a Postgres-only feature; if it does, record the delta here
  and scenario-prove the Go behavior on both dialects anyway.
- **D5 — session/token interop.** Django session rows/password hashes are not guaranteed
  import-compatible at swap time. Decision after spike: either read Django's
  `django_session` + PBKDF2 hashes (zero-logout upgrade) or document a one-time logout
  (remove-when: interop test green).
- **D6 — password hashers / throttle caches.** Same KDF parameters (probed from oracle),
  Go maps Django's cache-based throttling to its own store; observable behavior
  (429 shapes, retry-after) is scenario-proven, storage is not.
- **D7 — static serving.** whitenoise compressed-manifest → embedded precompressed
  assets with the same `Content-Encoding`/`ETag`/cache-header behavior (probed).
- **D8 — OpenAPI.** drf-spectacular output is compared structurally; exact byte parity
  of generator ordering is not claimed. The committed `apps/ai/contract/openapi.yaml`
  IS compared exactly.
- **D9 — emails.** Django template rendering of verify/reset emails → Go templates;
  parity asserted on extracted URLs/tokens/text structure, not HTML bytes (until a
  scenario proves more).

## 7. Timeboxed spikes (before their phase, results recorded as probes)

- **S0 (blocking #11):** Y server-side in Go — evaluate maintained yrs Go bindings /
  WASM embedding vs implementing sync-step + `merge_updates` semantics; build a
  convergence test against pycrdt-produced fixtures.
- **S1 (#9):** playwright-go maturity (PDF flags identical to upstream call sites);
  fallback: drive Chromium over CDP directly with the exact flags from
  `apps/exports/services.py`.
- **S2 (#6,#7):** inventory every fonttools/Pillow/defusedxml/pypdf call site; map each
  to a Go library or hand-rolled equivalent; record the map in PORT_MAP rows.
- **S3 (#D2):** admin surface inventory (registered ModelAdmins, actions, custom admin
  views/templates under `apps/*/templates/admin/`).
- **S4 (#1):** DRF semantics table (pagination `next/previous` shapes, filter/search/
  ordering param names, exception→JSON mapping, `FieldError` nesting, 404/405/415
  shapes) compiled from the pinned DRF source; drives kernel comparators.

## 8. Milestones & exit criteria

- **M0 — scaffold green.** Mirror+pin test, PORT_MAP skeleton (every backend file listed,
  all ⬜), families declared, the pnpm gate ladder runs `pnpm check` on an empty port
  set, divguard baseline exists and only shrinks.
- **M1 — kernel parity.** Family 1 scenarios green at structural level; schema-driver
  digests match for all 17 migrations **on both dialects** (SQLite always in-memory or
  temp-file; Postgres from the local docker test instance) — Go schema complete even
  though handlers are ⬜.
- **M2..M13 — per-family done-criteria:** (a) every PORT_MAP row in family reviewed;
  (b) every route in family covered by ≥1 behavioral scenario at structural+;
  (c) every upstream test file in family has disposition; (d) unit evidence with
  compiling mutations; (e) no unnumbered divergence markers.
- **M14 — release candidate.** Single binary: `doory serve` boots with **no database
  config at all** (embedded SQLite default, D4) and also against a Postgres
  `DATABASE_URL` from the same binary; embedded frontend serves the
  editor, WS collab works between two browsers, MCP works from an assistant client,
  export renders PNG/PDF, docker-compose replacement image runs the full stack,
  `pnpm parity` green, coverage report embedded in AGENTS.md with no weak-only routes
  counted as verified.

## 9. Acceptance ladder (what "faithful" means, in strength order)

1. **Wire parity** — status, headers allowlist, JSON body (typed-aliased) per route,
   including every error path the oracle can produce.
2. **State parity** — schema digest + row semantics for the writes a scenario makes,
   on every dialect the scenario declares; revision/compaction counters.
3. **Side-effect parity** — outbound emails (extracted links/tokens), files written
   (media/export tree digests), provider calls (recorded request wire from fake
   providers), audit-throttling behavior.
4. **Realtime parity** — frame transcripts, convergence to identical document JSON,
   limit/refusal behavior, reconnect, maintenance command effects.
5. **Ops parity** — env rows, CLI commands, health checks, compose/nginx drop-in,
   single-binary mode as an additive convenience (D1), not a behavioral fork.

## 10. Progress log (M0) and next steps

Done in the first build session:

1. ✅ Scaffold: `.upstream/current` pristine mirror (gitignored, recreated by
   `pnpm upstream-mirror`; the pin in `internal/dooryversion` is checked by
   `test/upstream-parity`).
2. ✅ `internal/dooryversion/dooryversion.go` + mirror version test (green).
3. ✅ `tools/oracle/introspect.py` (Django introspection probe against the pinned
   oracle) → `ledger/routes-v*.json` (214 HTTP + 1 WS route) and
   `ledger/interfaces-v*.json` (31 models, 74 serializers, DRF config; 1 visible
   introspect gap recorded, not dropped).
4. ✅ Full file denominator → `docs/parity/PORT_MAP.md` (477 tracked files →
   325 rows, generated by `pnpm generate`) + `pnpm port-map-drift` gate.
5. ✅ `families.toml` with 15 families (scope declared before scenarios) +
   `pnpm lint-scenarios` gate.
6. ✅ Oracle harness basics: `pnpm oracle:setup` (uv venv outside the mirror),
   `pnpm oracle:serve` (pinned Django on SQLite, 127.0.0.1:8012),
   `pnpm oracle:probe` → first raw transcripts in
   `test/parity/probes/0001-kernel-boot/` (custom error envelope, 401-not-403,
   Permissions-Policy header, the 404/405 body-spacing quirk). All 17 apps'
   migrations apply cleanly to SQLite (D4 evidence).
7. ✅ `docs/parity/DIVERGENCES.md` D1–D9 + `DIVERGENCE-IDS.txt` +
   `pnpm divergence-consistency` gate.

Next steps (M1 largely done — see the M1 log below):

8. ⬜ Spikes S0–S4; fold results into PORT_MAP rows and DIVERGENCES entries.
9. ⬜ Remaining M1 items: DRF semantics table extracted to a doc (the Allow/
   envelope rules currently live in code comments + scenario evidence),
   http dialect lane on Postgres (schema-driver already covers both
   dialects), allowed-origin preflight probe, Accept: text/html probe.
10. ⬜ Fixture seed export (Django factories → typed JSON) and the full
    http-driver request features (multipart, auth cookies).

M1 progress log (first build session after M0):

- ✅ Schema-driver both dialects: `automation/ci/schemagen` extracts DDL
  verbatim from the oracle (sqlite_master; pg_dump for PG, bookkeeping
  tables stripped, `\\restrict` directives stripped); `doory migrate`
  applies it; `pnpm schema-parity` digests and compares — **35 tables,
  oracle == Go on SQLite AND Postgres** (local docker postgres:16-alpine,
  `scripts/pg-oracle.sh`). Migration runs on a pinned connection (pg_dump
  scripts reset search_path; sqlite pragmas are per-conn — DSN params +
  pgx OptionAfterConnect).
- ✅ Go kernel (`doory/` package): settings env contract (base.py reads 1:1,
  EMAIL_BACKEND correctly console — a settings-file constant, probed on
  prod), middleware stack in Django's exact order (buffered responses mirror
  assemble-then-send), router with the source-verified Allow rule
  (@api_view set-order + View.setup head-alias), error envelope in BOTH
  JSON dialects (DRF compact / Django JsonResponse spaced), csrf cookie
  semantics (32-char secret, renew-same-value), config endpoint including
  mlmodels manifest port, CORS preflight short-circuit, APPEND_SLASH.
- ✅ Parity runner (`test/parity/runner`, `pnpm parity-fast`): boots the
  oracle (prod settings — dev's DEBUG pages are not honest targets) and the
  Go binary on free ports, replays paired requests with per-side cookie
  jars, compares with the recorded-rule comparator.
- ✅ First scenario `01-kernel-http` **8/8 steps green at BYTE level**
  (health, config, csrf fresh+renew,404/405/401 envelopes, preflight).
  The loop found and fixed real bugs: slice-nil JSON (`models: null` vs
  `[]`), a 4-vs-5-char prefix check that disabled the cache layer, DSN
  slash trimming; and two oracle facts: `@api_view` Allow order is
  PYTHONHASHSEED-nondeterministic (comparator rule: token set) and wsgiref
  emits Content-Length: 0 on 204 where Go strips it (D11).
- ✅ Divergences D10 (process identity headers) and D11 (204
  Content-Length) recorded with reasons; 11 records, gate green.
- ✅ Kernel unit tests (`doory/kernel_test.go`): Allow rule, both envelope
  dialects, Vary merging, csrf secret shape, env coercions,
  EmailDelivers — all probe-anchored.

M2 progress log (accounts family, auth slice):

- ✅ Probe campaign: `tools/oracle/probe-accounts.sh` + microprobes →
  `test/parity/probes/0002-accounts/` (35 transcripts + password-hash
  rows). Every auth shape is probe-proven before porting: the four CSRF
  failure messages by cause (cookie-not-set / token-missing /
  incorrect-length / incorrect), validator messages+codes in settings
  order, session lifecycle (login REUSES the session for the same user,
  password change CYCLES the key and kills other sessions, logout clears
  without HttpOnly, dead-session responses clear the cookie), lockout
  10-failures→429 with DRF's wait sentence + Retry-After, pbkdf2 1.5M
  format, email/display_name normalization.
- ✅ Port (`doory/`): user.go (row/envelope/name property/uuid formats),
  password.go (pbkdf2_sha256 matching probed hashes + the four Django
  validators incl. embedded common-passwords list and quick_ratio
  similarity), session.go (django_session-backed Go-native payloads — D5
  open — with exact login/cycle/flush rules), csrf_verify.go (four causes,
  masked-token unmask), lockout.go (10/900s guard), views_auth.go
  (register/login/logout/me GET+PATCH/password-change with declaration-
  ordered validation envelopes), router dispatch modes (CSRF-always vs
  when-authed with Django's exact consult order), Retry-After on 429.
- ✅ Runner: multi-jar (named browser profiles) + `{{cookie:NAME}}`
  templates + per-step status; comparator body shape rules (id uuid,
  created_at micro-Z, retry_after/wait-sentence) each with a recorded
  reason; sessionid cookie shape rule.
- ✅ Scenarios `02-accounts-auth` (23 steps) + `03-accounts-lockout`
  (12 steps) GREEN at byte level; the loop found real bugs: uncleaned
  display_name write-back, missing Retry-After header, missing
  dead-session cookie clear, an off-by-one DSN-style slip in ordering
  (PATCH-after-logout in the scenario itself).
- ✅ M2 done-criteria artifacts: PORT_MAP rows reviewed for the family
  slice (lockout + core/csrf.py ✅, views/serializers/models 🟡 with
  pending lists), `ledger/test-mapping-v*.json` (all 11 accounts test
  files dispositioned), `doory/accounts_test.go` mutation-provable unit
  evidence incl. cross-verification of a REAL probed Django hash.
- ⬜ Family remains 🟡: password/reset+confirm + email/verify+resend
  (need email-capture runner support; D9), google (fake-provider probe),
  me DELETE (confirm_identity + cascade), avatar upload (Pillow), DRF
  rate throttles (kernel family).
