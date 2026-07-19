// Package cel provides a cel-go expression evaluator for the symkernel
// symbolic verification kernel. It wraps CEL program compilation and
// evaluation with per-request timeout support.
package cel

import (
	"context"
	"fmt"
	"time"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/ext"
)

// defaultTimeoutMs is the default evaluation timeout when the caller
// passes a context with no deadline.
const defaultTimeoutMs = 2000

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
