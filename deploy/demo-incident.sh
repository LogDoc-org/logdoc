#!/bin/sh
# Demo incident: a database failure cascading through three services.
#
#   web -> api -> billing -> postgres     (checkout path, breaks)
#          api -> catalog                 (browse path, stays healthy)
#
# Phase 1: healthy traffic. Phase 2: billing 2.3.1 is deployed with a
# connection leak; postgres runs out of connections; billing, api and web
# degrade one after another while catalog keeps working.
#
# Usage: deploy/demo-incident.sh [host:port]
set -eu

HOST="${1:-localhost:9001}"
KEY="${LOGDOC_API_KEY:-}"

auth() {
  if [ -n "$KEY" ]; then printf 'X-API-Key: %s' "$KEY"; else printf 'X-Demo: 1'; fi
}

send() { # send JSON_ARRAY
  curl -sS -o /dev/null -X POST "http://$HOST/api/v1/ingest" -H "$(auth)" -d "$1"
}

trace() { od -An -N8 -tx1 /dev/urandom | tr -d ' \n'; }

checkout_ok() {
  t=$(trace)
  send '[
    {"msg":"GET /checkout","app":"web","lvl":"INFO","src":"http","fields":{"trace_id":"'"$t"'"}},
    {"msg":"create order","app":"api","lvl":"INFO","src":"orders","fields":{"trace_id":"'"$t"'"}},
    {"msg":"acquire connection from pool","app":"billing","lvl":"DEBUG","src":"db","fields":{"trace_id":"'"$t"'","pool_in_use":"'"$1"'"}},
    {"msg":"charge succeeded","app":"billing","lvl":"INFO","src":"charge","fields":{"trace_id":"'"$t"'"}},
    {"msg":"connection authorized: user=billing database=payments","app":"postgres","lvl":"LOG","src":"postmaster","fields":{"trace_id":"'"$t"'"}}
  ]'
}

checkout_fail() {
  t=$(trace)
  send '[
    {"msg":"GET /checkout","app":"web","lvl":"INFO","src":"http","fields":{"trace_id":"'"$t"'"}},
    {"msg":"create order","app":"api","lvl":"INFO","src":"orders","fields":{"trace_id":"'"$t"'"}},
    {"msg":"FATAL: sorry, too many clients already","app":"postgres","lvl":"ERROR","src":"postmaster","fields":{"trace_id":"'"$t"'","max_connections":"100"}},
    {"msg":"acquire connection: pool exhausted (100/100 in use, waited 5s)","app":"billing","lvl":"ERROR","src":"db","fields":{"trace_id":"'"$t"'","pool_in_use":"100"}},
    {"msg":"charge failed: billing database unavailable","app":"billing","lvl":"ERROR","src":"charge","fields":{"trace_id":"'"$t"'"}},
    {"msg":"POST /billing/charge -> 503 Service Unavailable","app":"api","lvl":"ERROR","src":"orders","fields":{"trace_id":"'"$t"'"}},
    {"msg":"checkout failed: internal error","app":"web","lvl":"ERROR","src":"http","fields":{"trace_id":"'"$t"'"}}
  ]'
}

browse_ok() {
  t=$(trace)
  send '[
    {"msg":"GET /products","app":"web","lvl":"INFO","src":"http","fields":{"trace_id":"'"$t"'"}},
    {"msg":"list products","app":"api","lvl":"INFO","src":"products","fields":{"trace_id":"'"$t"'"}},
    {"msg":"catalog lookup ok","app":"catalog","lvl":"INFO","src":"lookup","fields":{"trace_id":"'"$t"'"}}
  ]'
}

echo "phase 1: healthy traffic..."
i=0
while [ "$i" -lt 25 ]; do
  i=$((i + 1))
  checkout_ok $((10 + i % 20))
  browse_ok
  sleep 0.1
done

echo "phase 2: deploying billing 2.3.1 (with a connection leak)..."
send '[{"msg":"deploy billing 2.3.1: connection pool 100, keepalive disabled","app":"billing","lvl":"WARN","src":"deploy","fields":{"version":"2.3.1"}}]'

# The pool fills up: in-use count climbs, then everything fails.
i=0
while [ "$i" -lt 10 ]; do
  i=$((i + 1))
  checkout_ok $((60 + i * 4))
  browse_ok
  sleep 0.1
done

echo "phase 3: the incident — checkout is down, browse still works..."
i=0
while [ "$i" -lt 25 ]; do
  i=$((i + 1))
  checkout_fail
  browse_ok
  sleep 0.1
done

echo "incident injected: ask the agent why checkout is failing on web"
