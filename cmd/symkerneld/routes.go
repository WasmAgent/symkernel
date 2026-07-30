package main

import (
	"net/http"

	"github.com/WasmAgent/symkernel/internal/audit"
	"github.com/WasmAgent/symkernel/internal/cache"
	cellib "github.com/WasmAgent/symkernel/internal/cel"
	"github.com/WasmAgent/symkernel/internal/composed"
	criterion "github.com/WasmAgent/symkernel/internal/criterion"
	"github.com/WasmAgent/symkernel/internal/diagnostics"
	"github.com/WasmAgent/symkernel/internal/orchestrator"
	"github.com/WasmAgent/symkernel/internal/otel"
	smthttp "github.com/WasmAgent/symkernel/internal/smthttp"
	"github.com/WasmAgent/symkernel/internal/verify"
)

// RegisterRoutes mounts the symkerneld HTTP route table onto mux. It is the
// single extension point endpoint issues edit to add routes, so main stays a
// thin lifecycle shell (otel/auth/tenant middleware, graceful shutdown). All
// routes are mounted under /v1/.
func RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/health", healthHandler)
	mux.Handle("POST /v1/verify/cel", cellib.Handler())
	mux.Handle("POST /v1/verify/z3", verify.Handler(&verify.Z3Solver{}))
	mux.Handle("POST /v1/verify/criterion", criterion.Handler())
	mux.Handle("POST /v1/verify/symbolic", verify.SymbolicHandler())

	// Milestone 12: Z3 SMT solver integration.
	mux.Handle("POST /v1/verify/smt", smthttp.Handler())
	mux.Handle("POST /v1/verify/composed", composed.Handler())

	// Prometheus metrics — exports GlobalSMTMetrics at GET /metrics.
	otel.RegisterMetricsRoute(mux)

	orchestrator.NewRouter().RegisterRoutes(mux)
	audit.New().RegisterRoutes(mux)
	cache.New().RegisterRoutes(mux)
	diagnostics.New().RegisterRoutes(mux)
}
