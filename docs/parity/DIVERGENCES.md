# DIVERGENCES

Deliberate, reviewed differences from the pinned upstream backend. Every entry
carries a global id `D<N>`, an owner-approved scrutiny line, and a
**remove-when** condition. Call sites in Go code mark their difference with
`// doory divergence (D<N>): <reason>`; `pnpm divergence-consistency` matches
records to markers and rejects unknown ids in either direction.

Rules (PiG §6):
- Only user-visible / interop-relevant differences get an id. Ordinary
  TypeScript/Python→Go mechanics do not belong here.
- No silent normalization: a difference is fixed, or numbered here with a
  remove-when, or it fails CI.
- Ids are global; duplicates fail even across files.

---

## D1 — three deploy units → one process

- SCRUTINIZED: approved
- status: active
- summary: Upstream deploys WSGI (API+admin), ASGI (WebSocket realtime) and MCP
  as three processes behind nginx. The Go port defaults to ONE process serving
  HTTP + WS + MCP on a single listener.
- remove-when: upstream changes its topology. `--ws-addr` / `--mcp-addr` flags
  restore the three-listener nginx topology and stay scenario-tested.
- evidence: PLAN.md §6 (D1), family ops. The collab channel layer follows from
  this: upstream's group_send/notify_design reaches rooms in other processes
  through Redis, while the single-process port calls its in-process
  RoomRegistry directly (doory/collab_room.go notify, doory/collab_signals.go).
  The observable behaviour is identical — the HTTP-written title reaches a
  joined socket as `{"type":"title",...}` (scenario 17-collab) and access
  changes re-check open rooms; only the transport differs.

## D2 — Django admin not ported as Django HTML

- SCRUTINIZED: approved
- status: active
- summary: django-unfold admin is not reproduced as Django HTML. Interim: a
  minimal Go admin covers the operations the self-host docs require
  (createsuperuser, template/content management, provider confirm-actions,
  moderation). Spike S3 inventories the admin surface first.
- remove-when: admin surface fully inventoried and scenario-covered, or the
  owner decides admin is unsupported (which fails self-hosting parity and
  needs a fresh approval here).
- evidence: PLAN.md §6 (D2), spike S3.

## D3 — CRDT engine

- SCRUTINIZED: approved
- status: active
- summary: pycrdt (yrs native) in Python → a Go yrs binding, yrs-WASM, or a
  minimal Y-protocol implementation (spike S0 decides). Observable contract:
  client protocol frames + final document JSON + stored-snapshot round-trip.
  Update blob BYTES are never compared (CRDTs converge regardless).
- remove-when: n/a (permanent engine substitution); every collab scenario
  asserts frames + document JSON on both sides.
- evidence: PLAN.md §6 (D3), spike S0, family collab. Conformance is
  fixture-driven in both directions: probes/0011-collab-crdt (snapshots, state
  vectors, 12-batch edit stream, concurrent convergence, oracle-applied Go
  witness) and probes/0015-array-index (array Insert/Delete over decoded
  multi-value batches — the two patches it forced are PATCHES.md 12/13). The
  wire document itself is compared in scenario 17-collab: the sync/step2 each
  server returns decodes to the same document on both sides.

## D4 — swappable database (SQLite default)

- SCRUTINIZED: approved
- status: active
- summary: Upstream deploys Postgres in prod and defaults to SQLite for dev.
  The port defaults to SQLite (zero-config embedded file; in-memory for tests)
  and supports Postgres via DATABASE_URL with no code change. Parity runs the
  dialect matrix: every scenario on SQLite, declared subsets additionally on
  local docker Postgres. Schema/type mapping is ledgered per dialect.
- remove-when: n/a (permanent, owner-requested). Re-verify on every upstream
  leap that upstream has not adopted a Postgres-only feature; record it here
  if it does.
- evidence: PLAN.md §4 (dialect matrix), §6 (D4), ledger/migrations-v*.json.

## D5 — Django session/token interop not guaranteed

- SCRUTINIZED: approved
- status: active
- summary: Django session rows and PBKDF2 password hashes are not guaranteed
  import-compatible at swap time. Decision after spike: either read Django's
  django_session + hashes (zero-logout upgrade) or document a one-time logout.
