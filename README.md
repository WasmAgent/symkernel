# symkernel

| | |
|---|---|
| **Status** | Experimental |
| **Contract stability** | Unstable |
| **Recommended for** | WasmAgent-ecosystem CEL/wazero/Z3 verification |
| **Not recommended for** | Standalone production policy engine |


Symbolic verification backend for the WasmAgent ecosystem, written in Go.

Provides a three-tier reasoning service over HTTP, consumed by wasmagent-js, wasmagent-py, and any runtime that speaks the Criterion/ConstraintIR protocol:

| Tier | Technology | Use case |
|---|---|---|
| Lightweight rules | cel-go | Fast policy evaluation (capability checks, constraint matching) |
| Hard isolation | wazero (WASM sandbox) | Untrusted code execution with memory + network caps |
| Formal proofs | Z3 SMT solver | Mathematical satisfiability for invariant and safety properties |

## Status

🚧 In development. See `docs/15-milestones.md`.

| Milestone | Status |
|---|---|
| M1 — Foundation & CEL Integration | planned |
| M2 — wazero Sandbox | planned |
| M3 — Z3 Formal Verification | planned |
| M4 — Schema Alignment & Upstream Collaboration | planned |
| M6 — Policy Composition & Developer Experience | planned |

## Architecture

```
wasmagent-js / wasmagent-py / bscode
        │  HTTP  (Criterion/ConstraintIR protocol)
        ▼
  symkerneld (Go HTTP server)
   ├── POST /v1/verify/cel        — cel-go expression evaluator
   ├── POST /v1/verify/criterion  — wasmagent-js Criterion adapter
   ├── POST /v1/sandbox/run       — wazero Wasm sandbox
   ├── POST /v1/verify/z3         — Z3 SMT satisfiability
   ├── POST /v1/verify/composed   — multi-tier policy composition
   └── POST /v1/verify/batch      — bulk verification (up to 1000 items)
        │
        ├── internal/cel          — cel-go wrapper, per-request timeout
        ├── internal/sandbox      — wazero runtime, trap→structured error
        ├── internal/smt          — go-z3 CGO binding
        ├── internal/compose      — policy composition (any_pass/all_pass/short_circuit)
        ├── internal/explain      — verification trace explainer
        └── internal/batch        — parallel batch execution
```

Every response carries a `decision_id` (UUID) and `evalMs` for traceability, following GENAI_SEMCONV field naming to align with `@wasmagent/otel-exporter`.

## Composed Policies

Composed policies chain multiple verification tiers (CEL → wazero → Z3) into a single
request, with configurable fallback behaviour. This lets callers express rich policies
like *"the CEL check passes OR the Wasm sandbox confirms"* without client-side
orchestration.

### `POST /v1/verify/composed`

```json
{
  "tiers": [
    { "type": "cel",    "expr": "input.age >= 18" },
    { "type": "wazero", "module": "<base64-wasm>", "memory_limit_mb": 64 }
  ],
  "mode": "any_pass"
}
```

### Composition modes

| Mode | Behaviour | Use case |
|---|---|---|
| `any_pass` | Returns **PASS** as soon as any tier passes; remaining tiers are skipped. If all tiers fail, returns **FAIL**. | "Fail-open" policies — one positive signal is enough (e.g. allow if either rule engine or sandbox approves). |
| `all_pass` | Runs **all** tiers; returns **PASS** only if every tier passes. Short-circuits on the first failure. | "Fail-strict" policies — every guard must agree (e.g. CEL check AND Z3 proof). |
| `short_circuit` | Runs tiers in order; stops on the **first** definitive result (pass or fail) and returns immediately. | Latency-sensitive paths — fastest tier answers; slower tiers are fallbacks only. |

Each tier can carry its own `timeout_ms` budget so that a slow Z3 call does not block
a fast CEL decision.

### Trace interpretation

Every composed response includes a `trace` array showing per-tier evaluation:

```json
{
  "result": { "ok": true },
  "decision_id": "uuid-v4",
  "evalMs": 1.2,
  "trace": [
    { "tier": 0, "type": "cel",    "status": "pass", "evalMs": 0.04, "value": true,     "skipped": false },
    { "tier": 1, "type": "wazero", "status": "skip", "evalMs": 0,    "value": null,     "skipped": true,  "reason": "any_pass: previous tier passed" }
  ]
}
```

- **`status`**: `pass`, `fail`, `error`, or `skip`.
- **`skipped`**: `true` when the composition mode caused the tier to be bypassed
  (e.g. `any_pass` short-circuits after a pass).
- **`reason`**: human-readable explanation of why the tier was skipped.
- Enable verbose traces with the `?explain=true` query parameter on any verification
  endpoint.

### `symk` CLI quickstart

`symk` is the local development CLI (Milestone 6). It evaluates CEL expressions
offline using the embedded evaluator — no running server required.

```bash
# Build
go build ./cmd/symk

# Single expression
symk verify cel --expr 'input.age > 18' --context ctx.json

# Without a context file (literal values)
symk verify cel --expr '42 > 0'

# Batch-validate a directory of policy YAML files
symk test-policy --dir policies/

# Check connectivity and auth against a remote symkerneld
symk doctor --addr http://localhost:8080
```

Example `ctx.json`:

```json
{ "age": 21, "role": "admin" }
```

### Batch verification

The `POST /v1/verify/batch` endpoint accepts up to 1000 verification items in a
single payload, executing them in parallel with a configurable concurrency limit
(`SYMKERNEL_BATCH_CONCURRENCY`, default 16).

```bash
curl -s -X POST http://localhost:8080/v1/verify/batch \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer $SYMKERNEL_CLIENT_TOKEN" \
  -d '{
    "items": [
      { "expr": "input.age >= 18", "context": {"age": 21} },
      { "expr": "input.age >= 18", "context": {"age": 15} },
      { "expr": "input.role == \"admin\"", "context": {"role": "viewer"} }
    ]
  }' | jq .
```

Response:

```json
{
  "results": [
    { "index": 0, "ok": true,  "decision_id": "a", "evalMs": 0.03 },
    { "index": 1, "ok": false, "decision_id": "b", "evalMs": 0.02 },
    { "index": 2, "ok": false, "decision_id": "c", "evalMs": 0.03, "hint": "condition false" }
  ],
  "totalMs": 12
}
```

Items that time out carry `"timeout": true`. Partial results are returned so the
caller can identify and retry only the unfinished items.

## Deployment

```bash
docker compose up          # local dev
make wasm && make test     # build + test
```

Designed for Cloudflare Containers (Phase 0/1) with a migration path to self-hosted VPS if Z3 p99 latency exceeds threshold.

## Schema alignment

`schemas/` holds `constraint-ir.schema.json` and `constraint-violation.schema.json` pinned from `wasmagent-js`. `make sync-schemas` refreshes them; CI fails on drift.

Part of the [WasmAgent](https://github.com/WasmAgent) ecosystem.
