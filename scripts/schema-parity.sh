#!/usr/bin/env bash
# Schema-driver (PLAN.md §4): digest-compare the pinned oracle's schema
# against the Go-migrated schema, per dialect. SQLite always; Postgres when
# the local docker lane (doory-parity-pg) is running.
set -euo pipefail

REPO="$(cd "$(dirname "$0")/.." && pwd)"
cd "$REPO"

ORACLE_DB=".oracle/oracle.sqlite3"
GO_DB=".oracle/go.sqlite3"

# Ensure both sqlite DBs are migrated (oracle via its manage.py).
[ -f "$ORACLE_DB" ] || env PYTHONDONTWRITEBYTECODE=1 DJANGO_SETTINGS_MODULE=doory.settings.test \
  DATABASE_URL="sqlite:///$REPO/$ORACLE_DB" \
  .oracle/venv/bin/python -c "import sys; sys.path.insert(0, '.upstream/current/backend'); import os; os.chdir('.upstream/current/backend'); import django; os.environ['DJANGO_SETTINGS_MODULE']='doory.settings.test'; django.setup(); from django.core.management import call_command; call_command('migrate', '--noinput', verbosity=0)" > /dev/null
DATABASE_URL="sqlite:///$REPO/$GO_DB" go run ./cmd/doory migrate > /dev/null
go run ./automation/ci/schemadigest -a "$ORACLE_DB" -b "$GO_DB"

if docker ps --format '{{.Names}}' | grep -q '^doory-parity-pg$'; then
  PG="postgres://doory:doory@127.0.0.1:55432"
  docker exec doory-parity-pg psql -U doory -d doory -c 'SELECT 1' > /dev/null 2>&1 || {
    echo "schema-parity: doory-parity-pg is running but not reachable; start it with scripts/pg-oracle.sh" >&2
    exit 1
  }
  # Oracle side: migrate its PG database if needed (test settings read
  # TEST_DATABASE_URL; the DB itself is created by pg-oracle.sh).
  (cd .upstream/current/backend && env PYTHONDONTWRITEBYTECODE=1 DJANGO_SETTINGS_MODULE=doory.settings.test \
    TEST_DATABASE_URL="$PG/doory" "$REPO/.oracle/venv/bin/python" manage.py migrate --noinput > /dev/null)
  docker exec doory-parity-pg createdb -U doory doory_go 2>/dev/null || true
  DATABASE_URL="$PG/doory_go" go run ./cmd/doory migrate > /dev/null
  go run ./automation/ci/schemadigest -driver pgx -a "$PG/doory" -b "$PG/doory_go"
else
  echo "schema-parity: postgres lane skipped (start with scripts/pg-oracle.sh)"
fi
