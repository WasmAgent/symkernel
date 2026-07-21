# Health Check Interpretation

## Current Endpoints

symkerneld does not yet have dedicated `/healthz` endpoints (planned in Milestone 8).
Use the following operational endpoints to infer service health:

### Audit Stats — `GET /v1/audit/stats`

Returns a snapshot of the in-memory audit trail. A `200` response confirms the
server process is alive and the audit subsystem is functional.

```json
{
  "total_entries": 1247,
  "rotation_count": 0,
  "max_len": 10000,
  "tier_breakdown": { "cel": 981, "z3": 266 }
}
```

- **`total_entries`** approaching `max_len` (10 000) indicates the ring buffer is
  nearing capacity; oldest entries will be evicted on rotation.
- **`rotation_count`** increasing rapidly means high throughput — consider increasing
  `max_len` or exporting audit data more frequently.
- **`tier_breakdown`** shows the distribution of verification decisions across
  backends. An unexpected tier (e.g. no `z3` entries when Z3 is expected) may
  indicate a Z3 subprocess issue.

```bash
curl -sf -H "Authorization: Bearer $TOKEN" \
  http://localhost:8080/v1/audit/stats | jq .
```

### Cache Stats — `GET /v1/admin/cache/stats`

Returns cache hit/miss/eviction metrics. A `200` response confirms the cache
subsystem is operational.

```json
{
  "size": 4291,
  "capacity": 10000,
  "hits": 89234,
  "misses": 12053,
  "evictions": 712,
  "tags": 3,
  "default_ttl": "5m0s",
  "tag_count": { "schema:v1": 4291 }
}
```

Key signals:

| Metric | Healthy | Warning |
|---|---|---|
| `hits / (hits + misses)` | > 80% | < 50% — cache keys may be too granular or TTL too short |
| `evictions` growth rate | Low relative to `misses` | High — `capacity` may need increasing |
| `size` near `capacity` | < 80% | > 90% — frequent LRU evictions expected |

Hit ratio:
```bash
curl -sf -H "Authorization: Bearer $TOKEN" \
  http://localhost:8080/v1/admin/cache/stats | \
  jq '{hit_ratio: (.hits / (.hits + .misses) * 100 | floor)}'
```

### Orchestrator Stats — `GET /v1/orchestration/stats`

Returns tier routing statistics. Confirms the orchestrator is classifying requests.

```bash
curl -sf -H "Authorization: Bearer $TOKEN" \
  http://localhost:8080/v1/orchestration/stats | jq .
```

### Diagnostics — `GET /v1/diagnostics/{decision_id}`

Returns `404` for unknown IDs (normal). Returns `200` with `WhyFailed` details
for failed verifications. Use to investigate specific decision outcomes.

```bash
curl -sf -H "Authorization: Bearer $TOKEN" \
  http://localhost:8080/v1/diagnostics/<decision_id> | jq .
```

## Planned (Milestone 8)

Dedicated health endpoints are planned but not yet implemented:

| Endpoint | Purpose | Checks |
|---|---|---|
| `GET /healthz` | Liveness | Returns 200 if server is responding |
| `GET /healthz/ready` | Readiness | Checks Z3 subprocess, wazero warmup, cache state |
| `GET /healthz/deep` | Deep dependency ping | CelExprParser compile test, wazero.CompileModule, Z3 satisfiability check |

Until these land, use the audit stats endpoint as a liveness proxy:

```bash
# Quick liveness check
curl -sf -o /dev/null -w "%{http_code}" \
  -H "Authorization: Bearer $TOKEN" \
  http://localhost:8080/v1/audit/stats
# Expect: 200
```