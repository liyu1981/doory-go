# Configuration

Doory is configured with **environment variables**. The binary uses the same
names and defaults as the upstream backend, and adds an optional dotenv file so
a self-hosted install can keep its settings in one place (D23).

## Where variables come from

In increasing priority:

1. `~/.config/doory/.env` (or `$XDG_CONFIG_HOME/doory/.env`) when that file
   exists.
2. A file passed on the command line:
   `doory --env-file /etc/doory/doory.env serve :8080`.
3. Variables already present in the process environment.

The dotenv file only fills variables that are **absent** from the process
environment, so systemd's `EnvironmentFile=`, Docker `-e`, or an `export` in
the shell always win. `--no-env-file` ignores the default file completely (used
by the parity harness so tests stay hermetic).

The file accepts `KEY=value`, optional single/double quotes, `export KEY=value`,
blank lines and `#` comments. The template itself is also valid as a systemd
`EnvironmentFile` (systemd does not understand the `export` prefix).

`.env.example` at the repo root is the canonical template: every setting the
binary reads is listed there with its default and a short explanation. It
carries a `# template-version: N` marker so upgrades can be tracked.

## Generating and upgrading the file

```bash
doory config init                # write ./.env from the embedded .env.example
doory config init --force        # overwrite it, keeping the old file as .env.bak
doory config upgrade             # merge the latest template into ./.env
doory config upgrade path/to/.env
```

`doory config upgrade`:

- keeps every **active** value you have configured;
- adds settings that are new in the current template (active defaults, or
  commented documentation for optional settings);
- keeps active settings that are no longer in the template in a trailing
  "Kept from the previous file" section;
- writes `FILE.bak` before rewriting and is a no-op when the file is current.

`doory config init` writes a mode-`0600` file in the current directory (or a
directory argument). A file there is not loaded automatically: use
`doory --env-file ./.env …`, or write it to the default user path with
`doory config init ~/.config/doory`. It is independent of the database, so it
can run before the first start.

## Providing variables (deployment recipes)

| Deployment | How |
|---|---|
| user service (`doory install-service`) | The binary writes/upgrades `~/.config/doory/.env` and reads it directly; an old `~/.doory/doory.env` is migrated once. `scripts/install-systemd.sh` is the build-and-delegate wrapper. |
| systemd (system-wide) | `doory config init /etc/doory` or copy `.env.example` to `/etc/doory/.env`; keep `EnvironmentFile=/etc/doory/doory.env` or rely on the default `~/.config` path of the service user. |
| container / supervisor | Pass the variables directly (`-e KEY=val`, `Environment=…`) or mount a file and use `--env-file`. |
| local dev / manual | `doory config init` then `doory --env-file .env serve :8111`; move the file to `~/.config/doory/.env` to load it without the flag. Shell `export` still works. |

## Required outside localhost

- `SITE_URL` — public origin with scheme, no trailing slash (e.g. `https://doory.example.com`). Used for emails, CSRF, exports.
- `DJANGO_SECRET_KEY` — long random value (`openssl rand -base64 48`). Changing it signs everyone out.
- `DJANGO_ALLOWED_HOSTS` — comma-separated hostnames the API answers to (default `localhost,127.0.0.1`).
- `CSRF_TRUSTED_ORIGINS` — defaults to `SITE_URL` when unset; add any extra origins.

## Storage (D4)

Everything that must survive lives under one directory:

- `DOORY_DATA_DIR` (default `./data`) — holds `doory.sqlite3`, `media/`, `exports/`.
- `DATABASE_URL` — default `sqlite:///./data/doory.sqlite3`; set `postgres://user:pass@host/db` for PostgreSQL (same binary).
- `MEDIA_ROOT`, `EXPORT_STORAGE_ROOT` — override individual paths.

## HTTPS / proxy

