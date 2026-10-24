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
| 14 | single-binary release | `embed.FS` frontend (`index.html`, `presenter-view.html`, `render.html`, hashed assets), `/static` (precompressed, whitenoise-manifest equivalent), `/media` local or S3, dev-mode proxy parity with `frontend/vite.config.ts`, **no container image** (D18: the binary is the release artifact), docs updates | `render.html` export path must hit the embedded origin; CORS not needed same-origin but must still behave |

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
  export renders PNG/PDF, the binary itself runs the full stack (no container
  image, D18), `pnpm parity` green, coverage report embedded in AGENTS.md with
  no weak-only routes counted as verified.

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

M3 progress log (apitokens family):

- ✅ Probe: `test/parity/probes/0003-apitokens/` (10 transcripts + DB
  rows): `[]`-never-null list, scopes in DECLARATION order, the nested
  ListField choice shape ({"scopes":{"0":[...]}} with nested codes),
  identity-confirm failures, sorted scopes + shown-once plaintext, both
  404 dialects (compact DRF "Not found." for unknown uuid vs SPACED
  handler404 for non-uuid path segments), plus hash rows for the digest
  proofs.
- ✅ Port `doory/apitokens.go` + routes: issue/live/revoke with salted
  HMAC-SHA256 password digests (+SECRET_KEY_FALLBACKS acceptance),
  DRF create validation in declaration order (required/choice/min/max,
  nested raw shapes), clean_text (Cc + _INVISIBLE set), confirm_identity
  (password path + lockout interplay),409 limit envelope, scopes table.
  Router gained {uuid} path templates with Django's converter semantics
  (non-matching segments fall to handler404's dialect).
- ✅ Runner: per-side response **capture** ({capture:name} substitution —
  each side consumes its own issued id; the future D9 email-link
  mechanism too). New body shape rules: prefix, issued token, expires_at
  (each format-asserting, reason recorded).
- ✅ Scenario `04-apitokens` (14 steps) green; loop-caught bug: Raw/RawCodes
  split for nested field_codes.
- ✅ Cross-implementation unit proof: Go reproduces the ORACLE's
  secret_hash (sha256 plaintext) and password_digest (salted HMAC key
  derivation sha256(key_salt+secret)) byte-for-byte from probed rows
  (apitokens_test.go) — mutation-provable.
- ✅ M2-style artifacts: PORT_MAP rows reviewed (scopes/urls ✅,
  models/serializers/services/views 🟡 with pending lists),
  test-mapping disposition for apitokens/tests/test_tokens.py (partial,
  evidence+missing).
- ⬜ Family pending: authenticate() + last_used tracking (mcp family),
  fallback-key rotation branch, confirm_email path (google), 409 limit
  scenario (needs 20 live tokens).

M4 progress log (designs + presets families):

- ✅ Probe: `test/parity/probes/0004-designs/` (15 transcripts) + a
  folder/message micro-campaign during the loop: create bytes, alphabet-
  ized absolute pagination links (replace_query_param SORTS query items),
  ApiError-style validator envelopes, the custom409 conflict body with
  `current`, blank-vs-empty-after-clean name distinction, DRF 6.1's two
  UUID messages (typographic quotes in serializer field context,
  plain `Must be a valid UUID.` for bare run_validation).
- ✅ Port: `document.go` (COMPLETE 310-line validator: structure, motion,
  audio tracks, iterative walk, size bound — all messages/codes),
  `designs.go` (list filters/ordering/pagination, create incl. blank-
  document struct with probe-exact key order, PATCH with revision
  conflict, trash/restore/duplicate/purge/trash-empty),
  `designs_views.go` (conflict envelope, auto-version snapshotting +
  prune, services), `designs_versions.go` (list/create/detail/restore with
  snapshot-before semantics), `folders.go` (depth5/cycle/unique/reparent
  rules + design_count annotation), `presets.go` (seeded tree with exact
  big.Rat to_px rounding + raw JSON column passthrough), plus seedgen
  (0002 seed rows dumped verbatim from the oracle — content parity
  without porting the importer).
- ✅ Runner/comparator: pagination-origin normalization (path+query still
  compared), updated_at/trashed_at/folder/parent shape rules, first-diff
  byte-context reporting (drove every fix), capture debug print.
- ✅ Loop-caught bugs (all real): Sprintf-before-args leaving literal `%s`
  in SQL, dashed-vs-hex uuid storage lookups (detail routes, folder FKs,
  filters), map-marshaled documents randomizing key order, blank-document
  key order, to_px identity mistake (px*96), missing cache header on
  size-presets, conflict-body map order, recordVersion snapshotting the
  version instead of the current document, parent-field name/blank errors.
- ✅ Scenarios `05-designs` (30 steps) + `06-folders` (23 steps) green;
  suite now 6/6 (110 steps).
- ✅ Artifacts: PORT_MAP rows reviewed for designs/presets/pagination/
  ordering (complete: validators, exceptions, urls, presets views,
  seed path ✅; partial with pending lists: views/services/serializers/
  folders/models/parsers), test-mapping dispositions for all 16 designs
  test files + presets test file.
- ⬜ Family pending: template creation (gallery), fill/bulk_fill/fields/
  thumbnail/bulk actions (+doc/ package), DesignJSONParser media-type
  probes, purge_expired_trash command, thumbnails/signals file cleanup,
  folder/design limit409 scenarios.

M5 progress log (elements + gallery + brand families):

- ✅ Probe: `test/parity/probes/0005-elements-gallery-brand/` (15
  transcripts): empty packs, elements cache 300 (elements-only — gallery
  views set none, middleware no-store applies), template detail
  "Not found.", use "Template not found." (DRF custom NotFound message
  vs get_object_or_404 default), exact sitemap bytes off SITE_URL, brand
  create/list shapes with cleansed fields (hex lowercased) and the two
  nested child-error shapes (colors hex, fonts role — straight-quote DRF
  choice message) vs flat list-level messages.
- ✅ Ports: `elements.go` (public reads, django-FORM choice messages with
  invalid_choice codes, pagination 48, cache finalize), `gallery.go`
  (categories/list/detail/use + TemplateFilter semantics, exact sitemap
  with the published-template loop, slug-converter routing), `brand.go`
  (full CRUD: declaration-order validation with partial-mode PATCH,
  nested-vs-flat error shapes, single-default flip, ordering, limits),
  router gained {slug} converter validation + AllowNone for non-DRF
  Django views (no Allow on success).
- ✅ Content strategy: element/gallery catalogs are SEED-COMMAND content
  (ops family) — M5 asserts the full empty-state + validation surface;
  rows note the content paths (seed_elements/seed_templates/import) as
  pending rather than pretending coverage.
- ✅ Loop-caught: django-filter choice errors carry code invalid_choice
  (not invalid), PATCH partial mode must not demand absent required fields.
- ✅ Scenarios `07-brand` (20 steps) + `08-catalog` (16 steps) green;
  suite 8/8 (154 steps).
- ✅ Artifacts: PORT_MAP rows reviewed (brand fully ✅; elements/gallery
  partial with content-seeding pending lists), test-mapping dispositions
  for every elements/gallery/brand test file (sitemap partial with
  scenario evidence; seed/search/import paths pending ops content).
- ⬜ Family pending: seeded-content happy paths (seed_elements/
  seed_templates via row+file dumps), q/relevance search (needs rows),
  template document validator + apply_document, write paths (packsync,
  bulk, normalize).

M6 progress log (fonts family — the S2 spike):

- ✅ Probe: `test/parity/probes/0006-fonts/` (12 transcripts): empty
  catalogue + cache 300, FilterSet messages (django-FORM
  `Select a valid choice.` vs the upload serializer's straight-quote
  choice vs the Django-core script validator whose field_code renders
  JSON **null**), upload201 envelope, the409 variant clash, the sorted
  family-mismatch sentence, junk-file prefixes.
- ✅ Spike S2 delivered: `internal/fontparse` — sniff (ttc/magic), sfnt
  table directory, name (platform preference + UTF-16BE), cmap (formats
  0/4/6/12 with fontTools getBestCmap preference), OS/2/head/post probes
  (weight with Python round-half-to-EVEN, italic, PANOSE category with the
  correct byte offsets), script coverage table, WOFF per-table zlib, and
  WOFF2 (brotli decode with a hard budget + the transformed-directory
  rules recovered from the pinned fontTools source: transformLength IFF
  transformed, glyf/loca default-transform semantics). Cross-proven
  probe-first: fontparse_test.go asserts the ORACLE's probe_worker facts
  for the TTF AND WOFF2 fixtures (family/weight/style/category/glyphs/
  scripts) — mutation-provable.
- ✅ Ports: `fonts.go` (public list filters/messages incl. code-NULL,
  pagination 50, cache finalize; mine list), `fonts_upload.go`
  (FontUploadSerializer validation, add_user_font merge/quotas/clashes,
  probe-accurate storage paths with DASHED uuids, slugify, cleanFamilyName,
  delete lifecycle).
- ✅ Runner: deterministic multipart bodies (fixed boundary, byte-identical
  to both sides) + a media-user-font URL shape rule (per-side random ids,
  format asserted).
- ✅ Envelope engine: field_codes render `null` for code-less Django-core
  validator errors (probe-proven).
- ✅ Loop-caught: media path uuids are DASHED (Django str(uuid)),
  post.isFixedPitch sits after the underline fields (fontTools postFormat
  struct), WOFF2 transformLength presence rule.
- ✅ Scenario `10-fonts` (21 steps) green; suite 9/9 (175 steps).
- ✅ Artifacts: PORT_MAP fonts rows reviewed (uploads/inspector/probe/
  filters/serializers ✅ complete; views/models partial with sync/catalog
  pending), all 12 fonts test files dispositioned.
- ⬜ Family pending: sync_fonts content + catalogue data (ops), usage
  stats, admin (D2), subprocess-isolation substitution note (D6-style
  in-process bounded parse).

M7 progress log (assets family — core upload slice + no-config stock):

- ✅ Probe: `test/parity/probes/0007-assets/` (18 transcripts): auth on
  EVERY asset/stock route (stock is not public — probe-caught), create201
  envelope (dashed media paths, ORIGINAL bytes stored for clean PNGs so
  size == upload), name/tag cleaning chain, invalid_file message, usage
  shape, media-path header set.
- ✅ Spike: the raster pipeline is far smaller than feared —
  `_normalise` stores original bytes when nothing needs stripping, so
  Go only needs DECODE for dims/mime/extent (image/png+gif+jpeg+webp via
  x/image decode). Encoder-byte parity (JPEG/PNG re-encode with EXIF,
  AVIF, WebP thumbnails) deferred with explicit pending notes — no pure-Go
  WebP ENCODER exists (thumbnail = name/URL parity until M14).
- ✅ Ports: `assets.go` (list filters/ordering/usage, multipart upload with
  content-decide sniffing, quota, clean_name/clean_tags shared with
  brand/fonts, patch/delete), `stock.go` (the five stock endpoints in the
  no-configuration world: _not_configured payloads, param validation incl.
  audio vocabularies read from freesound.py, Freesound-specific messages,
  preview -> NotFound), `middleware.go` gained mediaSecurityHeaders at
  chain position #10 (nosniff overwrite + sandbox CSP + svg attachment).
- ✅ Loud-pending discipline: SVG and audio CONTENT pipes answer explicit
  notPorted errors (no silent divergence); AVIF/EXIF paths pending with
  notes; live provider search/import await the remote/SSRF slice with
  fake-provider fixtures.
- ✅ Divergence D12 (non-API error pages: Django templates vs Go defaults)
  with the `headers_only` step flag: body + content-type/content-length
  skip, everything else (incl. the media CSP) compares byte-for-byte.
- ✅ Loop-caught: audio search `results: []` never null, guessed
  Freesound vocabularies corrected from source (music/effects/ambience,
  5 lengths, 5 sorts), MAX_PAGE=100/per_page=30 bounds, media-header chain
  placement.
- ✅ Scenario `11-assets-stock` (28 steps) green; suite 10/10 (203 steps).
- ✅ Artifacts: PORT_MAP assets/core-images rows reviewed (serializers/
  uploads/middleware/ordering done; stock/middleware partial with the
  remote+media pending lists), all 16 assets test files dispositioned.
- ⬜ Family pending: remote/SSRF fetch slice (S-item with fake-provider
  fixtures), audio pipeline, SVG sanitize, EXIF/AVIF parity, quota
  boundary, stock credits/limits, file-lifecycle signals.

M8 progress log (sharing family — links, comments, public token views):

- ✅ Probe: `test/parity/probes/0008-sharing/` (37 transcripts): link
  envelope (SITE_URL url + 32-char urlsafe token), never-cache string +
  `Vary: Cookie, X-Share-Password, origin` order, shared design payload
  bytes (owner pseudonym, document passthrough, fonts []), password gate
  (missing/wrong401, LOCK at exactly the 10th wrong with the DEFAULT DRF
  detail, right password clears only the pair window), the shared
  conflict message that DIFFERS from the owner path, comments_disabled as
  its own code (NotFound subclass), comment author order (name,
  avatar_url, id), x/y float rendering (10.0), append-slash 301 on
  slash-less shared paths.
- ✅ Ports: `sharing.go` (owner link CRUD with active-limit + reactivation
  count, expiry validation, owner comments with anchor/reply rules and
  reply-following moves) + `sharing_public.go` (resolve + never-cache
  finalize headers on EVERY response incl. dispatch401 — Route flag
  SharedHeaders; three-window password gate with probe-exact lock
  semantics; edit-save with CAS + shared conflict message; fonts-usage
  document walk).
- ✅ Runner/router: trailing-slash DISCIPLINE (segments keep their empty
  tail — Django's `<converter>/` patterns only match slashed paths, so
  slash-less requests get the APPEND_SLASH 301); append-slash moved to
  middleware position #5 (short-circuits #6..#12 like CommonMiddleware:
  no Cache-Control/Permissions-Policy/XFO on the redirect — probe18);
  design_id uuid shape rule; never-cache `Expires` (wall-clock, denied
  like Date/D10).
