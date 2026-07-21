package smt

import (
	"fmt"
	"os/exec"
	"testing"
)

// mockSolver is a test stub that returns predetermined results.
type mockSolver struct {
	result SMTResult
	err    error
}

func (m *mockSolver) Solve(_ string, _ int) (SMTResult, error) {
	return m.result, m.err
}

func TestParseOutput_Sat(t *testing.T) {
	got, err := parseOutput("sat\n((x 6))\n((y 10))")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Sat != "sat" {
		t.Errorf("Sat = %q, want %q", got.Sat, "sat")
	}
	if got.Model == nil {
		t.Fatal("Model is nil, want non-nil")
	}
	if got.Model["x"] != "6" {
		t.Errorf("Model[x] = %v, want %q", got.Model["x"], "6")
	}
	if got.Model["y"] != "10" {
		t.Errorf("Model[y] = %v, want %q", got.Model["y"], "10")
	}
	if got.UnsatCore != nil {
		t.Errorf("UnsatCore = %v, want nil", got.UnsatCore)
	}
}

func TestParseOutput_Unsat(t *testing.T) {
	got, err := parseOutput("unsat\n(a1 a2)")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Sat != "unsat" {
		t.Errorf("Sat = %q, want %q", got.Sat, "unsat")
	}
	if got.Model != nil {
		t.Errorf("Model = %v, want nil", got.Model)
	}
	if len(got.UnsatCore) != 2 || got.UnsatCore[0] != "a1" || got.UnsatCore[1] != "a2" {
		t.Errorf("UnsatCore = %v, want [a1 a2]", got.UnsatCore)
	}
}

func TestParseOutput_Unknown(t *testing.T) {
	got, err := parseOutput("unknown")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Sat != "unknown" {
		t.Errorf("Sat = %q, want %q", got.Sat, "unknown")
	}
	if got.Model != nil {
		t.Errorf("Model = %v, want nil", got.Model)
	}
	if got.UnsatCore != nil {
		t.Errorf("UnsatCore = %v, want nil", got.UnsatCore)
	}
}

func TestParseOutput_Empty(t *testing.T) {
	_, err := parseOutput("")
	if err == nil {
		t.Fatal("expected error for empty output, got nil")
	}
}

func TestParseOutput_Unexpected(t *testing.T) {
	_, err := parseOutput("maybe")
	if err == nil {
		t.Fatal("expected error for unexpected result, got nil")
	}
}

func TestParseModel(t *testing.T) {
	tests := []struct {
		name  string
		lines []string
		want  map[string]any
	}{
		{
			name:  "simple binding",
			lines: []string{"((x 6))"},
			want:  map[string]any{"x": "6"},
		},
		{
			name:  "multiple bindings",
			lines: []string{"((x 6))", "((y 10))"},
			want:  map[string]any{"x": "6", "y": "10"},
		},
		{
			name:  "empty input",
			lines: nil,
			want:  nil,
		},
		{
			name:  "model envelope markers",
			lines: []string{"(model", "((x 1))", ")"},
			want:  map[string]any{"x": "1"},
		},
		{
			name:  "blank lines ignored",
			lines: []string{"", "((x 2))", ""},
			want:  map[string]any{"x": "2"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseModel(tt.lines)
			if tt.want == nil {
				if got != nil {
					t.Errorf("parseModel() = %v, want nil", got)
				}
				return
			}
			if len(got) != len(tt.want) {
				t.Errorf("parseModel() len = %d, want %d", len(got), len(tt.want))
				return
			}
			for k, v := range tt.want {
				if got[k] != v {
					t.Errorf("parseModel()[%q] = %v, want %v", k, got[k], v)
				}
			}
		})
	}
}

