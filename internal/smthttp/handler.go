// Package smt provides the HTTP handler for POST /v1/verify/smt.
// It accepts a JSON request with SMTLIB2 constraints and an optional variable
// model, submits them to Z3, and returns a structured result with sat status,
// model, decision_id, and solver timing.
package smt

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	intz3 "github.com/WasmAgent/symkernel/internal/z3"
	"github.com/google/uuid"
)

// SMTRequest is the request body for POST /v1/verify/smt.
type SMTRequest struct {
	// Constraints is the SMTLIB2 assertion block (without (check-sat) —
	// the handler appends solver commands automatically).
	Constraints string `json:"constraints"`
	// Model is an optional map of variable name → sort hint, used to emit
	// (declare-const) declarations before the constraints.
	Model map[string]any `json:"model,omitempty"`
	// TimeoutMs is the solver timeout in milliseconds. Defaults to 5000.
	TimeoutMs int `json:"timeout_ms,omitempty"`
}

// SMTResponse is the response body for POST /v1/verify/smt.
type SMTResponse struct {
	// Sat is "sat", "unsat", or "unknown".
	Sat bool `json:"sat"`
	// SatRaw is the raw solver result for consumers that need "unknown".
	SatRaw string `json:"sat_raw"`
	// Model contains variable assignments when Sat is true.
	Model map[string]any `json:"model,omitempty"`
	// UnsatCore contains named assertion labels when SatRaw is "unsat".
	UnsatCore []string `json:"unsat_core,omitempty"`
	// DecisionID is the per-request UUID for traceability.
	DecisionID string `json:"decision_id"`
	// SolverMs is the Z3 subprocess wall-clock time in milliseconds.
	SolverMs int64 `json:"solver_ms"`
}

// defaultTimeout is the fallback solver budget when timeout_ms is absent.
const defaultTimeout = 5 * time.Second

// Solver abstracts Z3 invocation for testability.
type Solver interface {
	Solve(constraints string, model map[string]any, timeout time.Duration) (intz3.Solution, error)
}

// defaultSolver wraps internal/z3 for production use.
type defaultSolver struct{}

func (d *defaultSolver) Solve(constraints string, model map[string]any, timeout time.Duration) (intz3.Solution, error) {
	ctx, cancel := withDuration(timeout)
	defer cancel()
	return intz3.SolveConstraintsCtx(ctx, constraints, model)
}

// Handler returns an http.HandlerFunc for POST /v1/verify/smt.
func Handler() http.HandlerFunc {
	return HandlerWithSolver(&defaultSolver{})
}

// HandlerWithSolver returns an http.HandlerFunc for POST /v1/verify/smt backed
// by the supplied Solver (used in tests).
func HandlerWithSolver(s Solver) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req SMTRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, fmt.Sprintf("invalid request: %v", err), http.StatusBadRequest)
			return
		}
		if strings.TrimSpace(req.Constraints) == "" {
			http.Error(w, "constraints is required", http.StatusBadRequest)
			return
		}

		timeout := defaultTimeout
		if req.TimeoutMs > 0 {
			timeout = time.Duration(req.TimeoutMs) * time.Millisecond
		}

		sol, err := s.Solve(req.Constraints, req.Model, timeout)
		if err != nil {
			http.Error(w, fmt.Sprintf("solver error: %v", err), http.StatusInternalServerError)
			return
		}

		resp := SMTResponse{
			Sat:        sol.Sat == "sat",
			SatRaw:     sol.Sat,
			Model:      sol.Model,
			UnsatCore:  sol.UnsatCore,
			DecisionID: uuid.NewString(),
			SolverMs:   sol.SolverMs,
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp) //nolint:errcheck
	}
}