- remove-when: the interop scenario turns green (zero-logout path chosen and
  proven) or the one-time-logout decision is recorded here with owner
  approval.
- evidence: PLAN.md §6 (D5).

## D6 — password hashers / throttle caches

- SCRUTINIZED: approved
- status: active
- summary: Same KDF parameters as probed from the oracle; Django's cache-based
  throttling maps to a Go-native store. Observable behavior (429 shapes,
  retry-after) is scenario-proven; internal storage is not.
- remove-when: n/a (implementation substitution with proven observable
  behavior).
- evidence: PLAN.md §6 (D6), family accounts.

## D7 — static serving: whitenoise → embedded precompressed assets

- SCRUTINIZED: approved
- status: active
- summary: whitenoise compressed-manifest storage is replaced by embedded
  precompressed assets. Parity asserts the same Content-Encoding / ETag /
  cache-header behavior as probed from the oracle.
- remove-when: n/a; header-behavior scenario proves the substitution.
- evidence: PLAN.md §6 (D7), family kernel.

## D8 — OpenAPI: structural, not byte, parity for generated schema

- SCRUTINIZED: approved
- status: active
- summary: drf-spectacular output is compared structurally; byte parity of
  generator ordering is not claimed. The committed
  backend/apps/ai/contract/openapi.yaml IS compared exactly.
- remove-when: n/a; structural comparator is the strongest stable one for the
  generated schema.
- evidence: PLAN.md §6 (D8), contract driver.

## D9 — email templates

- SCRUTINIZED: approved
- status: active
- summary: Django template rendering of verify/reset emails → Go templates.
  Parity asserts extracted URLs/tokens/text structure, not HTML bytes, until a
  scenario proves more.
- remove-when: an HTML-bytes scenario turns green (renderer produces identical
  bytes), after which this entry retires.
- evidence: PLAN.md §6 (D9), family accounts.

## D10 — process identity headers excluded from comparison


- SCRUTINIZED: approved
- status: active
- summary: The `Server` (and proxy `Date`) response headers identify the
  serving stack (wsgiref/gunicorn vs Go net/http), not application behavior.
  Behind the documented nginx/caddy deployments neither value reaches a
  client. The http comparator denies them with a recorded reason; everything
  else about headers is compared exactly (names case-folded — HTTP header
  names are case-insensitive and Django/nginx mix cases).
- remove-when: n/a (stack identity is not app behavior). If upstream ever
  asserts Server in a scenario, revisit.
- evidence: probe 0001 headers, PLAN.md §4 comparator rules.


## D11 — Content-Length on bodyless (204) responses

- SCRUTINIZED: approved
- status: active
- summary: wsgiref/gunicorn emit `Content-Length: 0` on 204 responses (the
  csrf endpoint); Go's net/http deletes Content-Length from bodyless
  statuses (204/304) and there is no supported way to override it through
  ResponseWriter. The http comparator drops content-length on 204 pairs with
  this recorded reason; everything else about the response compares exactly.
- remove-when: Go changes its framing policy, or the http stack is switched
  to a raw writer that can emit the header (unlikely to be worth it).
- evidence: probe 0001 step 03 (oracle 204 carries Content-Length: 0),
  parity run 01-kernel-http step 03/08.


## D12 — non-API error pages (Django templates vs Go defaults)

- SCRUTINIZED: approved
- status: active
- summary: Django serves template-driven HTML error pages for non-/api/
  paths (404 on unknown URLs, /media/ misses outside the API); the Go port
  serves its own plain pages. The http comparator therefore skips the BODY
  of non-/api/ error responses (step flag `headers_only`) together with
  content-type/content-length (they describe that body), while status and
  every OTHER header — including MediaSecurityHeaders' nosniff + CSP and the
  whole security stack — keeps comparing byte-for-byte.
- remove-when: the single-binary release (M14) defines the SPA's real
  non-API routes (index fallback), at which point upstream/nginx parity for
  those paths becomes meaningful and this allowance can shrink to specific
  paths.
- evidence: probes/0007-assets step18 (media 404 headers), PLAN.md §4.

## D13 — single-binary CLI surface (management commands)

