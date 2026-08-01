// Package z3 provides a Go-Z3 bindings bridge for formal constraint solving.
// It wraps the Z3 SMT solver subprocess with SMTLIB2 translation support for
// symbolic variable representations, covering linear arithmetic, bitvectors,
// and array theories.
package z3

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/WasmAgent/symkernel/internal/symbolic/cache"
)

// Solution is the outcome of SolveConstraints.
type Solution struct {
	// Sat is "sat", "unsat", or "unknown".
	Sat string `json:"sat"`
	// Model contains variable assignments when Sat is "sat", keyed by name.
	Model map[string]any `json:"model,omitempty"`
	// UnsatCore holds named assertion labels forming the minimal unsat core
	// when Sat is "unsat" and the input used named assertions.
	UnsatCore []string `json:"unsat_core,omitempty"`
	// SolverMs is the elapsed wall-clock time in the Z3 subprocess, in
	// milliseconds.
	SolverMs int64 `json:"solver_ms"`
}

var decisionCache = cache.NewFromEnv()

// SolveConstraints submits an SMTLIB2 constraint string to Z3 and returns the
// result. model is an optional map of variable name → sort hint (or concrete
// Go value) used to emit (declare-const) declarations before the constraints.
//
// Supported sort hints (string values):
//   - "Int", "Bool", "Real"      — standard SMT sorts
//   - "BitVec_N"                 → (_ BitVec N) bitvectors of width N
//   - "Array_K_V"                → (Array K V) with key sort K and value sort V
//   - any other non-empty string — passed verbatim as an SMTLIB2 sort name
//
// Go bool/int/float values are also accepted; their sorts are inferred.
//
// A 5-second default timeout is applied. Use SolveConstraintsCtx for a
// caller-controlled deadline.
func SolveConstraints(constraints string, model map[string]any) (Solution, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return SolveConstraintsCtx(ctx, constraints, model)
}

// SolveConstraintsCtx is like SolveConstraints but honours the caller's ctx.
func SolveConstraintsCtx(ctx context.Context, constraints string, model map[string]any) (Solution, error) {
	smt2 := buildSMT2(constraints, model)
	if err := ctx.Err(); err != nil {
		return Solution{Sat: "unknown"}, nil
	}
	if decision, ok := decisionCache.Get(smt2); ok {
		return solutionFromDecision(decision), nil
	}

	start := time.Now()
	cmd := exec.CommandContext(ctx, "z3", "-in")
	cmd.Stdin = strings.NewReader(smt2)

	// Capture stdout into a buffer so we can read it even when z3 exits with
	// a non-zero code (e.g., when (get-model) is called on an unsat result).
	var stdout bytes.Buffer
	cmd.Stdout = &stdout

	runErr := cmd.Run()
	solverMs := time.Since(start).Milliseconds()

	if runErr != nil {
		// Context cancellation or deadline: return unknown rather than error.
		if ctx.Err() != nil {
			return Solution{Sat: "unknown", SolverMs: solverMs}, nil
		}
		// Z3 may exit non-zero when a follow-up command (e.g., (get-model))
		// is invalid for the current result. Attempt to parse whatever stdout
		// we captured; if parseable, treat it as success.
		if stdout.Len() > 0 {
			sol, parseErr := parseOutput(stdout.String())
			if parseErr == nil {
				sol.SolverMs = solverMs
				if sol.Sat != "unknown" {
					decisionCache.Set(smt2, decisionFromSolution(sol))
				}
				return sol, nil
			}
		}
		return Solution{}, fmt.Errorf("z3: %w", runErr)
	}

	sol, parseErr := parseOutput(stdout.String())
	if parseErr != nil {
		return Solution{}, parseErr
	}
	sol.SolverMs = solverMs
	if sol.Sat != "unknown" {
		decisionCache.Set(smt2, decisionFromSolution(sol))
	}
	return sol, nil
}

func decisionFromSolution(solution Solution) cache.Decision {
	return cache.Decision{
		Sat:       solution.Sat,
		Model:     solution.Model,
		UnsatCore: solution.UnsatCore,
	}
}

func solutionFromDecision(decision cache.Decision) Solution {
	return Solution{
		Sat:       decision.Sat,
		Model:     decision.Model,
		UnsatCore: decision.UnsatCore,
	}
}

// hasNamedAssertions reports whether the constraints string uses SMTLIB2
// named assertions (:named keyword), which enables unsat-core extraction.
func hasNamedAssertions(constraints string) bool {
	return strings.Contains(constraints, ":named")
}

// buildSMT2 assembles the full SMTLIB2 program by prepending (set-option) and
// (declare-const) statements for the model map, then the caller's constraints,
// then (check-sat), conditionally (get-model) or (get-unsat-core), and (exit).
//
// (get-model) and (get-unsat-core) are not both appended unconditionally:
// z3 exits with an error when (get-model) is called after an unsat result, or
// (get-unsat-core) after a sat result. Instead, the parser handles error lines
// gracefully, but we still avoid the non-zero exit when possible by only
// including the command most likely to succeed.
func buildSMT2(constraints string, model map[string]any) string {
	var b strings.Builder
	// Enable unsat-core production when the constraints use :named assertions.
	if hasNamedAssertions(constraints) {
		b.WriteString("(set-option :produce-unsat-cores true)\n")
	}

	names := make([]string, 0, len(model))
	for name := range model {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		val := model[name]
		sort := inferSort(val)
		fmt.Fprintf(&b, "(declare-const %s %s)\n", name, sort)
	}

	b.WriteString(constraints)
	b.WriteString("\n(check-sat)\n(get-model)\n")
	if hasNamedAssertions(constraints) {
		b.WriteString("(get-unsat-core)\n")
	}
	b.WriteString("(exit)\n")
	return b.String()
}

