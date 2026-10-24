# The Doory single binary (D1 + D7): one image serves the API, the realtime
# WebSockets, MCP and the embedded frontend on one port — it replaces the
# upstream api + realtime + mcp + web (nginx) services from docker-compose.yml.
#
#   docker build -t doory .
#   docker run -p 8080:8080 -v doory-data:/app/data \
#     -e DJANGO_SECRET_KEY=... -e SITE_URL=http://localhost:8080 doory
#
# With no DATABASE_URL the binary keeps its SQLite database (and media) under
# /app/data (D4), so that one volume is the whole persistent state; set
# DATABASE_URL=postgres://… to use PostgreSQL instead. Server-side PDF export
# needs Chromium: build with --build-arg INSTALL_CHROMIUM=true and set
# SERVER_PDF_ENABLED=true.
# syntax=docker/dockerfile:1
FROM golang:1.27-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
COPY third_party ./third_party
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/doory ./cmd/doory

FROM debian:bookworm-slim AS runtime
ARG INSTALL_CHROMIUM=false
RUN if [ "$INSTALL_CHROMIUM" = "true" ]; then \
      apt-get update \
      && apt-get install -y --no-install-recommends chromium fonts-liberation ca-certificates \
      && rm -rf /var/lib/apt/lists/*; \
    else \
      apt-get update && apt-get install -y --no-install-recommends ca-certificates \
      && rm -rf /var/lib/apt/lists/*; \
    fi \
    && groupadd --system --gid 10001 doory \
    && useradd --system --uid 10001 --gid doory --home-dir /app --shell /usr/sbin/nologin doory
ENV DOORY_ADDR=:8080 \
    DOORY_DATA_DIR=/app/data \
    HEALTH_PORT=8080 \
    SERVER_PDF_ENABLED=${INSTALL_CHROMIUM} \
    EXPORT_CHROMIUM_NO_SANDBOX=true
COPY --from=build /out/doory /app/doory
RUN mkdir -p /app/data && chown doory:doory /app /app/data
WORKDIR /app
USER doory
EXPOSE 8080
VOLUME ["/app/data"]
HEALTHCHECK --interval=30s --timeout=5s --start-period=60s --retries=3 \
    CMD ["/app/doory", "healthcheck"]
# The entrypoint waits for the database, migrates and runs the DOORY_SYNC_*
# bootstraps only when asked (they need network), then serves.
ENTRYPOINT ["/app/doory", "entrypoint"]