- SCRUTINIZED: approved
- status: active
- summary: `manage.py <command>` becomes `doory <command>`; the command names,
  options, stdout/stderr text, exit codes and side effects are the same, but
  the framework-level shell is not: there is no Django global option block
  (--settings/--pythonpath/--traceback/--no-color/--force-color/--skip-checks
  — a single binary has no settings module), no `createcachetable` (the cache
  is in-process, D6) and no `collectstatic` (assets are embedded, D7). The
  parity cli-driver normalizes exactly those: the program name, the global
  option block, argparse's line wrapping, and the framework tables
  (django_migrations vs schema_migrations, django_content_type,
  auth_permission/auth_group, django_session's signed payload — D5). Every
  command's own output, rows and files compare byte for byte.
- remove-when: n/a (structural); if a Django global option is ever needed the
  driver's normalization entry goes away with it.
- evidence: PLAN.md §5 #13 and the M13 logs, scenario 22-ops-commands,
  doory/cli.go + cli_args.go + entrypoint.go.

## D14 — code generators → pinned generated artwork (seed_elements)

- SCRUTINIZED: approved
- status: active
- summary: `seed_elements` upstream RUNS the Python artwork generators
  (`backend/data/generate_shapes.py`, `backend/data/graphics/*.py`, ~5.3k lines
  producing SVG strings) and imports what they write. The Go port imports the
  same artwork from the generators' pinned output, embedded byte-identically in
  `doory/data/elements_generated/` — the repository already treats generated
  seed content this way (the presets seed rows are a verbatim dump, PLAN §2).
  Observable result is identical: scenario 22-ops-commands seeds both sides and
  compares every `elements_element`/`elements_elementpack` column (name, kind,
  style, tags, search columns, content hash, width/height/colours) and the
  stored SVG bytes for all 572 elements, plus the eight generated graphics
  packs — all equal.
- remove-when: the generators are ported to Go (or the seed path stops being
  part of the release). The artwork bytes are what parity needs; regenerating
  them at runtime buys nothing.
- evidence: PLAN.md M13 log, scenario 22-ops-commands (seed_elements /
  sync_element_packs), probe 0017-elements-normalize, PORT_MAP `backend/data/`
  rows.

## D15 — derived images (template thumbnails) compared structurally

- SCRUTINIZED: approved
- status: active
- summary: `seed_templates`/`import_template` pass every thumbnail through
  Pillow (`process_image_upload` + `make_thumbnail`: decode, EXIF-strip, LANCZOS
  resize to 720 px, WebP quality 82). There is no pure-Go WebP ENCODER (the M7
  finding); the port validates the raster, bounds it, and stores the source
  bytes when they already are WebP. File name/URL and decoded dimensions match,
  the bytes do not. The cli-driver therefore compares files directly inside
  `templates/` by decoded dimensions (`media_structural` in scenario 22) and
  everything else in the tree by content hash.
- remove-when: a Go WebP encoder is adopted (or the thumbnails ship
  pre-generated and the command copies them), then the structural rule and this
  record go away.
- evidence: PLAN.md M13 log, scenario 22-ops-commands (seed_templates),
  doory/commands_gallery.go.

## D16 — embedded frontend: pinned build output (D7 companion)

- SCRUTINIZED: approved
- status: active
- summary: the single binary ships the built React frontend (`index.html`,
  `presenter-view.html`, `render.html`, hashed `/assets/`, icons, manifest) in
  `doory/webapp/` (go:embed). It is the pinned upstream source's Vite build
  output, regenerated with `pnpm frontend:build` (git archive of the mirror →
  temp dir → `npm ci && npm run build` → copy) — the mirror stays pristine and
  the artifact is reproducible. Serving follows upstream's nginx rules
  (deploy/nginx.conf + frontend/nginx/security-headers.conf): immutable hashed
  assets that 404 rather than fall back to the shell, no-cache shell and SPA
  routes, 86400 s for root files, the app CSP/header set on every page, and
  `/media/` with the media headers (immutable, nosniff, sandbox CSP, no-cache
  audio catalogue). `/static/` keeps whitenoise semantics (ETag/Vary/304,
  immutable manifest-hashed names) over the embedded static tree, which is
  empty in the port (no Django admin assets; D2).
- remove-when: n/a (structural: the release installs the same frontend the
  upstream image serves). The artifact is rebuilt on every upstream leap.
- evidence: PLAN.md M14 log, doory/webapp.go + webapp_test.go (nginx-rule
  golden tests), scenario 22 (media 404 falls through to the app stack),
  AGENTS.md "Testing scope" (the frontend itself is validated later).

## D17 — Postgres driver seams (placeholder + timestamp normalisation)

- SCRUTINIZED: approved
- status: active
- summary: the port's SQL is written once with `?` placeholders and
  Django/SQLite-shaped datetime text. PostgreSQL needs `$N` placeholders and
  returns `timestamp with time zone` as `time.Time`, so a database/sql driver
  wrapper (`doory/pgq.go`) rewrites placeholders outside string literals /
  quoted identifiers / comments / dollar-quoted bodies and renders timestamps
  as Django's storage text ("YYYY-MM-DD HH:MM:SS.ffffff", UTC). Boolean columns
  take Go bools and `TRUE`/`FALSE` literals (ported across the handlers), and
  uuid columns are read as nullable instead of `COALESCE(uuid, '')`. The
  observable contract is identical on both dialects; the SQL text sent to
  Postgres differs from what Django emits (its ORM parameterises the same way
  through psycopg).
- remove-when: n/a (dialect seam). Re-verify on every upstream leap that no
  Postgres-only feature is adopted (D4).
- evidence: PLAN.md M14 log, doory/pgq_test.go (rewrite cases), scenario-free
  `pnpm pg-smoke` (test/pgsmoke: API flow + error paths on Postgres),
  `pnpm schema-parity` (both dialects).

## D18 — no container image: the binary is the release artifact

- SCRUTINIZED: approved (owner decision, 2026-10-25)
- status: active
- summary: upstream deploys four services (api/gunicorn, realtime/uvicorn,
  mcp/uvicorn, web/nginx) as images behind `docker-compose.yml`. This port
  ships ONE executable that serves the API, the realtime WebSockets, MCP and
  the embedded frontend on one listener, reads its configuration from the
  environment and keeps all state in one data directory (`DOORY_DATA_DIR`:
  SQLite database, media, exports). There is deliberately **no Dockerfile and
  no compose file**: deployment is "install the binary" — `deploy/doory.service`
  is the systemd unit, `doory entrypoint` migrates and runs the optional
  content bootstraps, `doory healthcheck` is the liveness probe.
- remove-when: n/a (permanent owner decision). If the owner reverses it, the
  image returns as an additive packaging artifact — the binary stays the unit
  of parity. Test infrastructure (a local Postgres container for the dialect
  lane) is test tooling and not affected by this record.
- evidence: PLAN.md §5 #14 and §8 M14, the M14 log, README "Deploying it (no
  containers)", `deploy/doory.service`, `.env.example`.