func TestParseUnsatCore(t *testing.T) {
	tests := []struct {
		name  string
		lines []string
		want  []string
	}{
		{
			name:  "single assertion",
			lines: []string{"(a1)"},
			want:  []string{"a1"},
		},
		{
			name:  "multiple assertions",
			lines: []string{"(a1 a2 a3)"},
			want:  []string{"a1", "a2", "a3"},
		},
		{
			name:  "empty core",
			lines: []string{"()"},
			want:  nil,
		},
		{
			name:  "blank lines",
			lines: []string{"", "  (x y)  ", ""},
			want:  []string{"x", "y"},
		},
		{
			name:  "no core output",
			lines: nil,
			want:  nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseUnsatCore(tt.lines)
			if tt.want == nil {
				if got != nil {
					t.Errorf("parseUnsatCore() = %v, want nil", got)
				}
				return
			}
			if len(got) != len(tt.want) {
				t.Errorf("parseUnsatCore() len = %d, want %d", len(got), len(tt.want))
				return
			}
			for i, v := range tt.want {
				if got[i] != v {
					t.Errorf("parseUnsatCore()[%d] = %q, want %q", i, got[i], v)
				}
			}
		})
	}
}

func TestSolver_Sat(t *testing.T) {
	solver := &mockSolver{
		result: SMTResult{Sat: "sat", Model: map[string]any{"x": "42"}},
	}
	got, err := solver.Solve("(declare-const x Int) (assert (> x 40)) (check-sat) (get-model)", 5000)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Sat != "sat" {
		t.Errorf("Sat = %q, want %q", got.Sat, "sat")
	}
}

func TestSolver_Unsat(t *testing.T) {
	solver := &mockSolver{
		result: SMTResult{Sat: "unsat", UnsatCore: []string{"c1"}},
	}
	got, err := solver.Solve("(assert false) (check-sat)", 5000)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Sat != "unsat" {
		t.Errorf("Sat = %q, want %q", got.Sat, "unsat")
	}
}

func TestSolver_Error(t *testing.T) {
	solver := &mockSolver{
		err: fmt.Errorf("z3: executable not found"),
	}
	_, err := solver.Solve("(assert true) (check-sat)", 5000)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestSolver_Interface(t *testing.T) {
	// Verify Z3Solver satisfies the Solver interface at compile time.
	var _ Solver = (*Z3Solver)(nil)
}

// TestZ3Solver_SolveIntegration exercises the real z3 binary end-to-end
// through the public Solve(smt2, timeoutMs) signature. It is skipped when
// z3 is not on PATH so the suite stays green in solver-less environments.
func TestZ3Solver_SolveIntegration(t *testing.T) {
	if _, err := exec.LookPath("z3"); err != nil {
		t.Skip("z3 not installed; skipping integration test")
	}

	solver := &Z3Solver{}

	// Trivially satisfiable: some integer x > 5 exists.
	satRes, err := solver.Solve("(declare-const x Int)(assert (> x 5))(check-sat)", 5000)
	if err != nil {
		t.Fatalf("sat case: unexpected error: %v", err)
	}
	if satRes.Sat != "sat" {
		t.Errorf("sat case: Sat = %q, want %q", satRes.Sat, "sat")
	}

	// Trivially unsatisfiable: x > 5 and x < 1 cannot both hold.
	unsatRes, err := solver.Solve("(declare-const x Int)(assert (> x 5))(assert (< x 1))(check-sat)", 5000)
	if err != nil {
		t.Fatalf("unsat case: unexpected error: %v", err)
	}
	if unsatRes.Sat != "unsat" {
		t.Errorf("unsat case: Sat = %q, want %q", unsatRes.Sat, "unsat")
	}
	if unsatRes.Model != nil {
		t.Errorf("unsat case: Model = %v, want nil", unsatRes.Model)
	}
}

// TestZ3Solver_SolveDefaultTimeout confirms a non-positive timeoutMs falls
// back to the internal default budget rather than short-circuiting.
func TestZ3Solver_SolveDefaultTimeout(t *testing.T) {
	if _, err := exec.LookPath("z3"); err != nil {
		t.Skip("z3 not installed; skipping integration test")
	}
	solver := &Z3Solver{}
	got, err := solver.Solve("(declare-const x Int)(assert (> x 5))(check-sat)", 0)
	if err != nil {
		t.Fatalf("unexpected error with default timeout: %v", err)
	}
	if got.Sat != "sat" {
		t.Errorf("Sat = %q, want %q", got.Sat, "sat")
	}
}