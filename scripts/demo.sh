#!/usr/bin/env bash
# Budget Governor — 30-second end-to-end demo.
#
# Shows the full control loop: provision → allow → budget exhaust → 429
# with advisor suggestion → policy reroute to cheaper model → spend query.
#
# Usage:
#   bash scripts/demo.sh          # assumes stack is already up
#   bash scripts/demo.sh --up     # start the stack first, then demo
#   bash scripts/demo.sh --down   # tear down the stack after the demo
#   bash scripts/demo.sh --up --down
#
# Requires: docker, curl, jq.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

GATEWAY_URL="${GATEWAY_URL:-http://localhost:8080}"
CONTROLPLANE_URL="${CONTROLPLANE_URL:-http://localhost:8081}"
PLATFORM_TOKEN="${PLATFORM_BOOTSTRAP_TOKEN:-dev-only-change-me}"
COMPOSE="docker compose -f deploy/docker-compose.yml"

# ── ANSI colours ────────────────────────────────────────────────────────────
GRN='\033[0;32m'; YLW='\033[0;33m'; RED='\033[0;31m'; CYN='\033[0;36m'
BLD='\033[1m'; RST='\033[0m'
ok()   { printf "${GRN}✓${RST}  %s\n" "$*"; }
warn() { printf "${YLW}⚠${RST}  %s\n" "$*"; }
err()  { printf "${RED}✗${RST}  %s\n" "$*"; }
hdr()  { printf "\n${BLD}${CYN}══ %s ══${RST}\n" "$*"; }
# ────────────────────────────────────────────────────────────────────────────

BRING_UP=false
TEAR_DOWN=false
for arg in "$@"; do
  case "$arg" in
    --up)   BRING_UP=true ;;
    --down) TEAR_DOWN=true ;;
  esac
done

# ── Optionally start the stack ───────────────────────────────────────────────
if $BRING_UP; then
  hdr "Starting stack"
  $COMPOSE up -d --build
  printf "  Waiting for all services to be healthy"
  for _ in $(seq 1 60); do
    if $COMPOSE ps --format json 2>/dev/null \
        | jq -e '[.[] | select(.Health != "" and .Health != "healthy")] | length == 0' \
        >/dev/null 2>&1; then
      printf "\n"
      break
    fi
    printf "."
    sleep 2
  done
fi

# ── 1. Health check ──────────────────────────────────────────────────────────
hdr "1. Stack health"
for svc in "gateway:$GATEWAY_URL/healthz" "controlplane:$CONTROLPLANE_URL/v1/health"; do
  name="${svc%%:*}"; url="${svc#*:}"
  if curl -sf "$url" >/dev/null 2>&1; then
    ok "$name is up"
  else
    err "$name not reachable at $url — is the stack running? (try --up)"
    exit 1
  fi
done

# ── 2. Provision demo tenant + agent + budget ────────────────────────────────
hdr "2. Provisioning demo tenant, agent, and budget"

TENANT=$(curl -sf -X POST "$CONTROLPLANE_URL/v1/tenants" \
  -H "Content-Type: application/json" \
  -H "X-Platform-Token: $PLATFORM_TOKEN" \
  -d '{"name":"demo-tenant-'"$(date +%s)"'"}')
TENANT_ID=$(echo "$TENANT" | jq -r '.id')
ok "tenant created  id=$TENANT_ID"

ADMIN_KEY_RESP=$(curl -sf -X POST "$CONTROLPLANE_URL/v1/tenants/$TENANT_ID/api-keys" \
  -H "Content-Type: application/json" \
  -H "X-Platform-Token: $PLATFORM_TOKEN" \
  -d '{"scopes":["admin"],"label":"demo-admin"}')
ADMIN_KEY=$(echo "$ADMIN_KEY_RESP" | jq -r '.api_key')
ok "admin key issued  ${ADMIN_KEY:0:16}..."

# Tenant-level ceiling: large (not the binding constraint).
curl -sf -X POST "$CONTROLPLANE_URL/v1/budgets/$TENANT_ID" \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer $ADMIN_KEY" \
  -d '{"token_limit":10000000,"refill_rate_per_min":0}' >/dev/null
ok "tenant budget set  token_limit=10,000,000"

