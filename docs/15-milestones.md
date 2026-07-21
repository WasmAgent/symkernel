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
- [ ] `docker-compose.yml` — local dev environment: single `symkerneld` service with env vars; `docker compose up` is the quickstart
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

- [x] `internal/smt` — Z3 binding via `go-z3` CGO: `Solve(smt2 string, timeoutMs int) (SMTResult, error)` returning `{Sat: "sat"|"unsat"|"unknown", Model: map[string]any, UnsatCore: []string}`
- [ ] `POST /v1/verify/z3` — OPA-envelope endpoint: `{"input":{"constraints_smt2":"...","timeout_ms":2000}}` → `{"result":{"sat":"sat","model":{...}},"decision_id":"uuid"}`; test cases: trivially sat, trivially unsat, timeout-induced unknown
- [ ] `internal/repair` — generate-error-fix loop: `Repair(program string, trapResult SandboxResult) (RepairPrompt, error)` assembles a structured prompt from trap details for LLM-driven fix; does NOT call LLM itself (caller injects); covered by unit test with a mock trap
- [ ] `deploy/Dockerfile` — update to include Z3 shared library (`libz3.so`) in final image; document CGO linker flags in Makefile
- [ ] `docker-compose.yml` — add `z3` service or build Z3 from source in builder stage; document which approach is chosen and why
- [ ] Load test: measure Cloudflare Containers cold-start and p99 latency under 10 concurrent Z3 requests; record results in `bench/z3-load-test.md`; decision gate: if p99 > 2s or scale-to-zero cold start > 5s, document migration path to self-hosted VPS

---

## Milestone 4 — Schema Alignment & Upstream Collaboration (Phase 2)

- [ ] `schemas/` — `make sync-schemas` integrated into CI via `go generate ./...`; PR fails if schema drift detected; tested with an intentional drift scenario
- [ ] wasmagent-js adapter PR — `CelGoVerifier` implementation: `methods: ["cel_expr"]`, `verify()` calls `POST /v1/verify/criterion`; submitted as standalone PR to `WasmAgent/wasmagent-js` (or own fork first); does NOT touch `packages/core`
- [ ] `internal/criterion` — Go-side `Criterion` and `ConstraintIR` types generated from `schemas/*.schema.json` via `go generate`; used by `/v1/verify/criterion` handler; no hand-maintained type duplication
- [x] Evaluate `PolicyRule.evaluateAsync` upstream PR to `WasmAgent/wasmagent-js`: prototype a backwards-compatible `evaluateAsync?: (toolName, args, vetting) => Promise<InvocationDecision|undefined>` overload; submit as draft PR with benchmarks showing latency impact of pre-fetch vs. inline async
  - Go-side prototype landed in `internal/policy/policyrule.go`: `PolicyRule` with an OPTIONAL `EvaluateAsync` hook (nil = defer, preserving the pre-overload contract), `InvocationDecision`/`Vetting` mirrors of the wasmagent-js types, a `Registry` dispatcher (first-non-deferring-wins), and `Registry.Prefetch` modelling the pre-fetch vs. inline-async pattern.
  - `BenchmarkInlineVsPrefetch` in `internal/policy/policyrule_test.go` measures the latency advantage of overlapping the rule evaluation with the tool body (prefetch ≈ `max(rule, tool)` vs. inline `rule + tool`).
  - Upstream draft-PR submission to `WasmAgent/wasmagent-js` remains a cross-repo follow-up (see `cross_repo_notes` on the closing issue); the in-repo prototype is the evaluation deliverable.

## Milestone 5 — Distributed Orchestration & Policy Intelligence (Phase 2)

> Multi-tier coordination and observability: symkernel becomes a intelligent verification orchestrator, not just isolated endpoints.
> Goal: enable automatic tier selection, enterprise-grade auditability, and verification efficiency at scale.

