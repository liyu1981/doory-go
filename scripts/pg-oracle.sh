#!/usr/bin/env bash
# Start (or restart) the local Postgres lane for the dialect matrix (D4):
# postgres:16-alpine on 127.0.0.1:55432 with doory/doory credentials, plus
# an empty doory_go database for the Go side. Used by schema-parity and the
# dialect-declared http scenarios.
set -euo pipefail

NAME=doory-parity-pg
docker rm -f "$NAME" > /dev/null 2>&1 || true
docker run -d --name "$NAME" \
  -e POSTGRES_USER=doory -e POSTGRES_PASSWORD=doory -e POSTGRES_DB=doory \
  -p 55432:5432 postgres:16-alpine > /dev/null
for _ in $(seq 1 30); do
  if docker exec "$NAME" pg_isready -U doory > /dev/null 2>&1; then
    docker exec "$NAME" createdb -U doory doory_go
    echo "pg-oracle: postgres:16-alpine ready on 127.0.0.1:55432 (doory/doory, dbs: doory, doory_go)"
    exit 0
  fi
  sleep 1
done
echo "pg-oracle: postgres did not become ready" >&2
exit 1