AGENT_RESP=$(curl -sf -X POST "$CONTROLPLANE_URL/v1/agents" \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer $ADMIN_KEY" \
  -d '{"name":"demo-agent"}')
AGENT_ID=$(echo "$AGENT_RESP" | jq -r '.id')
ok "agent created  id=$AGENT_ID"

# Tight agent budget: 500 tokens, no refill.
curl -sf -X POST "$CONTROLPLANE_URL/v1/budgets/$TENANT_ID" \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer $ADMIN_KEY" \
  -d "{\"agent_id\":\"$AGENT_ID\",\"token_limit\":500,\"refill_rate_per_min\":0}" >/dev/null
ok "agent budget set  token_limit=500 (tight, for demo)"

# Wait for the gateway's budget config cache to pick up the new rows.
printf "  Waiting 6s for gateway config-cache refresh"
for _ in $(seq 1 6); do sleep 1; printf "."; done
printf "\n"

# ── 3. Several requests succeed ──────────────────────────────────────────────
hdr "3. Sending 4 requests through the gateway (mock-large)"
printf "  Each call uses ~113 estimated tokens; budget starts at 500.\n\n"

for i in 1 2 3 4; do
  RESP=$(curl -sf -w '\n%{http_code}' -X POST "$GATEWAY_URL/v1/chat/completions" \
    -H "Content-Type: application/json" \
    -H "Authorization: Bearer $ADMIN_KEY" \
    -H "X-Agent-Id: $AGENT_ID" \
    -d '{"model":"mock-large","max_tokens":100,"messages":[{"role":"user","content":"What is 2+2?"}]}')
  CODE=$(echo "$RESP" | tail -n1)
  BODY=$(echo "$RESP" | sed '$d')
  MODEL=$(echo "$BODY" | jq -r '.model // "?"')
  COST=$(echo "$BODY"  | jq -r '.cost_usd // 0 | . * 1000000 | floor / 1000000')
  if [ "$CODE" = "200" ]; then
    ok "request $i  ${GRN}200 OK${RST}  model=$MODEL  cost_usd=$COST"
  else
    err "request $i  unexpected $CODE: $BODY"
  fi
done

# ── 4. Budget exhausts → 429 ─────────────────────────────────────────────────
hdr "4. Budget exhaustion — 5th request exceeds remaining ~112 tokens"

RESP=$(curl -s -w '\n%{http_code}' -X POST "$GATEWAY_URL/v1/chat/completions" \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer $ADMIN_KEY" \
  -H "X-Agent-Id: $AGENT_ID" \
  -d '{"model":"mock-large","max_tokens":100,"messages":[{"role":"user","content":"What is 2+2?"}]}')
CODE=$(echo "$RESP" | tail -n1)
BODY=$(echo "$RESP" | sed '$d')

if [ "$CODE" = "429" ]; then
  REASON=$(echo "$BODY" | jq -r '.error.reason')
  ADVICE=$(echo "$BODY" | jq -r '.error.advice // "(none)"')
  REMAINING=$(echo "$BODY" | jq -r '.error.remaining_tokens')
  warn "request 5  ${RED}429 Too Many Requests${RST}"
  printf "  ${YLW}reason:${RST}     %s\n"  "$REASON"
  printf "  ${YLW}remaining:${RST}  %s tokens\n" "$REMAINING"
  printf "  ${YLW}advice:${RST}     %s\n"  "$ADVICE"
  printf "  ${CYN}(advisor suggested mock-small; policy retry also exceeded budget → 429)${RST}\n"
else
  warn "expected 429 but got $CODE — budget may not have exhausted yet"
  echo "$BODY" | jq .
fi

# ── 5. Policy reroute to cheaper model ───────────────────────────────────────
hdr "5. Policy reroute — resend with mock-small (advisor suggestion)"
printf "  Using max_tokens=50 so estimate fits within remaining ~112 tokens.\n\n"

RESP=$(curl -s -w '\n%{http_code}' -X POST "$GATEWAY_URL/v1/chat/completions" \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer $ADMIN_KEY" \
  -H "X-Agent-Id: $AGENT_ID" \
  -d '{"model":"mock-small","max_tokens":50,"messages":[{"role":"user","content":"What is 2+2?"}]}')
CODE=$(echo "$RESP" | tail -n1)
BODY=$(echo "$RESP" | sed '$d')

if [ "$CODE" = "200" ]; then
  MODEL=$(echo "$BODY" | jq -r '.model')
  CONTENT=$(echo "$BODY" | jq -r '.choices[0].message.content' | cut -c1-80)
  ok "rerouted request  ${GRN}200 OK${RST}  model=$MODEL"
  printf "  ${GRN}content:${RST} %s...\n" "$CONTENT"
else
  err "policy reroute failed with $CODE: $BODY"
fi

# ── 6. Spend summary ─────────────────────────────────────────────────────────
hdr "6. Spend summary — accumulated spend_events"
sleep 1  # let the async spend writer flush (200ms batch interval)

SPEND=$(curl -sf "$CONTROLPLANE_URL/v1/spend/$TENANT_ID?window=1h" \
  -H "Authorization: Bearer $ADMIN_KEY")

TOTAL_REQ=$(echo "$SPEND"  | jq -r '.total_requests')
TOTAL_TOK_IN=$(echo "$SPEND"  | jq -r '.total_tokens_in')
TOTAL_TOK_OUT=$(echo "$SPEND" | jq -r '.total_tokens_out')
TOTAL_COST=$(echo "$SPEND"    | jq -r '.total_cost_usd')

ok "spend recorded"
printf "  ${CYN}total_requests:${RST}  %s\n"       "$TOTAL_REQ"
printf "  ${CYN}tokens_in:${RST}       %s\n"       "$TOTAL_TOK_IN"
printf "  ${CYN}tokens_out:${RST}      %s\n"       "$TOTAL_TOK_OUT"
printf "  ${CYN}total_cost_usd:${RST}  \$%s\n"     "$TOTAL_COST"
printf "\n"
echo "$SPEND" | jq '.by_model[] | "  \(.model): \(.request_count) req, \(.tokens_in+.tokens_out) tok"' -r 2>/dev/null || true

printf "\n${BLD}${GRN}Demo complete.${RST} Stack is still running — poke around at:\n"
printf "  Gateway:      %s\n"  "$GATEWAY_URL"
printf "  Control plane: %s\n" "$CONTROLPLANE_URL"
printf "  Grafana:      http://localhost:3000  (admin/admin)\n"
printf "  Prometheus:   http://localhost:9091\n\n"

# ── Optionally tear down ─────────────────────────────────────────────────────
if $TEAR_DOWN; then
  hdr "Tearing down stack"
  $COMPOSE down -v
  ok "stack stopped and volumes removed"
fi
