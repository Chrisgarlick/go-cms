#!/usr/bin/env bash
# Memory and latency budgets (spec 8.2). Fails the build on regression.
set -euo pipefail
BIN=$(realpath "${BIN:-./bin/cms}")
PORT=${PORT:-18090}
P95_MS=${P95_MS:-20}
RSS_MB=${RSS_MB:-128}
site=$(mktemp -d)/site
"$BIN" new "$site" --starter blog --git=false -q
(cd "$site" && "$BIN" dev --port "$PORT" >/tmp/cms-budget.log 2>&1 &)
for i in $(seq 1 120); do curl -fsS "http://localhost:$PORT/readyz" >/dev/null 2>&1 && break; sleep 1; done
curl -fsS "http://localhost:$PORT/readyz" >/dev/null 2>&1 || { echo "cms dev never became ready:"; cat /tmp/cms-budget.log; exit 1; }
sleep 3
cat >/tmp/budget.js <<JS
import http from 'k6/http';
export const options = { vus: 20, duration: '20s', thresholds: { http_req_duration: ['p(95)<${P95_MS}'], http_req_failed: ['rate<0.001'] } };
const paths = ['/', '/blog/', '/blog/why-this-site-is-fast/', '/about/', '/category/engineering/'];
export default function () { http.get('http://localhost:${PORT}' + paths[Math.floor(Math.random() * paths.length)]); }
JS
k6 run --quiet /tmp/budget.js
pid=$(pgrep -f "cms dev --port $PORT" | head -1)
rss_kb=$(ps -o rss= -p "$pid" | tr -d ' ')
rss_mb=$(( rss_kb / 1024 ))
echo "cms RSS after load: ${rss_mb} MB (budget ${RSS_MB} MB)"
kill "$pid" || true
(( rss_mb <= RSS_MB )) || { echo "memory budget exceeded"; exit 1; }
