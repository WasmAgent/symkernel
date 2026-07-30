// Package composed provides the HTTP handler for POST /v1/verify/composed.
// It accepts a multi-stage policy chain (cel, wasm, smt) and evaluates each
// stage sequentially, short-circuiting on the first failure. Each stage
// produces an independent decision_id and an optional diagnostic hint.
package composed

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/WasmAgent/symkernel/internal/cel"
	intz3 "github.com/WasmAgent/symkernel/internal/z3"
	"github.com/google/uuid"
)

// StageSpec is the opaque per-stage configuration passed through from the
// caller. Its semantics depend on the stage type.
type StageSpec struct {
	// Expression is used for the "cel" stage.
	Expression string `json:"expression,omitempty"`
	// Variables is used for the "cel" stage variable bindings.
	Variables map[string]any `json:"variables,omitempty"`
	// Constraints is used for the "smt" stage.
	Constraints string `json:"constraints,omitempty"`
	// Model is used for the "smt" stage variable declarations.
	Model map[string]any `json:"model,omitempty"`
	// TimeoutMs applies to any stage. Defaults to 5000.
	TimeoutMs int `json:"timeout_ms,omitempty"`
	// Result is used for the stub "wasm" stage.
	Result *bool `json:"result,omitempty"`
}

// PolicyStage is one stage in a composed policy.
type PolicyStage struct {
	// Stage is "cel", "wasm", or "smt".
	Stage string `json:"stage"`
	// Spec contains stage-specific configuration.
	Spec StageSpec `json:"spec"`
}

// ComposedRequest is the request body for POST /v1/verify/composed.
type ComposedRequest struct {
	// Policies is the ordered list of stages to evaluate.
	Policies []PolicyStage `json:"policies"`
	// Input is the request-scoped variable bag passed to each stage that
	// accepts it (cel variables, smt model enrichment, etc.).
	Input map[string]any `json:"input,omitempty"`
}

// StageReport is the per-stage evaluation result included in ComposedReport.
type StageReport struct {
	// Stage is the stage type that produced this report.
	Stage string `json:"stage"`
	// OK is true when this stage passed.
	OK bool `json:"ok"`
	// DecisionID is the per-stage UUID.
	DecisionID string `json:"decision_id"`
	// Hint is an optional diagnostic message produced by the stage.
	Hint string `json:"hint,omitempty"`
	// EvalMs is the wall-clock time for this stage in milliseconds.
	EvalMs int64 `json:"eval_ms"`
}

// ComposedReport aggregates all stage results.
type ComposedReport struct {
	// Stages holds one entry per policy stage, in evaluation order.
	Stages []StageReport `json:"stages"`
	// DecisionID is the top-level UUID for the composed request.
	DecisionID string `json:"decision_id"`
}

// ComposedResponse is the response body for POST /v1/verify/composed.
type ComposedResponse struct {
	// OK is true when all stages passed.
	OK bool `json:"ok"`
	// Report contains the per-stage details.
	Report ComposedReport `json:"report"`
	// EvalMs is the total wall-clock time for all stages in milliseconds.
	EvalMs int64 `json:"eval_ms"`
}

// Handler returns an http.HandlerFunc for POST /v1/verify/composed.
func Handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req ComposedRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, fmt.Sprintf("invalid request: %v", err), http.StatusBadRequest)
			return
		}
		if len(req.Policies) == 0 {
			http.Error(w, "policies must not be empty", http.StatusBadRequest)
			return
		}

		total := time.Now()
		reports := make([]StageReport, 0, len(req.Policies))
		allOK := true

		for _, p := range req.Policies {
			sr := evalStage(r.Context(), p, req.Input)
			reports = append(reports, sr)
			if !sr.OK {
				allOK = false
				break // short-circuit on first failure
			}
		}

		resp := ComposedResponse{
			OK: allOK,
			Report: ComposedReport{
				Stages:     reports,
				DecisionID: uuid.NewString(),
			},
			EvalMs: time.Since(total).Milliseconds(),
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp) //nolint:errcheck
	}
}

// evalStage dispatches to the appropriate stage evaluator.
func evalStage(ctx context.Context, p PolicyStage, input map[string]any) StageReport {
	start := time.Now()
	decisionID := uuid.NewString()

	switch strings.ToLower(p.Stage) {
	case "cel":
		return evalCELStage(ctx, p.Spec, input, decisionID, start)
	case "smt":
		return evalSMTStage(ctx, p.Spec, input, decisionID, start)
	case "wasm":
		return evalWasmStage(p.Spec, decisionID, start)
	default:
		return StageReport{
			Stage:      p.Stage,
			OK:         false,
			DecisionID: decisionID,
			Hint:       fmt.Sprintf("unknown stage type %q; supported: cel, wasm, smt", p.Stage),
			EvalMs:     time.Since(start).Milliseconds(),
		}
	}
}