// inferSort maps a Go value to an SMTLIB2 sort string.
func inferSort(v any) string {
	switch x := v.(type) {
	case string:
		switch {
		case strings.HasPrefix(x, "BitVec_"):
			n := strings.TrimPrefix(x, "BitVec_")
			return fmt.Sprintf("(_ BitVec %s)", n)
		case strings.HasPrefix(x, "Array_"):
			parts := strings.SplitN(strings.TrimPrefix(x, "Array_"), "_", 2)
			if len(parts) == 2 {
				return fmt.Sprintf("(Array %s %s)", parts[0], parts[1])
			}
		}
		if x != "" {
			return x
		}
		return "Int"
	case bool:
		return "Bool"
	case int, int8, int16, int32, int64,
		uint, uint8, uint16, uint32, uint64:
		return "Int"
	case float32, float64:
		return "Real"
	default:
		return "Int"
	}
}

// parseOutput parses z3 stdout into a Solution. It understands both the
// compact ((var value)) format and the define-fun multi-line model format
// emitted by Z3 4.x in interactive (-in) mode. Error lines from z3 (e.g.,
// "(error "model is not available")") are silently skipped so that a non-zero
// exit code does not prevent result extraction.
func parseOutput(output string) (Solution, error) {
	lines := strings.Split(strings.TrimSpace(output), "\n")
	if len(lines) == 0 || (len(lines) == 1 && lines[0] == "") {
		return Solution{}, fmt.Errorf("z3: empty output")
	}

	switch lines[0] {
	case "sat":
		return Solution{Sat: "sat", Model: parseModel(lines[1:])}, nil
	case "unsat":
		return Solution{Sat: "unsat", UnsatCore: parseUnsatCore(lines[1:])}, nil
	case "unknown":
		return Solution{Sat: "unknown"}, nil
	default:
		return Solution{}, fmt.Errorf("z3: unexpected result %q", lines[0])
	}
}

// parseModel extracts variable bindings from the (get-model) output.
//
// It handles two formats emitted by Z3 4.x in interactive (-in) mode:
//
// Compact ((var value)) format:
//
//	((x 6))
//
// Multi-line define-fun format (Z3 4.8):
//
//	(
//	  (define-fun x () Int
//	    6)
//	)
//
// Single-line define-fun format:
//
//	(define-fun x () Int 6)
//
// Lines starting with "(error" are silently skipped (z3 prints these when
// (get-model) is unavailable, e.g., after an unsat result).
func parseModel(lines []string) map[string]any {
	m := make(map[string]any)
	// pendingFun holds the parsed variable name while we wait for the value
	// line in the multi-line define-fun format.
	pendingName := ""
	for _, line := range lines {
		line = strings.TrimSpace(line)
		switch {
		case line == "", line == "(model", line == ")", line == "(", line == "unsat", line == "unknown":
			continue
		case strings.HasPrefix(line, "(error"):
			// z3 reports errors when model is not available (e.g., after unsat).
			return nil
		case strings.HasPrefix(line, "(define-fun "):
			// May be single-line: (define-fun x () Int 6)
			// or multi-line header: (define-fun x () Int
			pendingName = "" // reset
			inner := strings.TrimPrefix(line, "(define-fun ")
			// Strip trailing ')' unconditionally (TrimSuffix is a no-op when absent).
			inner = strings.TrimSuffix(inner, ")")
			parts := strings.Fields(inner)
			// parts: [name, "()", sort, value?...]
			if len(parts) >= 4 && parts[1] == "()" {
				// Single-line: value is already in parts[3:].
				m[parts[0]] = strings.Join(parts[3:], " ")
			} else if len(parts) == 3 && parts[1] == "()" {
				// Multi-line: value comes on the next line.
				pendingName = parts[0]
			}
			continue
		}

		// If we are waiting for the value in a multi-line define-fun, this
		// line is the value: "    6)" → "6".
		if pendingName != "" {
			val := strings.TrimRight(line, ")")
			val = strings.TrimSpace(val)
			if val != "" {
				m[pendingName] = val
			}
			pendingName = ""
			continue
		}

		// Compact format: ((x 6)) — strip balanced outer parens.
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

// parseUnsatCore extracts named assertion labels from the (get-unsat-core)
// output. Returns nil when the core is empty or not available.
//
// Z3 error lines (e.g., "(error "model is not available")") are silently
// skipped — they may appear before the unsat-core output when (get-model)
// was also requested on an unsat result.
func parseUnsatCore(lines []string) []string {
	for _, line := range lines {
		line = strings.TrimSpace(line)
		switch {
		case line == "", line == "()", line == "sat", line == "unknown":
			continue
		case strings.HasPrefix(line, "(error"):
			// Skip error lines; unsat core may appear on a subsequent line.
			continue
		}
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