## D19 — the binary installs its embedded content on the first start

- SCRUTINIZED: pending (owner-visible default: `entrypoint` seeds content)
- status: active
- summary: upstream gates every content bootstrap behind an env flag that
  defaults to `false` (`DOORY_SEED_TEMPLATES`, `DOORY_SYNC_ELEMENTS`,
  `DOORY_SYNC_AUDIO`), so a fresh upstream install has no templates, no
  element library and no audio library until an operator runs the commands.
  The Go binary embeds all three (pinned upstream artifacts, no download, the
  same add-only/idempotent commands: `seed_gallery` categories come from the
  gallery migration, `seed_templates`, `seed_elements` +
  `sync_element_packs`, `sync_audio`), so `doory entrypoint` runs them by
  default on every start and the editor is usable out of the box. Setting the
  env vars to `false` restores upstream's flags exactly. The font catalogue
  and the background-removal model keep upstream's `false` default: they
  download from the internet.
- remove-when: upstream (or the owner) makes the embedded content
  first-start-only in the image, or decides the flags must keep their
  upstream defaults in this port — then `prepareEnvFromOS` flips back to
  `false` and the README tells operators to run the four commands.
- evidence: `doory/entrypoint.go` (prepareEnvFromOS), `.env.example`
  (DOORY_SYNC_ELEMENTS/DOORY_SYNC_AUDIO/DOORY_SEED_TEMPLATES), PLAN.md §11.

