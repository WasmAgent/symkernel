//go:build !z3

// Package smt provides SMT-LIB2 solving through Z3.
package smt

import "fmt"

// Solve requires the z3 build tag and libz3 development files.
func Solve(_ string, _ int) (SMTResult, error) {
	return SMTResult{}, fmt.Errorf("smt: z3 support not built; build with -tags z3 and libz3 installed")
}
