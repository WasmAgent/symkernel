// k6 cold-start probe for POST /v1/verify/z3.
//
// Issues a small number of SEQUENTIAL requests from a single VU and records
// each request's duration. When the container has scaled to zero (idle for
// longer than its sleepAfter), the FIRST request pays the cold-start cost
// and dominates the max; subsequent requests are warm. Subtracting the warm
// median from the max yields the cold-start overhead.
//
// The threshold encodes the M3 decision gate (docs/15-milestones.md): fail
// if any request (i.e. the cold one) exceeds 5s.
//
// Usage:
//   1. Ensure the container is stopped: wait longer than sleepAfter (5m,
//      see deploy/worker/index.js) or stop it via the Cloudflare dashboard
//      / `wrangler containers` CLI.
//   2. export SYMKERNEL_BASE_URL=https://symkernel.YOUR-SUBDOMAIN.workers.dev
//      export SYMKERNEL_CLIENT_TOKEN=<token>
//   3. k6 run bench/k6/z3-cold-start.js
//
// See bench/load-test-infra.md.

import http from 'k6/http';
import { check, sleep } from 'k6';
import { Trend } from 'k6/metrics';

const baseUrl = __ENV.SYMKERNEL_BASE_URL || 'http://localhost:8080';
const token = __ENV.SYMKERNEL_CLIENT_TOKEN || '';

const requestDuration = new Trend('request_duration', true);

export const options = {
  // One VU, sequential iterations: iteration 0 is cold, the rest are warm.
  vus: 1,
  iterations: 5,
  thresholds: {
    // Decision gate: cold start (the max) must stay under 5s.
    request_duration: ['max<5000'],
  },
};

const PAYLOAD = JSON.stringify({
  input: {
    constraints_smt2: '(declare-const x Int) (assert (> x 5))',
    timeout_ms: 2000,
  },
});

const PARAMS = {
  headers: {
    'Content-Type': 'application/json',
    Authorization: `Bearer ${token}`,
  },
};

export default function () {
  const res = http.post(`${baseUrl}/v1/verify/z3`, PAYLOAD, PARAMS);
  requestDuration.add(res.timings.duration);
  check(res, { 'status is 200': (r) => r.status === 200 });
  // Brief pause keeps the instance warm between iterations without
  // resetting the idle timer so aggressively that it never sleeps.
  sleep(0.5);
}