- `SECURE_COOKIES` (default `true`) — Secure flag on session/CSRF cookies. Set `false` only for plain HTTP.
- `SECURE_SSL_REDIRECT` and `NUM_PROXIES` are accepted for upstream
  compatibility but **not read by this binary yet**: terminate TLS and set
  forwarding headers in the reverse proxy instead.

## Listener

- Address is a positional arg: `doory serve :8080` / `doory entrypoint :8080` (default `:8111`).
- `DOORY_ADDR` overrides the default.
- `HEALTH_PORT` / `HEALTH_PATH` configure `doory healthcheck` (default `8111`, `/api/v1/health/`).

## Admin site

- URL: `/admin/`. Access requires `is_active && is_staff`; superusers bypass per-model permissions.
- Bootstrap: `doory createsuperuser` (interactive) or non-interactive via
  `DJANGO_SUPERUSER_EMAIL` + `DJANGO_SUPERUSER_PASSWORD` + `--noinput`.
- Promote an existing account: `doory make-admin [--superuser] <email>`.
- Passwords: `doory changepassword <email>` or `/admin/password_change/` in the UI.
- Per-model permissions are seeded automatically (`auth_permission` + `django_content_type`); manage groups at `/admin/auth/group/`.

## Authentication / SSO

- **Email + password** is always available.
- **Google sign-in:** set `GOOGLE_CLIENT_ID`. The frontend shows the Google
  button, but backend verification (`POST /api/v1/auth/google/`) is not ported
  yet (gap G3) — sign-in will not complete. Track G3 before relying on it.
- **Generic OIDC SSO (working today):** set
  - `OIDC_ISSUER` (e.g. `https://accounts.google.com`)
  - `OIDC_CLIENT_ID`, `OIDC_CLIENT_SECRET`
  - `OIDC_SCOPES` (default `openid email profile`)
  - `OIDC_TRUST_EMAIL` (default `false`)
  - `OIDC_BUTTON_LABEL`, `OIDC_BUTTON_ICON` (optional)
  - Google is an OIDC provider, so pointing `OIDC_ISSUER` at Google with a
    web-application OAuth client is the working path today. Register the
    callback `/api/v1/auth/oidc/callback/` in the provider.

## First-run content

`doory entrypoint` can run optional syncs before serving (the embedded content
runs by default; failed downloads are logged and never stop the server):

- `DOORY_SYNC_ELEMENTS`, `DOORY_SYNC_ELEMENTS_ARGS`
- `DOORY_ELEMENTS_BRAND_LOGOS`
- `DOORY_SYNC_FONTS`, `DOORY_SYNC_FONTS_ARGS`
- `DOORY_SEED_TEMPLATES`, `DOORY_SEED_TEMPLATE_FONTS`
- `DOORY_SYNC_MODELS`, `DOORY_SYNC_MODELS_ARGS`
- `DOORY_SYNC_AUDIO`, `DOORY_SYNC_AUDIO_ARGS`
- `DOORY_SIZE_PRESETS_FILE`, `DOORY_SIZE_PRESETS_MODE`

Or run the management commands by hand: `doory seed_gallery`,
`doory seed_size_presets`, `doory seed_elements`, `doory sync_fonts`, etc.
(`doory help` lists them).

## Server-side export (optional)

Requires Chromium on the host:

- `SERVER_PDF_ENABLED`, `SERVER_IMAGE_EXPORT_ENABLED`
- `EXPORT_CHROMIUM_PATH`, `EXPORT_CHROMIUM_NO_SANDBOX`
- `EXPORT_RENDER_PAGE_URL`, `EXPORT_ALLOWED_ORIGINS`
- `EXPORT_FILENAME_STYLE`, `EXPORT_FILENAME_TIMESTAMP`
- `EXPORT_IMAGE_MAX_PIXELS`, `EXPORT_IMAGE_MAX_DOCUMENT_BYTES`
- `EXPORT_MAX_CONCURRENCY`, `EXPORT_TIMEOUT_SECONDS`, `EXPORT_WORK_DIR`

