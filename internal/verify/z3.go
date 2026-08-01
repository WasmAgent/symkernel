package verify

import (
	"context"
	"fmt"
	"os/exec"
	"strings"

	"github.com/WasmAgent/symkernel/internal/symbolic/cache"
)

// Result represents the outcome of an SMT2 constraint verification.
type Result struct {
	// Sat is one of "sat", "unsat", or "unknown".
	Sat string `json:"sat"`

	// Model contains variable assignments when Sat is "sat", or is nil
	// for unsat and unknown results.
	Model map[string]any `json:"model"`
}

// Solver abstracts SMT solver invocation for testability.
type Solver interface {
	Solve(ctx context.Context, smt2 string) (Result, error)
}

// Z3Solver invokes the z3 SMT solver as an external process.
type Z3Solver struct{}

var z3DecisionCache = cache.NewFromEnv()

// Solve sends smt2 to the z3 binary via stdin (SMTLIB2 interactive mode)
// and parses the check-sat result and optional model output.
func (z *Z3Solver) Solve(ctx context.Context, smt2 string) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{Sat: "unknown"}, nil
	}
	if decision, ok := z3DecisionCache.Get(smt2); ok {
		return Result{Sat: decision.Sat, Model: decision.Model}, nil
	}

	cmd := exec.CommandContext(ctx, "z3", "-in")
	cmd.Stdin = strings.NewReader(smt2)

	out, err := cmd.Output()
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return Result{Sat: "unknown", Model: nil}, nil
		}
		return Result{}, fmt.Errorf("z3: %w", err)
	}

	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) == 0 {
		return Result{}, fmt.Errorf("z3: empty output")
	}

	var result Result
	switch lines[0] {
	case "sat":
		result = Result{Sat: "sat", Model: parseModel(lines[1:])}
	case "unsat":
		result = Result{Sat: "unsat", Model: nil}
	case "unknown":
		return Result{Sat: "unknown", Model: nil}, nil
	default:
		return Result{}, fmt.Errorf("z3: unexpected result %q", lines[0])
	}
	z3DecisionCache.Set(smt2, cache.Decision{Sat: result.Sat, Model: result.Model})
	return result, nil
}

// parseModel extracts variable bindings from z3 model output lines.
// It supports the simple ((var value)) format emitted by z3 -in and
// skips the SMTLIB2 model envelope markers.
func parseModel(lines []string) map[string]any {
	m := make(map[string]any)
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || line == "(model" || line == ")" {
			continue
		}
		// Strip balanced outer parentheses: ((x 6)) → x 6.
		for len(line) >= 2 && line[0] == '(' && line[len(line)-1] == ')' {
			line = line[1 : len(line)-1]
		}
		parts := strings.Fields(line)
		if len(parts) >= 2 {
			m[parts[0]] = parts[1]
		}
	}
	if len(m) == 0 {
		return nil
	}
	return m
}