- [ ] `internal/orchestrator` — verification routing engine: `Route(query VerificationRequest) (*TierSelection, error)` analyzing query complexity, cost targets, and accuracy requirements to automatically choose CEL vs wazero vs Z3 tier; expose routing metrics via `GET /v1/orchestration/stats`
- [ ] `internal/cache` — multi-tier caching with smart invalidation: LRU cache for CEL expressions keyed by `(expr_hash, context_hash)` with TTL-based expiry and tag-based invalidation when upstream schemas drift; expose `POST /v1/admin/cache/invalidate` for ops control
- [ ] `POST /v1/verify/orchestrated` — unified verification endpoint: `{"query":{"type":"auto","expr":"...","context":{...},"options":{"maxCostMs":500,"minConfidence":0.95}}}` → routes to optimal tier automatically with `{"result":{"tier":"cel","value":...},"routing":{"selectionReason":"expr_complexity_low","alternatives":["wazero"]}}`
- [ ] `internal/audit` — enterprise-grade audit trail: immutable append-only log of all verification decisions with `decision_id`, input hash, tier selected, result, and timestamp; implement log rotation and retention policies; export via `GET /v1/audit/export?format=jsonl&from=<timestamp>`
- [ ] `internal/diagnostics` — verification explainability: for failed verifications, generate structured explanations (`WhyFailed{"constraint":"max_memory","actual":256,"limit":128,"remediation":"reduce_allocation_or_increase_limit"}`); expose via `GET /v1/diagnostics/<decision_id>`
- [ ] `internal/batch` — bulk verification API: `POST /v1/verify/batch` accepting up to 1000 verification requests in a single payload; process in parallel with worker pool; return aggregated results with per-item status and batch-level timing statistics
- [ ] `deploy/helm/` — Kubernetes deployment charts: Helm templates for production deployment with HPA (horizontal pod autoscaling), resource limits, pod disruption budgets, and ConfigMap/Secret management for multi-environment configs
- [ ] `internal/metrics` — enhanced Prometheus metrics: expose tier selection counts, cache hit/miss ratios, per-endpoint latency histograms, and verification outcome counts; documented in `metrics/README.md` with Grafana dashboard queries

This milestone transforms symkernel from isolated verification endpoints into an intelligent, production-grade orchestration layer that automatically optimizes for cost, accuracy, and operational scale—addressing the natural next question after "can we verify?" which is "how do we verify efficiently and reliably at scale?"

## Milestone 6 — Policy Composition & Developer Experience (Phase 2)

> **From verification primitive to verification platform:** Enable sophisticated multi-tier policies and frictionless local development.

- [ ] `internal/compose` — Policy composition engine: evaluate `CEL → wazero → Z3` tier chains with configurable fallback (`any_pass`, `all_pass`, `short_circuit`) and timeout budgets per tier; expose `POST /v1/verify/composed` endpoint accepting `{"tiers":[{"type":"cel","expr":"..."},{"type":"wazero","module":"..."}],"mode":"any_pass"}`
- [ ] `internal/cache` — Multi-layer caching: L1 in-process LRU for hot CEL expressions (configurable TTL via `SYMKERNEL_CACHE_TTL_SEC`), L2 Redis-backed optional cache for cross-instance wazero compilation results; cache key = `SHA256(expr + context schema)`; invalidate on schema version bump
- [ ] `internal/explain` — Verification trace explainer: attach structured trace to every response showing which tier fired, why subsequent tiers skipped (or executed), and intermediate values; return in `trace` field aligning with OpenTelemetry decision spans; enable `?explain=true` query param
- [ ] `cmd/symk` — CLI tool for local development: `symk verify cel --expr "input.age > 18" --context ctx.json` runs offline using embedded CEL evaluator; `symk test-policy --dir policies/` batches `.yaml` policy files against fixture contexts; `symk doctor` validates connectivity and auth against remote `symkerneld`
- [ ] `policies/` — Policy definition framework (optional library): YAML DSL for composed policies with metadata, tiers, and test fixtures; `symk validate-policy` checks schema compliance; generates OpenAPI snippets for each policy
- [ ] `internal/batch` — Batch verification endpoint: `POST /v1/verify/batch` accepting `{"items":[{"expr":"...","context":{...}}...]}` → `{"results":[...],"totalMs":12}` with parallel execution (configurable concurrency limit `SYMKERNEL_BATCH_CONCURRENCY`); returns partial results on timeout with `timeout: true` flag
- [ ] `api/openapi.yaml` — Extended spec: add `/v1/verify/composed`, `/v1/verify/batch`, and `trace` response field definitions; include example policy composition YAMLs and batch request patterns
- [ ] `bench/` — Composition tier benchmarks: compare end-to-end latency for single-tier CEL vs. three-tier chains under cache cold/warm conditions; measure fallback behavior impact on tail latency (p95, p99); output trace examples for each path
- [ ] README — Add "Composed Policies" section: walkthrough of `any_pass` vs. `all_pass` modes, trace interpretation, and quickstart for `symk` CLI; include batch verification example for bulk validation workflows

