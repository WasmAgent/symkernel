// k6 composition fallback tail-latency benchmark.
//
// Measures the impact of fallback behaviour on tail latency (p95, p99) when
// individual tiers in a composed chain fail or time out. The test drives a
// three-tier chain (CEL → wazero → Z3) where the first tier is deliberately
// designed to fail, forcing the composition engine to evaluate subsequent tiers.
//
// Three scenarios compare fallback overhead across composition modes:
//
//   1. any_pass — skips remaining tiers on first pass; fallback fires when
//      the first tier fails.
//   2. all_pass — continues evaluating all tiers on first failure; fallback
//      must evaluate the full chain.
//   3. short_circuit — stops on first definitive result; fallback fires when
//      the first tier returns a definitive failure.
//
// Each scenario injects a "failing" CEL expression (always evaluates to false)
// to trigger fallback, then a "failing" wazero module (traps immediately) to
// force Z3 evaluation. The Z3 tier succeeds, so the overall composed result
// is pass (any_pass/short_circuit) or fail (all_pass).
//
// Trace examples are emitted for each scenario showing the full fallback path.
//
// Prerequisites:
//   - symkerneld running with compose endpoint
//   - SYMKERNEL_BASE_URL and SYMKERNEL_CLIENT_TOKEN set
//
// Usage:
//   export SYMKERNEL_BASE_URL=http://localhost:8080
//   export SYMKERNEL_CLIENT_TOKEN=<token>
//   k6 run bench/k6/composition-fallback-tail-latency.js
//
// See bench/load-test-infra.md for infrastructure details.

import http from 'k6/http';
import { check } from 'k6';
import { Trend, Rate } from 'k6/metrics';

const baseUrl = __ENV.SYMKERNEL_BASE_URL || 'http://localhost:8080';
const token = __ENV.SYMKERNEL_CLIENT_TOKEN || '';

// --- Custom metrics ---

const fallbackAnyPassDuration = new Trend('fallback_any_pass_duration', true);
const fallbackAllPassDuration = new Trend('fallback_all_pass_duration', true);
const fallbackShortCircuitDuration = new Trend('fallback_short_circuit_duration', true);

const fallbackAnyPassErrorRate = new Rate('fallback_any_pass_errors');
const fallbackAllPassErrorRate = new Rate('fallback_all_pass_errors');
const fallbackShortCircuitErrorRate = new Rate('fallback_short_circuit_errors');

// --- Shared headers ---

const PARAMS = {
  headers: {
    'Content-Type': 'application/json',
    ...(token ? { Authorization: `Bearer ${token}` } : {}),
  },
  timeout: '10s',
};

// --- Payloads ---
//
// Each payload designs a chain where early tiers fail, forcing fallback.
//
// Tier 1 (CEL): expr that evaluates to false → tier fails.
// Tier 2 (wazero): module that traps immediately → tier fails.
// Tier 3 (Z3): trivially satisfiable constraint → tier passes.

const FALLBACK_PAYLOAD = (mode) =>
  JSON.stringify({
    tiers: [
      {
        type: 'cel',
        expr: 'input.age >= 999', // Always false — triggers fallback.
      },
      {
        type: 'wazero',
        // Minimal Wasm module with an unreachable instruction (traps immediately).
        module: 'AGFzbQEAAAABBQFgAAF/AwIBAAETAQZ1bnJlYWNoAA==',
        memory_limit_mb: 64,
        timeout_ms: 500,
      },
      {
        type: 'z3',
        constraints_smt2: '(declare-const x Int) (assert (> x 5))',
        timeout_ms: 1000,
      },
    ],
    mode: mode,
    context: { age: 25 },
  });

// Normal (non-fallback) payload for baseline comparison.
const NORMAL_PAYLOAD = JSON.stringify({
  tiers: [
    {
      type: 'cel',
      expr: 'input.age >= 18',
    },
    {
      type: 'wazero',
      module: 'AGFzbQEAAAABBQFgAAF/AwIBAAcKAQZhZGQAAgABChABbXNnAGkAAQABAgME',
      memory_limit_mb: 64,
      timeout_ms: 500,
    },
    {
      type: 'z3',
      constraints_smt2: '(declare-const x Int) (assert (> x 5))',
      timeout_ms: 1000,
    },
  ],
  mode: 'any_pass',
  context: { age: 25 },
});

// --- k6 options with tail-latency thresholds ---

