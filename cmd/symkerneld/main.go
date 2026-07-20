// Package main is the entry point for the symkerneld server. It wires
// together the HTTP router, middleware chain (OpenTelemetry, auth), and
// route handlers for the symkernel API.
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/WasmAgent/symkernel/internal/auth"
	"github.com/WasmAgent/symkernel/internal/audit"
	"github.com/WasmAgent/symkernel/internal/cache"
	cellib "github.com/WasmAgent/symkernel/internal/cel"
	"github.com/WasmAgent/symkernel/internal/diagnostics"
	"github.com/WasmAgent/symkernel/internal/otel"
	"github.com/WasmAgent/symkernel/internal/orchestrator"
	"github.com/WasmAgent/symkernel/internal/verify"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	shutdown, err := otel.InitProvider(ctx, os.Stdout)
	if err != nil {
		log.Fatalf("otel init: %v", err)
	}
	defer func() { _ = shutdown(ctx) }()

	mux := http.NewServeMux()
	mux.Handle("POST /v1/verify/cel", cellib.Handler())
	mux.Handle("POST /v1/verify/z3", verify.Handler(&verify.Z3Solver{}))

	orch := orchestrator.NewRouter()
	orch.RegisterRoutes(mux)

	auditLog := audit.New()
	auditLog.RegisterRoutes(mux)

	cacheStore := cache.New()
	cacheStore.RegisterRoutes(mux)

	diagStore := diagnostics.New()
	diagStore.RegisterRoutes(mux)

	// Middleware chain: otel (outer) → auth (inner) → mux.
	var handler http.Handler = mux
	handler = auth.Middleware(handler)
	handler = otel.Middleware(handler)

	addr := ":8080"
	if a := os.Getenv("SYMKERNEL_ADDR"); a != "" {
		addr = a
	}

	srv := &http.Server{Addr: addr, Handler: handler}

	go func() {
		<-ctx.Done()
		srv.Close() //nolint:errcheck // graceful shutdown best-effort
	}()

	log.Printf("symkerneld listening on %s", addr)
	if err := srv.ListenAndServe(); err != http.ErrServerClosed {
		log.Fatalf("server: %v", err)
	}
}
