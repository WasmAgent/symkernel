// k6 composition cache cold/warm benchmark.
//
// Measures the latency difference between cold-cache and warm-cache conditions
// for composed verification requests. The test runs in two phases:
//
//   Phase 1 (cold) — cache is flushed before each request batch, so every
//     request pays the full evaluation cost. After the batch, a single trace
//     example is emitted showing the full chain execution.
//
//   Phase 2 (warm) — the same payloads are sent again without flushing, so
//     cached CEL results are served from the L1 in-process cache. A single
//     trace example is emitted showing tier-skip behaviour.
//
// The script also queries GET /v1/admin/cache/stats before and after each phase
// to capture cache hit/miss ratios.
//
// Prerequisites:
//   - symkerneld running with compose endpoint and cache (POST /v1/admin/cache/invalidate)
//   - SYMKERNEL_BASE_URL and SYMKERNEL_CLIENT_TOKEN set
//
// Usage:
//   export SYMKERNEL_BASE_URL=http://localhost:8080
//   export SYMKERNEL_CLIENT_TOKEN=<token>
//   k6 run bench/k6/composition-cache-cold-warm.js
//
// See bench/load-test-infra.md for infrastructure details.

import http from 'k6/http';
import { check, sleep } from 'k6';
import { Trend, Rate } from 'k6/metrics';

const baseUrl = __ENV.SYMKERNEL_BASE_URL || 'http://localhost:8080';
const token = __ENV.SYMKERNEL_CLIENT_TOKEN || '';

// --- Custom metrics ---

// Cold-phase duration (every request is a cache miss).
const coldDuration = new Trend('cold_cache_duration', true);
const coldErrorRate = new Rate('cold_cache_errors');

// Warm-phase duration (requests should hit cache for CEL tier).
const warmDuration = new Trend('warm_cache_duration', true);
const warmErrorRate = new Rate('warm_cache_errors');

// --- Shared headers ---

const PARAMS = {
  headers: {
    'Content-Type': 'application/json',
    ...(token ? { Authorization: `Bearer ${token}` } : {}),
  },
  timeout: '10s',
};

// --- Payloads ---

// Composed verification with a CEL tier that is cacheable.
// The context values are varied across iterations to exercise different cache keys.
const CEL_COMPOSED_PAYLOADS = [
  {
    tiers: [
      { type: 'cel', expr: 'input.age >= 18 && input.score > 50' },
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
    context: { age: 25, score: 78 },
  },
  {
    tiers: [
      { type: 'cel', expr: 'input.age >= 18 && input.score > 50' },
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
    context: { age: 30, score: 92 },
  },
  {
    tiers: [
      { type: 'cel', expr: 'input.level >= 3 && input.active == true' },
      {
        type: 'wazero',
        module: 'AGFzbQEAAAABBQFgAAF/AwIBAAcKAQZhZGQAAgABChABbXNnAGkAAQABAgME',
        memory_limit_mb: 64,
        timeout_ms: 500,
      },
      {
        type: 'z3',
        constraints_smt2: '(declare-const y Int) (assert (>= y 10))',
        timeout_ms: 1000,
      },
    ],
    mode: 'any_pass',
    context: { level: 5, active: true },
  },
];

// --- k6 options ---

export const options = {
  scenarios: {
    // Phase 1: cold cache — flush before each request.
    cold_phase: {
      executor: 'shared-iterations',
      vus: 5,
      iterations: 60,
      exec: 'coldRequest',
      thresholds: {
        cold_cache_duration: ['p(95)<1000', 'p(99)<2000'],
        cold_cache_errors: ['rate<0.01'],
      },
    },
    // Phase 2: warm cache — reuse cached results.
    warm_phase: {
      executor: 'shared-iterations',
      vus: 5,
      iterations: 60,
      startTime: '35s', // Start after cold_phase + 5s gap.
      exec: 'warmRequest',
      thresholds: {
        warm_cache_duration: ['p(95)<200', 'p(99)<500'],
        warm_cache_errors: ['rate<0.01'],
      },
    },
  },
};

// --- Helpers ---

// flushCache sends a POST to invalidate all cache entries.
function flushCache() {
  http.post(
    `${baseUrl}/v1/admin/cache/invalidate`,
    JSON.stringify({ all: true }),
    PARAMS,
  );
}

// emitTrace logs the trace array from a composed response.
function emitTraceExample(label, res) {
  try {
    const body = JSON.parse(res.body);
    if (body.trace && body.trace.length > 0) {
      console.log(`[${label}] trace: ${JSON.stringify(body.trace)}`);
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

// logCacheStats fetches and logs the current cache statistics.
function logCacheStats(label) {
  const res = http.get(`${baseUrl}/v1/admin/cache/stats`, PARAMS);
  try {
    const stats = JSON.parse(res.body);
    console.log(`[${label}] cache stats: size=${stats.size} hits=${stats.hits} misses=${stats.misses} evictions=${stats.evictions}`);
  } catch {
    console.log(`[${label}] failed to parse cache stats`);
  }
}

// --- Phase 1: cold cache ---

export function coldRequest() {
  // Flush cache before each cold request to guarantee a miss.
  flushCache();

  const payload = CEL_COMPOSED_PAYLOADS[__ITER % CEL_COMPOSED_PAYLOADS.length];
  const res = http.post(
    `${baseUrl}/v1/verify/composed`,
    JSON.stringify(payload),
    PARAMS,
  );
  coldDuration.add(res.timings.duration);
  coldErrorRate.add(res.status !== 200);
  check(res, {
    'cold: status is 200': (r) => r.status === 200,
    'cold: result.ok present': (r) => {
      try { return JSON.parse(r.body).result?.ok !== undefined; } catch { return false; }
    },
  });

  // Emit trace example on first iteration and every 20th.
  if (__ITER === 0 || __ITER % 20 === 0) {
    emitTraceExample('cold', res);
  }

  // Log cache stats at the start and midpoint.
  if (__ITER === 0) {
    logCacheStats('cold_start');
  }
  if (__ITER === 30) {
    logCacheStats('cold_mid');
  }
}

// --- Phase 2: warm cache ---

export function warmRequest() {
  // Do NOT flush — let the cache serve warm results.
  const payload = CEL_COMPOSED_PAYLOADS[__ITER % CEL_COMPOSED_PAYLOADS.length];
  const res = http.post(
    `${baseUrl}/v1/verify/composed`,
    JSON.stringify(payload),
    PARAMS,
  );
  warmDuration.add(res.timings.duration);
  warmErrorRate.add(res.status !== 200);
  check(res, {
    'warm: status is 200': (r) => r.status === 200,
    'warm: result.ok present': (r) => {
      try { return JSON.parse(r.body).result?.ok !== undefined; } catch { return false; }
    },
  });

  // Emit trace example on first iteration and every 20th.
  if (__ITER === 0 || __ITER % 20 === 0) {
    emitTraceExample('warm', res);
  }

  // Log cache stats at the start and midpoint.
  if (__ITER === 0) {
    logCacheStats('warm_start');
  }
  if (__ITER === 30) {
    logCacheStats('warm_mid');
  }
}