export const options = {
  scenarios: {
    // Baseline: normal chain where the first tier passes (no fallback).
    baseline: {
      executor: 'constant-arrival-rate',
      rate: 30,
      timeUnit: '1s',
      duration: '20s',
      preAllocatedVUs: 5,
      exec: 'baselineRequest',
    },
    // Fallback scenario 1: any_pass — first fail triggers fallback.
    fallback_any_pass: {
      executor: 'constant-arrival-rate',
      rate: 30,
      timeUnit: '1s',
      duration: '30s',
      preAllocatedVUs: 10,
      startTime: '25s',
      exec: 'fallbackAnyPass',
      thresholds: {
        fallback_any_pass_duration: ['p(95)<1500', 'p(99)<3000'],
        fallback_any_pass_errors: ['rate<0.01'],
      },
    },
    // Fallback scenario 2: all_pass — all tiers must pass.
    fallback_all_pass: {
      executor: 'constant-arrival-rate',
      rate: 30,
      timeUnit: '1s',
      duration: '30s',
      preAllocatedVUs: 10,
      startTime: '60s',
      exec: 'fallbackAllPass',
      thresholds: {
        fallback_all_pass_duration: ['p(95)<2000', 'p(99)<4000'],
        fallback_all_pass_errors: ['rate<0.01'],
      },
    },
    // Fallback scenario 3: short_circuit — stops on first definitive result.
    fallback_short_circuit: {
      executor: 'constant-arrival-rate',
      rate: 30,
      timeUnit: '1s',
      duration: '30s',
      preAllocatedVUs: 10,
      startTime: '95s',
      exec: 'fallbackShortCircuit',
      thresholds: {
        fallback_short_circuit_duration: ['p(95)<1500', 'p(99)<3000'],
        fallback_short_circuit_errors: ['rate<0.01'],
      },
    },
  },
};

// --- Helper ---

function emitTraceExample(label, res) {
  try {
    const body = JSON.parse(res.body);
    if (body.trace && body.trace.length > 0) {
      console.log(`[${label}] trace example: ${JSON.stringify(body.trace)}`);
      console.log(`[${label}] evalMs=${body.evalMs || 'n/a'} decision_id=${body.decision_id || 'n/a'}`);
      for (const entry of body.trace) {
        if (!entry.skipped) {
          console.log(`  tier ${entry.tier} (${entry.type}): status=${entry.status} evalMs=${entry.evalMs}`);
        } else {
          console.log(`  tier ${entry.tier} (${entry.type}): SKIPPED — ${entry.reason}`);
        }
      }
    }
  } catch {
    // Non-JSON — skip.
  }
}

// --- Baseline: normal chain (no fallback) ---

export function baselineRequest() {
  const res = http.post(`${baseUrl}/v1/verify/composed`, NORMAL_PAYLOAD, PARAMS);
  check(res, {
    'baseline: status is 200': (r) => r.status === 200,
    'baseline: result.ok present': (r) => {
      try { return JSON.parse(r.body).result?.ok !== undefined; } catch { return false; }
    },
  });
  if (__ITER % 50 === 0) {
    emitTraceExample('baseline', res);
  }
}

// --- Fallback any_pass ---

export function fallbackAnyPass() {
  const res = http.post(
    `${baseUrl}/v1/verify/composed`,
    FALLBACK_PAYLOAD('any_pass'),
    PARAMS,
  );
  fallbackAnyPassDuration.add(res.timings.duration);
  fallbackAnyPassErrorRate.add(res.status !== 200);
  check(res, {
    'fallback_any_pass: status is 200': (r) => r.status === 200,
    'fallback_any_pass: result present': (r) => {
      try { return JSON.parse(r.body).result !== undefined; } catch { return false; }
    },
  });
  if (__ITER % 50 === 0) {
    emitTraceExample('fallback_any_pass', res);
  }
}

// --- Fallback all_pass ---

export function fallbackAllPass() {
  const res = http.post(
    `${baseUrl}/v1/verify/composed`,
    FALLBACK_PAYLOAD('all_pass'),
    PARAMS,
  );
  fallbackAllPassDuration.add(res.timings.duration);
  fallbackAllPassErrorRate.add(res.status !== 200);
  check(res, {
    'fallback_all_pass: status is 200': (r) => r.status === 200,
    'fallback_all_pass: result present': (r) => {
      try { return JSON.parse(r.body).result !== undefined; } catch { return false; }
    },
  });
  if (__ITER % 50 === 0) {
    emitTraceExample('fallback_all_pass', res);
  }
}

// --- Fallback short_circuit ---

export function fallbackShortCircuit() {
  const res = http.post(
    `${baseUrl}/v1/verify/composed`,
    FALLBACK_PAYLOAD('short_circuit'),
    PARAMS,
  );
  fallbackShortCircuitDuration.add(res.timings.duration);
  fallbackShortCircuitErrorRate.add(res.status !== 200);
  check(res, {
    'fallback_short_circuit: status is 200': (r) => r.status === 200,
    'fallback_short_circuit: result present': (r) => {
      try { return JSON.parse(r.body).result !== undefined; } catch { return false; }
    },
  });
  if (__ITER % 50 === 0) {
    emitTraceExample('fallback_short_circuit', res);
  }
}
