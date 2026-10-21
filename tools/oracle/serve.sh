#!/usr/bin/env bash
# Boot the pinned oracle (Django) for probing or paired scenarios.
# Env overrides (defaults suit interactive probing):
#   ORACLE_SETTINGS   Django settings module (default doory.settings.test)
#   ORACLE_PORT       listen port                (default 8012)
#   ORACLE_DATABASE_URL  DATABASE_URL for the oracle
# The mirror stays pristine: PYTHONDONTWRITEBYTECODE=1 and every writable
# path points into .oracle/ or tempfile space.
set -euo pipefail

REPO="$(cd "$(dirname "$0")/../.." && pwd)"
BACKEND="$REPO/.upstream/current/backend"
VENV="$REPO/.oracle/venv"
PORT="${ORACLE_PORT:-8012}"
DB="${ORACLE_DATABASE_URL:-sqlite:///$REPO/.oracle/oracle.sqlite3}"

export PYTHONDONTWRITEBYTECODE=1
export DJANGO_SETTINGS_MODULE="${ORACLE_SETTINGS:-doory.settings.test}"
export DATABASE_URL="$DB"
export TEST_DATABASE_URL="$DB" # settings/test.py reads this one
export MEDIA_ROOT="${MEDIA_ROOT:-$REPO/.oracle/media}"
export EXPORT_STORAGE_ROOT="${EXPORT_STORAGE_ROOT:-$REPO/.oracle/exports}"

cd "$BACKEND"
"$VENV/bin/python" manage.py migrate --noinput > /dev/null
exec "$VENV/bin/python" manage.py runserver "127.0.0.1:$PORT" --noreload

