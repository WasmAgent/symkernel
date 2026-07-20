# symkernel — Milestone Plan

Symbolic verification kernel for the WasmAgent ecosystem (Go).
Provides a CEL + wazero + Z3 three-tier symbolic reasoning backend via HTTP,
language-agnostic and non-invasive to existing runtimes.

---

## Milestone 1 — Foundation & CEL Integration (Phase 0)

> Engineering validation: wire up infrastructure, not product differentiation.
> Goal: prove the integration protocol, deploy pipeline, auth, and observability work.

- [ ] `cmd/symkerneld` — HTTP server entrypoint with graceful shutdown and configurable listen address (`SYMKERNEL_ADDR`)
- [ ] `internal/cel` — cel-go expression evaluator: `Evaluate(expr string, context map[string]any) (any, error)` wrapping `cel-go` program compilation and evaluation with per-request timeout
- [ ] `internal/auth` — Bearer token middleware: validate `Authorization: Bearer <token>` against `SYMKERNEL_CLIENT_TOKEN` env var; return 401 on mismatch
- [ ] `internal/otel` — OpenTelemetry span setup: instrument every handler with `decision_id` (UUID) in span attributes and response JSON; follow GENAI_SEMCONV field naming to align with `@wasmagent/otel-exporter`
- [ ] `POST /v1/verify/cel` — OPA-envelope endpoint: `{"input":{"expr":"...","context":{...}}}` → `{"result":{"ok":true,"value":...},"decision_id":"uuid","evalMs":0.04}`; add unit tests covering: valid expr, compile error, type mismatch, timeout
- [ ] `POST /v1/verify/criterion` — wasmagent-js Criterion adapter: `{"criterion":{"id":"...","verify_method":"cel_expr","arg":{...}}}` → `{"ok":true,"criterionId":"..."}` or `{"ok":false,"criterionId":"...","hint":"..."}`; does NOT use OPA envelope (direct protocol match)
- [ ] `api/openapi.yaml` — OpenAPI 3.1 spec covering all Phase 0 endpoints with request/response examples and error codes
- [ ] `schemas/` — sync script `make sync-schemas` that pulls `constraint-ir.schema.json` and `constraint-violation.schema.json` from `wasmagent-js/packages/compliance/schemas` at a pinned commit; CI step fails if local copy drifts
- [ ] `deploy/Dockerfile` — multi-stage build: `golang:1.22-bookworm` builder → `gcr.io/distroless/static` final image; static binary, no CGO in Phase 0
- [ ] `deploy/wrangler.toml` — Cloudflare Containers deployment config: container binding name `SYMKERNEL`, memory/vCPU sizing for Phase 0 load
- [ ] `deploy/docker-compose.yml` — local dev environment: single `symkerneld` service with env vars; `docker compose up` is the quickstart
- [ ] `bench/` — comparison harness: run the 6 `policy-compliance` tasks from `bscode/fixtures/bench-v0/tasks/` against both the existing keyword+n-gram path and the new `cel_expr` path; output a markdown table of accuracy/false-positive rates
- [ ] README — 5-minute quickstart: `docker run`, example `curl` request against each endpoint, output expected

---

## Milestone 2 — Core Differentiation: wazero Sandbox (Phase 1a)

> First real differentiator: hardware-isolated code execution.

- [ ] `internal/sandbox` — wazero runtime wrapper: `Run(wasmModuleB64 string, args map[string]any, memLimitMB int, timeoutMs int) (SandboxResult, error)`; configure WASI permissions to deny filesystem/network by default; enforce memory cap via wazero `MemoryLimitPages`
- [ ] `internal/sandbox` — trap → structured error protocol: WASM traps (unreachable, memory OOB, stack overflow, timeout) mapped to `{"kind":"<trap_kind>","message":"..."}` in `SandboxResult.Trap`; all trap kinds covered by table-driven tests
- [ ] `POST /v1/sandbox/run` — OPA-envelope endpoint: `{"input":{"wasm_module_b64":"...","args":{...},"memory_limit_mb":64,"timeout_ms":5000}}` → `{"result":{"ok":true,"output":...,"trap":null},"decision_id":"uuid"}` or `{"result":{"ok":false,"trap":{"kind":"...","message":"..."}},"decision_id":"uuid"}`
- [ ] Integration test: compile a minimal WAT module that reads `args`, computes a result, and returns it; verify round-trip through `/v1/sandbox/run` produces correct output and correct `decision_id`
- [ ] Integration test: WAT module that traps on purpose (unreachable instruction); verify `trap.kind` is `"unreachable"` in response
- [ ] Integration test: WAT module that allocates beyond `memory_limit_mb`; verify `trap.kind` is `"memory_limit"` and process does not crash

