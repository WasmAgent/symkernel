package smt

import "testing"

func TestParseModel(t *testing.T) {
	model := `(
  (define-fun x () Int
    6)
  (define-fun enabled () Bool
    true)
)`

	got := parseModel(model)
	if got["x"] != int64(6) {
		t.Fatalf("model[x] = %v, want 6", got["x"])
	}
	if got["enabled"] != true {
		t.Fatalf("model[enabled] = %v, want true", got["enabled"])
	}
}

func TestSMTResultShape(t *testing.T) {
	result := SMTResult{
		Sat:       "unsat",
		Model:     map[string]any{"x": int64(6)},
		UnsatCore: []string{"named-assertion"},
	}

	if result.Sat != "unsat" {
		t.Fatalf("Sat = %q, want unsat", result.Sat)
	}
	if result.Model["x"] != int64(6) {
		t.Fatalf("Model[x] = %v, want 6", result.Model["x"])
	}
	if len(result.UnsatCore) != 1 || result.UnsatCore[0] != "named-assertion" {
		t.Fatalf("UnsatCore = %v, want [named-assertion]", result.UnsatCore)
	}
}
