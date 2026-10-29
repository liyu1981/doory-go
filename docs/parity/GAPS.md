# GAPS — open work between the port and upstream

Single tracker for **what is not done yet**. Three other documents keep the
detail, and this file is the index that must stay in sync with them:

| document | what it answers |
|---|---|
| `docs/parity/GAPS.md` (this file) | Which behaviours are still missing/partial, why, and what closes each one |
| `docs/parity/PORT_MAP.md` | Per-upstream-file status (ported / partial / not started) — the file-level view |
| `docs/parity/DIVERGENCES.md` | Deliberate differences from upstream (D1–D22), each with a remove-when |
| `AGENTS.md` coverage table | Which PORT_MAP rows a parity scenario actually covers (proof, not status) |

Rules:

- **One row per gap**, stable id (`G<n>`), never reused.
- Every *registered but not ported* route or branch in the Go code
  (`route registered but not ported: …`) MUST have a row here: the
  `pnpm gaps-check` gate fails when a route from the pinned routes ledger is
  neither registered by the port nor listed below.
- When a gap closes, move the row to "Closed" with the commit, and update the
  corresponding PORT_MAP row in the same change.
- `Last verified` names the pin the route-level table was checked against.

Last verified: pin `2fb4d0297ec4c6e417b5225215502f8217c2a4dc` (doory@v0.2.0).

## A. Routes upstream has and the port does not register

| id | route | upstream behaviour | why it is open / what closes it |
|---|---|---|---|
| G1 | `POST /api/v1/designs/{id}/fill/` | `filling.fill_design`: fill the template's tagged fields (`{key: value}`), as a copy by default | Needs the document-mutation engine: `doc/ops.py` (`update_object`, `replace_image`), `edits.apply_edit` (locking, revision, auto-version). Same engine as G2; the ops subset + validation is the work item. |
| G2 | `POST /api/v1/designs/{id}/bulk-fill/` | `filling.bulk_fill`: one new design per row, each row reporting its design or its error | Same engine as G1 plus `BulkFillSerializer` (rows, `row_numbers`, `name_pattern`, `folder`, `export`), per-row rate charges and the image exporter. **The rebuilt frontend calls this** (fill-from-data dialog). |
| G4 | `GET /api/v1/schema/`, `GET /api/v1/docs/` | drf-spectacular OpenAPI document + Swagger UI | D8 promises *structural* schema parity but nothing is served today. Needs a generated OpenAPI document (from the routes/interfaces ledger or hand-kept) and the docs page. Lowest priority of the route gaps; no frontend client depends on it. |

Documented additions (not gaps): `/mcp`, `/mcp/health`, `/sitemap.xml`,
`/ws/designs/{uuid}/` come from D1/D7/D8 (single process, embedded SPA,
sitemap view).

## B. Registered routes with unported branches (loud `notPorted` errors)

These answer the ported validation/lookups, then a clear 500 saying exactly
which upstream function is missing — never a silent wrong answer.

| id | route | branch | what closes it |
|---|---|---|---|
| G5 | `POST /api/v1/templates/{slug}/use/` | `gallery.create_from_template` (design from a seeded template) | Copy the template document into a new design with `source_template` provenance, category/preset handling and the version row. `gallery.services`/`models.apply_document`. The frontend's "use this template" path. |
| G7 | `POST /api/v1/stock/videos/import/` (and the live branch of `GET /stock/videos/`) | `video_stock.import_video`: resolve via the provider API, download with host checks, probe, convert, insert the asset | Video family (G15). The no-config payloads and serializer bounds are ported. |
| G8 | `POST /api/v1/exports/optimize/` with an off-site optimizer | `optimizers/{tinypng,kraken,custom}.shrink` (HTTP contract, offsite charging) | Needs the optimizer HTTP transport (`optimizers/http.py`) + rate charge. The built-in optimizer is implemented in process (D22), so `optimize: true` works today. |
| G9 | `POST /api/v1/ai/providers/{id}/test/`, `/load-models/` | `services.test_provider` (sample picture call, preview, AICall log), `catalog.list_remote_models` | Same provider transport as the AI family's run path, plus the sample image and the remote catalogue mapping. Row lookups are ported. |
| G10 | `POST /api/v1/assets/` with an SVG upload | `assets._prepare_svg` (`core/svg.py`: sanitise, cap, rasterise preview) | SVG pipeline. The port answers the loud notPorted error today. |
| G11 | `POST /api/v1/assets/` with an audio upload | `assets._prepare_audio` (`assets/audio.py`: container/codec probe, duration, waveform) | Audio pipeline. Note the audio *library* install (`sync_audio`) is ported; only uploads are not. |

