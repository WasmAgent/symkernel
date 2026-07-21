// Package smt provides Z3 SMT solver bindings for formal verification.
// It exposes a Solve function that accepts SMTLIB2 input and returns
// structured satisfiability results including models and unsat cores.
package smt

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// SMTResult captures the outcome of an SMT solver invocation.
type SMTResult struct {
	// Sat is one of "sat", "unsat", or "unknown".
	Sat string `json:"sat"`

	// Model contains variable assignments when Sat is "sat", or is nil
	// for unsat and unknown results.
	Model map[string]any `json:"model,omitempty"`

	// UnsatCore contains the names of assertions that form an unsatisfiable
	// core when Sat is "unsat", or is nil otherwise.
	UnsatCore []string `json:"unsat_core,omitempty"`
}

// Solver abstracts SMT solver invocation for testability.
type Solver interface {
	Solve(smt2 string, timeoutMs int) (SMTResult, error)
}

// Z3Solver invokes the z3 SMT solver as an external process.
type Z3Solver struct{}

// Solve sends smt2 to the z3 binary via stdin (SMTLIB2 interactive mode)
// and parses the check-sat result, optional model output, and unsat core.
// If timeoutMs is zero or negative, a default of 5000 ms is used.
func (z *Z3Solver) Solve(smt2 string, timeoutMs int) (SMTResult, error) {
	if timeoutMs <= 0 {
		timeoutMs = 5000
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeoutMs)*time.Millisecond)
	defer cancel()

	cmd := exec.CommandContext(ctx, "z3", "-in")
	cmd.Stdin = strings.NewReader(smt2)

	out, err := cmd.Output()
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return SMTResult{Sat: "unknown"}, nil
		}
		return SMTResult{}, fmt.Errorf("z3: %w", err)
	}

	return parseOutput(string(out))
}

// parseOutput parses z3 stdout into an SMTResult.
func parseOutput(output string) (SMTResult, error) {
	lines := strings.Split(strings.TrimSpace(output), "\n")
	if len(lines) == 0 {
		return SMTResult{}, fmt.Errorf("z3: empty output")
	}

	switch lines[0] {
	case "sat":
		return SMTResult{Sat: "sat", Model: parseModel(lines[1:])}, nil
	case "unsat":
		return SMTResult{Sat: "unsat", UnsatCore: parseUnsatCore(lines[1:])}, nil
	case "unknown":
		return SMTResult{Sat: "unknown"}, nil
	default:
		return SMTResult{}, fmt.Errorf("z3: unexpected result %q", lines[0])
	}
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

// parseUnsatCore extracts named assertion labels from z3 unsat-core output.
// When (get-unsat-core) is used with named assertions, z3 produces lines
// like "(assertion1 assertion2)".
func parseUnsatCore(lines []string) []string {
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || line == "()" {
			continue
		}
		// Strip balanced outer parentheses.
		for len(line) >= 2 && line[0] == '(' && line[len(line)-1] == ')' {
			line = line[1 : len(line)-1]
		}
		tokens := strings.Fields(line)
		if len(tokens) > 0 {
			return tokens
		}
	}
	return nil
}