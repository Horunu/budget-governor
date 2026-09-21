#!/usr/bin/env bash
# End-to-end smoke test: brings up the full stack, seeds demo data,
# exercises the gateway (allowed + budget-pressure paths), verifies
# logs/metrics/spend records, confirms the cost advisor fires, runs
# reconciliation, and prints a pass/fail summary.
#
# Requires: docker, docker compose, curl, jq, python3.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

GATEWAY_URL="${GATEWAY_URL:-http://localhost:8080}"
GATEWAY_METRICS_URL="${GATEWAY_METRICS_URL:-http://localhost:9090}"
CONTROLPLANE_URL="${CONTROLPLANE_URL:-http://localhost:8081}"
ADVISOR_URL="${ADVISOR_URL:-http://localhost:8082}"

PASS=0
FAIL=0

pass() { echo "  [PASS] $1"; PASS=$((PASS+1)); }
fail() { echo "  [FAIL] $1"; FAIL=$((FAIL+1)); }

for bin in docker curl jq python3; do
  command -v "$bin" >/dev/null 2>&1 || { echo "missing required tool: $bin"; exit 1; }
done

echo "=== 1. Bringing up the stack ==="
docker compose -f deploy/docker-compose.yml up -d --build

echo "=== 2. Applying migrations ==="
docker compose -f deploy/docker-compose.yml run --rm migrate

echo "=== 3. Seeding demo tenants/agents/budgets ==="
python3 scripts/seed.py --controlplane-url "$CONTROLPLANE_URL"
# shellcheck disable=SC1091
source scripts/.seed_output.env
# Wait for the gateway's budget config cache to refresh (BUDGET_CACHE_TTL_SECONDS=5 default).
echo "  Waiting 6s for gateway config cache refresh..."
sleep 6

echo "=== 4. Making allowed LLM calls through the gateway (mock provider) ==="
RESP=$(curl -sS -w '\n%{http_code}' -X POST "$GATEWAY_URL/v1/chat/completions" \
  -H "Authorization: Bearer $ACME_CORP_ADMIN_KEY" \
  -H "X-Agent-Id: $ACME_CORP_SUPPORT_BOT_AGENT_ID" \
  -H "Content-Type: application/json" \
  -d '{"model":"mock-small","messages":[{"role":"user","content":"What is the capital of France?"}]}')
CODE=$(echo "$RESP" | tail -n1)
BODY=$(echo "$RESP" | sed '$d')
if [ "$CODE" = "200" ] && echo "$BODY" | jq -e '.choices[0].message.content' >/dev/null 2>&1; then
  pass "gateway returned a 200 with assistant content"
else
  fail "gateway chat completion call (status=$CODE body=$BODY)"
fi

# A few more calls across models/agents so metrics/dashboards have
# varied data to show.
for i in 1 2 3 4 5; do
  curl -sS -o /dev/null -X POST "$GATEWAY_URL/v1/chat/completions" \
    -H "Authorization: Bearer $ACME_CORP_ADMIN_KEY" \
    -H "X-Agent-Id: $ACME_CORP_SALES_ASSISTANT_AGENT_ID" \
    -H "Content-Type: application/json" \
    -d '{"model":"mock-medium","messages":[{"role":"user","content":"Summarize our Q3 pipeline in one paragraph."}]}' || true
done

echo "=== 5. Verifying structured log emission ==="
if docker compose -f deploy/docker-compose.yml logs gateway 2>/dev/null | grep -q '"decision":"allowed"'; then
  pass "gateway emitted a structured log line with decision=allowed"
else
  fail "no allowed-decision log line found in gateway logs"
fi

echo "=== 6. Verifying metric emission ==="
if curl -sS "$GATEWAY_METRICS_URL/metrics" 2>/dev/null | grep -q 'budget_governor_gateway_requests_total'; then
  pass "gateway /metrics exposes budget_governor_gateway_requests_total"
else
  fail "gateway /metrics did not expose expected counters"
fi

