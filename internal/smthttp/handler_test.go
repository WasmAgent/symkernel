package smt

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	intz3 "github.com/WasmAgent/symkernel/internal/z3"
)

// mockSolver satisfies the Solver interface for unit tests.
type mockSolver struct {
	sol intz3.Solution
	err error
}

func (m *mockSolver) Solve(_ string, _ map[string]any, _ time.Duration) (intz3.Solution, error) {
	return m.sol, m.err
}

func TestHandler_Sat(t *testing.T) {
	handler := HandlerWithSolver(&mockSolver{
		sol: intz3.Solution{Sat: "sat", Model: map[string]any{"x": "42"}, SolverMs: 5},
	})

	body := `{"constraints":"(assert (> x 5))","model":{"x":"Int"}}`
	req := httptest.NewRequest(http.MethodPost, "/v1/verify/smt", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}

	var resp SMTResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.Sat {
		t.Error("Sat = false, want true")
	}
	if resp.SatRaw != "sat" {
		t.Errorf("SatRaw = %q, want sat", resp.SatRaw)
	}
	if resp.DecisionID == "" {
		t.Error("DecisionID is empty")
	}
}

func TestHandler_Unsat(t *testing.T) {
	handler := HandlerWithSolver(&mockSolver{
		sol: intz3.Solution{Sat: "unsat", UnsatCore: []string{"c1"}, SolverMs: 3},
	})

	body := `{"constraints":"(assert false)"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/verify/smt", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	var resp SMTResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Sat {
		t.Error("Sat = true, want false for unsat")
	}
	if resp.SatRaw != "unsat" {
		t.Errorf("SatRaw = %q, want unsat", resp.SatRaw)
	}
	if len(resp.UnsatCore) != 1 || resp.UnsatCore[0] != "c1" {
		t.Errorf("UnsatCore = %v, want [c1]", resp.UnsatCore)
	}
}

func TestHandler_Unknown(t *testing.T) {
	handler := HandlerWithSolver(&mockSolver{
		sol: intz3.Solution{Sat: "unknown", SolverMs: 100},
	})

	body := `{"constraints":"(assert true)","timeout_ms":1}`
	req := httptest.NewRequest(http.MethodPost, "/v1/verify/smt", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	var resp SMTResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.SatRaw != "unknown" {
		t.Errorf("SatRaw = %q, want unknown", resp.SatRaw)
	}
}

func TestHandler_SolverError(t *testing.T) {
	handler := HandlerWithSolver(&mockSolver{err: fmt.Errorf("z3: not found")})

	body := `{"constraints":"(assert true)"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/verify/smt", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
}

func TestHandler_EmptyConstraints(t *testing.T) {
	handler := HandlerWithSolver(&mockSolver{})

	body := `{"constraints":""}`
	req := httptest.NewRequest(http.MethodPost, "/v1/verify/smt", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestHandler_InvalidJSON(t *testing.T) {
	handler := Handler()

	req := httptest.NewRequest(http.MethodPost, "/v1/verify/smt", strings.NewReader("not-json"))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}
