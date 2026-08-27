-- checkAndDecrement.lua
--
-- Atomically evaluates and, if all pass, decrements every applicable
-- budget bucket (task -> agent -> tenant, most-specific first) for a
-- single LLM call, in one Redis round trip. Either every bucket in KEYS
-- is decremented, or none is -- there is no partial-decrement state a
-- concurrent request could observe.
--
-- Why lazy refill computed here instead of a background refill worker:
-- a worker would need to tick every bucket in existence on a fixed
-- schedule regardless of traffic, and would be one more thing that can
-- fall behind/crash without anyone noticing until budgets look wrong.
-- Computing "tokens_remaining as of right now" from last_refill_ts on
-- every check is O(1), needs no scheduler, and is self-correcting -- a
-- bucket nobody has touched in an hour just quietly refills to full the
-- next time anyone asks, without a worker having "wasted" 3600 ticks on
-- it.
--
-- KEYS[i]  = bucket state hash key, i = 1..N, most-specific scope first
--            (e.g. task, then agent, then tenant -- whichever exist).
--            Hash fields: tokens_remaining, usd_remaining, last_refill_ts
-- ARGV[1]  = requested_tokens (integer, pre-flight estimate)
-- ARGV[2]  = requested_usd (number, 0 if the model is unpriced)
-- ARGV[3]  = state_ttl_seconds (EXPIRE applied to touched bucket keys so
--            buckets for deleted tenants/agents don't accumulate forever)
-- ARGV[3 + 4*(i-1) + 1] = token_limit for KEYS[i]      (0 = not enforced)
-- ARGV[3 + 4*(i-1) + 2] = usd_limit for KEYS[i]         (0 = not enforced)
-- ARGV[3 + 4*(i-1) + 3] = refill_tokens_per_sec for KEYS[i]
-- ARGV[3 + 4*(i-1) + 4] = refill_usd_per_sec for KEYS[i]
--
-- Returns a flat array:
--   {1, 0, min_remaining_tokens, min_remaining_usd}                 on success
--   {0, failing_index, remaining_tokens_at_failure, remaining_usd_at_failure}
--                                                                    on rejection
-- failing_index is 1-based into KEYS, indicating which scope rejected;
-- the Go caller maps that back to over_task_budget / over_agent_budget /
-- over_tenant_budget using the same order it built KEYS with. A
-- rejection purely on requested_tokens/requested_usd being negative
-- (caller bug) returns failing_index = -1.

local requested_tokens = tonumber(ARGV[1])
local requested_usd = tonumber(ARGV[2])
local state_ttl = tonumber(ARGV[3])

if requested_tokens < 0 or requested_usd < 0 then
  return {0, -1, 0, 0}
end

local time_result = redis.call('TIME')
local now = tonumber(time_result[1]) + (tonumber(time_result[2]) / 1000000)

local n = #KEYS
local computed_tokens = {}
local computed_usd = {}
local token_limits = {}
local usd_limits = {}

-- Phase 1: compute post-refill balances for every bucket without
-- mutating anything yet.
for i = 1, n do
  local base = 3 + 4 * (i - 1)
  local token_limit = tonumber(ARGV[base + 1])
  local usd_limit = tonumber(ARGV[base + 2])
  local refill_tokens_per_sec = tonumber(ARGV[base + 3])
  local refill_usd_per_sec = tonumber(ARGV[base + 4])

  token_limits[i] = token_limit
  usd_limits[i] = usd_limit

  local state = redis.call('HMGET', KEYS[i], 'tokens_remaining', 'usd_remaining', 'last_refill_ts')
  local tokens_remaining = tonumber(state[1])
  local usd_remaining = tonumber(state[2])
  local last_refill_ts = tonumber(state[3])

  if tokens_remaining == nil then
    tokens_remaining = token_limit
  end
  if usd_remaining == nil then
    usd_remaining = usd_limit
  end
  if last_refill_ts == nil then
    last_refill_ts = now
  end

  local elapsed = now - last_refill_ts
  if elapsed < 0 then
    elapsed = 0
  end

  local new_tokens = tokens_remaining
  if token_limit > 0 then
    new_tokens = tokens_remaining + elapsed * refill_tokens_per_sec
    if new_tokens > token_limit then
      new_tokens = token_limit
    end
  end

  local new_usd = usd_remaining
  if usd_limit > 0 then
    new_usd = usd_remaining + elapsed * refill_usd_per_sec
    if new_usd > usd_limit then
      new_usd = usd_limit
    end
  end

  computed_tokens[i] = new_tokens
  computed_usd[i] = new_usd
end

-- Phase 2: check sufficiency across all buckets before committing any
-- write. First insufficient bucket wins (most-specific scope is
-- KEYS[1], so a task-level exhaustion is reported before a tenant-level
-- one even if both would fail).
for i = 1, n do
  if token_limits[i] > 0 and computed_tokens[i] < requested_tokens then
    return {0, i, computed_tokens[i], computed_usd[i]}
  end
  if usd_limits[i] > 0 and computed_usd[i] < requested_usd then
    return {0, i, computed_tokens[i], computed_usd[i]}
  end
end

-- Phase 3: all buckets have sufficient budget -- commit the decrement.
local min_remaining_tokens = nil
local min_remaining_usd = nil

for i = 1, n do
  local final_tokens = computed_tokens[i] - requested_tokens
  local final_usd = computed_usd[i] - requested_usd

  redis.call('HSET', KEYS[i],
    'tokens_remaining', tostring(final_tokens),
    'usd_remaining', tostring(final_usd),
    'last_refill_ts', tostring(now))
  if state_ttl > 0 then
    redis.call('EXPIRE', KEYS[i], state_ttl)
  end

  if token_limits[i] > 0 and (min_remaining_tokens == nil or final_tokens < min_remaining_tokens) then
    min_remaining_tokens = final_tokens
  end
  if usd_limits[i] > 0 and (min_remaining_usd == nil or final_usd < min_remaining_usd) then
    min_remaining_usd = final_usd
  end
end

if min_remaining_tokens == nil then
  min_remaining_tokens = 0
end
if min_remaining_usd == nil then
  min_remaining_usd = 0
end

return {1, 0, min_remaining_tokens, min_remaining_usd}
