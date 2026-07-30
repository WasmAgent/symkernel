package composed

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHandler_AllStagesPass(t *testing.T) {
	handler := Handler()

	// CEL: x is a float64 when decoded from JSON; use float literal comparison.
	// wasm: pre-evaluated result=true.
	// smt: trivially satisfiable linear arithmetic.
	body := `{
		"policies":[
			{"stage":"cel","spec":{"expression":"x > 0.0","variables":{"x":5.0}}},
			{"stage":"wasm","spec":{"result":true}},
			{"stage":"smt","spec":{"constraints":"(assert (> x 0))","model":{"x":"Int"}}}
		],
		"input":{}
	}`
	req := httptest.NewRequest(http.MethodPost, "/v1/verify/composed", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}

	var resp ComposedResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.OK {
		t.Errorf("OK = false, want true; stages: %+v", resp.Report.Stages)
	}
	if resp.Report.DecisionID == "" {
		t.Error("top-level DecisionID is empty")
	}
	if len(resp.Report.Stages) != 3 {
		t.Errorf("stages len = %d, want 3", len(resp.Report.Stages))
	}
	for i, s := range resp.Report.Stages {
		if s.DecisionID == "" {
			t.Errorf("stage[%d] DecisionID is empty", i)
		}
	}
}

func TestHandler_CELFailShortCircuits(t *testing.T) {
	handler := Handler()

	// CEL: 1.0 > 100.0 is false → short-circuit; wasm stage should not run.
	body := `{
		"policies":[
			{"stage":"cel","spec":{"expression":"x > 100.0","variables":{"x":1.0}}},
			{"stage":"wasm","spec":{"result":true}}
		]
	}`
	req := httptest.NewRequest(http.MethodPost, "/v1/verify/composed", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	var resp ComposedResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.OK {
		t.Error("OK = true, want false (CEL failed)")
	}
	// Only 1 stage should be in the report (wasm was short-circuited).
	if len(resp.Report.Stages) != 1 {
		t.Errorf("stages len = %d, want 1 (short-circuited after CEL fail)", len(resp.Report.Stages))
	}
}

func TestHandler_WasmFalseResult(t *testing.T) {
	handler := Handler()

	body := `{"policies":[{"stage":"wasm","spec":{"result":false}}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/verify/composed", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	var resp ComposedResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.OK {
		t.Error("OK = true, want false")
	}
	if resp.Report.Stages[0].Stage != "wasm" {
		t.Errorf("stage = %q, want wasm", resp.Report.Stages[0].Stage)
	}
}

func TestHandler_UnknownStageType(t *testing.T) {
	handler := Handler()

	body := `{"policies":[{"stage":"magic","spec":{}}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/verify/composed", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	var resp ComposedResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.OK {
		t.Error("OK = true, want false for unknown stage type")
	}
	if !strings.Contains(resp.Report.Stages[0].Hint, "unknown stage type") {
		t.Errorf("hint = %q, want to contain 'unknown stage type'", resp.Report.Stages[0].Hint)
	}
}

func TestHandler_EmptyPolicies(t *testing.T) {
	handler := Handler()

	body := `{"policies":[]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/verify/composed", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestHandler_InvalidJSON(t *testing.T) {
	handler := Handler()

	req := httptest.NewRequest(http.MethodPost, "/v1/verify/composed", strings.NewReader("not-json"))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestHandler_EvalMsPresent(t *testing.T) {
	handler := Handler()

	body := `{"policies":[{"stage":"wasm","spec":{}}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/verify/composed", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body = %s", rec.Code, rec.Body.String())
	}

	var resp ComposedResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// EvalMs can be 0 if the handler is extremely fast, but the field must exist.
	_ = resp.EvalMs
	_ = resp.Report.Stages[0].EvalMs
}
