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
