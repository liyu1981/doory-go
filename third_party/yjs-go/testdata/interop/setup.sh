#!/bin/sh
# Install interop test dependencies.
# Run once before YJS_GO_INTEROP=1 go test ./transport/...
set -e
cd "$(dirname "$0")"
npm install --silent
