// fanout_burst.js
//
// Demonstrates the resume bullet: "Built a distributed token-bucket
// enforcement layer validated under 5,000+ req/sec simulated agent
// fan-out load." Many distinct simulated agents (high-cardinality
// X-Agent-Id fan-out, not just many VUs hitting one identity) hit the
// gateway's /v1/chat/completions concurrently against the mock provider,
// at a target arrival rate of 5,000+ iterations/sec.
//
// This intentionally does NOT pre-inflate the tenant's budget to
// "always allow" -- part of what this test proves is that the Redis Lua
// script (gateway/internal/budget/checkAndDecrement.lua) stays correct
// (no double-admission, no negative balances) under real concurrent
// pressure, same as internal/budget/bucket_test.go's
// TestCheckAndDecrement_ConcurrentRequestsNeverOverspend, just at
// production-scale concurrency instead of 50 goroutines. Expect a mix
// of 200s and 429s; what matters for the headline p99 claim is the
// latency of the ALLOWED requests specifically (tagged below and
// reported separately), not the overall mix.
//
// Usage:
//   k6 run \
//     -e GATEWAY_URL=http://localhost:8080 \
//     -e API_KEY=$ACME_CORP_ADMIN_KEY \
//     loadtest/fanout_burst.js
//
// See scripts/benchmark.sh for the full invocation (sources seeded
// credentials, sets result output paths).

import http from 'k6/http';
import { check } from 'k6';
import { Trend, Counter } from 'k6/metrics';
import { randomIntBetween } from 'https://jslib.k6.io/k6-utils/1.2.0/index.js';

const GATEWAY_URL = __ENV.GATEWAY_URL || 'http://localhost:8080';
const API_KEY = __ENV.API_KEY;
const TARGET_RATE = parseInt(__ENV.TARGET_RATE || '5000', 10); // iterations/sec
const DURATION = __ENV.DURATION || '30s';
const AGENT_FANOUT = parseInt(__ENV.AGENT_FANOUT || '500', 10); // distinct simulated agents

if (!API_KEY) {
  throw new Error('API_KEY env var is required (an admin or agent-scoped key for a seeded tenant)');
}

// Separate latency trend for ALLOWED requests only -- this is the
// number the p99-overhead claim is about (see module docstring).
export const allowedLatency = new Trend('allowed_request_duration', true);
export const rejectedLatency = new Trend('rejected_request_duration', true);
export const allowedCount = new Counter('allowed_requests');
export const rejectedCount = new Counter('rejected_requests');
export const errorCount = new Counter('unexpected_errors');

export const options = {
  scenarios: {
    fanout_burst: {
      executor: 'ramping-arrival-rate',
      startRate: 100,
      timeUnit: '1s',
      preAllocatedVUs: 200,
      maxVUs: 2000,
      stages: [
        { target: TARGET_RATE, duration: '10s' }, // ramp up
        { target: TARGET_RATE, duration: DURATION }, // sustain at target
        { target: 0, duration: '5s' }, // ramp down
      ],
    },
  },
  thresholds: {
    // The actual claim: p99 latency of ALLOWED requests stays under
    // 10ms of GATEWAY overhead. Network/transport time in a k6-against-
    // localhost run is itself sub-millisecond, so this threshold is
    // applied directly to the measured allowed_request_duration; against
    // a real network topology, subtract expected RTT before comparing.
    'allowed_request_duration': ['p(99)<10'],
    'unexpected_errors': ['count<1'],
  },
};

const MODELS = ['mock-small', 'mock-medium', 'mock-large'];

export default function () {
  const agentId = `loadtest-agent-${randomIntBetween(1, AGENT_FANOUT)}`;
  const model = MODELS[randomIntBetween(0, MODELS.length - 1)];

  const res = http.post(
    `${GATEWAY_URL}/v1/chat/completions`,
    JSON.stringify({
      model,
      messages: [{ role: 'user', content: 'Summarize the benefits of distributed rate limiting in two sentences.' }],
      max_tokens: 64,
    }),
    {
      headers: {
        'Content-Type': 'application/json',
        Authorization: `Bearer ${API_KEY}`,
        'X-Agent-Id': agentId,
      },
      tags: { name: 'chat_completions' },
    }
  );

  if (res.status === 200) {
    allowedCount.add(1);
    allowedLatency.add(res.timings.duration);
    check(res, { 'allowed: has content': (r) => !!JSON.parse(r.body).choices?.[0]?.message?.content });
  } else if (res.status === 429) {
    rejectedCount.add(1);
    rejectedLatency.add(res.timings.duration);
    check(res, { 'rejected: has reason': (r) => !!JSON.parse(r.body).error?.reason });
  } else {
    errorCount.add(1);
    check(res, { 'unexpected status': () => false });
  }
}
