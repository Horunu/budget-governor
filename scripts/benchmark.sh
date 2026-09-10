#!/usr/bin/env bash
# Runs the k6 fan-out burst load test (loadtest/fanout_burst.js) against
# a running gateway, reports p50/p95/p99 latency, rejection rate, and
# cost attribution, and saves results to loadtest/results/.
#
# Requires: docker compose stack already up (`make up`) and seeded
# (`make seed`). Uses a local `k6` binary if installed; otherwise falls
# back to running k6 via its official Docker image on the compose
# network, so no local install is required.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

TARGET_RATE="${TARGET_RATE:-5000}"
DURATION="${DURATION:-30s}"
AGENT_FANOUT="${AGENT_FANOUT:-500}"

if [ ! -f scripts/.seed_output.env ]; then
  echo "scripts/.seed_output.env not found -- run 'make seed' first." >&2
  exit 1
fi
# shellcheck disable=SC1091
source scripts/.seed_output.env

mkdir -p loadtest/results
TIMESTAMP="$(date -u +%Y%m%dT%H%M%SZ)"
JSON_OUT="loadtest/results/fanout_burst_${TIMESTAMP}.json"
SUMMARY_OUT="loadtest/results/fanout_burst_${TIMESTAMP}_summary.txt"

echo "=== Running fanout_burst.js: target ${TARGET_RATE} req/s for ${DURATION}, ${AGENT_FANOUT} simulated agents ==="

if command -v k6 >/dev/null 2>&1; then
  k6 run \
    -e GATEWAY_URL="http://localhost:8080" \
    -e API_KEY="$ACME_CORP_ADMIN_KEY" \
    -e TARGET_RATE="$TARGET_RATE" \
    -e DURATION="$DURATION" \
    -e AGENT_FANOUT="$AGENT_FANOUT" \
    --summary-export "$JSON_OUT" \
    loadtest/fanout_burst.js | tee "$SUMMARY_OUT"
else
  echo "local k6 binary not found -- running via grafana/k6 Docker image on the compose network"
  NETWORK="budget-governor_default"
  docker run --rm -i \
    --network "$NETWORK" \
    -e GATEWAY_URL="http://gateway:8080" \
    -e API_KEY="$ACME_CORP_ADMIN_KEY" \
    -e TARGET_RATE="$TARGET_RATE" \
    -e DURATION="$DURATION" \
    -e AGENT_FANOUT="$AGENT_FANOUT" \
    -v "$REPO_ROOT/loadtest:/scripts" \
    -w /scripts \
    grafana/k6:0.54.0 run \
    --summary-export "/scripts/results/fanout_burst_${TIMESTAMP}.json" \
    fanout_burst.js | tee "$SUMMARY_OUT"
fi

echo
echo "=== Cost attribution (last 5 minutes, acme-corp) ==="
curl -sS "http://localhost:8081/v1/spend/${ACME_CORP_TENANT_ID}?window=1h" \
  -H "Authorization: Bearer $ACME_CORP_ADMIN_KEY" | \
  python3 -c "
import json, sys
data = json.load(sys.stdin)
print(f\"  total requests:  {data['total_requests']}\")
print(f\"  total tokens in:  {data['total_tokens_in']}\")
print(f\"  total tokens out: {data['total_tokens_out']}\")
print(f\"  total cost:      \${data['total_cost_usd']:.4f}\")
for m in data['by_model']:
    print(f\"    - {m['model']}: {m['request_count']} reqs, \${m['cost_usd']:.4f}\")
"

echo
echo "Results saved to: $JSON_OUT"
echo "Summary saved to: $SUMMARY_OUT"
echo
echo "=== Headline check: p99 latency on ALLOWED requests vs. 10ms budget ==="
if [ -f "$JSON_OUT" ]; then
  python3 -c "
import json
with open('$JSON_OUT') as f:
    data = json.load(f)
metrics = data.get('metrics', {})
allowed = metrics.get('allowed_request_duration', {})
p99 = allowed.get('values', {}).get('p(99)')
rejected_count = metrics.get('rejected_requests', {}).get('values', {}).get('count', 0)
allowed_count = metrics.get('allowed_requests', {}).get('values', {}).get('count', 0)
total = rejected_count + allowed_count
if p99 is not None:
    status = 'PASS' if p99 < 10 else 'FAIL'
    print(f'  p99 (allowed): {p99:.3f}ms [{status} vs 10ms budget]')
if total:
    print(f'  rejection rate: {100*rejected_count/total:.1f}% ({rejected_count}/{total})')
"
else
  echo "  (summary JSON not found -- inspect $SUMMARY_OUT for k6's console output instead)"
fi
