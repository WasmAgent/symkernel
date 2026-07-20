package diagnostics

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRecordAndLookup(t *testing.T) {
	s := New()

	reasons := []WhyFailed{
		{Constraint: "max_memory", Actual: 256, Limit: 128, Remediation: "reduce_allocation_or_increase_limit"},
	}
	s.Record("dec-1", reasons)

	got, ok := s.Lookup("dec-1")
	if !ok {
		t.Fatal("expected lookup to succeed")
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 reason, got %d", len(got))
	}
	if got[0].Constraint != "max_memory" {
		t.Errorf("constraint = %q, want %q", got[0].Constraint, "max_memory")
	}
	if got[0].Actual != 256 {
		t.Errorf("actual = %v, want 256", got[0].Actual)
	}
	if got[0].Limit != 128 {
		t.Errorf("limit = %v, want 128", got[0].Limit)
	}
	if got[0].Remediation != "reduce_allocation_or_increase_limit" {
		t.Errorf("remediation = %q, want %q", got[0].Remediation, "reduce_allocation_or_increase_limit")
	}

	// Returned slice must be a copy — mutations must not affect the store.
	got[0].Constraint = "mutated"
	got2, _ := s.Lookup("dec-1")
	if got2[0].Constraint == "mutated" {
		t.Error("Lookup returned a reference to the internal slice")
	}
}

func TestLookupMissing(t *testing.T) {
	s := New()
	_, ok := s.Lookup("nonexistent")
	if ok {
		t.Error("expected lookup to return false for missing key")
	}
}

func TestRecordEmptyReasons(t *testing.T) {
	s := New()
	s.Record("dec-empty", nil)

	reasons, ok := s.Lookup("dec-empty")
	if !ok {
		t.Fatal("expected lookup to succeed")
	}
	if len(reasons) != 0 {
		t.Errorf("expected 0 reasons, got %d", len(reasons))
	}
}

func TestRecordOverwrites(t *testing.T) {
	s := New()

	s.Record("dec-1", []WhyFailed{{Constraint: "c1"}})
	s.Record("dec-1", []WhyFailed{{Constraint: "c2"}, {Constraint: "c3"}})

	reasons, _ := s.Lookup("dec-1")
	if len(reasons) != 2 {
		t.Fatalf("expected 2 reasons after overwrite, got %d", len(reasons))
	}
	if reasons[0].Constraint != "c2" {
		t.Errorf("first constraint = %q, want %q", reasons[0].Constraint, "c2")
	}
}

// --- HTTP handler tests ---

func TestRegisterRoutes_MountsEndpoint(t *testing.T) {
	mux := http.NewServeMux()
	s := New()
	s.RegisterRoutes(mux)

	// Verify the route is mounted by inspecting registered patterns.
	// ServeMux doesn't expose registered patterns directly, so we test
	// via an actual request below.
	_ = mux
}

func TestLookupHandler_Found(t *testing.T) {
	mux := http.NewServeMux()
	s := New()
	s.Record("dec-42", []WhyFailed{
		{Constraint: "max_memory", Actual: 256, Limit: 128, Remediation: "reduce_allocation_or_increase_limit"},
	})
	s.RegisterRoutes(mux)

	req := httptest.NewRequest(http.MethodGet, "/v1/diagnostics/dec-42", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	ct := rec.Header().Get("Content-Type")
	if ct != "application/json" {
		t.Errorf("Content-Type = %q, want %q", ct, "application/json")
	}

	var body DiagnosticRecord
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.DecisionID != "dec-42" {
		t.Errorf("decision_id = %q, want %q", body.DecisionID, "dec-42")
	}
	if len(body.Reasons) != 1 {
		t.Fatalf("reasons count = %d, want 1", len(body.Reasons))
	}
	if body.Reasons[0].Constraint != "max_memory" {
		t.Errorf("constraint = %q, want %q", body.Reasons[0].Constraint, "max_memory")
	}
}

func TestLookupHandler_NotFound(t *testing.T) {
	mux := http.NewServeMux()
	s := New()
	s.RegisterRoutes(mux)

	req := httptest.NewRequest(http.MethodGet, "/v1/diagnostics/unknown", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusNotFound, rec.Body.String())
	}
}

func TestLookupHandler_MissingDecisionID(t *testing.T) {
	mux := http.NewServeMux()
	s := New()
	s.RegisterRoutes(mux)

	// Request the base path without a decision_id — should not match the
	// route pattern, so ServeMux returns 404 (Not Found) or 405 (Method
	// Not Allowed).
	req := httptest.NewRequest(http.MethodGet, "/v1/diagnostics/", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	// ServeMux returns 404 for unmatched paths.
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
}

func TestWhyFailedJSONRoundTrip(t *testing.T) {
	original := WhyFailed{
		Constraint:  "max_memory",
		Actual:      256,
		Limit:       128,
		Remediation: "reduce_allocation_or_increase_limit",
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded WhyFailed
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if decoded.Constraint != original.Constraint {
		t.Errorf("constraint = %q, want %q", decoded.Constraint, original.Constraint)
	}
	// JSON numbers unmarshal as float64 by default.
	if decoded.Actual != float64(256) {
		t.Errorf("actual = %v, want 256", decoded.Actual)
	}
	if decoded.Limit != float64(128) {
		t.Errorf("limit = %v, want 128", decoded.Limit)
	}
	if decoded.Remediation != original.Remediation {
		t.Errorf("remediation = %q, want %q", decoded.Remediation, original.Remediation)
	}
}
