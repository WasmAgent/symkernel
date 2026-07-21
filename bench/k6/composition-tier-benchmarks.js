// k6 composition tier benchmark: single-tier CEL vs. three-tier chains.
//
// Compares end-to-end latency for:
//   Scenario A — single-tier CEL only
//   Scenario B — three-tier chain (CEL → wazero → Z3) in each composition mode
//
// Each scenario runs as a separate k6 scenario with its own VU allocation and
// thresholds. The results expose the overhead of multi-tier composition relative
// to a single-tier path and the latency differences between any_pass, all_pass,
// and short_circuit modes.
//
// Trace examples are captured in the console output for each path when the
// response includes a trace array.
//
// Prerequisites:
//   - symkerneld running with the compose endpoint (POST /v1/verify/composed)
//   - SYMKERNEL_BASE_URL and SYMKERNEL_CLIENT_TOKEN set (or defaults to
//     localhost:8080 with no auth)
//
// Usage:
//   export SYMKERNEL_BASE_URL=http://localhost:8080
//   export SYMKERNEL_CLIENT_TOKEN=<token>
//   k6 run bench/k6/composition-tier-benchmarks.js
//
// See bench/load-test-infra.md for infrastructure details.

import http from 'k6/http';
import { check } from 'k6';
import { Trend, Rate } from 'k6/metrics';

const baseUrl = __ENV.SYMKERNEL_BASE_URL || 'http://localhost:8080';
const token = __ENV.SYMKERNEL_CLIENT_TOKEN || '';

// --- Custom metrics ---

// Single-tier CEL request duration.
const celOnlyDuration = new Trend('cel_only_duration', true);

// Three-tier chain duration per composition mode.
const threeTierAnyPassDuration = new Trend('three_tier_any_pass_duration', true);
const threeTierAllPassDuration = new Trend('three_tier_all_pass_duration', true);
const threeTierShortCircuitDuration = new Trend('three_tier_short_circuit_duration', true);

// Error rate per scenario.
const celOnlyErrorRate = new Rate('cel_only_errors');
const threeTierAnyPassErrorRate = new Rate('three_tier_any_pass_errors');
const threeTierAllPassErrorRate = new Rate('three_tier_all_pass_errors');
const threeTierShortCircuitErrorRate = new Rate('three_tier_short_circuit_errors');

// --- Shared headers ---

const PARAMS = {
  headers: {
    'Content-Type': 'application/json',
    ...(token ? { Authorization: `Bearer ${token}` } : {}),
  },
  timeout: '10s',
};

// --- Payloads ---

// Single-tier CEL payload.
const CEL_ONLY_PAYLOAD = JSON.stringify({
  tiers: [
    {
      type: 'cel',
      expr: 'input.age >= 18 && input.score > 50',
    },
  ],
  mode: 'any_pass',
});

