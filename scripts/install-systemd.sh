#!/usr/bin/env bash
# Build (if needed) and install Doory as a systemd USER service.
#
#   ./scripts/install-systemd.sh                 # build, then install + enable + start
#   ./scripts/install-systemd.sh --no-build      # use dist/doory as it is
#   ./scripts/install-systemd.sh --port 8111     # listen address (default 8111)
#   ./scripts/install-systemd.sh --uninstall     # stop, disable and remove the unit
#
# The installation itself lives in the binary now:
#
#   dist/doory install-service [--port PORT] [--uninstall]
#
# It installs the binary to ~/.local/bin/doory, writes/upgrades
# ~/.config/doory/.env (migrating an old ~/.doory/doory.env once), writes the
# unit to ~/.config/systemd/user/doory.service and keeps state in ~/.doory.
# This script is only the build-and-delegate wrapper.
set -euo pipefail

REPO="$(cd "$(dirname "$0")/.." && pwd)"
cd "$REPO"

PORT="8111"
BUILD=1
UNINSTALL=0
BIN="$REPO/dist/doory"

while [ $# -gt 0 ]; do
  case "$1" in
    --no-build) BUILD=0; shift ;;
    --uninstall) UNINSTALL=1; shift ;;
    --port) PORT="${2:-}"; shift 2 ;;
    --port=*) PORT="${1#--port=}"; shift ;;
    -h|--help) sed -n '2,16p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) echo "install-systemd: unknown argument: $1 (see --help)" >&2; exit 2 ;;
  esac
done
case "$PORT" in
  ''|*[!0-9]*) echo "install-systemd: --port must be a number" >&2; exit 2 ;;
esac

if [ "$UNINSTALL" = "1" ]; then
  if [ -x "$BIN" ]; then
    exec "$BIN" install-service --uninstall
  fi
  if [ -x "$HOME/.local/bin/doory" ]; then
    exec "$HOME/.local/bin/doory" install-service --uninstall
  fi
  echo "install-systemd: no doory binary found (neither $BIN nor ~/.local/bin/doory)" >&2
  exit 1
fi

if [ "$BUILD" = "1" ]; then
  ./scripts/build.sh
fi
if [ ! -x "$BIN" ]; then
  echo "install-systemd: $BIN not found — run ./scripts/build.sh (or pnpm build) first" >&2
  exit 1
fi
exec "$BIN" install-service --port "$PORT"
