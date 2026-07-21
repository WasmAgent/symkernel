# Capacity Planning Guidelines

## Current Deployment Target

The default deployment is a single Cloudflare Containers `standard-1` instance:
1/2 vCPU, 4 GiB RAM, 8 GB disk. This serves the current development load test
(10 concurrent requests).

## Resource Consumption by Tier

| Tier | CPU | Memory | Latency (typical) | Notes |
|---|---|---|---|---|
| CEL | Low | ~10 MB per evaluator | < 5 ms | In-process; no subprocess. Cache hits are near-zero cost. |
| wazero (WASM sandbox) | Medium | Configurable per module (default 64 MB) | 10–100 ms | In-process WASM runtime. Memory bounded by `memory_limit_mb`. |
| Z3 (SMT solver) | High | 100 MB–2 GB (problem-dependent) | 50–500 ms | Subprocess (`z3 -in`). Memory unbounded by symkernel — depends on assertion complexity. |

## Sizing Guidelines

### Memory

- **Base process**: ~30 MB (Go runtime, HTTP server, cache structures)
- **Cache**: ~50 bytes per entry × capacity (default 10 000 = ~500 KB)
- **Z3 subprocess**: 100 MB minimum; complex assertions can push to 1 GB+
- **wazero modules**: 1–64 MB per concurrent execution (configurable)

**Minimum recommendation for single instance**: 512 MB RAM (CEL-only) or
2 GB RAM (with Z3 support).

**Production recommendation**: 4 GB RAM per instance, allowing concurrent
Z3 calls without OOM risk.

### CPU

- CEL workloads: 0.1–0.25 vCPU per 100 QPS
- Z3 workloads: 0.5–1.0 vCPU per 10 QPS (highly assertion-dependent)

For mixed workloads, target 70% CPU utilization for headroom during spikes.

### Concurrency

- The HTTP server uses Go's default goroutine-per-request model.
- The batch endpoint defaults to 16 concurrent items (`SYMKERNEL_BATCH_CONCURRENCY`).
- Each concurrent Z3 call spawns a subprocess — limit concurrent Z3 requests
  to avoid CPU thrashing.

## Scaling Strategy

### Vertical (Single Instance)

Increase container size in `deploy/wrangler.toml`:

```toml
instance_type = "standard-2"   # 1 vCPU, 8 GiB RAM
max_instances = 1
```

Suitable for: low-to-moderate traffic, single-region deployment.

### Horizontal (Multiple Instances)

Bump `max_instances` in `deploy/wrangler.toml`:

```toml
instance_type = "standard-1"
max_instances = 4
```

Note: the in-memory cache is not shared across instances. Each instance
maintains its own LRU. Cache hit ratio may decrease initially until each
instance warms up.

Planned (M8): Redis-backed cache layer for cross-instance deduplication.

### Kubernetes (Planned — M8)

Milestone 8 plans `deploy/kubernetes/` with:
- HPA: 2–20 pods targeting 70% CPU
- PodDisruptionBudget: minimum 2 available
- ConfigMap for environment variable injection

## Latency SLO Targets (Planned — M8)

| Tier | p50 | p95 | p99 |
|---|---|---|---|
| CEL | < 20 ms | < 50 ms | < 100 ms |
| wazero | < 50 ms | < 100 ms | < 200 ms |
| Z3 | < 100 ms | < 300 ms | < 500 ms |

These will be validated by `bench/latency-slos` (M8).

## Cache Sizing

| Parameter | Default | Tuning Guidance |
|---|---|---|
| `capacity` | 10 000 entries | Increase for high-cardinality expression sets. Each entry is ~50 bytes. |
| `default_ttl` | 5 minutes | Increase for stable policies. Decrease if context changes frequently. |

Monitor via `GET /v1/admin/cache/stats`. If `evictions` grows rapidly relative
to `misses`, increase capacity.

## Monitoring Recommendations

1. **Request rate**: track `total_entries` growth in `GET /v1/audit/stats`
2. **Error rate**: count entries with `"result":"error"` in audit export
3. **Cache hit ratio**: compute from `GET /v1/admin/cache/stats`
   (`hits / (hits + misses)`)
4. **Z3 latency**: track `evalMs` in Z3 verification responses
5. **Memory pressure**: monitor container RSS; Z3 assertions can cause
   unbounded memory growth