package verify

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
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

// TestSymbolicHandler_Placeholder asserts the documented placeholder
// behaviour: SymbolicHandler always responds 200 OK with the fixed
// acknowledgement body and a JSON content type, regardless of the request
// body, so the /v1/verify/symbolic route contract is exercised end-to-end
// while the symbolic engine matures.
func TestSymbolicHandler_Placeholder(t *testing.T) {
	t.Parallel()

	handler := SymbolicHandler()

	// The handler is a placeholder that ignores the body; send a plausible
	// symbolic request to prove no parsing is attempted yet.
	body := `{"input":{"wasmBinary":"AGVzbQ==","entry":"_start","args":[]}}`
	req := httptest.NewRequest(http.MethodPost, "/v1/verify/symbolic", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want %q", ct, "application/json")
	}

	var resp symbolicPlaceholderResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v; body = %s", err, rec.Body.String())
	}
	const want = "Symbolic execution endpoint placeholder"
	if resp.Message != want {
		t.Errorf("message = %q, want %q", resp.Message, want)
	}
}

// TestSymbolicHandler_IgnoresEmptyBody confirms the placeholder responds 200
// even when no body is posted, matching how the route is exercised through the
// registered mux (e.g. liveness-style probes).
func TestSymbolicHandler_IgnoresEmptyBody(t *testing.T) {
	t.Parallel()

	handler := SymbolicHandler()

	req := httptest.NewRequest(http.MethodPost, "/v1/verify/symbolic", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusOK, rec.Body.String())
	}
}