// evalCELStage evaluates a CEL expression stage.
func evalCELStage(ctx context.Context, spec StageSpec, input map[string]any, decisionID string, start time.Time) StageReport {
	if strings.TrimSpace(spec.Expression) == "" {
		return StageReport{
			Stage:      "cel",
			OK:         false,
			DecisionID: decisionID,
			Hint:       "spec.expression is required for cel stage",
			EvalMs:     time.Since(start).Milliseconds(),
		}
	}

	// Merge stage-level variables with request-scoped input.
	vars := make(map[string]any)
	for k, v := range input {
		vars[k] = v
	}
	for k, v := range spec.Variables {
		vars[k] = v
	}

	timeoutMs := spec.TimeoutMs
	if timeoutMs <= 0 {
		timeoutMs = 5000
	}
	ctx2, cancel := context.WithTimeout(ctx, time.Duration(timeoutMs)*time.Millisecond)
	defer cancel()
	_ = ctx2 // cel evaluator uses its own timeout logic

	eval, err := cel.NewEvaluator()
	if err != nil {
		return StageReport{
			Stage:      "cel",
			OK:         false,
			DecisionID: decisionID,
			Hint:       fmt.Sprintf("cel: init: %v", err),
			EvalMs:     time.Since(start).Milliseconds(),
		}
	}

	ok, err := eval.Evaluate(spec.Expression, vars)
	hint := ""
	if err != nil {
		hint = fmt.Sprintf("cel: %v", err)
	}
	return StageReport{
		Stage:      "cel",
		OK:         ok && err == nil,
		DecisionID: decisionID,
		Hint:       hint,
		EvalMs:     time.Since(start).Milliseconds(),
	}
}

// evalSMTStage evaluates an SMT constraint stage using Z3.
func evalSMTStage(ctx context.Context, spec StageSpec, input map[string]any, decisionID string, start time.Time) StageReport {
	if strings.TrimSpace(spec.Constraints) == "" {
		return StageReport{
			Stage:      "smt",
			OK:         false,
			DecisionID: decisionID,
			Hint:       "spec.constraints is required for smt stage",
			EvalMs:     time.Since(start).Milliseconds(),
		}
	}

	// Merge stage-level model with request-scoped input for variable hints.
	model := make(map[string]any)
	for k, v := range spec.Model {
		model[k] = v
	}
	// Input values that look like sort hints contribute to the model.
	for k, v := range input {
		if _, ok := model[k]; !ok {
			model[k] = v
		}
	}

	timeoutMs := spec.TimeoutMs
	if timeoutMs <= 0 {
		timeoutMs = 5000
	}
	solveCtx, cancel := context.WithTimeout(ctx, time.Duration(timeoutMs)*time.Millisecond)
	defer cancel()

	sol, err := intz3.SolveConstraintsCtx(solveCtx, spec.Constraints, model)
	if err != nil {
		return StageReport{
			Stage:      "smt",
			OK:         false,
			DecisionID: decisionID,
			Hint:       fmt.Sprintf("smt: %v", err),
			EvalMs:     time.Since(start).Milliseconds(),
		}
	}

	hint := ""
	if sol.Sat == "unsat" && len(sol.UnsatCore) > 0 {
		hint = fmt.Sprintf("unsat-core: %s", strings.Join(sol.UnsatCore, ", "))
	} else if sol.Sat == "unknown" {
		hint = "solver returned unknown (timeout or out-of-resources)"
	}

	return StageReport{
		Stage:      "smt",
		OK:         sol.Sat == "sat",
		DecisionID: decisionID,
		Hint:       hint,
		EvalMs:     time.Since(start).Milliseconds(),
	}
}

// evalWasmStage is a stub for the wasm stage. The wasm sandbox stage is
// exercised by the dedicated POST /v1/sandbox/run endpoint; in the composed
// pipeline it delegates to the spec.result field when present, enabling callers
// to pre-evaluate wasm constraints and inject the decision. When spec.result is
// nil it returns OK=true as a pass-through to allow composed policies that omit
// the wasm stage.
func evalWasmStage(spec StageSpec, decisionID string, start time.Time) StageReport {
	ok := true
	hint := ""
	if spec.Result != nil {
		ok = *spec.Result
		if !ok {
			hint = "wasm stage: result=false (pre-evaluated by caller)"
		}
	} else {
		hint = "wasm stage: no pre-evaluated result; pass-through"
	}
	return StageReport{
		Stage:      "wasm",
		OK:         ok,
		DecisionID: decisionID,
		Hint:       hint,
		EvalMs:     time.Since(start).Milliseconds(),
	}
}
