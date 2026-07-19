// Package verify provides the HTTP handler for the POST /v1/verify/z3
// endpoint. The endpoint accepts SMT2 constraints in an OPA-envelope
// request, submits them to a Z3 SMT solver, and returns the verification
// result (sat/unsat/unknown) with optional model data.
package verify

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/WasmAgent/symkernel/internal/otel"
)

// VerifyInput is the "input" field of the OPA-envelope request.
type VerifyInput struct {
	ConstraintsSMT2 string `json:"constraints_smt2"`
	TimeoutMs       int    `json:"timeout_ms"`
}

// opaRequest wraps the OPA-envelope request body.
type opaRequest struct {
	Input VerifyInput `json:"input"`
}

// VerifyResult is the "result" field of the OPA-envelope response.
type VerifyResult struct {
	Sat   string         `json:"sat"`
	Model map[string]any `json:"model"`
}

// opaResponse wraps the OPA-envelope response body.
type opaResponse struct {
	Result     VerifyResult `json:"result"`
	DecisionID string       `json:"decision_id"`
}

// defaultTimeoutMs is the default solver timeout when none is specified.
const defaultTimeoutMs = 2000

// Handler returns an http.HandlerFunc for the POST /v1/verify/z3 endpoint.
// It uses the provided Solver to check SMT2 constraints and returns an
// OPA-envelope response containing the result and the decision_id from the
// OpenTelemetry middleware context.
func Handler(solver Solver) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req opaRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, fmt.Sprintf("invalid request: %v", err), http.StatusBadRequest)
			return
		}

		if strings.TrimSpace(req.Input.ConstraintsSMT2) == "" {
			http.Error(w, "constraints_smt2 is required", http.StatusBadRequest)
			return
		}

		timeoutMs := req.Input.TimeoutMs
		if timeoutMs <= 0 {
			timeoutMs = defaultTimeoutMs
		}

		// Append solver commands to the user-provided constraints.
		smt2 := req.Input.ConstraintsSMT2 + "\n(check-sat)\n(get-model)\n(exit)\n"

		ctx, cancel := context.WithTimeout(r.Context(), time.Duration(timeoutMs)*time.Millisecond)
		defer cancel()

		result, err := solver.Solve(ctx, smt2)
		if err != nil {
			if ctx.Err() == context.DeadlineExceeded {
				result = Result{Sat: "unknown", Model: nil}
			} else {
				http.Error(w, fmt.Sprintf("solver error: %v", err), http.StatusInternalServerError)
				return
			}
		}

		resp := opaResponse{
			Result:     VerifyResult(result),
			DecisionID: otel.DecisionIDFromContext(r.Context()),
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp) //nolint:errcheck // partial write to ResponseWriter is unrecoverable
	}
}
