# symkernel

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

## Architecture

```
wasmagent-js / wasmagent-py / bscode
        │  HTTP  (Criterion/ConstraintIR protocol)
        ▼
  symkerneld (Go HTTP server)
   ├── POST /v1/verify/cel        — cel-go expression evaluator
   ├── POST /v1/verify/criterion  — wasmagent-js Criterion adapter
   ├── POST /v1/sandbox/run       — wazero Wasm sandbox
   └── POST /v1/verify/z3         — Z3 SMT satisfiability
        │
        ├── internal/cel     — cel-go wrapper, per-request timeout
        ├── internal/sandbox — wazero runtime, trap→structured error
        └── internal/smt     — go-z3 CGO binding
```

Every response carries a `decision_id` (UUID) and `evalMs` for traceability, following GENAI_SEMCONV field naming to align with `@wasmagent/otel-exporter`.

## Deployment

```bash
docker compose up          # local dev
make wasm && make test     # build + test
```

Designed for Cloudflare Containers (Phase 0/1) with a migration path to self-hosted VPS if Z3 p99 latency exceeds threshold.

## Schema alignment

`schemas/` holds `constraint-ir.schema.json` and `constraint-violation.schema.json` pinned from `wasmagent-js`. `make sync-schemas` refreshes them; CI fails on drift.

Part of the [WasmAgent](https://github.com/WasmAgent) ecosystem.
