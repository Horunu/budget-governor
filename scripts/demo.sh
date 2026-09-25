#!/usr/bin/env bash
# Walks one agent through the whole control loop: provision a tight budget,
# spend it, get throttled with advice, retry on a cheaper model, then read
# back the spend.
#
#   scripts/demo.sh           stack already running
#   scripts/demo.sh --up      start the stack first
#   scripts/demo.sh --down    stop the stack and drop volumes afterwards
#
# Needs docker, curl and jq.
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/.."

GATEWAY_URL="${GATEWAY_URL:-http://localhost:8080}"
CONTROLPLANE_URL="${CONTROLPLANE_URL:-http://localhost:8081}"
PLATFORM_TOKEN="${PLATFORM_BOOTSTRAP_TOKEN:-dev-only-change-me}"
COMPOSE="docker compose -f deploy/docker-compose.yml"

if [ -t 1 ]; then
  GRN=$'\033[32m' YLW=$'\033[33m' RED=$'\033[31m' BLD=$'\033[1m' RST=$'\033[0m'
else
  GRN='' YLW='' RED='' BLD='' RST=''
fi
step() { printf '\n%s==> %s%s\n' "$BLD" "$*" "$RST"; }
ok()   { printf '  %sok%s    %s\n' "$GRN" "$RST" "$*"; }
warn() { printf '  %swarn%s  %s\n' "$YLW" "$RST" "$*"; }
fail() { printf '  %sfail%s  %s\n' "$RED" "$RST" "$*"; }

up=false down=false
for arg in "$@"; do
  case "$arg" in
    --up) up=true ;;
    --down) down=true ;;
    *) echo "unknown option: $arg" >&2; exit 2 ;;
  esac
done

# Prints the response body, then the status code on its own line.
chat() {
  local model=$1 max_tokens=$2
  curl -s -w '\n%{http_code}' -X POST "$GATEWAY_URL/v1/chat/completions" \
    -H "Content-Type: application/json" \
    -H "Authorization: Bearer $ADMIN_KEY" \
    -H "X-Agent-Id: $AGENT_ID" \
    -d "{\"model\":\"$model\",\"max_tokens\":$max_tokens,\"messages\":[{\"role\":\"user\",\"content\":\"What is 2+2?\"}]}"
}

if $up; then
  step "Starting stack"
  $COMPOSE up -d --build
  printf '  waiting for healthchecks'
  for _ in $(seq 1 60); do
    # compose prints one JSON object per line; -s collects them.
    if $COMPOSE ps --format json 2>/dev/null \
        | jq -se 'flatten | map(select(.Health != "" and .Health != "healthy")) | length == 0' >/dev/null 2>&1; then
      break
    fi
    printf '.'
    sleep 2
  done
  printf '\n'
fi

step "1. Stack health"
for svc in "gateway $GATEWAY_URL/healthz" "controlplane $CONTROLPLANE_URL/v1/health"; do
  read -r name url <<<"$svc"
  if curl -sf "$url" >/dev/null; then
    ok "$name is up"
  else
    fail "$name not reachable at $url (is the stack running? try --up)"
    exit 1
  fi
done

step "2. Provision tenant, agent and budgets"
TENANT_ID=$(curl -sf -X POST "$CONTROLPLANE_URL/v1/tenants" \
  -H "Content-Type: application/json" \
  -H "X-Platform-Token: $PLATFORM_TOKEN" \
  -d "{\"name\":\"demo-tenant-$(date +%s)\"}" | jq -r '.id')
ok "tenant       $TENANT_ID"

ADMIN_KEY=$(curl -sf -X POST "$CONTROLPLANE_URL/v1/tenants/$TENANT_ID/api-keys" \
  -H "Content-Type: application/json" \
  -H "X-Platform-Token: $PLATFORM_TOKEN" \
  -d '{"scopes":["admin"],"label":"demo-admin"}' | jq -r '.api_key')
ok "admin key    ${ADMIN_KEY:0:16}..."

# The tenant limit is set high so the agent limit is the one that binds.
curl -sf -X POST "$CONTROLPLANE_URL/v1/budgets/$TENANT_ID" \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer $ADMIN_KEY" \
  -d '{"token_limit":10000000,"refill_rate_per_min":0}' >/dev/null
ok "tenant budget 10,000,000 tokens"

AGENT_ID=$(curl -sf -X POST "$CONTROLPLANE_URL/v1/agents" \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer $ADMIN_KEY" \
  -d '{"name":"demo-agent"}' | jq -r '.id')
ok "agent        $AGENT_ID"

curl -sf -X POST "$CONTROLPLANE_URL/v1/budgets/$TENANT_ID" \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer $ADMIN_KEY" \
  -d "{\"agent_id\":\"$AGENT_ID\",\"token_limit\":500,\"refill_rate_per_min\":0}" >/dev/null
ok "agent budget 500 tokens, no refill"

# The gateway only sees new budgets after its config cache refreshes.
printf '  waiting for gateway config cache'
for _ in 1 2 3 4 5 6; do sleep 1; printf '.'; done
printf '\n'

step "3. Four mock-large calls (113 tokens reserved each)"
for i in 1 2 3 4; do
  resp=$(chat mock-large 100)
  code=$(tail -n1 <<<"$resp")
  body=$(sed '$d' <<<"$resp")
  if [ "$code" = 200 ]; then
    ok "call $i  200  model=$(jq -r '.model' <<<"$body")  cost_usd=$(jq -r '.cost_usd' <<<"$body")"
  else
    fail "call $i  $code  $body"
  fi
done

step "4. Fifth call needs 113 tokens, 112 left"
resp=$(chat mock-large 100)
code=$(tail -n1 <<<"$resp")
body=$(sed '$d' <<<"$resp")
if [ "$code" = 429 ]; then
  warn "call 5  429"
  printf '        reason     %s\n' "$(jq -r '.error.reason' <<<"$body")"
  printf '        remaining  %s tokens\n' "$(jq -r '.error.remaining_tokens' <<<"$body")"
  printf '        advice     %s\n' "$(jq -r '.error.advice // "none"' <<<"$body")"
else
  fail "expected 429, got $code"
  jq . <<<"$body"
fi

step "5. Retry on mock-small with max_tokens=50"
resp=$(chat mock-small 50)
code=$(tail -n1 <<<"$resp")
body=$(sed '$d' <<<"$resp")
if [ "$code" = 200 ]; then
  ok "retry   200  model=$(jq -r '.model' <<<"$body")"
  printf '        %s\n' "$(jq -r '.choices[0].message.content' <<<"$body" | cut -c1-72)"
else
  fail "retry   $code  $body"
fi

step "6. Spend for this tenant"
sleep 1  # spend writer flushes every 200ms
spend=$(curl -sf "$CONTROLPLANE_URL/v1/spend/$TENANT_ID?window=1h" \
  -H "Authorization: Bearer $ADMIN_KEY")
jq -r '"  requests    \(.total_requests)",
       "  tokens in   \(.total_tokens_in)",
       "  tokens out  \(.total_tokens_out)",
       "  cost usd    \(.total_cost_usd)",
       (.by_model[] | "  \(.model)  \(.request_count) req, \(.tokens_in + .tokens_out) tokens")' <<<"$spend"

cat <<EOF

Done. The stack is still up:
  gateway        $GATEWAY_URL
  control plane  $CONTROLPLANE_URL
  grafana        http://localhost:3000  (admin/admin)
  prometheus     http://localhost:9091
EOF

if $down; then
  step "Stopping stack"
  $COMPOSE down -v
fi