## C. Live-network paths that only work in their no-config shape

| id | area | today | what closes it |
|---|---|---|---|
| G12 | Stock photos: Unsplash / Pexels / Pixabay search + import | Validation + `_not_configured` payloads match the oracle; no provider call | Provider HTTP clients with the fake-provider fixtures (`assets/providers.py`, `remote.py`, counting/pausing/redaction). |
| G13 | Freesound: search, info, import, preview | Same (no-config shapes + messages) | `assets/freesound.py` client + preview streaming. |
| G14 | Remote image fetch (`URL_IMAGE_UPLOADS`, bulk-fill URLs, stock import downloads) | Not ported | `assets/remote.py` SSRF-pinned fetch; also needed by G1/G2 (URL values). |
| G15 | Video family | Availability + limits + no-config payloads only (D20-era) | `assets/video.py` (ffprobe/ffmpeg probe + conversion), `video_jobs.py` (queue, 202 processing, poster/filmstrip/audio), `ingest_video_upload`, `sweep_video_jobs`, live stock-video providers, and video export in `exports/`. |
| G16 | AI provider response readers / style encoders (`ai/styles/{base,multipart,json_b64,mask,custom}.py`, `results.py`) | The port builds the doory multipart request and maps errors; response decoding and style variants are partial | Port the styles + result readers; needs the fake-provider harness to prove wire behaviour. |

## D. Deliberate differences that are still open work

| id | divergence | status |
|---|---|---|
| G17 | **D2 — Django admin** | Owner approved the Go admin on 2026-10-26: full route-shape coverage, server-rendered (no-Unfold) HTML, staff read + superuser writes until per-model permissions land. Work is split into G27–G31. |
| G18 | **D20 — WebP/JPEG bytes** | In-process encoders (lossless WebP, stdlib JPEG) instead of Pillow/mozjpeg: valid files, different bytes. Image responses are compared structurally (D15/D20). Closes if a libwebp/libjpeg build is adopted. |
| G19 | **D21 — AVIF** | Not made at all (no pure-Go encoder); `/config/` advertises `png,jpg,webp` and the endpoint rejects `avif`. Closes with an AVIF encoder. |
| G20 | **D7 — S3 media storage** | Local media only; the `storages` seam is not ported. |
| G21 | **D4 — Postgres dialect lane** | Schema digest + `pg-smoke` run on Postgres; the runner does **not** replay each scenario on Postgres. Wiring the dialect matrix into `test/parity` closes it. |
| G22 | **D6/D11-family leftovers** | Scoped DRF rate throttles are not enforced end-to-end (the account lockout is); `REST_FRAMEWORK` rates still need the kernel throttle slice. |
| G23 | Image normalisation edges | EXIF orientation/normalisation and AVIF decode paths differ from Pillow's; loud or structural today, not byte-equal. |

## E. Verification and bookkeeping deficits

