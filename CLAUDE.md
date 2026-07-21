# symkernel — CLAUDE.md

## Project overview
Go symbolic verification backend for the WasmAgent ecosystem.
Provides a three-tier reasoning service over HTTP consumed by `wasmagent-js`,
`wasmagent-py`, and any runtime speaking the Criterion/ConstraintIR protocol.

| Tier | Technology | Use case |
|---|---|---|
| Lightweight rules | cel-go | Fast policy evaluation, capability checks |
| Hard isolation | wazero (WASM sandbox) | Untrusted code execution with memory + network caps |
| Formal proofs | Z3 SMT solver | Mathematical satisfiability for invariant and safety properties |

**Status**: In development — all milestones planned. See `docs/15-milestones.md`.

## Repository maturity

| | |
|---|---|
| **Status** | Experimental |
| **Contract stability** | Unstable (API may change without notice) |
| **Recommended for** | WasmAgent-ecosystem consumers needing CEL/wazero/Z3 verification |
| **Not recommended for** | Production deployments; standalone policy engine outside WasmAgent |

## Repository Boundaries

### This repository owns
- `symkerneld` HTTP server — CEL, wazero sandbox, Z3 SMT, composed policy, batch endpoints
- `aep-core` Go package — CEL/wazero/Z3 shared types (RecordingMode, ConstraintIR adapter)
- Criterion/ConstraintIR protocol implementation
- Policy composition engine (`any_pass` / `all_pass` / `short_circuit`)
- Verification trace explainer (`internal/explain`)
- Batch verification (`internal/batch`)
- Cloudflare Containers deployment

### Other repositories own — do not duplicate here

| Capability | Owner |
|---|---|
| AEP schema definition and emission | `wasmagent-js` (`@wasmagent/aep`) |
| MCP firewall, process-level policy enforcement | `wasmagent-js` (`@wasmagent/mcp-gateway`) |
| Gateway-level HTTP evidence (Proxy-Wasm) | `wasmagent-proxy` |
| AgentBOM / MCP Posture specifications | `agent-trust-infra` |
| Trust Passport specification and product | `open-agent-audit` (`@openagentaudit/passport`) |
| Enterprise audit report generation | `open-agent-audit` |
| Training data pipeline | `trace-pipeline` |

### Allowed cross-repo patterns
- `wasmagent-js` calls symkernel via HTTP (`CelGoVerifier`, `Z3Verifier`) using the Criterion/ConstraintIR protocol — keep that HTTP API stable and versioned.
- `decision_id` (UUID) and `evalMs` in every response follow GENAI_SEMCONV field naming to align with `@wasmagent/otel-exporter`; do not rename these fields.
- Verification results feed `open-agent-audit` audit reports via `wasmagent-js` — symkernel itself does not emit AEP records or call audit APIs directly.
- No LLM calls inside symkernel — LLM routing belongs in `wasmagent-js` smartrouter.

## Tech stack
- Go 1.21+, standard `go build ./...`
- cel-go for CEL evaluation
- wazero for WASM sandbox
- go-z3 CGO binding for Z3 SMT
- Tests: `go test ./...`
- Lint: `staticcheck ./...` + `go vet ./...`
- `/home/dev/go/bin/staticcheck` on VPS

## Build and verify
```bash
go build ./...
go test ./...
/home/dev/go/bin/staticcheck ./...
```

## Bot instructions
- Run `go build ./...` and `go test ./...` before every commit
- `staticcheck ./...` must pass — no unused exports, no suspicious constructs
- All new endpoints must have unit tests covering PASS, FAIL, and error cases
- Keep `decision_id` and `evalMs` in every response — they are cross-repo contracts
- Do not add LLM calls or AEP emission — symkernel is a pure verification service
- API changes require a version bump in the URL path (`/v1/`, `/v2/`)

## Roadmap

Bot: implement milestones in order per `docs/15-milestones.md`.

### M1 — Foundation & CEL Integration
- [ ] HTTP server scaffold with health endpoint
- [ ] `POST /v1/verify/cel` — cel-go expression evaluator with timeout
- [ ] `POST /v1/verify/criterion` — wasmagent-js Criterion adapter
- [ ] `decision_id` + `evalMs` in all responses
- [ ] Unit tests: PASS / FAIL / timeout / malformed-expression cases

### M2 — wazero Sandbox
- [ ] `POST /v1/sandbox/run` — wazero Wasm execution with memory + network caps
- [ ] Trap → structured error conversion
- [ ] Unit tests: normal execution / memory-limit / network-blocked / trap cases

### M3 — Z3 Formal Verification
- [ ] `POST /v1/verify/z3` — Z3 SMT satisfiability with go-z3 CGO binding
- [ ] Timeout budget per Z3 call
- [ ] Unit tests: SAT / UNSAT / timeout cases

### M4 — Schema Alignment & Upstream Collaboration
- [ ] Align ConstraintIR schema with `wasmagent-js` Criterion protocol
- [ ] `POST /v1/verify/composed` — multi-tier policy composition (any_pass / all_pass / short_circuit)
- [ ] `POST /v1/verify/batch` — parallel batch execution (up to 1000 items)
- [ ] Integration test: wasmagent-js CelGoVerifier calling symkerneld

### M6 — Policy Composition & Developer Experience
- [ ] `internal/explain` — verification trace explainer
- [ ] Per-tier `timeout_ms` budget in composed requests
- [ ] Cloudflare Containers deployment config
- [ ] Developer guide: how to add a new verification tier

## How patrol sweep drives progress
Patrol reads this CLAUDE.md roadmap section.
Unchecked items → patrol opens issues with `claude` label → workers implement.