## Milestone 7 — Distributed Execution & Performance Optimization (Phase 3)

> Production-scale capabilities: turn a single-instance service into a horizontally-scalable platform with intelligent caching and query optimization.

- [ ] `internal/cache` — multi-tier caching layer: in-memory LRU for hot CEL expressions (1min TTL) with optional Redis backend for cross-instance cache invalidation; cache key includes expr hash + context schema fingerprint
- [ ] `internal/router` — query classification router: classify incoming requests by complexity (simple CEL vs. WASM vs. Z3) and route to specialized worker pools; add `POST /v1/verify` automatic endpoint selection
- [ ] `internal/optimizer` — CEL expression optimizer: constant folding, dead branch elimination, and common subexpression elimination using cel-go's AST inspection; benchmark 15-30% latency reduction on typical policy workloads
- [ ] `deploy/helm/` — Kubernetes Helm chart: horizontal pod autoscaling based on CPU/memory custom metrics, rolling deployment config, and pod disruption budgets
- [ ] `POST /v1/batch` — bulk verification endpoint: accept up to 100 Criterion/ConstraintIR requests in a single JSON array; return aggregated results with per-item status; reduce round-trip overhead for batch workloads
- [ ] `internal/queue` — async work queue for long-running Z3 proofs: enqueue requests >5s timeout, return `decision_id` immediately, add `GET /v1/results/:decision_id` polling endpoint; integrate with Cloudflare Queues or RabbitMQ
- [ ] `internal/metrics` — Prometheus metrics exposition: `/metrics` endpoint with counter/histogram for request latency by tier, cache hit/miss rates, and queue depth; include Grafana dashboard JSON
- [ ] Chaos testing harness: `test/chaos/` package with simulated instance failures during batch operations; verify graceful degradation and cache consistency; add to CI pipeline

---

## Milestone 8 — Production Scale & Reliability (Phase 4)

> Transform from functional prototype to production-grade verification service.
> Goal: enable horizontal scale, minimize tail latency, and provide enterprise reliability guarantees.

- [ ] `internal/cache` — multi-tier caching layer: in-memory LRU for hot CEL expressions (~10k entries, 5min TTL), Redis-backed optional cache for cross-instance deduplication; cache keys hash expr + context schema; invalidate on type errors
- [ ] `internal/ratelimit` — token-bucket rate limiter per client token: configurable QPS/burst via `SYMKERNEL_RATE_LIMIT_QPS` and `SYMKERNEL_RATE_LIMIT_BURST`; return `429 Too Many Requests` with `Retry-After` header; pass-through for unauthenticated local dev
- [ ] `internal/batcher` — request batching for Z3 proofs: accumulate up to 16 satisfiability checks or 50ms window, emit single Z3 `check-sat` batch; reduces per-request overhead by ~8x for high-volume verification pipelines
- [ ] `deploy/kubernetes/` — production deployment manifests: `Deployment` with HPA (2–20 pods targeting 70% CPU), `PodDisruptionBudget` (min 2 available), `Service` with `loadBalancer` spec, `ConfigMap` for env var injection
- [ ] `internal/health` — hierarchical health endpoints: `/healthz` (liveness — returns 200 if server responding), `/healthz/ready` (readiness — checks Z3/wazero warmup, cache connectivity), `/healthz/deep` (dependency ping — CelExprParser compile test, wazero.CompileModule, Z3 simple satisfiability)
- [ ] `internal/tracing` — decision logging: append-only write of `decision_id`, expr, context hash, result, latency to `$SYMKERNEL_DECISION_LOG_PATH` (default `/var/log/symkernel/decisions.jsonl`); optional daily S3 upload via `SYMKERNEL_DECISION_LOG_BUCKET`; rotation at 100MB
- [ ] `internal/circuitbreaker` — Z3 timeout protection: after 3 consecutive timeouts >30s in 60s window, open circuit for 90s, return fast-fail `503 Service Unavailable` with `{\"error\":\"Z3 circuit open\"}`; half-open state allows single probe request
- [ ] `deploy/terraform/` — infrastructure as code for cloud deployment: Terraform modules for Cloudflare Containers (existing), GKE + CloudSQL (optional Redis), and AWS EKS + ElastiCache; outputs include service endpoint and monitoring dashboard URLs
- [ ] `bench/latency-slos` — SLO benchmark harness: measure p50/p95/p99 latency across 10k requests at 10/100/1000 QPS; validate targets: p50 < 20ms (CEL), p95 < 100ms (wazero), p99 < 500ms (Z3); fail CI if regressions > 15% above baseline
- [ ] README production — operational runbook: deployment checklist, environment variable reference, health check interpretation, circuit breaker recovery procedures, decision log analysis (sample `jq` one-liners), and capacity planning guidelines

