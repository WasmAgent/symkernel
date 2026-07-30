package smt

import (
	"context"
	"time"
)

// withDuration returns a context.WithTimeout backed by a background context.
// It is a thin helper to allow Solver.Solve to receive a duration rather than
// a context, keeping the Solver interface simple while still applying a timeout.
func withDuration(d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), d)
}
