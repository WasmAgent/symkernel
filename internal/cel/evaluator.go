// Package cel provides a cel-go expression evaluator for the symkernel
// symbolic verification kernel. It wraps CEL program compilation and
// evaluation with per-request timeout support.
package cel

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/ext"

	"github.com/WasmAgent/symkernel/internal/otel"
)

// defaultTimeoutMs is the default evaluation timeout when the caller
// passes a context with no deadline.
const defaultTimeoutMs = 2000

// Evaluator holds a CEL environment and program for expression parsing,
// compilation, and validation. Use NewEvaluator to create an instance, then
// call Evaluate to assess CEL expressions against a map of arguments.
type Evaluator struct {
	env    *cel.Env
	prg    cel.Program
}

// NewEvaluator initialises an Evaluator with a base CEL environment that
// includes the standard string, list, and math extensions.
func NewEvaluator() (*Evaluator, error) {
	env, err := cel.NewEnv(
		ext.Strings(),
		ext.Lists(),
		ext.Math(),
	)
	if err != nil {
		return nil, fmt.Errorf("cel: new evaluator: %w", err)
	}
	return &Evaluator{env: env}, nil
}

// Evaluate parses, validates, compiles, and evaluates a CEL expression
// against the provided args map. Variable types are inferred from the args
// map so expressions can reference them by name. The compiled program is
// stored on the Evaluator for potential reuse.
//
// It returns the boolean result of the expression, or an error for invalid
// syntax, type mismatches, or evaluation failures.
func (e *Evaluator) Evaluate(expr string, args map[string]interface{}) (bool, error) {
	// Build a per-call environment with variable declarations inferred
	// from the args map so expressions can reference them by name.
	opts := []cel.EnvOption{
		ext.Strings(),
		ext.Lists(),
		ext.Math(),
	}
	for name, val := range args {
		opts = append(opts, cel.Variable(name, celTypeOf(val)))
	}

	env, err := cel.NewEnv(opts...)
	if err != nil {
		return false, fmt.Errorf("cel: create env: %w", err)
	}

	ast, iss := env.Compile(expr)
	if iss.Err() != nil {
		return false, fmt.Errorf("cel: compile: %w", iss.Err())
	}

	prg, err := env.Program(ast)
	if err != nil {
		return false, fmt.Errorf("cel: program: %w", err)
	}

	// Store the compiled program on the struct.
	e.prg = prg

	out, _, err := prg.Eval(args)
	if err != nil {
		return false, fmt.Errorf("cel: eval: %w", err)
	}

	result, ok := out.Value().(bool)
	if !ok {
		return false, fmt.Errorf("cel: expected bool result, got %T", out.Value())
	}

	return result, nil
}

// Evaluate compiles and evaluates a CEL expression against the provided
// context variables. Variables in vars are auto-declared in the CEL
// environment based on their Go types, so expressions can reference them
// directly (e.g. Evaluate(ctx, "x + y", map[string]any{"x": 1, "y": 2})).
//
// It returns the unwrapped evaluation result or an error if compilation
// or evaluation fails (including timeout).
//
// The ctx parameter controls the evaluation lifetime: pass a context
// with a deadline to enforce a per-request timeout. If ctx has no
// deadline, the default timeout (2s) is applied.
func Evaluate(ctx context.Context, expr string, vars map[string]any) (any, error) {
	// Build environment options: declare variables inferred from vars,
	// and enable standard extensions (strings, lists, maps, etc.).
	opts := []cel.EnvOption{
		ext.Strings(),
		ext.Lists(),
		ext.Math(),
	}
	for name, val := range vars {
		opts = append(opts, cel.Variable(name, celTypeOf(val)))
	}

	env, err := cel.NewEnv(opts...)
	if err != nil {
		return nil, fmt.Errorf("cel: create env: %w", err)
	}

	ast, iss := env.Compile(expr)
	if iss.Err() != nil {
		return nil, fmt.Errorf("cel: compile: %w", iss.Err())
	}

	prg, err := env.Program(ast)
	if err != nil {
		return nil, fmt.Errorf("cel: program: %w", err)
	}

	// Apply a default timeout if the caller's context has no deadline.
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(defaultTimeoutMs)*time.Millisecond)
		defer cancel()
	}

	out, _, err := prg.Eval(vars)
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return nil, fmt.Errorf("cel: evaluation timeout: %w", ctx.Err())
		}
		return nil, fmt.Errorf("cel: eval: %w", err)
	}

	return out.Value(), nil
}