## Image optimizers (optional)

- `IMAGE_OPTIMIZERS` (default `builtin`), `IMAGE_OPTIMIZER_DEFAULT`
- `TINYPNG_API_KEY`, `KRAKEN_API_KEY`, `KRAKEN_API_SECRET`
- `IMAGE_OPTIMIZER_CUSTOM_NAME`, `IMAGE_OPTIMIZER_CUSTOM_URL`,
  `IMAGE_OPTIMIZER_CUSTOM_KEY`, `IMAGE_OPTIMIZER_MAX_BYTES`

## AI providers (optional)

- `AI_PROVIDERS_ENABLED`, `AI_USER_PROVIDERS` (`off`/`staff`/`everyone`)
- `AI_SECRETS_KEY` (defaults to a key derived from `DJANGO_SECRET_KEY`)
- Operator providers: `AI_PROVIDER_1_URL`, `AI_PROVIDER_1_STYLE`,
  `AI_PROVIDER_1_TASKS`, `AI_PROVIDER_1_MODELS`, … up to slot 9 (see
  `.env.example` for every field).
- Stock providers: `STOCK_PROVIDERS`, `STOCK_VIDEO_PROVIDERS`,
  `UNSPLASH_ACCESS_KEY`, `PEXELS_API_KEY`, `PIXABAY_API_KEY`,
  `FREESOUND_ACCESS_KEY`, `PEXELS_VIDEO_API_KEY`, `PIXABAY_VIDEO_API_KEY`.

## Video assets (optional)

- `VIDEO_ENABLED`, `FFMPEG_PATH`, `FFPROBE_PATH`
- `MAX_VIDEO_UPLOAD_BYTES`, `MAX_VIDEO_DURATION_SECONDS`

## Limits

- `MAX_DESIGNS_PER_USER` (default 5000), `MAX_API_TOKENS_PER_USER` (default 20),
  `USER_STORAGE_QUOTA_BYTES` (default 2 GiB), `BULK_FILL_MAX_ROWS` (default 100).

## MCP server (optional)

- `MCP_ENABLED=false` by default; `MCP_PATH=/mcp`, `MCP_CALLS_PER_MINUTE=120`,
  `MCP_MAX_REQUEST_BYTES=4194304`, `MCP_EXPORT_INLINE_MAX_BYTES=1048576`,
  `MCP_EXPORT_URL_TTL_SECONDS=600`, `URL_IMAGE_UPLOADS=true`.
- Clients authenticate with a personal access token (Settings → API tokens).

## Email

- `EMAIL_URL` (default `consolemail://` — prints to stdout; set an `smtp://…`
  URL for real delivery). `EMAIL_SEND_ASYNC`, `AUTH_EMAIL_COOLDOWN_SECONDS`.

## Real-time co-editing

- `REALTIME_ENABLED`, frame/connection/timeouts and rate-limit knobs:
  `REALTIME_MAX_FRAME_BYTES`, `REALTIME_MAX_CONNECTIONS_PER_DESIGN`,
  `REALTIME_MAX_CONNECTIONS_PER_USER`, `REALTIME_AUTH_TIMEOUT_SECONDS`,
  `REALTIME_IDLE_TIMEOUT_SECONDS`, `REALTIME_FLUSH_INTERVAL_SECONDS`,
  `REALTIME_RECHECK_SECONDS`, `REALTIME_COMPACT_AFTER_UPDATES`,
  `REALTIME_SLOW_APPLY_SECONDS`, `REALTIME_CONNECTS_PER_MINUTE`,
  `REALTIME_BLOCK_SECONDS`, `REALTIME_RATE_*` (message/byte token buckets).
  `.env.example` lists the defaults.

## Development

- `DOORY_DEV_PROXY=http://127.0.0.1:5173` — proxy page requests to the Vite dev server.
- `DJANGO_DEBUG=true` — Django-style debug errors (never on a public install).
