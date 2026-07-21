//go:build z3

package smt

import "testing"

func TestSolveSatWithZ3(t *testing.T) {
	got, err := Solve("(declare-const x Int) (assert (= x 6))", 1000)
	if err != nil {
		t.Fatalf("Solve returned error: %v", err)
	}
	if got.Sat != "sat" {
		t.Fatalf("Sat = %q, want sat", got.Sat)
	}
	if got.Model["x"] != int64(6) {
		t.Fatalf("Model[x] = %v, want 6", got.Model["x"])
	}
}

func TestSolveUnsatWithZ3(t *testing.T) {
	got, err := Solve("(assert (! false :named contradiction))", 1000)
	if err != nil {
		t.Fatalf("Solve returned error: %v", err)
	}
	if got.Sat != "unsat" {
		t.Fatalf("Sat = %q, want unsat", got.Sat)
	}
	if len(got.UnsatCore) != 1 || got.UnsatCore[0] != "contradiction" {
		t.Fatalf("UnsatCore = %v, want [contradiction]", got.UnsatCore)
	}
}
