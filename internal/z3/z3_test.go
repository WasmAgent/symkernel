package z3

import (
	"context"
	"os/exec"
	"strings"
	"testing"
)

func TestInferSort(t *testing.T) {
	tests := []struct {
		val  any
		want string
	}{
		{"Int", "Int"},
		{"Bool", "Bool"},
		{"Real", "Real"},
		{"BitVec_32", "(_ BitVec 32)"},
		{"Array_Int_Bool", "(Array Int Bool)"},
		{"CustomSort", "CustomSort"},
		{true, "Bool"},
		{false, "Bool"},
		{42, "Int"},
		{int64(7), "Int"},
		{3.14, "Real"},
		{float32(1.5), "Real"},
	}
	for _, tt := range tests {
		got := inferSort(tt.val)
		if got != tt.want {
			t.Errorf("inferSort(%v) = %q, want %q", tt.val, got, tt.want)
		}
	}
}

func TestBuildSMT2_NoModel(t *testing.T) {
	smt2 := buildSMT2("(assert true)", nil)
	if !strings.Contains(smt2, "(check-sat)") {
		t.Error("expected (check-sat) in output")
	}
	if !strings.Contains(smt2, "(get-model)") {
		t.Error("expected (get-model) in output")
	}
	if !strings.Contains(smt2, "(exit)") {
		t.Error("expected (exit) in output")
	}
}

func TestBuildSMT2_WithModel(t *testing.T) {
	smt2 := buildSMT2("(assert (> x 5))", map[string]any{"x": "Int"})
	if !strings.Contains(smt2, "(declare-const x Int)") {
		t.Errorf("expected declare-const x Int; got:\n%s", smt2)
	}
}

func TestBuildSMT2_NamedAssertionsIncludeUnsatCore(t *testing.T) {
	smt2 := buildSMT2("(assert (! (> x 5) :named a1))", map[string]any{"x": "Int"})
	if !strings.Contains(smt2, "(get-unsat-core)") {
		t.Errorf("expected (get-unsat-core) when :named assertions present; got:\n%s", smt2)
	}
	if !strings.Contains(smt2, ":produce-unsat-cores") {
		t.Errorf("expected produce-unsat-cores option; got:\n%s", smt2)
	}
}

func TestBuildSMT2_NoNamedAssertionsOmitUnsatCore(t *testing.T) {
	smt2 := buildSMT2("(assert (> x 5))", nil)
	if strings.Contains(smt2, "(get-unsat-core)") {
		t.Error("expected no (get-unsat-core) when no :named assertions")
	}
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
		t.Errorf("Model[x] = %v, want 6", got.Model["x"])
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
	if len(got.UnsatCore) != 2 || got.UnsatCore[0] != "a1" {
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

func TestParseModel_DefineFun(t *testing.T) {
	// Single-line format: (define-fun x () Int 6)
	lines := []string{
		"(model",
		"(define-fun x () Int 6)",
		"(define-fun y () Bool true)",
		")",
	}
	m := parseModel(lines)
	if m == nil {
		t.Fatal("parseModel returned nil (single-line format)")
	}
	if m["x"] != "6" {
		t.Errorf("m[x] = %v, want 6", m["x"])
	}
	if m["y"] != "true" {
		t.Errorf("m[y] = %v, want true", m["y"])
	}
}

func TestParseModel_DefineFunMultiline(t *testing.T) {
	// Multi-line format as emitted by z3 4.8:
	// (
	//   (define-fun x () Int
	//     6)
	// )
	lines := []string{
		"(",
		"(define-fun x () Int",
		"    6)",
		"(define-fun y () Int",
		"    42)",
		")",
	}
	m := parseModel(lines)
	if m == nil {
		t.Fatal("parseModel returned nil (multi-line format)")
	}
	if m["x"] != "6" {
		t.Errorf("m[x] = %v, want 6", m["x"])
	}
	if m["y"] != "42" {
		t.Errorf("m[y] = %v, want 42", m["y"])
	}
}

func TestParseUnsatCore(t *testing.T) {
	got := parseUnsatCore([]string{"(c1 c2 c3)"})
	if len(got) != 3 || got[0] != "c1" {
		t.Errorf("parseUnsatCore = %v, want [c1 c2 c3]", got)
	}
}

// Integration tests — skipped when z3 is not on PATH.

func TestSolveConstraints_SatIntegration(t *testing.T) {
	if _, err := exec.LookPath("z3"); err != nil {
		t.Skip("z3 not on PATH")
	}
	sol, err := SolveConstraints(
		"(assert (> x 5))",
		map[string]any{"x": "Int"},
	)
	if err != nil {
		t.Fatalf("SolveConstraints error: %v", err)
	}
	if sol.Sat != "sat" {
		t.Errorf("Sat = %q, want sat", sol.Sat)
	}
	if sol.Model == nil {
		t.Error("Model is nil for sat result")
	}
}

func TestSolveConstraints_UnsatIntegration(t *testing.T) {
	if _, err := exec.LookPath("z3"); err != nil {
		t.Skip("z3 not on PATH")
	}
	sol, err := SolveConstraints(
		"(assert (> x 5))\n(assert (< x 1))",
		map[string]any{"x": "Int"},
	)
	if err != nil {
		t.Fatalf("SolveConstraints error: %v", err)
	}
	if sol.Sat != "unsat" {
		t.Errorf("Sat = %q, want unsat", sol.Sat)
	}
}

func TestSolveConstraintsCtx_CancelledIntegration(t *testing.T) {
	if _, err := exec.LookPath("z3"); err != nil {
		t.Skip("z3 not on PATH")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancel

	// A pre-cancelled context may cause exec.Command to fail before z3 starts.
	// The implementation must return unknown (not an error) in this case.
	sol, err := SolveConstraintsCtx(ctx, "(assert true)", nil)
	if err != nil {
		t.Fatalf("unexpected hard error: %v", err)
	}
	if sol.Sat != "unknown" {
		t.Errorf("Sat = %q, want unknown after cancel", sol.Sat)
	}
}

func TestSolveConstraints_BitvectorIntegration(t *testing.T) {
	if _, err := exec.LookPath("z3"); err != nil {
		t.Skip("z3 not on PATH")
	}
	// Bitvector: (bvadd #x0003 #x0004) == #x0007
	sol, err := SolveConstraints(
		"(assert (= (bvadd a b) c))\n(assert (= a #x0003))\n(assert (= b #x0004))",
		map[string]any{"a": "BitVec_16", "b": "BitVec_16", "c": "BitVec_16"},
	)
	if err != nil {
		t.Fatalf("SolveConstraints bitvector error: %v", err)
	}
	if sol.Sat != "sat" {
		t.Errorf("bitvector Sat = %q, want sat", sol.Sat)
	}
}

func TestSolveConstraints_NamedUnsatCoreIntegration(t *testing.T) {
	if _, err := exec.LookPath("z3"); err != nil {
		t.Skip("z3 not on PATH")
	}
	sol, err := SolveConstraints(
		"(assert (! (> x 5) :named a1))\n(assert (! (< x 1) :named a2))",
		map[string]any{"x": "Int"},
	)
	if err != nil {
		t.Fatalf("SolveConstraints named unsat error: %v", err)
	}
	if sol.Sat != "unsat" {
		t.Errorf("Sat = %q, want unsat", sol.Sat)
	}
	if len(sol.UnsatCore) == 0 {
		t.Error("UnsatCore is empty, want named assertions")
	}
}
