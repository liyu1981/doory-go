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
  Parity scenarios run the dialect matrix (SQLite always; Postgres from a local
  test instance — test tooling only, the release itself is the binary, D18).
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
pnpm build             # build the single binary (frontend embedded) into dist/doory
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
pnpm build               # -> dist/doory (46 MB, frontend embedded; no node needed)
dist/doory migrate       # or let `doory entrypoint` do it
DJANGO_SECRET_KEY=$(openssl rand -base64 48) SITE_URL=http://localhost:8080 \
  dist/doory serve :8080
```

`dist/doory` with no arguments prints the command overview (`doory help` shows
the same, `doory <command> -h` the options of one command).

The plain Go equivalent (note the output path: `doory/` is the package
directory, so the binary goes to `dist/`):

```bash
go build -trimpath -ldflags="-s -w" -o dist/doory ./cmd/doory
# cross-compile: GOOS=linux GOARCH=arm64 go build -trimpath -ldflags="-s -w" \
#   -o dist/doory-linux-arm64 ./cmd/doory
```

- `DATABASE_URL=postgres://user:pass@host/db` switches to PostgreSQL (same
  binary, same code: the driver layer rewrites the port's `?` placeholders and
  normalises timestamps, D4).
- `DOORY_DATA_DIR` moves the data directory (`data/doory.sqlite3`,
  `data/media`, `data/exports`).
- `./doory entrypoint` boots a real install: wait for the database, migrate,
  run the `DOORY_SYNC_*` / `DOORY_SEED_TEMPLATES` bootstraps when asked (never
  fatal), then serve. `./doory healthcheck` is the liveness probe
  (`HEALTH_PORT`, `HEALTH_PATH`, exit 0 only on 200) — use it from systemd or
  any supervisor.
- `DOORY_DEV_PROXY=http://127.0.0.1:5173 ./doory serve` runs the API while
  every page request is proxied to the Vite dev server (the mirror image of
  `frontend/vite.config.ts`'s `server.proxy`).

### Deploying it (no containers)

The release artifact IS the binary — there is no image to build and no compose
file to run (D18). Install it and point a systemd unit at it; `deploy/doory.service`
is a working example:

```ini
[Unit]
Description=Doory (single binary)
After=network-online.target

[Service]
Type=simple
User=doory
WorkingDirectory=/var/lib/doory
EnvironmentFile=/etc/doory/doory.env      # DJANGO_SECRET_KEY, SITE_URL, ...
ExecStart=/usr/local/bin/doory entrypoint :8080
Restart=on-failure
# Server-side PDF export only: the port uses Chromium in a child process.
# NoNewPrivileges=true

[Install]
WantedBy=multi-user.target
```

```bash
sudo useradd --system --home /var/lib/doory doory
sudo install -d -o doory -g doory /var/lib/doory
sudo install -m 0755 doory /usr/local/bin/doory
sudo install -d /etc/doory
sudo cp .env.example /etc/doory/doory.env   # fill DJANGO_SECRET_KEY + SITE_URL
sudo cp deploy/doory.service /etc/systemd/system/doory.service
sudo systemctl daemon-reload && sudo systemctl enable --now doory
```

`/var/lib/doory` (the `WorkingDirectory`) is the data directory: SQLite
database, media and exports live there and that one directory is the backup
unit. Put a TLS-terminating proxy (Caddy/nginx/…) in front and set
`SECURE_SSL_REDIRECT=true` + `SECURE_COOKIES=true`. Server-side PDF export
needs Chromium installed on the host (`EXPORT_CHROMIUM_PATH`) and
`SERVER_PDF_ENABLED=true`.

Paths the single listener owns (the nginx rules upstream serves, D7):
`/api/`, `/admin/`, `/ws/`, `/mcp`, `/sitemap.xml` and `/static/` go to the
app; `/media/` is served with the media headers; `/assets/` is immutable;
everything else is the SPA shell. See `doory/webapp.go` for the exact header
set (`frontend/nginx/security-headers.conf`).
