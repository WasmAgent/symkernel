# Circuit Breaker Recovery Procedures

## Current State

symkerneld does not yet have a built-in circuit breaker (planned in Milestone 8).
Z3 requests are bounded by the caller's context deadline — if the deadline
expires, the `z3 -in` subprocess is killed via `exec.CommandContext`.

## Z3 Timeout Behaviour (Current)

When a Z3 request exceeds its context deadline:

1. The subprocess receives `SIGKILL`
2. The handler returns a timeout error to the caller
3. No circuit is opened — subsequent Z3 requests proceed normally

This means a pathological Z3 assertion can repeatedly consume resources until
the caller stops sending it.

## Manual Mitigation (Until M8 Circuit Breaker Lands)

### 1. Identify Timeout Storms

Timeout responses from Z3 include an error message. Monitor via audit logs:

```bash
# Export recent audit entries and filter for Z3 errors
curl -sf -H "Authorization: Bearer $TOKEN" \
  "http://localhost:8080/v1/audit/export?from=$(date -u -d '5 min ago' -I | head -1)" | \
  grep '"tier":"z3"' | grep '"result":"error"' | wc -l
```

### 2. Drain Z3 Traffic

If Z3 is consistently timing out, route traffic away by using the orchestrator's
automatic tier selection (which avoids Z3 for simple expressions) or by adjusting
upstream load balancer rules.

### 3. Restart the Service

The in-memory cache survives restarts only if externalized. A restart clears
all cached results:

```bash
# In a container environment
docker restart symkernel

# As a systemd service
sudo systemctl restart symkerneld
```

### 4. Invalidate Stale Cache Entries

If cached Z3 results are suspected to be stale (e.g. after a Z3 version upgrade):

```bash
# Purge all cache entries
curl -sf -X POST -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  http://localhost:8080/v1/admin/cache/invalidate \
  -d '{"all":true}' | jq .

# Purge entries by tag
curl -sf -X POST -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  http://localhost:8080/v1/admin/cache/invalidate \
  -d '{"tag":"schema:v1"}' | jq .
```

## Planned Circuit Breaker (Milestone 8)

When `internal/circuitbreaker` lands, the behaviour will be:

| State | Behaviour |
|---|---|
| **Closed** (normal) | Requests pass through to Z3. Consecutive timeouts are tracked. |
| **Open** (tripped) | After 3 consecutive timeouts >30s within a 60s window, returns `503 Service Unavailable` with `{"error":"Z3 circuit open"}` immediately — no subprocess spawned. |
| **Half-Open** (probing) | After 90s in open state, allows a single probe request. If it succeeds, returns to closed. If it times out, re-opens. |

### Recovery from Open Circuit

1. **Wait** — the circuit auto-transitions to half-open after 90 seconds.
2. **Monitor** the next Z3 request. If it succeeds, the circuit closes.
3. **Investigate** the root cause of the original timeouts (complex assertions,
   Z3 resource exhaustion, host memory pressure).
4. **If persistent**: scale horizontally (add more instances) or increase Z3
   timeout budgets in composed policy tier configurations.