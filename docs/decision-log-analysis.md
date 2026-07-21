# Decision Log Analysis

symkerneld records every verification decision in an in-memory append-only audit
trail. Entries are exported as JSONL via `GET /v1/audit/export`.

## Audit Log Schema

Each JSONL line is a JSON object:

```json
{
  "decision_id": "550e8400-e29b-41d4-a716-446655440000",
  "input_hash": "a591a6d40bf420404a011733cfb7b190d62c65bf0bcda32b57b277d9ad9f146e",
  "tier": "cel",
  "result": "pass",
  "timestamp": "2026-07-22T14:30:00.123456789Z"
}
```

| Field | Description |
|---|---|
| `decision_id` | UUID matching the response field and OpenTelemetry span. Correlates with `@wasmagent/otel-exporter` traces. |
| `input_hash` | SHA-256 digest of the verification request body. Use to detect duplicate or replayed requests. |
| `tier` | Verification backend: `cel`, `wazero`, `z3`, or orchestrator-assigned tier. |
| `result` | Verdict: `pass`, `fail`, or `error`. |
| `timestamp` | UTC time in RFC 3339 Nano format. |

## Exporting Audit Data

```bash
# Export all entries (JSONL)
curl -sf -H "Authorization: Bearer $TOKEN" \
  http://localhost:8080/v1/audit/export?format=jsonl

# Export entries from a specific time
curl -sf -H "Authorization: Bearer $TOKEN" \
  "http://localhost:8080/v1/audit/export?format=jsonl&from=2026-07-22T14:00:00Z"
```

## Sample `jq` One-Liners

### Pass/Fail Rate Over Last N Entries

```bash
# Export and compute pass rate
curl -sf -H "Authorization: Bearer $TOKEN" \
  http://localhost:8080/v1/audit/export?format=jsonl | \
  jq -s '{total: length, pass: map(select(.result == "pass")) | length, fail: map(select(.result == "fail")) | length, error: map(select(.result == "error")) | length}'
```

### Tier Distribution

```bash
curl -sf -H "Authorization: Bearer $TOKEN" \
  http://localhost:8080/v1/audit/export?format=jsonl | \
  jq -s 'group_by(.tier) | map({tier: .[0].tier, count: length})'
```

### Errors in the Last 5 Minutes

```bash
FROM=$(date -u -d '5 min ago' +%Y-%m-%dT%H:%M:%SZ)
curl -sf -H "Authorization: Bearer $TOKEN" \
  "http://localhost:8080/v1/audit/export?format=jsonl&from=$FROM" | \
  jq -s 'map(select(.result == "error"))'
```

### Detect Duplicate Input Hashes (Replay or Batching)

```bash
curl -sf -H "Authorization: Bearer $TOKEN" \
  http://localhost:8080/v1/audit/export?format=jsonl | \
  jq -r '.input_hash' | sort | uniq -c | sort -rn | head -20
```

### Decision Volume Per Minute

```bash
curl -sf -H "Authorization: Bearer $TOKEN" \
  http://localhost:8080/v1/audit/export?format=jsonl | \
  jq -r '.timestamp[:16]' | sort | uniq -c
```

### Z3-Specific Decisions

```bash
curl -sf -H "Authorization: Bearer $TOKEN" \
  http://localhost:8080/v1/audit/export?format=jsonl | \
  jq -s 'map(select(.tier == "z3")) | {total: length, pass: map(select(.result == "pass")) | length, fail: map(select(.result == "fail")) | length, error: map(select(.result == "error")) | length}'
```

### Correlate Decision with Diagnostics

When a decision fails, look up its `WhyFailed` explanation:

```bash
# Get a failed decision_id
DECISION_ID=$(curl -sf -H "Authorization: Bearer $TOKEN" \
  http://localhost:8080/v1/audit/export?format=jsonl | \
  jq -r 'select(.result == "fail") | .decision_id' | head -1)

# Fetch diagnostics
curl -sf -H "Authorization: Bearer $TOKEN" \
  "http://localhost:8080/v1/diagnostics/$DECISION_ID" | jq .
```

## Audit Log Limits

The audit trail is an in-memory ring buffer (default: 10 000 entries). Older
entries are evicted when the buffer is full. For persistent audit storage, pipe
the export to a file or external system before entries rotate out:

```bash
# Snapshot to file
curl -sf -H "Authorization: Bearer $TOKEN" \
  http://localhost:8080/v1/audit/export?format=jsonl > audit-$(date +%Y%m%d-%H%M%S).jsonl
```

## Planned (Milestone 8)

Milestone 8 adds a file-based decision log with rotation and optional S3 upload,
providing persistent storage beyond the in-memory ring buffer.