# doory-go

Faithful Go rewrite of the [doory](https://github.com/shaongitbd/doory) backend
(Django + DRF + channels + MCP), pinned as a behavior oracle under
`.upstream/current/`, with the existing React frontend embedded into a single
binary release.

The port follows the PiG ↔ Pi parity methodology (see `PLAN.md`):
pinned oracle, probe-first paired scenarios, compiler-derived ledgers,
divergence ledger with a shrink-only ratchet, and a pnpm gate ladder (`package.json`
scripts are the build-target manager; there is no Makefile).

Headline divergences (planned, ledgered in `docs/parity/DIVERGENCES.md`):

- **Swappable database (D4):** SQLite by default (zero-config embedded, in-memory for
  tests) with optional Postgres via `DATABASE_URL` — upstream ships Postgres-only prod.
  Parity scenarios run the dialect matrix (SQLite always, Postgres from local docker).
- **Testing scope:** parity proves backend behavior only (API/WS/MCP contracts, DB
  state, side effects); the embedded frontend is smoke-tested later, case by case.

## Layout

- `PLAN.md` — the rewrite plan (methodology, families, phases, divergences)
- `.upstream/current/` — frozen mirror of upstream @ the pin in
  `internal/dooryversion/dooryversion.go` (never edit)
- `internal/dooryversion/` — the sole version pin
- `doory/` — the Go port (every file carries a `// Ports backend/...` comment)
- `doory/webapp/` — the embedded frontend build (regenerate with
  `pnpm frontend:build`; provenance in D16)
- `cmd/doory/` — the single binary (`serve`, `entrypoint`, `migrate`,
  `healthcheck`, `version` and the management commands)
- `docs/parity/` — `PORT_MAP.md` (file denominator), `DIVERGENCES.md` (D\<N\> ledger)
- `ledger/` — compiler-derived inventories (routes, interfaces, env, migrations, tests)
- `test/parity/` — families, paired scenarios, drivers, unit evidence, probes
- `automation/ci/` — drift gates and the divergence guard

## Commands

```bash
pnpm run help          # list all build targets
pnpm upstream-mirror   # refresh .upstream/current to the pinned commit
pnpm generate          # regenerate inventories/ledgers (never hand-edit)
pnpm check             # build, vet, lint, unit, drift gates, divergence guards, parity-fast
pnpm verify            # check + full parity + contracts
pnpm parity            # parallel oracle/Go scenario pairs
pnpm coverage          # regenerate the coverage report embedded in AGENTS.md
pnpm frontend:build    # rebuild doory/webapp from the pinned frontend source
pnpm pg-smoke          # PostgreSQL smoke test against the local parity container
pnpm schema-parity     # schema digest, SQLite + Postgres
```

## Running the single binary (D1/D4/D7)

One process serves the API, the realtime WebSockets, MCP and the embedded
frontend on one listener. With no configuration at all it keeps everything it
needs under `./data` (SQLite database, media, exports):

```bash
go build -o doory ./cmd/doory
./doory migrate          # or let `doory entrypoint` do it
DJANGO_SECRET_KEY=$(openssl rand -base64 48) SITE_URL=http://localhost:8080 \
  ./doory serve :8080
```

- `DATABASE_URL=postgres://user:pass@host/db` switches to PostgreSQL (same
  binary, same code: the driver layer rewrites the port's `?` placeholders and
  normalises timestamps, D4).
- `DOORY_DATA_DIR` moves the data directory (`data/doory.sqlite3`,
  `data/media`, `data/exports`).
- `./doory entrypoint` is the container entrypoint: wait for the database,
  migrate, run the `DOORY_SYNC_*` / `DOORY_SEED_TEMPLATES` bootstraps when
  asked (never fatal), then serve. `./doory healthcheck` is the container
  probe (`HEALTH_PORT`, `HEALTH_PATH`).
- `DOORY_DEV_PROXY=http://127.0.0.1:5173 ./doory serve` runs the API while
  every page request is proxied to the Vite dev server (the mirror image of
  `frontend/vite.config.ts`'s `server.proxy`).

### Docker

```bash
cp .env.example .env     # set DJANGO_SECRET_KEY and SITE_URL
docker compose up -d --build
```

`Dockerfile` builds the binary (frontend included) and `docker-compose.yml`
replaces upstream's api + realtime + mcp + web services with that one
container. The data volume holds the SQLite database and media; the optional
`postgres` profile runs PostgreSQL when `DATABASE_URL` points at it. Server-side
PDF export needs Chromium: build with `--build-arg INSTALL_CHROMIUM=true` (the
default in compose) and `SERVER_PDF_ENABLED=true`.

Paths the single listener owns (the nginx rules upstream serves, D7):
`/api/`, `/admin/`, `/ws/`, `/mcp`, `/sitemap.xml` and `/static/` go to the
app; `/media/` is served with the media headers; `/assets/` is immutable;
everything else is the SPA shell. See `doory/webapp.go` for the exact header
set (`frontend/nginx/security-headers.conf`).