// celTypeOf maps a Go value to a CEL type for variable declaration.
// Unrecognised types default to cel.DynType so expressions can still
// reference them.
func celTypeOf(v any) *cel.Type {
	switch v.(type) {
	case bool:
		return cel.BoolType
	case int, int32, int64:
		return cel.IntType
	case uint, uint32, uint64:
		return cel.UintType
	case float32, float64:
		return cel.DoubleType
	case string:
		return cel.StringType
	default:
		// Slices and maps are represented as list/dyn in CEL.
		return cel.DynType
	}
}

// CELInput is the "input" field of the OPA-envelope request for the
// POST /v1/verify/cel endpoint.
type CELInput struct {
	Expr      string         `json:"expr"`
	Context   map[string]any `json:"context"`
	TimeoutMs int            `json:"timeout_ms"`
}

// celOpaRequest wraps the OPA-envelope request body.
type celOpaRequest struct {
	Input CELInput `json:"input"`
}

// CELResult is the "result" field of the OPA-envelope response.
type CELResult struct {
	Ok     bool    `json:"ok"`
	Value  any     `json:"value"`
	EvalMs float64 `json:"evalMs"`
}

// celOpaResponse wraps the OPA-envelope response body.
type celOpaResponse struct {
	Result     CELResult `json:"result"`
	DecisionID string    `json:"decision_id"`
}

// Handler returns an http.HandlerFunc for the POST /v1/verify/cel endpoint.
// It accepts an OPA-envelope request with a CEL expression and optional
// context variables, evaluates it using Evaluate, and returns an
// OPA-envelope response with the result, decision_id from the OpenTelemetry
// middleware context, and evaluation timing.
func Handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req celOpaRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, fmt.Sprintf("invalid request: %v", err), http.StatusBadRequest)
			return
		}

		if strings.TrimSpace(req.Input.Expr) == "" {
			http.Error(w, "expr is required", http.StatusBadRequest)
			return
		}

		timeoutMs := req.Input.TimeoutMs
		if timeoutMs <= 0 {
			timeoutMs = defaultTimeoutMs
		}

		// Normalise JSON-decoded float64 values to int64 when they are
		// whole numbers, so that CEL type inference maps them to int
		// rather than double. This avoids overload errors when expressions
		// mix context variables with int literals (e.g. x + 1).
		normalizeContext(req.Input.Context)

		ctx, cancel := context.WithTimeout(r.Context(), time.Duration(timeoutMs)*time.Millisecond)
		defer cancel()

		start := time.Now()
		result, err := Evaluate(ctx, req.Input.Expr, req.Input.Context)
		elapsed := time.Since(start)
		evalMs := float64(elapsed) / float64(time.Millisecond)

		resp := celOpaResponse{
			DecisionID: otel.DecisionIDFromContext(r.Context()),
		}
		if err != nil {
			resp.Result = CELResult{Ok: false, Value: err.Error(), EvalMs: evalMs}
		} else {
			resp.Result = CELResult{Ok: true, Value: result, EvalMs: evalMs}
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp) //nolint:errcheck // partial write to ResponseWriter is unrecoverable
	}
}

// normalizeContext converts JSON-decoded float64 values to int64 when they
// have no fractional part, so that CEL type inference maps them to
// cel.IntType instead of cel.DoubleType. This matches user expectations when
// sending integer context values via JSON.
func normalizeContext(m map[string]any) {
	for k, v := range m {
		if f, ok := v.(float64); ok {
			if f == math.Trunc(f) && !math.IsInf(f, 0) && !math.IsNaN(f) {
				m[k] = int64(f)
			}
		}
	}
}
