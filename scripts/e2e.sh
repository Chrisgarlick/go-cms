#!/usr/bin/env bash
# Starts a blog starter site with `cms dev` and runs the Playwright suite.
set -euo pipefail
BIN=$(pwd)/bin/cms
PORT=${PORT:-18081}
tmp=$(mktemp -d)
site=$tmp/site
"$BIN" new "$site" --starter blog --git=false -q
(cd "$site" && "$BIN" dev --port "$PORT" >/tmp/cms-e2e.log 2>&1 &)
cleanup() {
  pkill -f "cms dev --port $PORT" || true
  sleep 2          # let it stop Postgres before the files go
  rm -rf "$tmp"    # embedded Postgres data is ~200MB per run
}
trap cleanup EXIT
for i in $(seq 1 120); do curl -fsS "http://localhost:$PORT/readyz" >/dev/null 2>&1 && break; sleep 1; done
export CMS_BASE_URL="http://localhost:$PORT" CMS_CREDENTIALS="$site/.cms/dev/credentials"
cd e2e && npx playwright test
