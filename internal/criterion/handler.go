package criterion

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/WasmAgent/symkernel/internal/cel"
)

// defaultTimeout bounds a single criterion evaluation.
const defaultTimeout = 5 * time.Second

// celArg is the expected shape of ConstraintIR.Arg when VerifyMethod is
// "cel_expr": a CEL boolean expression plus the variable bindings it
// references.
//
//	"arg": {"expr": "len(output) <= 200", "context": {"output": "…"}}
type celArg struct {
	Expr    string         `json:"expr"`
	Context map[string]any `json:"context"`
}

// Handler returns an http.HandlerFunc for POST /v1/verify/criterion.
//
// It decodes a VerifyCriterionRequest, dispatches on the criterion's
// verify_method, and returns a VerifyCriterionResponse. The only method wired
// today is "cel_expr", which delegates to the shared CEL evaluator; unknown
// methods yield 400 so callers learn immediately rather than silently passing.
func Handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req VerifyCriterionRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, fmt.Sprintf("invalid request: %v", err), http.StatusBadRequest)
			return
		}
		c := req.Criterion
		if c.ID == "" {
			http.Error(w, "criterion.id is required", http.StatusBadRequest)
			return
		}
		if c.VerifyMethod == "" {
			http.Error(w, "criterion.verify_method is required", http.StatusBadRequest)
			return
		}

		ctx, cancel := context.WithTimeout(r.Context(), defaultTimeout)
		defer cancel()

		resp, status, err := evaluate(ctx, c)
		if err != nil {
			http.Error(w, err.Error(), status)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}
}

// evaluate dispatches a single criterion to its verifier and shapes the
// pass/fail response. It returns the response, or an HTTP status + error when
// the request cannot be evaluated at all (bad arg, unknown method, eval error).
func evaluate(ctx context.Context, c ConstraintIR) (VerifyCriterionResponse, int, error) {
	switch c.VerifyMethod {
	case "cel_expr":
		arg, err := decodeCELArg(c.Arg)
		if err != nil {
			return VerifyCriterionResponse{}, http.StatusBadRequest, err
		}
		out, err := cel.Evaluate(ctx, arg.Expr, arg.Context)
		if err != nil {
			return VerifyCriterionResponse{}, http.StatusUnprocessableEntity,
				fmt.Errorf("cel evaluation failed: %w", err)
		}
		ok, isBool := out.(bool)
		if !isBool {
			return VerifyCriterionResponse{}, http.StatusUnprocessableEntity,
				fmt.Errorf("cel expression did not return bool, got %T", out)
		}
		return pass(c, ok), http.StatusOK, nil
	default:
		return VerifyCriterionResponse{}, http.StatusBadRequest,
			fmt.Errorf("unknown verify_method %q", c.VerifyMethod)
	}
}

// pass builds a response for a completed evaluation, attaching the
// constraint's description as the failure hint when the criterion was not met.
func pass(c ConstraintIR, ok bool) VerifyCriterionResponse {
	resp := VerifyCriterionResponse{OK: ok, CriterionID: c.ID}
	if !ok {
		resp.Hint = c.Description
	}
	return resp
}

// decodeCELArg re-decodes the opaque criterion Arg into the celArg shape.
func decodeCELArg(raw any) (celArg, error) {
	b, err := json.Marshal(raw)
	if err != nil {
		return celArg{}, fmt.Errorf("arg is not encodable: %w", err)
	}
	var a celArg
	if err := json.Unmarshal(b, &a); err != nil {
		return celArg{}, fmt.Errorf("arg is not a valid cel_expr arg: %w", err)
	}
	if a.Expr == "" {
		return celArg{}, fmt.Errorf("cel_expr arg requires a non-empty \"expr\"")
	}
	return a, nil
}
