package verify

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
)

// TestRun_NotImplementedStub asserts the documented stub behaviour: Run
// returns ErrNotImplemented together with a result carrying a valid,
// freshly generated DecisionID, regardless of input.
func TestRun_NotImplementedStub(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   SymbolicInput
	}{
		{
			name: "empty input",
			in:   SymbolicInput{},
		},
		{
			name: "populated input",
			in: SymbolicInput{
				WasmBinary: "AGVzbQ==", // arbitrary base64; not decoded by the stub
				Entry:      "_start",
				Args:       []any{1, "two", true},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			res, err := Run(context.Background(), tt.in)

			if !errors.Is(err, ErrNotImplemented) {
				t.Fatalf("err = %v, want ErrNotImplemented", err)
			}

			// The stub always returns a fresh decision UUID so handlers can
			// surface a decision_id even before the engine is implemented.
			if res.DecisionID == "" {
				t.Fatal("DecisionID is empty, want a generated UUID")
			}
			if _, parseErr := uuid.Parse(res.DecisionID); parseErr != nil {
				t.Errorf("DecisionID = %q is not a valid UUID: %v", res.DecisionID, parseErr)
			}

			// No paths are explored by the stub.
			if len(res.Paths) != 0 {
				t.Errorf("Paths len = %d, want 0", len(res.Paths))
			}
			if res.Explored != 0 {
				t.Errorf("Explored = %d, want 0", res.Explored)
			}
		})
	}
}

// TestRun_ToleratesCancelledContext confirms the stub mints a decision_id
// even when the caller's context is already cancelled — no work is
// performed, so the sentinel is returned rather than ctx.Err().
func TestRun_ToleratesCancelledContext(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	res, err := Run(ctx, SymbolicInput{})
	if !errors.Is(err, ErrNotImplemented) {
		t.Fatalf("err = %v, want ErrNotImplemented", err)
	}
	if res.DecisionID == "" {
		t.Fatal("DecisionID is empty, want a generated UUID")
	}
}

// TestRun_DecisionIDIsUnique confirms each call mints a distinct decision_id.
func TestRun_DecisionIDIsUnique(t *testing.T) {
	t.Parallel()

	a, errA := Run(context.Background(), SymbolicInput{})
	b, errB := Run(context.Background(), SymbolicInput{})

	if !errors.Is(errA, ErrNotImplemented) || !errors.Is(errB, ErrNotImplemented) {
		t.Fatalf("errors = %v, %v; both want ErrNotImplemented", errA, errB)
	}
	if a.DecisionID == "" {
		t.Fatal("first DecisionID is empty")
	}
	if a.DecisionID == b.DecisionID {
		t.Fatalf("DecisionIDs collided: %s", a.DecisionID)
	}
}