// Three-tier chain payload (CEL → wazero → Z3).
// The wazero tier uses a minimal identity Wasm module (base64-encoded).
// The Z3 tier uses a trivially-satisfiable SMT2 constraint.
const THREE_TIER_ANY_PASS_PAYLOAD = JSON.stringify({
  tiers: [
    {
      type: 'cel',
      expr: 'input.age >= 18 && input.score > 50',
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
});

// Same three-tier chain with all_pass mode — all tiers must pass.
const THREE_TIER_ALL_PASS_PAYLOAD = JSON.stringify({
  tiers: [
    {
      type: 'cel',
      expr: 'input.age >= 18 && input.score > 50',
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
  mode: 'all_pass',
});

// Same three-tier chain with short_circuit mode — stops on first definitive result.
const THREE_TIER_SHORT_CIRCUIT_PAYLOAD = JSON.stringify({
  tiers: [
    {
      type: 'cel',
      expr: 'input.age >= 18 && input.score > 50',
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
  mode: 'short_circuit',
});

const CONTEXT = { age: 25, score: 78 };

// --- k6 options with per-scenario thresholds ---

export const options = {
  scenarios: {
    // Scenario A: single-tier CEL baseline.
    cel_only: {
      executor: 'constant-arrival-rate',
      rate: 50,
      timeUnit: '1s',
      duration: '30s',
      preAllocatedVUs: 10,
      exec: 'celOnly',
      thresholds: {
        cel_only_duration: ['p(95)<100', 'p(99)<200'],
        cel_only_errors: ['rate<0.01'],
      },
    },
    // Scenario B1: three-tier chain with any_pass.
    three_tier_any_pass: {
      executor: 'constant-arrival-rate',
      rate: 30,
      timeUnit: '1s',
      duration: '30s',
      preAllocatedVUs: 10,
      exec: 'threeTierAnyPass',
      thresholds: {
        three_tier_any_pass_duration: ['p(95)<500', 'p(99)<1000'],
        three_tier_any_pass_errors: ['rate<0.01'],
      },
    },
    // Scenario B2: three-tier chain with all_pass.
    three_tier_all_pass: {
      executor: 'constant-arrival-rate',
      rate: 30,
      timeUnit: '1s',
      duration: '30s',
      preAllocatedVUs: 10,
      exec: 'threeTierAllPass',
      thresholds: {
        three_tier_all_pass_duration: ['p(95)<1000', 'p(99)<2000'],
        three_tier_all_pass_errors: ['rate<0.01'],
      },
    },
    // Scenario B3: three-tier chain with short_circuit.
    three_tier_short_circuit: {
      executor: 'constant-arrival-rate',
      rate: 30,
      timeUnit: '1s',
      duration: '30s',
      preAllocatedVUs: 10,
      exec: 'threeTierShortCircuit',
      thresholds: {
        three_tier_short_circuit_duration: ['p(95)<500', 'p(99)<1000'],
        three_tier_short_circuit_errors: ['rate<0.01'],
      },
    },
  },
};

// --- Helper: emit trace example for a response ---

function emitTraceExample(label, res) {
  try {
    const body = JSON.parse(res.body);
    if (body.trace && body.trace.length > 0) {
      console.log(`[${label}] trace example: ${JSON.stringify(body.trace)}`);
      // Log per-tier timings from trace.
      for (const entry of body.trace) {
        if (!entry.skipped) {
          console.log(`  tier ${entry.tier} (${entry.type}): status=${entry.status} evalMs=${entry.evalMs}`);
        } else {
          console.log(`  tier ${entry.tier} (${entry.type}): SKIPPED — ${entry.reason}`);
        }
      }
    }
  } catch {
    // Non-JSON response — skip trace output.
  }
}

// --- Scenario A: single-tier CEL ---

export function celOnly() {
  const res = http.post(
    `${baseUrl}/v1/verify/composed`,
    CEL_ONLY_PAYLOAD,
    PARAMS,
  );
  celOnlyDuration.add(res.timings.duration);
  celOnlyErrorRate.add(res.status !== 200);
  check(res, {
    'cel_only: status is 200': (r) => r.status === 200,
    'cel_only: result.ok present': (r) => {
      try { return JSON.parse(r.body).result?.ok !== undefined; } catch { return false; }
    },
  });
  // Emit a single trace example (not every iteration — sampled).
  if (__ITER % 50 === 0) {
    emitTraceExample('cel_only', res);
  }
}

// --- Scenario B1: three-tier any_pass ---

export function threeTierAnyPass() {
  const res = http.post(
    `${baseUrl}/v1/verify/composed`,
    THREE_TIER_ANY_PASS_PAYLOAD,
    PARAMS,
  );
  threeTierAnyPassDuration.add(res.timings.duration);
  threeTierAnyPassErrorRate.add(res.status !== 200);
  check(res, {
    'any_pass: status is 200': (r) => r.status === 200,
    'any_pass: result.ok present': (r) => {
      try { return JSON.parse(r.body).result?.ok !== undefined; } catch { return false; }
    },
  });
  if (__ITER % 50 === 0) {
    emitTraceExample('any_pass', res);
  }
}

// --- Scenario B2: three-tier all_pass ---

export function threeTierAllPass() {
  const res = http.post(
    `${baseUrl}/v1/verify/composed`,
    THREE_TIER_ALL_PASS_PAYLOAD,
    PARAMS,
  );
  threeTierAllPassDuration.add(res.timings.duration);
  threeTierAllPassErrorRate.add(res.status !== 200);
  check(res, {
    'all_pass: status is 200': (r) => r.status === 200,
    'all_pass: result.ok present': (r) => {
      try { return JSON.parse(r.body).result?.ok !== undefined; } catch { return false; }
    },
  });
  if (__ITER % 50 === 0) {
    emitTraceExample('all_pass', res);
  }
}

// --- Scenario B3: three-tier short_circuit ---

export function threeTierShortCircuit() {
  const res = http.post(
    `${baseUrl}/v1/verify/composed`,
    THREE_TIER_SHORT_CIRCUIT_PAYLOAD,
    PARAMS,
  );
  threeTierShortCircuitDuration.add(res.timings.duration);
  threeTierShortCircuitErrorRate.add(res.status !== 200);
  check(res, {
    'short_circuit: status is 200': (r) => r.status === 200,
    'short_circuit: result.ok present': (r) => {
      try { return JSON.parse(r.body).result?.ok !== undefined; } catch { return false; }
    },
  });
  if (__ITER % 50 === 0) {
    emitTraceExample('short_circuit', res);
  }
}
