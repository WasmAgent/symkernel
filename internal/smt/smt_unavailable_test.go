//go:build !z3

package smt

import (
	"strings"
	"testing"
)

func TestSolveWithoutZ3BuildTag(t *testing.T) {
	_, err := Solve("(assert true)", 1)
	if err == nil {
		t.Fatal("Solve error = nil, want z3 build error")
	}
	if !strings.Contains(err.Error(), "z3 support not built") {
		t.Fatalf("Solve error = %q, want z3 support message", err)
	}
}
