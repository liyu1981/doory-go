#!/usr/bin/env bash
# Boot the pinned oracle (Django, test settings, SQLite file DB outside the
# mirror) on 127.0.0.1:8012. Ctrl-C or `kill` stops it. The mirror stays
# pristine: PYTHONDONTWRITEBYTECODE=1 and every writable path points into
# .oracle/ or tempfile space.
set -euo pipefail

REPO="$(cd "$(dirname "$0")/../.." && pwd)"
BACKEND="$REPO/.upstream/current/backend"
VENV="$REPO/.oracle/venv"
DB="$REPO/.oracle/oracle.sqlite3"

export PYTHONDONTWRITEBYTECODE=1
export DJANGO_SETTINGS_MODULE=doory.settings.test
export TEST_DATABASE_URL="sqlite:///$DB"

cd "$BACKEND"
"$VENV/bin/python" manage.py migrate --noinput > /dev/null
echo "oracle: db ready at $DB; serving on http://127.0.0.1:8012 (Ctrl-C to stop)"
exec "$VENV/bin/python" manage.py runserver 127.0.0.1:8012 --noreload