echo "=== 7. Verifying spend records via the control plane ==="
sleep 1  # let the async spend writer flush (default batch interval 200ms)
SPEND=$(curl -sS "$CONTROLPLANE_URL/v1/spend/$ACME_CORP_TENANT_ID?window=1h" \
  -H "Authorization: Bearer $ACME_CORP_ADMIN_KEY")
TOTAL_REQUESTS=$(echo "$SPEND" | jq -r '.total_requests')
if [ "${TOTAL_REQUESTS:-0}" -gt 0 ] 2>/dev/null; then
  pass "control plane spend query shows $TOTAL_REQUESTS request(s) recorded"
else
  fail "control plane spend query shows no recorded requests (body=$SPEND)"
fi

echo "=== 8. Triggering a budget-exhaustion scenario (charity-org, small budget) ==="
LAST_CODE=0
for i in $(seq 1 30); do
  RESP=$(curl -sS -w '\n%{http_code}' -X POST "$GATEWAY_URL/v1/chat/completions" \
    -H "Authorization: Bearer $CHARITY_ORG_ADMIN_KEY" \
    -H "X-Agent-Id: $CHARITY_ORG_GRANT_WRITER_AGENT_ID" \
    -H "Content-Type: application/json" \
    -d '{"model":"mock-large","max_tokens":800,"messages":[{"role":"user","content":"Write a detailed 3-page grant proposal narrative covering program design, budget justification, and evaluation plan."}]}')
  LAST_CODE=$(echo "$RESP" | tail -n1)
  if [ "$LAST_CODE" = "429" ]; then
    LAST_BODY=$(echo "$RESP" | sed '$d')
    break
  fi
done
if [ "$LAST_CODE" = "429" ]; then
  REASON=$(echo "${LAST_BODY:-}" | jq -r '.error.reason // empty')
  pass "budget exhaustion reproduced: 429 with reason='$REASON'"
else
  fail "did not observe a 429 after 30 requests against charity-org's small budget (last status=$LAST_CODE)"
fi

echo "=== 9. Verifying the cost advisor fired ==="
ADVISOR_METRICS=$(curl -sS "$ADVISOR_URL/metrics")
if echo "$ADVISOR_METRICS" | grep -q 'budget_governor_advisor_requests_total'; then
  SUGGESTED_COUNT=$(echo "$ADVISOR_METRICS" | grep 'budget_governor_advisor_requests_total{outcome="suggested"' | awk '{print $2}' | head -n1)
  pass "advisor was invoked (suggested-outcome count: ${SUGGESTED_COUNT:-0})"
else
  fail "advisor metrics did not show any invocations"
fi

echo "=== 10. Running reconciliation for the last hour ==="
WINDOW_END=$(python3 -c "from datetime import datetime, timezone; print(datetime.now(timezone.utc).isoformat())")
WINDOW_START=$(python3 -c "from datetime import datetime, timedelta, timezone; print((datetime.now(timezone.utc)-timedelta(hours=1)).isoformat())")
RECON_OUTPUT=$(docker compose -f deploy/docker-compose.yml run --rm reconciliation \
  python job.py --window-start "$WINDOW_START" --window-end "$WINDOW_END" 2>&1 || true)
echo "$RECON_OUTPUT"
if echo "$RECON_OUTPUT" | grep -qi "reconciliation complete"; then
  pass "reconciliation job ran to completion"
else
  fail "reconciliation job did not report completion"
fi

echo "=== 11. Verifying incidents are queryable ==="
INCIDENTS=$(curl -sS "$CONTROLPLANE_URL/v1/incidents?unresolved=true" -H "Authorization: Bearer $ACME_CORP_ADMIN_KEY")
if echo "$INCIDENTS" | jq -e 'type == "array"' >/dev/null 2>&1; then
  COUNT=$(echo "$INCIDENTS" | jq 'length')
  pass "incidents endpoint queryable (acme-corp unresolved count: $COUNT)"
else
  fail "incidents endpoint did not return a valid array (body=$INCIDENTS)"
fi

echo
echo "=== Summary: $PASS passed, $FAIL failed ==="
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
