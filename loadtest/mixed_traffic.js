// mixed_traffic.js
//
// A more realistic sustained workload than fanout_burst.js's pure-burst
// scenario: a moderate constant request rate, a mix of streaming and
// non-streaming calls, varied token sizes, and occasional intentionally
// oversized requests (to naturally exercise the budget-pressure/advisor
// path rather than only the steady-state allowed path). Useful for
// soak-testing and for generating realistic-looking data in the Grafana
// dashboards during a demo.
//
// Usage:
//   k6 run \
//     -e GATEWAY_URL=http://localhost:8080 \
//     -e API_KEY=$ACME_CORP_ADMIN_KEY \
//     loadtest/mixed_traffic.js

import http from 'k6/http';
import { check, sleep } from 'k6';
import { randomIntBetween } from 'https://jslib.k6.io/k6-utils/1.2.0/index.js';

const GATEWAY_URL = __ENV.GATEWAY_URL || 'http://localhost:8080';
const API_KEY = __ENV.API_KEY;
const RATE = parseInt(__ENV.RATE || '50', 10); // iterations/sec
const DURATION = __ENV.DURATION || '2m';

if (!API_KEY) {
  throw new Error('API_KEY env var is required (an admin or agent-scoped key for a seeded tenant)');
}

export const options = {
  scenarios: {
    mixed_traffic: {
      executor: 'constant-arrival-rate',
      rate: RATE,
      timeUnit: '1s',
      duration: DURATION,
      preAllocatedVUs: Math.max(20, Math.ceil(RATE / 2)),
      maxVUs: RATE * 4,
    },
  },
};

const AGENTS = ['support-bot', 'billing-summarizer', 'sales-assistant', 'onboarding-bot'];
const MODELS = ['mock-small', 'mock-medium', 'mock-large'];

const PROMPTS = [
  'What is the capital of France?',
  'Draft a short follow-up email to a customer who has not responded in two weeks.',
  'Explain the CAP theorem to a new engineer in three sentences.',
  'List five ways to reduce cloud infrastructure costs.',
  'Summarize the key risks in launching a new SaaS product.',
];

export default function () {
  const agentId = AGENTS[randomIntBetween(0, AGENTS.length - 1)];
  const model = MODELS[randomIntBetween(0, MODELS.length - 1)];
  const prompt = PROMPTS[randomIntBetween(0, PROMPTS.length - 1)];
  // 1-in-20 requests deliberately asks for a large completion, to
  // occasionally trigger budget pressure / the advisor path under
  // otherwise-normal traffic.
  const isOversized = randomIntBetween(1, 20) === 1;

  const stream = randomIntBetween(0, 4) === 0; // ~20% streaming

  const res = http.post(
    `${GATEWAY_URL}/v1/chat/completions`,
    JSON.stringify({
      model,
      messages: [{ role: 'user', content: prompt }],
      max_tokens: isOversized ? 4000 : randomIntBetween(32, 256),
      stream,
    }),
    {
      headers: {
        'Content-Type': 'application/json',
        Authorization: `Bearer ${API_KEY}`,
        'X-Agent-Id': agentId,
      },
      tags: { name: 'chat_completions', oversized: String(isOversized), stream: String(stream) },
    }
  );

  check(res, {
    'status is 200 or 429': (r) => r.status === 200 || r.status === 429,
  });

  sleep(randomIntBetween(0, 200) / 1000);
}
