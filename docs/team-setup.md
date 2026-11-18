# Running Budget Governor for your team

You have one company Anthropic (or OpenAI) key and a few people running AI
agents. This page sets it up so each person has their own cap, nobody can
spend from anyone else's, and you hear about it before a cap is hit.

## The model, in one picture

```
                 your server (you run this once)
              ┌──────────────────────────────────────┐
 Bob's agent ─┤                                      │
 Sarah's    ──┤  Gateway: whose key is this?         ├──> Anthropic / OpenAI
 Kyle's     ──┤  is their cap and the team cap OK?   │    (the one real key
              │  yes -> forward. no -> 429.          │     lives only here)
              └──────────────────────────────────────┘
```

| Word in the code | What it means for you |
|---|---|
| tenant | your team |
| agent | one member (a person, or one bot you want capped separately) |
| task | optional, a sub-cap inside one member (`X-Task-Id` header) |

Two caps apply to every call: the member's own, and the team's. Whichever
runs out first blocks the call.

## 1. Run the stack and put the real key on it

```bash
cp .env.example .env      # set ANTHROPIC_API_KEY and/or OPENAI_API_KEY
make up
```

Members never see this key. Change `PLATFORM_BOOTSTRAP_TOKEN` in `.env`
before exposing the server to anyone.

## 2. Create the team and its members

```bash
python3 scripts/team.py init my-team --daily 100
# copy the two export lines it prints (keep the admin key private)

python3 scripts/team.py add bob   --daily 25
python3 scripts/team.py add sarah --daily 25
python3 scripts/team.py add kyle  --daily 25
```

Each `add` prints that member's key and setup lines. The key is shown once.

Caps are a bucket that refills evenly over 24 hours: someone who burns
their whole $25 by 9am gets about $1 back each hour, rather than nothing
until midnight. When a member is close to their cap, the gateway may switch
a call to a cheaper model (the response carries `budget_advisory`) before
it starts returning 429.

## 3. What each member does

Point their agent's OpenAI-compatible client at the gateway with their own
key:

```bash
export OPENAI_BASE_URL=https://your-gateway/v1
export OPENAI_API_KEY=<their bg_live_ key>
```

They can use Claude models through it: send `"model": "claude-sonnet-5"`
(or any `claude-*`) and the gateway routes it to Anthropic.

**Limit:** the gateway speaks the OpenAI chat-completions format only
(`POST /v1/chat/completions`, with streaming). Tools that only talk
Anthropic's own Messages API (`/v1/messages`) cannot point at it yet.

Their key always spends as them. Setting `X-Agent-Id` to someone else
gets a 403 `agent_mismatch`; leaving it out is fine.

## 4. Day to day

```bash
python3 scripts/team.py list               # caps and last-24h spend
python3 scripts/team.py cap bob --daily 40 # raise or lower a cap
python3 scripts/team.py cap --daily 150    # the team cap
python3 scripts/team.py rotate bob         # lost key: revoke + reissue
python3 scripts/team.py remove kyle        # left the team: revoke keys
```

Changes reach the gateway within `BUDGET_CACHE_TTL_SECONDS`; a revoked key
stops working within `AUTH_CACHE_TTL_SECONDS`. Removed members keep their
spend history.

Grafana (http://localhost:3000) has the charts.

## 5. Get alerted in Slack

Create a Slack incoming webhook and save its URL to
`observability/alertmanager/slack_webhook` (gitignored), then
`make up` again. You will get `MemberNearDailyCap` when anyone (or the team
as a whole) passes 80% of their daily cap, plus the existing burn-rate,
drift and gateway-health alerts. Without the file, alerts still show at
http://localhost:9093.

## Known gaps

* There is no web admin page; `team.py` and Grafana are the interface.
* On the AWS deploy, spend is written to DynamoDB, while `team.py list`, the
  control plane's spend API and the Slack alert read Postgres. Until that
  is reconciled, those show $0 on AWS (caps are still enforced).
* Caps below about $1/day refill imprecisely (the refill rate is stored with
  4 decimal places).