## D20 — WebP is encoded in-process, never byte-identical to Pillow's

- SCRUTINIZED: pending (image bytes are compared structurally, never byte-wise)
- status: active
- summary: upstream makes WebP with Pillow (design thumbnails, asset
  thumbnails, `format=webp` downloads and exports). Go's standard library has
  no WebP encoder, so the port uses a pure-Go lossless encoder
  (`github.com/HugoSmits86/nativewebp`): files are valid WebP with the same
  dimensions and alpha, but their bytes differ from Pillow's. Comparators
  therefore compare WebP responses structurally (decoded pixels/dimensions,
  media-tree digests excluded for generated thumbnails), and D15's
  template-thumbnail rule stays in place for the pinned upstream files.
- remove-when: a Go build with libwebp (cgo) is adopted, making encoder
  output comparable with Pillow's — or the comparators grow a byte-level
  WebP rule that the reason for this record disappears.
- evidence: `doory/images.go` (encodeWebP), `go.mod` (nativewebp), D15,
  PLAN.md §11.

## D21 — AVIF is advertised away: no pure-Go encoder exists

- SCRUTINIZED: pending (owner-visible: `/config/` offers fewer formats)
- status: active
- summary: upstream converts and exports AVIF when Pillow is built with
  libavif (the pinned oracle is), so its `/config/`
  `features.image_optimize.formats` ends with `avif` and
  `POST /exports/optimize/` accepts `format=avif`. Go has no pure-Go AVIF
  encoder (and the port deliberately stays dependency-free of libavif/cgo), so
  the port advertises only the formats it can actually make
  (`png`, `jpg`, `webp`) and answers `invalid_export` ("AVIF is not
  supported on this server.") when asked for avif. The frontend reads
  `formats`, so it never offers AVIF. The kernel scenario compares `/config/`
  with `features.image_optimize.formats` excluded (recorded
  `ignore_json_paths`, reason in the scenario header).
- remove-when: a pure-Go (or cgo libavif) AVIF encoder lands: add it to
  `imageFormats()`, drop the rejection, remove the scenario's
  `ignore_json_paths`.
- evidence: `doory/checks.go` (imageOptimizeConfig, imageFormats),
  `doory/exports_optimize.go`, test/parity/scenarios/kernel/01-kernel-http.toml.

## D22 — the built-in optimizer runs in process (no cjpeg/oxipng binaries)

- SCRUTINIZED: pending (owner-visible: `optimize` works without external tools)
- status: active
- summary: upstream's built-in optimizer shells out to mozjpeg's `cjpeg` and
  `oxipng`; where they are missing it reports "left out" (`exports.W002`) and
  `/config/` omits `builtin`. The port implements the same contract in
  process: JPG re-encoded with the standard library at the requested quality
  (kept only when smaller; above quality 90 the converted file stands, as
  upstream does), PNG recompressed losslessly by re-deflating its scanlines at
  the highest level (`doory/optimizer.go`; the pure-Go Zopfli port writes a
  broken zlib stream, so the standard library does it — sizes differ from
  oxipng's, which is what the structural rule already covers). So `builtin` is always ready: `/config/` lists it
  (`formats: [jpg, png]`, `default: "builtin"`) and
  `POST /exports/optimize/` really optimizes where the oracle on the same host
  would answer "no optimizer". For CLI parity the management-command warning
  keeps upstream's PATH-based wording (it still says cjpeg/oxipng are missing),
  and the kernel scenario excludes the two `image_optimize` optimizers/default
  fields with a recorded `ignore_json_paths`. The off-site optimizers
  (TinyPNG/Kraken/custom) keep their HTTP contract pending and refuse loudly.
- remove-when: the port ships native cjpeg/oxipng (or upstream makes the
  built-in optimizer pure-Python), and the config/check wording can follow the
  binaries again; then drop the scenario's `ignore_json_paths`.
- evidence: `doory/optimizer.go`, `doory/checks.go` (imageOptimizeConfig),
  `doory/exports_optimize.go`, test/parity/scenarios/kernel/01-kernel-http.toml,
  test/parity/scenarios/exports/13-exports.toml.
