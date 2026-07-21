// Package smt provides SMT-LIB2 solving through Z3.
package smt

// SMTResult is the normalized result returned by Solve.
type SMTResult struct {
	// Sat is one of "sat", "unsat", or "unknown".
	Sat string `json:"sat"`

	// Model contains Z3 model bindings when Sat is "sat".
	Model map[string]any `json:"model"`

	// UnsatCore contains the named assertions returned by Z3 for unsat inputs.
	UnsatCore []string `json:"unsat_core"`
}
