#!/bin/sh
# Sends demo traffic from three fake services (web -> api -> billing) so the
# architecture map has something to show. Usage:
#   deploy/demo-traffic.sh [host:port] [iterations]
set -eu

HOST="${1:-localhost:9001}"
ITERATIONS="${2:-30}"
KEY="${LOGDOC_API_KEY:-}"

auth() {
  if [ -n "$KEY" ]; then printf 'X-API-Key: %s' "$KEY"; else printf 'X-Demo: 1'; fi
}

i=0
while [ "$i" -lt "$ITERATIONS" ]; do
  i=$((i + 1))
  trace=$(od -An -N8 -tx1 /dev/urandom | tr -d ' \n')
  # Every 7th checkout fails at billing.
  lvl="INFO"; msg="charge succeeded"
  if [ $((i % 7)) -eq 0 ]; then lvl="ERROR"; msg="charge declined: insufficient funds"; fi

  curl -sS -o /dev/null -X POST "http://$HOST/api/v1/ingest" -H "$(auth)" -d '[
    {"msg":"GET /checkout","app":"web","lvl":"INFO","src":"http","fields":{"trace_id":"'"$trace"'","route":"/checkout"}},
    {"msg":"create order","app":"api","lvl":"INFO","src":"orders","fields":{"trace_id":"'"$trace"'","order_id":"ord-'"$i"'"}},
    {"msg":"'"$msg"'","app":"billing","lvl":"'"$lvl"'","src":"charge","fields":{"trace_id":"'"$trace"'","order_id":"ord-'"$i"'"}}
  ]'
  sleep 0.2
done
echo "sent $ITERATIONS checkout flows (web -> api -> billing) to $HOST"
