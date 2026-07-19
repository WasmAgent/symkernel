// k6 load test for POST /v1/verify/z3.
//
// Drives 10 concurrent virtual users against the Z3 endpoint and reports
// p99 request latency (k6's built-in http_req_duration metric). The
// threshold encodes the M3 decision gate (docs/15-milestones.md): fail if
// p99 > 2s, which triggers documenting a VPS migration path.
//
// Usage:
//   export SYMKERNEL_BASE_URL=https://symkernel.YOUR-SUBDOMAIN.workers.dev
//   export SYMKERNEL_CLIENT_TOKEN=<token>
//   k6 run --vus 10 --duration 30s bench/k6/z3-load.js
//
// Defaults (10 VUs / 30s) satisfy "simulate 10+ concurrent Z3 requests".
// Override with --vus / --duration. See bench/load-test-infra.md.

import http from 'k6/http';
import { check } from 'k6';
import { Trend } from 'k6/metrics';

const baseUrl = __ENV.SYMKERNEL_BASE_URL || 'http://localhost:8080';
const token = __ENV.SYMKERNEL_CLIENT_TOKEN || '';

// Explicit trend for the Z3 solve round-trip; k6 also reports
// http_req_duration (which carries p(99)) automatically.
const z3SolveDuration = new Trend('z3_solve_duration', true);

export const options = {
  vus: 10,
  duration: '30s',
  thresholds: {
    // Decision gate: p99 must stay under 2s.
    http_req_duration: ['p(99)<2000'],
    // No more than 1% failed requests.
    http_req_failed: ['rate<0.01'],
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
  z3SolveDuration.add(res.timings.duration);
  check(res, {
    'status is 200': (r) => r.status === 200,
    'result.sat present': (r) => {
      try {
        return JSON.parse(r.body).result.sat !== undefined;
      } catch {
        return false;
      }
    },
  });
}