- ✅ Loop-caught (all real): ambiguous join columns in share-link SQL
  (arg order too — design/token bound before `now`), trailing-slash
  matching, json.RawMessage needed for INPUT (rawCompacted only renders),
  uncaptured link1 in my own scenario, per-side design_id in payloads.
- ✅ Scenario `12-sharing` (41 steps) green; suite 11/11 (244 steps).
- ✅ Artifacts: PORT_MAP sharing rows reviewed (exceptions ✅; views/
  serializers/services/models partial with pending lists; fonts/usage
  ✅), all 9 sharing test files dispositioned.
- ⬜ Family pending: deactivate/reactivate flow (link-id capture after the
  public section), expiry-window boundaries, second-user author scoping,
  comment limit, shared-fonts fixture scenario.

M9 progress log (exports family — spike S1 delivered):

- ✅ Spike S1: `internal/chromium` — CDP driver (websocket) launching the
  SAME system Chromium on both sides (EXPORT_CHROMIUM_PATH): attach,
  setDocumentContent, printToPDF (px→inches), navigate, evaluate
  (awaitPromise), event fan-out (the Fetch interceptor must not eat
  waiters' load events — caught live), Fetch-requestPaused interception
  with the policy (fail/continue/fulfill, worker-side fetch with no
  redirects). `internal/renderworker` runs it as the per-render
  SUBPROCESS (`doory render-worker spec.json`) with the upstream spec/
  report JSON contract, env allowlist, TMPDIR isolation and kill-group
  semantics — chromium_worker.py's architecture preserved.
- ✅ `internal/svg` — full sanitizer port (allowlists, CSS safety with
  escape unescaping, href rules, use-expansion bounds, intrinsic size),
  DIFFERENTIAL-tested against the oracle sanitizer on a hostile fixture
  set (script/DTD/xlink/style).
- ✅ `internal/pdfmeta` — pypdf-equivalent surgery: exact boxes anchored
  top-left from chromium's rounded sheet, Trim/Bleed for bleed/marks,
  /Lang, /Creator //Title; number writing matches pypdf's round-6 +
  strip (empirically derived: 0.9599900000000048 -> 0.95999,
  1.0800019999999932 -> 1.080002); classic-xref parse + rebuild.
- ✅ `internal/pdfscan` + comparator mode `pdf`: page count, boxes,
  catalog values (PDF strings unescaped — pypdf octal escapes); PDF
  bytes are never compared (chromium embeds per-render ids/times).
- ✅ Ports: `exports.go` (pdf/image/download views, validation with
  DRF min/max messages, disabled gates IN ORACLE ORDER — design lookup
  precedes the image gate), `render.go` (geometry/crop marks/HTML/
  fonts embedding/render slots/worker model), `delivery.go` (Django-
  signing byte-compatible port incl. b62 timestamps + compression),
  `filenames.go` (simple + structured), runner stub render page served
  once per run with the render protocol (deterministic PNG bytes
  compared directly).
- ✅ Loop-caught (all real): expression-vs-call evaluation (playwright
  auto-calls, CDP doesn't → IIFEs), Fetch-enable racing navigation
  (enable AFTER attach on the page session), EXPORT_* env reads missing
  from FromEnv, regex without capture groups, pypdf round-6 number
  writing, empty-body multipart parse order, disabled-gate ordering.
- ✅ Scenarios `13-exports` (13 steps: validation, PDF structure through
  the shared chromium, image via the stub, delivery404) +
  `14-export-disabled` (6 steps) green; suite 13/13 (189 steps).
- ✅ Artifacts: PORT_MAP exports+core/svg rows reviewed (pdfmeta/policy/
  layout/delivery/fonts/exceptions/svg ✅; views/services/renderer/
  worker/filenames partial with pending lists), all 13 exports test files
  dispositioned.
- ⬜ Family pending: pdfmeta bleed+marks box cases, ExportBusy/timeout
  paths, jpg/webp conversion (no pure-Go WebP encoder — encoder
  substitution), fonts-in-pdf scenario, purge_exports command (ops).

M10 scoping (ai family — sources read, semantics derived; build next):

- **Secrets (the crypto heart)**: `apps/ai/secrets.py` — Fernet with
  keys: AI_SECRETS_KEY (+fallbacks) else HKDF-SHA256 derived from
  SECRET_KEY (salt `doory.ai.secrets`, info `provider secrets v1`).
  cryptography internals recovered: key = b64url-decoded 32 bytes,
  signing = key[:16], encryption = key[16:] (AES-128-CBC + HMAC-SHA256,
  version 0x80 + timestamp + IV). Go port = crypto/aes + HMAC; cross-impl
  unit proof plan: oracle encrypts under the probe SECRET_KEY → Go
  decrypts, and vice versa (pbkdf2/apitokens evidence pattern).
- **Transport/SSRF policy** (`transport.py`): user policy https-only,
  ports 443/8443 (+AI_USER_PROVIDERS_ALLOW_HTTP/ALLOWED_PORTS), every
  resolved address must be public; operator policy any scheme/port,
  private only for AI_ALLOWED_PRIVATE_HOSTS or IP literals. check_url
  form rules (no credentials, no query/fragment, unsafe-char refusal,
  2048 cap, host trailing-dot strip, unspecified/multicast refused).
  `resolve_hosts=False` = form-only check (the admin validation path).
- **Env providers** (env.py): AI_PROVIDER_<n>_* (URL/STYLE/STYLE_CONFIG/
  TASKS/MODELS/AUTH_HEADER/AUTH_VALUE/SEND_OFFSITE/TIMEOUT/MAX_IMAGE/
 QUOTA), id `env-<n>`, never persisted; validation → ConfigError.
- **Views**: ModelListView (options; unknown_task404; device+env+DB
  providers), ContractView (serve checked-in contract/openapi.yaml,
  public + cache3600), providers CRUD gated by UserProvidersAllowed
  (AI_USER_PROVIDERS=off default → 403 ai_user_providers_disabled —
  assertable), ProviderModels PUT (replace models), load-models/test
  (network → pending with the fake-provider harness), TaskView
  (unknown_task404, TaskRequestSerializer, asset404, option resolution:
  ModelNotAvailable404/RunsOnDevice400, provider paths pending).
- **Quotas**: AICall daily counting (local midnight) + QuotaReached429
  with wait sentence + Retry-After (same rendering as lockout/share).
- **Errors**: ai_<code> envelope mapping (STATUS table),
  ai_model_not_found/ai_runs_on_device/ai_unsupported_asset/ai_quota/
  ai_user_providers_disabled.
- **Slice**: secrets + catalog + contract + provider CRUD + models-put +
  task-view validation/resolution + quotas; pending: load-models/test/
  proxy/styles pipelines + transport call paths (fake-provider harness,
  like assets/remote), admin site (D2), reencrypt command (ops).

M10 progress log (ai family):

- ✅ `internal/aikeys` — Fernet (key[:16] HMAC / key[16:] AES-128-CBC,
  padded urlsafe b64 tokens) + HKDF-SHA256 derivation; CROSS-PROVEN both
  directions: two oracle-cryptography-encrypted fixtures decrypt here,
  a Go-encrypted token decrypts under cryptography.
- ✅ Ports: `doory/ai*.go` (settings/env providers, transport policies +
  check_url form rules, catalogue with audience merge, provider rows +
  ordered serializer output, gate + CRUD + models-put + contract view,
  task view with declaration-order validation, run pipeline to the
  refused dial, quotas/AICall/in-flight), `doory/contract_openapi.yaml`
  (embedded, byte-served with FileResponse headers), 5 app.go routes
  (slug LAST).
- ✅ Loop-caught (all real): net/http PathValue vs the router's own
  routeParam (slug task + uuid lookups were empty), append-slash
  redirecting EXACTLY-routed non-slash paths (contract.yaml -> 301),
  missing bound args in two loadAIProviders calls, dashless UUID storage
  vs dashed API ids (hexUUID canonicalization), NOT NULL
  secret_encrypted vs nil []byte, Go map key sorting vs DRF declaration
  order (options + nested models), created_at needing renderDRFDatetime
  (T+Z isoformat, +1 byte), newUUID4Hex vs newUUID4HexDashed for Django
  uuid pks.
- ✅ Scenarios `15-ai` (30 steps: contract bytes, env-provider options,
  CRUD with every validation message, keeps-secret rule, model replace,
  per-user limit, run to ai_unreachable) + `16-ai-disabled` (11 steps:
  gate 403s, public contract, validation-before-lookup) green; suite
  15/15.
- ✅ Artifacts: PORT_MAP ai section reviewed (secrets/serializers/tasks
  done; rest partial with pending lists), all 9 ai test files
  dispositioned.
- ⬜ Family pending: load-models/test views + the full provider exchange
  (fake-provider harness), style_config reader messages, DNS/private-
  address decision table, AI quota HTTP form (needs successful calls),
  device options (installed-model registry), throttles, admin (D2),
  reencrypt command (ops).

M11 scoping + spike S0 progress (collab family):

- **Sources read**: protocol.py (Yjs wire + bounded update scanner),
  consumers.py (handshake/login/refusals/frames), engine.py (open/merge/
  budgets), room.py (945 - RoomRegistry, outboxes, flush), store.py
  (admit/release/persistence), access.py (owner/link grants, origin),
  limits.py (TokenBucket), middleware.py, availability, notify, signals,
  schema.py (document<->Y mapping + anomalies/repair), models (YState,
  YUpdate, RoomConnection, CollabInstance), views (stats). WS route:
  `ws/designs/<uuid>/` behind RealtimeOriginValidator + session auth;
  REST: GET /api/v1/collab/stats/. Close codes 44xx are part of the
  contract (4401/4403/4404/4408/4413/4429/4400/1011/1013).
- **Engine decision (spike S0)**: upstream runs pycrdt (pyo3/yrs, Rust).
  The Go engine is the vendored `third_party/yjs-go` v0.1.6 with the
  patches in `third_party/yjs-go/PATCHES.md` (11 changes, all fixture-
  driven: parentSub-with-origin wire rule, map-set replacement form,
  nested type materialization + parentID, pending buffer, sorted struct
  store, snapshot delete set, multi-value array items, map slot walk).
  CONFORMANCE PROVEN BOTH DIRECTIONS against pycrdt 0.14.10 fixtures:
  `test/parity/probes/0011-collab-crdt/` (generator.py, verify.py,
  40 fixture files, go_updates/verdict.json = "documents equal") via
  `internal/crdt` (conformance_test.go: snapshots+SV byte-equal, 12-edit
  stream incl. merge paths, concurrent convergence both orders, witness).
- **Loop-caught during S0** (all real): parentSub read/written with
  origins present (fixture rejects), map replacement missing originLeft,
  nested ref-7 items never materialized (pages invisible), parentID
  dropped at decode, no pending buffer (edits-before-base client groups),
  unsorted struct store (wrong state vectors), snapshot missing its
  delete set (resurrections), multi-value ContentAny read as one value,
  map slot head stale after remote replace.
- ⬜ M11 remaining: protocol.py port (framing + scan_update +
  UpdateBudget), room/store/engine port on yjs-go, consumers + WS driver
  in the parity runner (dial, text/binary frames, close codes), access/
  limits/middleware, stats endpoint, then paired scenarios; known engine
  gap to cover: array index ops over decoded multi-value batches
  (PATCHES.md known gaps).

M11 progress log (continued):

- ✅ `internal/yproto` — protocol.py port: lib0 Reader/Writer (varuint,
  varbytes, varstring, UTF-16 length), frame parsing (sync/awareness/
  query-awareness with trailing-byte rules + 4400 ProtocolError),
  state vectors (bounded entries), awareness updates (bounded count +
  state bytes), and the BOUNDED UPDATE SCANNER (scan_update: gaps,
  unknown roots, XML/Skip refs, any-depth bound, reference/delete-set
  history checks, extents, new_clients, originless/right_only/inside_refs
  counters with the bisect over multi-value spans).
- ✅ Scanner conformance: `scan_fixtures.py` captures the oracle's
  verdicts for the real edit-stream updates (replayed against the state
  they would apply to), the concurrent streams, plus7 crafted negatives
  (truncated, trailing, empty, huge client count, unknown root,
  reference gap,40-level nesting) — `internal/yproto` reproduces every
  accept (with all counters/extents/new-clients/originless totals) and
  every reject message byte-exactly (22/22).
- ⬜ Next: schema.py + engine.py port (document<->Y conversion, budgets,
  compaction), room/store/consumers, WS driver, scenarios.

M11 progress log (schema + engine ports):

- ✅ `internal/yschema` — schema.py port: document <-> Y mapping (roots
  meta/pageOrder/pages; _clean/_unique_ids/_page_map/_object_map with
  the insert-before-populate builder discipline), the DEFENSIVE reader
  (props with depth bound, _ordered first-occurrence/dangling rules,
  anomalies incl. bad_props paths + no_pages), has_no_pages, repair
  (runs-removal, unlisted deletion, restored-page), json_equal.
  write_changes/sync_order (the REST-save diff engine) lands with the
  designs slice.
- ✅ Conformance: write/read roundtrips for all four fixture documents
  == the oracle's ydoc_to_document output; READ over the oracle's own
  snapshots; the replayed edit-stream final state; both converged
  concurrent states; live-document replacement (write_document clears
  the roots). Plain-JSON page values (pycrdt dict assignment) are
  flagged unlisted and skipped - exactly as the oracle reader does.
- ✅ `doory/collab_engine.go` — engine.py port: apply_client_updates
  (scanner + UpdateBudget + the exact/near-limit size logic + slow-apply
  poison + noop detection + merged delta + pages_gone), UpdateBudget
  (new-clients/right-only/inside-refs/originless caps with the same
  reject messages), Convert (validated DoryDocument via the M4
  validator), RepairDelta (state-vector delta), MakeReplacement,
  document size with Python json.dumps(ensure_ascii=False) byte rules.
- ✅ Engine conformance (engine_fixtures.py): the oracle's batch verdicts
  (12 updates incl. one no-op, outcomes/sv/size/applied-count/flags) and
  the no-op replay rejection reproduced exactly; the merged delta (from
  the pre-batch state vector - merge_updates storage semantics) rebuilds
  the same state on top of the base.
- ✅ Engine loop-caught: full states exclude DELETED structs (mid-stream
  clock gaps are legal), while a client group whose FIRST struct starts
  beyond the known state pends as a whole (open_doc of a merge_updates
  result on an empty document integrates nothing) - the pending rule in
  third_party/yjs-go is group-granular for the first clock and
  item-granular for parents.
- ⬜ Next: room/store/consumers (admit/release, outboxes, ready frame,
  44xx close codes), access/limits/middleware, stats endpoint, the WS
  driver in the parity runner, paired scenarios.

M11 progress log (consumer runtime + live WS verification):

- ✅ Consumer runtime live-verified against the Go server (probe0012
  phases replayed over the real socket, oracle-pinned contract): owner
  WS now completes the full handshake - session -> ownerGrant -> CollabAdmit
  -> enter -> ready frame -> binary welcome sync/step1 -> ping/pong all
  delivered; unknown text frame -> close4400 bad_frame (verified before
  the runtime fixes; re-verified after).
- ✅ Loop-caught #1 (access): the design uuid arrives in the URL dashed
  (`ws/designs/<uuid>/`, uuidShape) but designs_design stores uuid.hex
  (32 hex); ownerGrant/linkGrant/room keys must strip the dashes
  (hexUUID) or the design lookup misses (grant=nil -> no ready frame).
- ✅ Loop-caught #2 (room actor): the inbox batch loop was ported as
  `for len(items) < collabBatchLimit { select { case ...: append; default: } }`
  - never breaks on an empty inbox, so the room actor hung on the first
  item (welcome never dispatched; syncs never sent; pings worked because
  they bypass the room actor via the outbox). Fixed with an explicit
  batching flag.
- ✅ Runner WS driver landed (`test/parity/runner/ws.go` + step-level
  `ws`/`[ws]` TOML binding): dial (`ws://<host>/api/v1/ws/designs/<uuid>/`
  with jar cookies + Origin), waitForType (JSON `type`), send (JSON text /
  binary hex), close. Scenario 17-collab wired (connect+ping+pong+close).
  Run so far: dial/handshake reachable; oracle side still 404s on the
  design-scoped route in the scenario context (probe-proven path), Go side
  accepted and awaited ready.
- ✅ Client sync step2 batch/commit relay verified live (Go): client
  Sync/SyncStep1 -> server Sync/SyncStep2 full-state reply (binary,
  design content embedded), after the batch-loop fix.
- ✅ Item 3 finished this stretch (close codes + stats live): stats
  staff-only -> 403 permission_denied (non-staff); pending design -> WS
  close 4408 auth_timeout (auth timer). Close-code paths + stats shape
  now parity-verified on the live Go server.
- ✅ Item 4 finished this stretch (scenario 18 final shape): register ->
  design create -> live owner WS connect (ready) -> PATCH design
  (document) -> BOTH sides 409 ok (use_realtime guard). Scenario 18
  runnable + guard green.
- ⬜ Next: scenario 17 green on both sides (oracle-side design-scoped WS
  dial 404 root-cause open), final M11 log + sweep + M11 commit.

M11 remaining build order (sources read this session, semantics noted):

1. `doory/collab_access.go` — access.py: Grant/Forbidden/DesignGone/
   PasswordNeeded/Locked, owner_grant (trashed ->4404 design_trashed),
   link_grant over the M8 sharing helpers (resolve/enforce/record),
   origin_allowed (scheme+host match against CSRF_TRUSTED_ORIGINS +
   SITE_URL patterns + ALLOWED_HOSTS validation - port Django's
   validate_host logic).
2. `doory/collab_store.go` — store.py: LoadedState/load_or_init
   (select-for-update + staleness rule `base_revision == revision OR not
   reset_allowed`), reset_state, append_update, read/apply_compaction
   (snapshot-claim CAS), flush_document (up_to_date / external_change /
   revision bump + describeDocument facts + auto-version via
   maybeRecordAutoVersion), read/commit_replacement (ticket CAS),
   load_current, purge_stale_presence, unflushed_designs, admit (insert
   then count: design_full/too_many_connections), other_instance_serves,
   release, heartbeat (10s heartbeat,30s stale x4), room_is_live
   (the409 use_realtime guard input), check_access (session recheck via
   HASH_SESSION_KEY + auth-hash fallbacks, link role/expiry/revocation
   -> closed reasons) and its CLOSE_FOR_REASON map
   (4403/4401/4404 in room.py).
3. `doory/collab_room.go` — room.py (945): actor with inbox/outbox pumps
   (OUTBOX512/INBOX2000), Member (controlled/awareness/introduced sets,
   can_write = owner|edit), VersionTracker (contiguous+ahead set),
   awareness slots (2048B,60s expiry, palette colours), ready/pong
   frames, flush scheduling + compaction threshold, recheck cycles,
   db() wrapper = single-process direct calls.
   DECISION: channels/notify_design (signals -> group layer) becomes
   direct in-process calls into the RoomRegistry (single-process
   substitution; behaviour identical for paired scenarios - record in the
   M11 log; multi-process redis layer stays upstream-only).
4. `doory/collab_consumer.go` — consumers.py: RealtimeOriginValidator,
   client_ip (NUM_PROXIES/XFF), connect flow (4401/4429 rate +
   pending/auth timer4408, owner fast-path, DESIGN gone4404),
   enter/admit/ready frame (role/canWrite/user/title/revision/limits),
   auth frames (bad_auth4400, forbidden4403, password_required errors,
   throttled4429), receive path (4413 frame cap, TokenBucket4429,
   ping/flush/bad_frame, scan+room.submit), disconnect/release.
   notify/availability/signals/middleware/stats: in-process notify hub;
   realtime_enabled = settings flag; RealtimeGuardMiddleware = the same
   check in the designs PATCH path (409 use_realtime JSON); signals
   design_saved/link_changed -> registry notify (title/recheck); stats
   endpoint (staff) behind IsAdminUser.
5. WS driver in test/parity/runner: per-step `ws` support — dial with
   cookie jar + Origin header, text/binary frames (hex payloads), expect
   frames (match/capture ready fields), close code+reason assertions,
   multi-connection scenarios (A/B roles) + `env` per scenario.
6. Scenarios:17-collab (owner connect/ready/ping-pong/CRDT roundtrip via
   engine fixtures, close codes),18-collab-guard (409 use_realtime +
   stats staff-only). Then PORT_MAP/dispositions + PLAN final log +
   sweep + one M11 commit.

M11 probe 0012-collab (oracle WS via uvicorn doory.asgi, in-memory channel
layer) — raw evidence in test/parity/probes/0012-collab/frames.json:
- Phase A (owner): ready frame {role, canWrite, user{id,name,avatar_url},
  title, revision, limits{maxFrameBytes:1048576, maxParticipants:25}}
  immediately followed by binary sync/step1 (13B state-vector request);
  text ping -> {"type":"pong"}; client sync/step1 -> sync/step2 (345B
  full-state update); clean local close.
- Phase B: foreign Origin -> handshake refused with HTTP403 (no upgrade);
  no session cookie -> upgrade accepted then close4401 not_authenticated.
- Phase C: unknown text frame type -> close4400 bad_frame; unknown binary
  message type -> close4400 bad_frame (after the standard ready/sync
  prelude frames).
Probe harness notes: oracle served with uvicorn (prod settings +
DATABASE_URL=oracle.sqlite3), per-phase unique emails, websockets v17 API
(recv returns the message; send infers binary from bytes).
- ⬜ Build next: doory/collab_room.go (actor) + collab_consumer.go (the
  contract above), gorilla/websocket upgrade on ws/designs/{uuid}/ with
  the origin validator, then the runner's ws driver + scenarios.

M12 scoping (mcp family — sources read, semantics derived; build next):

- **Transport**: `doory/mcp_asgi.py` — Streamable HTTP app via the
  `mcp` python SDK (`MCPServer.streamable_http_app`, stateless_http=True,
  json_response=True, streamable_http_path=settings.MCP_PATH
  (/mcp), max_request_body_size=settings.MCP_MAX_REQUEST_BYTES (4MB)).
  Served by its own process (`uvicorn doory.mcp_asgi:application`, port
  8002) — NOT through the Django URL resolver.
- **Auth**: `apps/mcp/auth.py` TokenAuthMiddleware — bearer PAT via
  `apps/apitokens.services.authenticate`; `/mcp/health` DB health probe
  (no auth, 200/503); MCP_ENABLED off -> 404 {"error":"not_found", ...};
  missing/invalid bearer -> 401 {"error":"invalid_token",
  "error_description":"Send a Doory personal access token:
  Authorization: Bearer doory_…"}, `www-authenticate: Bearer realm="Doory"`.
- **Context**: apps/mcp/context.py ContextVar Caller(user, token) ->
  has(scope); current().label = token.name.
- **Tools**: registry.py `@tool(scope=…)` decorator wrapping sync fn with
  scope checks, rate limits (MCP_CALLS_PER_MINUTE per user, 120 default;
  0 = unlimited), DB hygiene, error translation (explain(): Http404,
  RevisionConflict, EditError, Pydantic/Drf/Django validation -> agent
  message); TOOLS/RESOURCES lists; ToolAnnotations (title, etc.).
- **Resources**: `mcp/resources.py` (@resource(uri, scope) -> content).
- **Prompts**: `mcp/prompts.py` (register(server)).
- **Resolver**: `mcp/resolver.py` (designs resolver over the design
  services, the same surface the MCP tools call).
- **Tools surface (tools/*)**: ai, assets, brand, catalog, comments,
  designs, export, history, organize, sharing — each wrapping existing
  REST service logic (designs CRUD, versions, comments, assets, AI
  providers, export, sharing).
- **Slice**: server bootstrap (MCPServer + transport security from
  ALLOWED_HOSTS/SITE_URL), bearer auth + health + off-switch, tool/resource/
  prompt registry + dispatch, resolver, then tools/* implementations.
  MCP env knobs: MCP_ENABLED, MCP_PATH, MCP_CALLS_PER_MINUTE,
  MCP_MAX_REQUEST_BYTES, MCP_EXPORT_INLINE_MAX_BYTES,
  MCP_EXPORT_URL_TTL_SECONDS, URL_IMAGE_UPLOADS (M10 export semantics).

M12 build order (sources read this session, semantics noted):

1. `doory/mcp*.go` — server bootstrap (MCPServer equivalent: tool/resource/
   prompt registries + JSON-RPC dispatch), `doory/mcp_asgi.go` (ASGI app),
   `doory/mcp_auth.go` (TokenAuthMiddleware + /mcp/health + off-switch),
   `doory/mcp_context.go` (Caller contextvar), env knobs (MCP_ENABLED,
   MCP_PATH, MCP_CALLS_PER_MINUTE, MCP_MAX_REQUEST_BYTES, MCP_EXPORT_*).
2. `doory/mcp_registry.go` — tools/resources/prompts specs + scope checks,
   rate limits, error translation.
3. `doory/mcp_resolver.go` — design resolver surface shared with tools.
4. `doory/mcp_tools_*.go` — the tools/* wrappers over designs/assets/AI/
   export/sharing services.
5. MCP ASGI transport: Streamable HTTP (stateless JSON responses, POST
   JSON-RPC, /mcp/health, bearer) — Go ASGI-ish net/http equivalent.
6. Probe 0013-mcp (oracle MCP via uvicorn doory.mcp_asgi, prod settings):
   health, off-switch 404, bearer 401 invalid_token (+www-authenticate),
   initialize, notifications/initialized, tools/list, tools/call.
7. MCP driver in test/parity/runner (JSON-RPC transcripts w/ bearer PAT),
   scenario 19-mcp (initialize/tools-list/tools-call/resources/prompts),
   PORT_MAP/dispositions + final M12 log + sweep + M12 commit.

M12 probe 0013-mcp progress (this session):
- ✅ Oracle booted (uvicorn doory.mcp_asgi:application, prod settings,
  MCP_ENABLED=true, MCP_PATH=/mcp, MCP_CALLS_PER_MINUTE=120,
  MCP_MAX_REQUEST_BYTES=4194304; Django API on 127.0.0.1:8012).
- ✅ GET /mcp/health -> 200 {"status":"ok"}.
- ✅ POST /mcp (unauth) initialize -> 401
  {"error":"invalid_token","error_description":"Send a Doory personal
  access token: Authorization: Bearer doory_…"}, www-authenticate
  `Bearer realm="Doory"`.
- ✅ Authenticated JSON-RPC round completed (probe artifacts in
  test/parity/probes/0013-mcp/, 6 transcripts: health 200; unauth init
  401; notif initialized 202; tools/list 200 (45 tools); tools/call
  list_designs 200 (empty designs list); tools/call create_design 200
  (design created)).
- ⬜ MCP off-switch (MCP_ENABLED=false) -> 404 not_found probe.

M12 progress log (go transport slice, this session):

- ✅ `doory/mcp_context.go` — McpCaller(user, token, scopes, label) +
  context bind/current.
- ✅ `doory/mcp_auth.go` — /mcp/health (dbReachable), /mcp off-switch
  (404 not_found), bearer token -> apps/apitokens services.authenticate
  equivalent (prefix + sha256 whole-token digest, expiry/revocation) ->
  401 invalid_token + `WWW-Authenticate: Bearer realm="Doory",
  error="invalid_token"`.
- ✅ `doory/mcp_server.go` + `doory/mcp_dispatch.go` — MCPServer +
  JSON-RPC dispatch (initialize/202-notification/tools/list/tools/call),
  stateless JSON responses.
- ✅ `doory/settings.go` — MCPPath/McpCallsPerMinute/McpMaxRequestBytes/
  McpExportInlineMaxBytes/McpExportURLUploads knobs.
- ✅ `doory/app.go` — routes /mcp (POST) + /mcp/health (GET).
- ✅ Live Go round green: health 200 {"status":"ok"}; unauth init ->
  401 invalid_token; PAT init -> 200 (capabilities/instructions/
  protocolVersion 2025-06-18/serverInfo/websiteUrl); notif initialized
  -> 202; tools/list -> 200 {"tools":[]}; tools/call -> unknown-tool
  envelope (tools not yet registered — next slice).

M12 progress log (registry slice, this session):

- ✅ `doory/mcp_registry.go` — probed tool surface: mcpToolDefs() -> 45
  tools (name/title/description/scope/readOnly/destructive/idempotent),
  mcpResources() (doory://designs/{design_id}), mcpPrompts().
- ✅ `doory/mcp_server.go` NewMcpServer() now builds Tools/Resources/
  Prompts from the registry (tool bodies land in the next slice; handlers
  answer the not-ported error envelope).
- ✅ Live Go round green: tools/list -> 200 {"tools":[45 entries]}
  (list_assets readOnlyHint=true, ...); tools/call list_designs ->
  isError envelope ("Tool body not ported yet: list_designs").
- ⬜ Tool bodies (designs/assets/AI/export/sharing wrappers), resolver,
  rate-limit (mcp_callwindow) enforcement, resources/prompts dispatch;
  then MCP driver + scenario 19-mcp; then PORT_MAP/dispositions + final
  M12 log + sweep + M12 commit (item 5).

M12 next build order delta (registry slice landed):
1. `doory/mcp_context.go` ✅, `doory/mcp_auth.go` ✅, `doory/mcp_server.go` +
   `doory/mcp_dispatch.go` ✅, `doory/mcp_registry.go` ✅ (tools surface).
2. `doory/mcp_resolver.go` — design resolver (ASSETS_WRITE gating) ⬜.
3. `doory/mcp_tools_*.go` — tool bodies ⬜.
4. MCP ASGI transport ✅ (Go net/http), probe 0013-mcp ✅, driver +
   scenario 19-mcp ⬜.
5. PORT_MAP/dispositions + final log + sweep + M12 commit ⬜.

M12 final log (this session — transport + registry slice, sweep green):

- ✅ M12 slices landed: doory/mcp_context.go, mcp_auth.go, mcp_server.go,
  mcp_dispatch.go, mcp_registry.go (+ settings knobs, app routes /mcp +
  /mcp/health).
- ✅ Live Go MCP rounds green: health 200; unauth init 401 + challenge;
  PAT init 200 (capabilities/instructions/protocolVersion 2025-06-18/
  serverInfo/websiteUrl); notif initialized 202; tools/list 200 with 45
  probed tools; tools/call -> registered-tool envelope.
- ✅ Probe 0013-mcp complete (health, unauth 401, authenticated round,
  off-switch 404).
- ✅ Final sweep green: pnpm run check, pnpm run parity-fast 15/15, port-map
  drift, divergence consistency, lint-scenarios.
- ⬜ M12 remaining build: tool bodies, design resolver, rate-limit
  enforcement (mcp_callwindow), resources/prompts dispatch, MCP driver +
  scenario 19-mcp; item 5 boundary reached (final log + sweep + commit).

M11 final log (this session — WS runtime + scenarios green, sweep green):

- ✅ Scenario 17-collab GREEN on both sides (14 steps): owner ready frame
  (role/canWrite asserted), welcome sync/step1, ping->pong, client
  sync/step1 -> sync/step2 whose decoded document is EQUAL across sides
  (page uuids normalized), HTTP title PATCH -> live `title` frame, unknown
  text/binary frame 4400 bad_frame, no-session 4401 not_authenticated,
  foreign Origin HTTP403 at the handshake, trashed design 4404
  design_trashed.
- ✅ Scenario 18-collab-guard GREEN (8 steps): designs PATCH document with a
  live room -> 409 both sides; stats 403 permission_denied (non-staff) and
  401 (no session) with the DRF `Allow: GET, OPTIONS`.
- ✅ Scenario 19-collab-share NEW and green (14): edit-link login -> ready
  role=edit canWrite=true, view-link -> canWrite=false, password link ->
  error password_required then ready with the password, forged token ->
  4403 forbidden, pending non-owner -> 4408 auth_timeout.
- ✅ Scenario 20-collab-limits NEW and green (5): REALTIME_MAX_FRAME_BYTES=64
  -> 4413 frame_too_large; REALTIME_RATE_MESSAGES_PER_SECOND=1/BURST=1 ->
  4429 rate_limited.
- ✅ Scenario 21-collab-peers NEW and green (12): TWO named sockets in one
  room (owner A + edit-link guest B), B's awareness update relayed to A, B's
  leave relayed as the `null` awareness entry.
- ✅ Probe 0014-collab-share pinned the login/limit contract first (link
  roles, password gate, 4403/4404/4408/4413/4429, sync round trip) — and
  caught a semantic worth recording: a share-link login REQUIRES a session
  (an anonymous socket is refused 4401 before auth frames are read).
- ✅ WS driver grew exactly what the plan listed: cookie jar + Origin
  override, text/binary (hex) frames, frame-kind matching (sync/step1,
  sync/step2, awareness), ready-field assertions, server close
  code+reason assertions, `sends` for rate probes, NAMED SESSIONS across
  steps (A/B peers) and per-side capture resolution; frames looked past are
  kept so `ready` and the welcome sync cannot swallow each other.
- ✅ Loop-caught this stretch (all real, each now covered):
  1. WS route shape: the oracle serves `/ws/designs/<uuid>/`, NOT
     `/api/v1/ws/...` (a WSGI-prefix assumption) — the Go route, the runner
     and the docs now use the bare path.
  2. Oracle boot: `manage.py runserver` serves the WSGI handler only (no
     --asgi flag in this Django line), so every design-scoped WS dial 404'd
     while the probe (uvicorn doory.asgi) was green. The runner now boots the
     oracle on `uvicorn doory.asgi:application` (and migrates the workdir DB
     itself; DATABASE_URL, not ORACLE_DATABASE_URL, is what settings read).
  3. Cookie jar: Go's cookiejar returns NO cookies for a ws:// URL — the
     dial went out session-less (4401 on the oracle, silence on Go). Lookups
     now go through the http:// form, and the cookies are joined into ONE
     Cookie header (Add appended a multi-value header, keeping only the
     first).
  4. Per-side captures: a WS step resolved `design_id` from the shared vars,
     so the Go server was dialed with the ORACLE's design id (no such design
     -> auth timer -> 4408 instead of ready).
  5. Awareness frames were batched as document updates: the port only
     excluded SyncStep1 from the update batch, while upstream requires an
     actual SyncFrame. Presence updates were applied to the engine and never
     relayed (scenario 21 caught it; peer A received nothing).
  6. stats `Allow`: a DRF `@api_view` function view answers `GET, OPTIONS`
     (only class views alias HEAD) — the collab route was class-styled.
  7. signals were ported but never called: title/trash/restore/delete and
     share-link create/update/delete now notify the registry
     (doory/collab_signals.go), so a live room broadcasts `title` and
     re-checks access; saves made by the room itself stay suppressed.
- ✅ Engine gap closed (PATCHES.md 12 + 13): array index operations over
  decoded multi-value ContentAny batches count VALUES and split the batch
  (Yjs Item.splitItem — no clock consumed); local interior inserts now carry
  OriginRight, without which a snapshot round trip appended them. Fixtures:
  probes/0015-array-index (9 pycrdt cases) reproduced by
  internal/crdt/array_index_test.go (values, Len, clock accounting,
  round trip).
- ✅ `doory collab-maintenance` ports the management command (purge stale
  presence, save unflushed designs unless a room is live, compact long
  update logs) with the command's exact summary line.
- ✅ PORT_MAP collab section reviewed (21 rows: 19 ✅, 2 🟡 with named
  gaps), `// Ports backend/apps/collab/...` claims added, collab family
  dispositions written for all 26 upstream test files
  (ledger/test-mapping-v*.json), D1/D3 evidence extended with the
  single-process notify substitution and the new engine fixtures.
- ⬜ Collab family remains 🟡 on named gaps only: password attempt lockout
  (5 wrong -> 4429 password_locked), connect-rate window, byte-rate bucket,
  idle timeout, max-participants design_full, link revocation mid-session,
  slow-apply poison/block_user, live `collab.update` relay for REST edit ops
  (needs the designs write_changes slice), stats 200 body (no staff
  promotion path in the paired runner), REALTIME_ENABLED=false route
  absence, and write_changes/sync_order (designs family).
- ✅ Final sweep green: pnpm run check, pnpm run parity-fast (15/15),
  collab scenarios 17/18/19/20/21 green, port-map drift, divergence
  consistency, lint-scenarios.

M13 scoping (commands + content family — sources read, semantics derived):

- **The 15 management commands**: ai/reencrypt_ai_secrets (provider secrets
  re-encrypted under the primary key, --dry-run), assets/sync_audio (audio
  library), collab/collab_maintenance (done in M11), core/sync_models
  (pinned ONNX install: --model/--all/--list/--dry-run/--force/--from-dir),
  designs/purge_trash (--days, purge_expired_trash), elements/{seed_elements,
  sync_element_packs, import_elements}, exports/purge_exports,
  fonts/sync_fonts (Fontsource via jsDelivr or --from-dir),
  gallery/{seed_gallery, seed_templates, import_template},
  presets/{seed_size_presets, import_size_presets}.
- **Content**: backend/data/*.json (size_presets, ml_models, popular_fonts,
  unicode_ranges, audio_library, element_packs, element_pack_files/,
  graphics/*.py generators, templates/ with manifest+documents+thumbs+photos)
  — the generated artwork/templates are the seed commands' input, so their
  BYTES are part of the port's denominator (compile-time embedded copies).
- **Entrypoint/healthcheck**: docker-entrypoint.sh (wait for DB, migrate,
  gates on `manage.py help <cmd>`, DOORY_SYNC_* / DOORY_SEED_TEMPLATES /
  DOORY_SIZE_PRESETS_FILE bootstraps, never fatal, then exec) and
  healthcheck.py (HEALTH_PORT/HEALTH_PATH, first non-* ALLOWED_HOSTS host,
  4s timeout, exit 0 only on 200).
- **cli-driver** (PLAN §4): run one command on both sides against the pair's
  own workdir DBs and compare exit code + stdout + stderr + a per-table
  database digest (rows normalized for per-side uuids, timestamps and secret
  columns, plus PRAGMA foreign_key_check) + the media/export trees (relative
  paths with uuids normalized, content hashes).
- **Deliberate exclusions** (recorded in D13): Django's global option block
  in usage text (no --settings/--pythonpath/--traceback in a single binary),
  its argparse line wrapping, the program name, and the framework tables
  (django_migrations vs schema_migrations, django_content_type,
  auth_permission/groups, django_session's signed payload — D5).

M13 progress log (this session — cli foundation + deterministic commands):

- ✅ cli-driver + scenario 22-ops-commands (33 steps) GREEN: purge_exports,
  collab_maintenance, reencrypt_ai_secrets (with/without --dry-run),
  seed_gallery (twice, --update), seed_size_presets (twice, --update),
  purge_trash (empty, --days 0 with a full cascade, --days -1, non-integer),
  import_size_presets (extend/repeat/dry-run/replace, missing file, bad mode),
  sync_models (--list/--dry-run/unknown id/--from-dir with a bad file and a
  directory without it), unknown command. Each step compares stdout, stderr,
  exit code, every table's rows and both media trees.
- ✅ `doory <command>` CLI (`doory/cli.go`, `cli_args.go`): argparse-shaped
  options/messages (unrecognized arguments, invalid int, invalid choice,
  required positional), CommandError → "CommandError: …" exit 1, usage →
  exit 2, unknown command text.
- ✅ Ports: purge_trash (+ full cascade delete: comments/links/presence/
  Y state/versions/thumbnails — the old port deleted only versions and
  answered 500 as soon as a link or comment existed), purge_exports,
  reencrypt_ai_secrets, seed_gallery, seed_size_presets,
  import_size_presets (full validators + ordered-node JSON so stored meta
  key order matches), collab_maintenance, sync_models (manifest
  validation shape, select/list, directory + HTTPS sources, size/SHA-256
  verify, .part + rename, NOTICE.txt, stale cleanup, 20% milestones).
- ✅ Entrypoint/healthcheck: `doory entrypoint [--prepare]` (DB wait, migrate,
  env-gated bootstraps guarded by command availability, never fatal) and
  `doory healthcheck` (probe with the ALLOWED_HOSTS host).
- ✅ Loop-caught (all real, each now covered):
  1. `import_size_presets --mode replace` bound updated_at to the first slug
     (argument order vs placeholder order) → deactivated two named rows.
  2. Stored JSON: Django's JSONField writes json.dumps text (", "/": "
     separators, ensure_ascii) while the port stored compact text — visible in
     the document/meta columns (and to any Django process reading the DB).
     doory/pyjson.go now renders Python storage text; API bytes unchanged.
  3. JSON columns were written as BLOBs ([]byte Exec args) instead of TEXT;
     SQLite typeof() and any JSON function treat them differently.
  4. Django's login signal stores last_login; the Go session flow did not.
  5. `import_size_presets` merges multiple files and rolls a --dry-run back.
- ✅ Second M13 stretch (content commands): gallery seed_templates +
  import_template (embedded `data/templates` tree, manifest/document
  validation, category resolve, photos copied byte-identically, thumbnails
  bounded and stored as WebP with Django's storage-suffix behaviour — the
  Pillow WebP re-encode is the M7 encoder gap, so the cli-driver compares
  files directly inside `templates/` by decoded dimensions via the new
  `media_structural` scenario option), fonts sync_fonts (curated catalogue +
  unicode ranges embedded, --from-dir import with per-family counts and
  rejections, --popular/--all ids, licence/size/magic checks, Fontsource HTTP
  source), audio sync_audio (embedded 6.9 MB library, SHA-256-verified
  install, index.json catalogue byte-compared, prune, --list/--dry-run/--
  force/--pack/--from-dir). Scenario 22 is now 58 steps green.
- ✅ Loop-caught in this stretch: Django's storage appends `_<7 random>` when a
  thumbnail name is taken (the driver normalizes the suffix and the port
  mimics get_available_name); `marshal`-of-a-map lost the document key order
  (template documents now keep the file's order via the ordered JSON node);
  audio `lufs` must render as a Python float (`-16.0`); media-tree diffs are
  bounded in the report (a 112-file tree used to flood it).
- ✅ Third M13 stretch (elements + audio packs, M13 complete): the elements
  family's write pipeline is ported end to end — manifest (all 25 packs
  validated, licence allow-list, fingerprints), sources (npm tarball with
  integrity check, pinned url files with SHA-256, directory source, glob
  matcher with Python `re.match` anchoring, archive path rules), Iconify JSON
  reader, normaliser (probe 0017: 48/48 byte-equal), tag parsers, batched
  import (per-key outcomes), packsync (fingerprints, stale/logo handling) and
  the three commands. `seed_elements` reproduces the oracle exactly: 572
  elements across 12 packs, every column (name/kind/style/tags/search
  columns/content_hash/width/height/colours/sort_order) and the stored SVG
  bytes equal. D14 records the generated-artwork substitution (pinned
  generator output embedded instead of running the Python generators).
  `sync_audio` gained its third-party pack path (pinned zip verify, member
  size/SHA-256/container checks, redirect allow-list) with offline unit
  evidence; the pinned manifest lists no packs. Scenario 22-ops-commands is now
  72 steps green; new unit evidence: elements_manifest_test (25 fingerprints
  equal), elements_sources_test (globs/archives/reader), audio pack archive
  tests.
- ✅ Loop-caught in this stretch: the SVG serialiser emitted `<rect></rect>`
  where ElementTree writes `<rect />` (element content_hash and stored bytes
  depended on it); `svgInfoFromRoot` overrode a viewBox-derived size with the
  300x150 fallback; the pack fingerprint payload needed Python's
  `json.dumps(sort_keys=True)` text (my `sortStrings` helper returns a copy);
  `bulkImport` never marked new rows as new (silent no-op inserts); generated
  packs seed with `sort_order = index * 10` (import_directory) while packsync
  leaves 0; the digest needed embedded-uuid normalisation for storage paths.
- ⬜ M13 remaining (named, all content-gated rather than behavioral): (a) the
  npm/url element packs over the NETWORK (implemented; directory/unit tested —
  CI has no network), brand-logo sync against a real pack, --from-dir with a
  real npm tarball; (b) template thumbnails stay dimension-compared until a Go
  WebP encoder lands (D15); (c) the deep audio metadata parse (duration/
  channels) belongs to the assets upload pipeline, still pending there;
  (d) `sync_audio --pack` has no manifest entry to exercise (unit-tested).

M14 scoping (single-binary release — sources read, semantics derived):

- **Frontend**: `.upstream/current/frontend` builds with `tsc -b && vite build`
  into `dist/` (3 entries: `index.html`, `presenter-view.html`,
  `render.html`; hashed `/assets/`, `/icons/`, `manifest.webmanifest`,
  `robots.txt`, `og-image.png`; 20 MB, 14 MB of it the onnxruntime wasm).
  Served by nginx upstream with the rules in `deploy/nginx.conf` +
  `frontend/nginx/security-headers.conf`: `/assets/` immutable + 404 (never the
  shell), `/index.html` and SPA routes no-cache, other root files 86400 s, the
  security header set on every page response, `/media/` immutable + nosniff +
  sandbox CSP (audio `index.json` no-cache), `/static/` proxied to Django
  (whitenoise compressed-manifest), `/api|/admin|/sitemap.xml` to the API,
  `/ws/` upgraded, `/mcp` streamed.
- **Dev proxy parity**: `frontend/vite.config.ts` proxies `/api`, `/media`,
  `/admin`, `/static` and `/ws` (ws upgrade) to `VITE_API_PROXY`
  (default http://localhost:8000) — the Go binary serves the SPA itself, so a
  dev mode must proxy those prefixes to a Vite dev server instead.
- **D4 no-config boot**: `DATABASE_URL` unset → embedded SQLite file in a data
  dir (WAL, busy_timeout, foreign_keys per connection, already implemented);
  M14 adds the data-dir default (`DOORY_DATA_DIR`, media/exports/db under it)
  so `doory serve` needs no environment at all. PG from the same binary is
  dialect-matrix tested (schema parity already green on both).
- **D7 static**: whitenoise `CompressedManifestStaticFilesStorage` semantics —
  accept-encoding negotiation (gzip/brotli), `Vary: Accept-Encoding`, ETag /
  If-None-Match, immutable caching for manifest-hashed names — over the
  embedded asset tree (the port has no admin assets; D2 keeps that surface
  minimal).
- **D2 admin**: spike S3 inventory lives in the upstream admin modules
  (registered ModelAdmins, actions, custom views). Interim scope: the
  operations the self-host docs require — createsuperuser, content
  (templates/elements/presets) management, AI provider confirm actions,
  user/asset moderation.
- **Coverage report**: `pnpm coverage` is a stub; M14 replaces it with a
  generator that reads scenarios + PORT_MAP dispositions and writes the table
  embedded in AGENTS.md (weak-only routes must not count as verified).
- **No containers (D18, owner decision 2026-10-25)**: the deployable is the
  binary. Upstream ships api + realtime + mcp + nginx web as four images
  behind compose; the port builds one executable with the frontend embedded,
  reads its configuration from the environment and keeps its state in one
  data directory. Deployment docs describe a systemd unit (`deploy/
  doory.service`) instead of compose; `doory entrypoint`/`healthcheck` keep
  the upstream supervisor contract. Test infrastructure may still start a
  Postgres container to run the dialect lane (that is test tooling, not a
  deployment target).

M14 build order: (1) webapp embedding + static/media/SPA serving + dev proxy +
tests derived from the nginx config (D7 update); (2) data-dir default +
no-config smoke + PG dialect run; (3) deployment docs for the binary (systemd
unit; no container image, D18); (4) coverage generator; (5) D2 admin inventory
+ minimal admin, or an explicit owner decision to defer (recorded).

M14 progress log (this session — release candidate slices 1-4):

- ✅ Frontend embedded (D16): `pnpm frontend:build` exports the pinned mirror
  frontend with `git archive` into a scratch dir, runs `npm ci && npm run build`
  (`tsc -b && vite build`) there and copies only `dist/` into `doory/webapp/`
  (20 MB, 14 MB of it the onnxruntime wasm). The Go server serves it with the
  nginx rules: `/assets/` immutable and 404 (never the shell), the shell and
  SPA routes no-cache, root files 86 400 s, the app CSP/header set on every
  page response, gzip by nginx's type list and 512-byte threshold, ETag/304.
  `/media/` is served with the nginx media headers (immutable, nosniff,
  sandbox CSP, svg attachment, audio index no-cache) and a missing media file
  falls through to the app stack (the oracle harness's behaviour, so scenario
  11 stays green). `/static/` keeps whitenoise semantics over an embedded
  static tree (empty: no Django admin assets). Golden tests encode every rule
  (doory/webapp_test.go).
- ✅ Dev proxy (vite parity): `DOORY_DEV_PROXY` makes the binary proxy every
  non-app path to the Vite dev server (verified: API local, pages proxied,
  502 when Vite is down).
- ✅ No-config boot (D4): `DATABASE_URL` unset → `sqlite:///<DOORY_DATA_DIR>/
  doory.sqlite3` with the directory created; media/exports default under the
  same data dir. Verified with `env -i` (no environment at all): migrate,
  serve, SPA and API answer.
- ✅ Postgres from the same binary (D4 + D17): the driver seam
  (`doory/pgq.go`) rewrites `?` placeholders to `$N` (string literals,
  identifiers, comments and dollar quotes skipped) and renders timestamps as
  Django's storage text; boolean writes take Go bools and `TRUE`/`FALSE`
  literals, uuid columns are read as nullable instead of `COALESCE(uuid, '')`,
  and the assets list's Django-style `-created_at` default became
  `created_at DESC`. New `test/pgsmoke` walks the API on Postgres (register,
  login, designs, share links, comments, folders, versions, catalogs,
  trash/restore/purge, duplicate email, revision conflict) — green.
- ✅ Deployment without containers (D18): the owner rejected the container
  path — the `Dockerfile`/`docker-compose.yml` drafted earlier were removed,
  and the release story is now "install the binary, run it under systemd".
  `deploy/doory.service` is the unit (EnvironmentFile, WorkingDirectory =
  data dir, `doory entrypoint :8080`, hardening notes for the Chromium child);
  a bare `doory` prints the command overview and exits 0 (manage.py's
  behaviour), `pnpm build` produces `dist/doory` (the `doory/` package directory
  would collide with `-o doory`);
  `.env.example` documents the same knobs for that file. The binary needs no
  configuration at all: SQLite + media + exports under one data directory.
- ✅ Coverage report (`pnpm coverage`, automation/ci/coveragereport): families
  with PORT_MAP rows reviewed vs. scenario-covered, test-file dispositions,
  and the review-only rows named — written between markers in AGENTS.md.
  Current totals: 106/325 rows scenario-covered, 63 review-only (mcp and ops
  dominate the weak list, matching the open M12/M13 items).
- ✅ `.env.example` for the single binary (same variable names as upstream,
  plus DOORY_DATA_DIR / DOORY_DEV_PROXY) and a README "Running the single
  binary" + "Deploying it (no containers)" section (no-config, Postgres,
  systemd, the path map).
- ⬜ M14 remaining: (a) D2 admin — the S3 inventory is 19 registered models
  (User, Provider/UserProvider/AICall, ApiToken, Asset, BrandKit,
  Design/DesignVersion/Folder, ElementPack/Element, FontFamily,
  TemplateCategory/Template, SizePresetGroup/SizePreset, ShareLink/Comment),
  ~20 bulk actions (publish/unpublish, trash/restore, verify/activate users,
  revoke tokens, feature templates), two confirm pages (AI provider
  load-models/test) and the multi-file element upload; the Go port answers
  `/admin/` with 404 today. Implementing a minimal Go admin is a sub-project
  and D2's remove-when asks the OWNER for the go/no-go (admin unsupported is
  an explicit decision, not a silent default) — awaiting that call.
  (b) The paired dialect matrix (each scenario additionally on Postgres) is
  not wired into the runner; Postgres is covered by schema-parity + pg-smoke.
  (c) S3 media storage stays pending with the assets family (D7 note).

## 11. 0.2.0 catch-up log (pin 2fb4d02)

The pin moved from the v0.1.0-era commit to **doory@v0.2.0** (2fb4d02,
2026-10-23). What the catch-up required, and where the port stands.

Oracle/env:
- ✅ `pnpm upstream-mirror` refreshed the mirror (55 commits; 92 backend files,
  +31 493/−610; frontend 268 added/128 modified — SSO icons and the video
  tool in the built bundle).
- ✅ Oracle venv updated from `requirements-dev.txt` (Django 6.1.2, Pillow 13,
  pycrdt 0.14.10, PyJWT 2.15.1). `pnpm oracle:setup` refuses an existing venv;
  `uv pip install --python .oracle/venv/bin/python -r …` is the working path.
- ✅ **Parity runs the oracle on SQLite** (`doory.settings.prod` with
  `DATABASE_URL=sqlite:///…`, the same as the Go side) — no Postgres server is
  needed for the scenario suite. Postgres stays an *opt-in* dialect lane
  (`pnpm schema-parity` PG branch, `pnpm pg-smoke`), used only when the PG
  migration/DDL changes.
- ✅ SQLite + Postgres oracle databases recreated at the new pin and the three
  generated migration artifacts regenerated, never hand-edited:
  `pnpm schemagen` (146 statements, `accounts_oidcidentity` first),
  `seedgen -tables presets_sizepresetgroup,presets_sizepreset` (1 749 rows;
  `presets.0003_seed_more_presets` + `0004_size_choices`), and
  `0001_initial.postgres.sql` from a `pg_dump --schema-only` of the migrated
  parity database (now normalized the same way as before: SET/COMMENT/CREATE/
  ALTER statements, blank-line separated, pg_dump banners dropped).
  `pnpm schema-parity`: **37 tables, oracle == Go on SQLite AND Postgres**.

Ported in this catch-up (parity restored, `pnpm parity` 21/21 green):
- ✅ User envelope gained `oidc_linked` (the OIDC identity row is an `EXISTS`
  subquery on both user lookups; `accounts/serializers.py`).
- ✅ `/api/v1/config/`: `oidc_button_label`, `oidc_button_icon`,
  `features.oidc_login` and `features.image_optimize`
  (`{formats, optimizers, default}`) — `doory/checks.go` ports
  `oidc.enabled/button_label/button_icon/configuration_problems` and the
  optimizer registry's `listed()/available()/default_optimizer()`.
- ✅ **Django system checks are back in the CLI**: every management command
  prints the `System check identified some issues:` banner before its body
  (`doory/checks.go`, buffered stderr in `RunCLI` so argparse errors stay
  banner-free, as in Django). Ported: `exports.W001`–`W005`
  (`IMAGE_OPTIMIZERS`, cjpeg/oxipng detection, provider keys, custom URL) and
  `accounts.W001` (OIDC settings problems). This is why the ops scenario's
  commands match again (the oracle's host has no mozjpeg/oxipng, so W002 fires
  on both sides).
- ✅ `assets` 0.2.0 shape: the five video columns (`poster`, `filmstrip`,
  `audio`, `status`, `error`) in row/insert/scan, the serializer's
  `status/error/poster_url/audio_url/video`, `url` empty while a video has no
  file, `thumb_url` only for picture kinds, `duration_ms` read from
  `meta[kind]`, `kind=video` accepted, and the default list filter is now the
  picture kinds (`kind IN ('image','svg')`) instead of "not audio".
- ✅ Embedded frontend rebuilt at the new pin (`pnpm frontend:build`, 21 MB);
  `backend/data/size_presets.json` refreshed (the only seed fixture that
  changed).

Pending (0.2.0 features — unported routes answer 404, PORT_MAP rows are 🟡/⬜):
- **OIDC sign-in**: `accounts/oidc.py` flows (discovery, PKCE, JWKS/JWT
  verification, state/session storage), `oidc_identities` writes, the three
  routes `/auth/oidc/{start,callback,connection}/`, `OidcConnectSerializer`,
  `OIDC_TRUST_EMAIL`. Config surfacing only is ported.
- **Video**: `assets/video.py`, `video_jobs.py`, `video_providers.py`,
  `video_stock.py`, the `ingest_video_upload` path (202 processing, poster/
  filmstrip/audio derivatives, ffmpeg/ffprobe availability), the three stock
  video routes (`/stock/videos/{,import/,providers/}`), `sweep_video_jobs`,
  and video export in `exports/`.
- **Image optimizers**: the shrink pipeline (`base.py`/`builtin.py`/
  `custom.py`/`http.py`/`kraken.py`/`tinypng.py`), `POST /exports/optimize/`,
  `optimize` on image exports and the MCP export tool; only detection +
  `/config/` + checks are ported.
- **Size choices**: `presets.SizeChoices`, `GET/PATCH /size-presets/mine/`
  (pinned/recent/collapsed) and the new `presets.0004` table writes.
- **Exports delta**: multi-page `pages` ZIP exports, `EXPORT_IMAGE_TIMEOUT/
  MAX_PAGES`, video export service, `filenames.py` changes, the MCP export
  tool's new options; `accounts/services.py` OIDC sign-in/connect paths; the
  new settings knobs the port doesn't read yet (VIDEO_*, STOCK_VIDEO_*,
  EXPORT_VIDEO_*, OIDC_*, `REST_FRAMEWORK` throttle scopes).