## Milestone 9 — Multi-Tenant Isolation & Performance Caching

> Enterprise readiness: transform from single-service to multi-organization platform.
> Goal: enable secure multi-tenancy with per-tenant resource isolation and intelligent result caching for production scale.

- [ ] `internal/tenant` — Tenant resolver middleware: extract tenant ID from JWT `sub` claim or `X-Tenant-ID` header; validate against `SYMKERNEL_ALLOWED_TENANTS` env var (comma-separated allowlist); propagate `tenant_id` through span attributes and response metadata
- [ ] `internal/cache` — Redis-backed result cache: `CacheKey(expr, context_hash, tenant_id)` with TTL from `SYMKERNEL_CACHE_TTL`; cache hits return immediately with `cached: true` flag; warm-up script `make warm-cache` for frequently-used CEL expressions from `bench/` fixtures
- [ ] `POST /v1/verify/cel` enhancement — add `?no-cache=true` query param to bypass cache for critical decisions; return `X-Cache-Status: hit/miss/bypass` response header
- [ ] `internal/quota` — Per-tenant rate limiting: `SYMKERNEL_TENANT_QUOTA_DEFAULT` and `SYMKERNEL_TENANT_QUOTA_OVERRIDES` (JSON map of tenant_id → requests_per_second); return 429 with `X-RateLimit-Tenant` and `Retry-After` headers
- [ ] `internal/isolation` — Tenant-scoped wazero sandboxes: separate memory limits per tenant via `SYMKERNEL_TENANT_MEMORY_LIMITS` JSON config; sandbox isolation prevents cross-tenant memory leaks; telemetry logs per-tenant memory usage
- [ ] `POST /v1/tenant/usage` — Usage analytics endpoint: `{"tenant_id":"...","from":"2026-07-01","to":"2026-07-22"}` → `{"totalRequests":1234,"cacheHitRate":0.42,"avgLatencyMs":45,"quotaExceeded":true}`; requires admin token (`SYMKERNEL_ADMIN_TOKEN`)
- [ ] `internal/metrics` — Per-tenant Prometheus metrics: `symkernel_requests_total{tenant_id, endpoint, status}`, `symkernel_cache_hits_total{tenant_id}`, `symkernel_quota_exceeded_total{tenant_id}`; scrape endpoint at `GET /metrics`
- [ ] `deploy/tenant-config.yaml` — Example tenant configuration manifest showing env var setup for 3-tenant deployment (org-a, org-b, org-c) with different quotas and memory limits; documented in README `## Multi-Tenancy` section
- [ ] `bench/multi-tenant` — Load testing harness: ` artillery run tenant-load-test.yml` simulates 5 concurrent tenants with different request patterns; validates cache isolation, quota enforcement, and sandbox memory limits
- [ ] `api/openapi.yaml` updates — Add tenant headers, cache headers, rate limit error codes (429), and `/v1/tenant/usage` endpoint with admin security scheme
- [ ] `docs/multi-tenant-deployment.md` — Deployment guide for Cloudflare Containers with per-tenant service instances or isolated environments; covers Redis configuration (Upstash), quota tuning, and monitoring dashboards