| id | item | state |
|---|---|---|
| G24 | Scenario coverage | 109 of 338 PORT_MAP rows are covered by ≥1 scenario (85 reviewed-but-uncovered). Weakest: `mcp` 0/21, `ops` 10/34, `elements` 3/24 — see the AGENTS.md table (`pnpm coverage`). |
| G25 | `ledger/test-mapping` dispositions | Pending/partial entries remain for mcp, ops, elements, gallery, design-doc and the 0.2.0 files (video, optimizers, OIDC, my-sizes). Each needs evidence or a named missing case. |
| G26 | PORT_MAP staleness | Some cells lag the code (e.g. `exports/optimizers/builtin.py` is implemented in process per D22 but still says "shrink pending"). Rule: whoever touches an upstream file updates its row in the same commit. |
| G32 | SSO paired-scenario coverage | OIDC and Google sign-in now have Go integration tests (`doory/oidc_sso_test.go`), but no paired oracle scenario yet. Wiring one needs a fake provider (discovery/JWKS/token/userinfo) and a trusted TLS endpoint the Django oracle can reach; until then the SSO rows are review/test-covered, not scenario-covered. |
| G33 | WASM codec output parity | The built-in optimizer now runs official cjpeg 4.1.5 / oxipng 10.2.1 WASI CLIs as child processes, but no fixture compares their optimized bytes with the native oracle tools. Add native-vs-WASI output fixtures (and decide whether `WITH_SIMD=0` is acceptable) before claiming byte parity. |

## F. Admin program (D2 — owner-approved 2026-10-26)

| id | slice | what closes it |
|---|---|---|
| G27 | Admin kernel and CLI | Staff gate (`is_active && is_staff`), session/CSRF login and logout, index/app index/search/object redirect/autocomplete/jsi18n, own password change, `never_cache`/`Vary` semantics, `django_admin_log` + history plumbing, `createsuperuser`/`changepassword`/`make-admin`, and the `django_content_type`/`auth_permission` seed. Evidence: probe `0018-admin` + scenario `22-admin` (kernel steps). |
| G28 | Admin model engine and moderation | Descriptor-driven changelist/add/change/delete/history/actions engine; User + auth.Group, ApiToken, Asset, BrandKit, Design/DesignVersion/Folder, ShareLink, Comment; upstream action semantics (mark-verified/activate/deactivate never self, revoke, trash/restore, link toggle, comment delete) and the delete-file cleanup those paths need. |
| G29 | Admin content authoring | TemplateCategory/Template (JSON document + thumbnail + validate/duplicate/feature), ElementPack/Element (SVG + bulk upload + publish), FontFamily/FontVariant inline upload, SizePresetGroup/SizePreset (tags/aliases/meta + activate clash check); post_delete cleanup for template thumbnails, element SVGs, pack thumbnails and font files. |
| G30 | Admin AI pages | Operator Provider CRUD (secret keep/clear/host-change drop, tasks/style validation, allowed users, provider-model inline), env-provider panel, UserProvider/AICall read-only; `load-models`/`test` confirm + result pages. The transport-dependent half waits on G9; until then those two actions answer a tracked not-ported page. |
| G31 | Admin verification and bookkeeping | Port the 73 upstream `test_admin.py` behaviours to Go integration tests; scenario `22-admin` covers all 11 `admin.py` rows + 4 admin templates with shared wire/state assertions; admin CRUD smoke in `test/pgsmoke`; PORT_MAP/test-mapping/coverage updates; self-hosting docs; D2 evidence (markup stays a recorded difference). |

## Closed (with the commit that closed it)

| id | gap | closed by |
|---|---|---|
| — | 0.2.0 routes the rebuilt frontend called: `size-presets/mine`, `auth/oidc/{start,callback,connection}` (no-provider surface), `stock/videos{,/providers,/import}`, `designs/{id}/{fields,thumbnail}`, `designs/bulk`, `exports/optimize`, `auth/password/reset{,/confirm}`, `auth/email/{verify,resend}` | `2892176`, `dc77682` |
| — | Starter content + fonts embedded and installed by `entrypoint` (D19); in-process built-in optimizer (D22) | `dc77682` |
| G3 | `POST /api/v1/auth/google/` — Google ID-token verification and link/create | this change |
| G6 | OIDC provider flows (start/callback/connect, discovery, PKCE, JWKS, ID-token verification) | this change |
| — | Clone-build without parity tooling; `:8111` default; `scripts/install-systemd.sh` (`~/.doory`) | `5a22cd7` |