---

## Milestone 3 — Core Differentiation: Z3 Formal Verification (Phase 1b)

> Second real differentiator: mathematical satisfiability proofs.

- [ ] `internal/smt` — Z3 binding via `go-z3` CGO: `Solve(smt2 string, timeoutMs int) (SMTResult, error)` returning `{Sat: "sat"|"unsat"|"unknown", Model: map[string]any, UnsatCore: []string}`
- [ ] `POST /v1/verify/z3` — OPA-envelope endpoint: `{"input":{"constraints_smt2":"...","timeout_ms":2000}}` → `{"result":{"sat":"sat","model":{...}},"decision_id":"uuid"}`; test cases: trivially sat, trivially unsat, timeout-induced unknown
- [ ] `internal/repair` — generate-error-fix loop: `Repair(program string, trapResult SandboxResult) (RepairPrompt, error)` assembles a structured prompt from trap details for LLM-driven fix; does NOT call LLM itself (caller injects); covered by unit test with a mock trap
- [ ] `deploy/Dockerfile` — update to include Z3 shared library (`libz3.so`) in final image; document CGO linker flags in Makefile
- [ ] `deploy/docker-compose.yml` — add `z3` service or build Z3 from source in builder stage; document which approach is chosen and why
- [ ] Load test: measure Cloudflare Containers cold-start and p99 latency under 10 concurrent Z3 requests; record results in `bench/z3-load-test.md`; decision gate: if p99 > 2s or scale-to-zero cold start > 5s, document migration path to self-hosted VPS

---

## Milestone 4 — Schema Alignment & Upstream Collaboration (Phase 2)

- [ ] `schemas/` — `make sync-schemas` integrated into CI via `go generate ./...`; PR fails if schema drift detected; tested with an intentional drift scenario
- [ ] wasmagent-js adapter PR — `CelGoVerifier` implementation: `methods: ["cel_expr"]`, `verify()` calls `POST /v1/verify/criterion`; submitted as standalone PR to `WasmAgent/wasmagent-js` (or own fork first); does NOT touch `packages/core`
- [x] `internal/criterion` — Go-side `Criterion` and `ConstraintIR` types generated from `schemas/*.schema.json` via `go generate`; used by `/v1/verify/criterion` handler; no hand-maintained type duplication
- [x] Evaluate `PolicyRule.evaluateAsync` upstream PR to `WasmAgent/wasmagent-js`: prototype a backwards-compatible `evaluateAsync?: (toolName, args, vetting) => Promise<InvocationDecision|undefined>` overload; submit as draft PR with benchmarks showing latency impact of pre-fetch vs. inline async
  - Go-side prototype landed in `internal/policy/policyrule.go`: `PolicyRule` with an OPTIONAL `EvaluateAsync` hook (nil = defer, preserving the pre-overload contract), `InvocationDecision`/`Vetting` mirrors of the wasmagent-js types, a `Registry` dispatcher (first-non-deferring-wins), and `Registry.Prefetch` modelling the pre-fetch vs. inline-async pattern.
  - `BenchmarkInlineVsPrefetch` in `internal/policy/policyrule_test.go` measures the latency advantage of overlapping the rule evaluation with the tool body (prefetch ≈ `max(rule, tool)` vs. inline `rule + tool`).
  - Upstream draft-PR submission to `WasmAgent/wasmagent-js` remains a cross-repo follow-up (see `cross_repo_notes` on the closing issue); the in-repo prototype is the evaluation deliverable.
