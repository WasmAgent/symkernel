package orchestrator

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// --- Route tests ---

func TestRoute_DefaultsToCEL(t *testing.T) {
	r := NewRouter()
	sel, err := r.Route(VerificationRequest{})
	if err != nil {
		t.Fatalf("Route() error = %v", err)
	}
	if sel.Tier != TierCEL {
		t.Errorf("Tier = %q, want %q", sel.Tier, TierCEL)
	}
	if sel.Reason == "" {
		t.Error("Reason is empty, want non-empty")
	}
	if sel.EstimatedCostMs <= 0 {
		t.Errorf("EstimatedCostMs = %d, want > 0", sel.EstimatedCostMs)
	}
}

func TestRoute_SimpleConstraints_SelectsCEL(t *testing.T) {
	r := NewRouter()
	sel, err := r.Route(VerificationRequest{
		Complexity: Complexity{
			ConstraintCount: 5,
			MaxNestingDepth: 2,
		},
	})
	if err != nil {
		t.Fatalf("Route() error = %v", err)
	}
	if sel.Tier != TierCEL {
		t.Errorf("Tier = %q, want %q for simple query", sel.Tier, TierCEL)
	}
}

func TestRoute_HighConstraintCount_SelectsWazero(t *testing.T) {
	r := NewRouter()
	sel, err := r.Route(VerificationRequest{
		Complexity: Complexity{
			ConstraintCount: 50,
			MaxNestingDepth: 5,
		},
	})
	if err != nil {
		t.Fatalf("Route() error = %v", err)
	}
	if sel.Tier != TierWazero {
		t.Errorf("Tier = %q, want %q for medium complexity", sel.Tier, TierWazero)
	}
}

func TestRoute_VeryHighConstraints_SelectsZ3(t *testing.T) {
	r := NewRouter()
	sel, err := r.Route(VerificationRequest{
		Complexity: Complexity{
			ConstraintCount: 500,
			MaxNestingDepth: 20,
		},
	})
	if err != nil {
		t.Fatalf("Route() error = %v", err)
	}
	if sel.Tier != TierZ3 {
		t.Errorf("Tier = %q, want %q for high complexity", sel.Tier, TierZ3)
	}
}

func TestRoute_Quantifiers_SkipsCEL(t *testing.T) {
	r := NewRouter()
	sel, err := r.Route(VerificationRequest{
		Complexity: Complexity{
			ConstraintCount: 2,
			HasQuantifiers:  true,
		},
	})
	if err != nil {
		t.Fatalf("Route() error = %v", err)
	}
	// Quantifiers should push to at least wazero (CEL can't handle forall/exists).
	if sel.Tier == TierCEL {
		t.Errorf("Tier = %q, want wazero or z3 when HasQuantifiers=true", sel.Tier)
	}
}

func TestRoute_HighAccuracy_SelectsZ3(t *testing.T) {
	r := NewRouter()
	sel, err := r.Route(VerificationRequest{
		Complexity: Complexity{
			ConstraintCount: 1,
		},
		AccuracyRequired: 99,
	})
	if err != nil {
		t.Fatalf("Route() error = %v", err)
	}
	if sel.Tier != TierZ3 {
		t.Errorf("Tier = %q, want %q for accuracy=99", sel.Tier, TierZ3)
	}
}

func TestRoute_LowCostBudget_SkipsExpensiveTiers(t *testing.T) {
	r := NewRouter()
	sel, err := r.Route(VerificationRequest{
		Complexity: Complexity{
			ConstraintCount: 200,
			MaxNestingDepth: 15,
		},
		CostTargetMs: 10, // too tight for Z3 (200ms) or wazero (50ms)
	})
	if err != nil {
		t.Fatalf("Route() error = %v", err)
	}
	// CEL can't structurally handle 200 constraints; wazero/Z3 exceed budget.
	// Falls back to Z3 (the ultimate fallback tier).
	if sel.Tier != TierZ3 {
		t.Errorf("Tier = %q, want %q (fallback) when budget is too tight for any structurally-fitting tier", sel.Tier, TierZ3)
	}
}

func TestRoute_ModerateCostBudget_SelectsBestFit(t *testing.T) {
	r := NewRouter()
	sel, err := r.Route(VerificationRequest{
		Complexity: Complexity{
			ConstraintCount: 5,
			MaxNestingDepth: 2,
		},
		CostTargetMs: 3, // too tight even for CEL (baseCostMs=5)
	})
	if err != nil {
		t.Fatalf("Route() error = %v", err)
	}
	// Query is simple enough for CEL but budget is too tight for all tiers.
	// Falls back to Z3.
	if sel.Tier != TierZ3 {
		t.Errorf("Tier = %q, want %q (fallback) when budget too tight for all tiers", sel.Tier, TierZ3)
	}
}

func TestRoute_MethodHintHonoured(t *testing.T) {
	r := NewRouter()
	sel, err := r.Route(VerificationRequest{
		MethodHint: "z3",
	})
	if err != nil {
		t.Fatalf("Route() error = %v", err)
	}
	if sel.Tier != TierZ3 {
		t.Errorf("Tier = %q, want %q with method_hint=z3", sel.Tier, TierZ3)
	}
	if !strings.Contains(sel.Reason, "method_hint") {
		t.Errorf("Reason %q should mention method_hint", sel.Reason)
	}
}

func TestRoute_MethodHintViolatesBudget_FallsThrough(t *testing.T) {
	r := NewRouter()
	sel, err := r.Route(VerificationRequest{
		MethodHint:    "z3",
		CostTargetMs: 10, // Z3 base cost is 200ms — exceeds budget
	})
	if err != nil {
		t.Fatalf("Route() error = %v", err)
	}
	// Hint should be ignored since Z3 exceeds the budget; falls to auto-select.
	if sel.Tier == TierZ3 {
		t.Errorf("Tier = %q, want != z3 when method_hint violates cost budget", sel.Tier)
	}
}

func TestRoute_InvalidMethodHint(t *testing.T) {
	r := NewRouter()
	_, err := r.Route(VerificationRequest{
		MethodHint: "invalid_tier",
	})
	if err == nil {
		t.Fatal("expected error for invalid method_hint, got nil")
	}
	if !strings.Contains(err.Error(), "invalid method_hint") {
		t.Errorf("error = %v, want invalid method_hint message", err)
	}
}

func TestRoute_InvalidAccuracy(t *testing.T) {
	r := NewRouter()
	_, err := r.Route(VerificationRequest{
		AccuracyRequired: 150,
	})
	if err == nil {
		t.Fatal("expected error for accuracy > 100, got nil")
	}
	if !strings.Contains(err.Error(), "accuracy_required") {
		t.Errorf("error = %v, want accuracy_required message", err)
	}
}

func TestRoute_FallbackToZ3(t *testing.T) {
	// Edge case: very high accuracy + high constraints + tight cost.
	// Only Z3 meets accuracy=100; cost is violated for all, so CEL wins.
	r := NewRouter()
	sel, err := r.Route(VerificationRequest{
		Complexity: Complexity{
			ConstraintCount: 1,
		},
		CostTargetMs:     2,  // nothing fits 2ms (CEL is 5ms)
		AccuracyRequired: 100, // only Z3 meets 100
	})
	if err != nil {
		t.Fatalf("Route() error = %v", err)
	}
	// Z3 is the fallback since it always qualifies (unlimited thresholds).
	if sel.Tier != TierZ3 {
		t.Errorf("Tier = %q, want %q (fallback)", sel.Tier, TierZ3)
	}
}

// --- Stats tests ---

func TestStats_InitiallyEmpty(t *testing.T) {
	r := NewRouter()
	stats := r.Stats()
	if stats.TotalRoutes != 0 {
		t.Errorf("TotalRoutes = %d, want 0", stats.TotalRoutes)
	}
	if len(stats.Tiers) != 3 {
		t.Errorf("len(Tiers) = %d, want 3", len(stats.Tiers))
	}
}

func TestStats_AccumulatesRoutes(t *testing.T) {
	r := NewRouter()

	// Route several queries.
	for i := 0; i < 10; i++ {
		_, _ = r.Route(VerificationRequest{}) // defaults to CEL
	}
	for i := 0; i < 3; i++ {
		_, _ = r.Route(VerificationRequest{
			Complexity: Complexity{ConstraintCount: 200, MaxNestingDepth: 15},
		}) // selects Z3
	}

	stats := r.Stats()
	if stats.TotalRoutes != 13 {
		t.Errorf("TotalRoutes = %d, want 13", stats.TotalRoutes)
	}
	if stats.Tiers["cel"].RouteCount != 10 {
		t.Errorf("cel RouteCount = %d, want 10", stats.Tiers["cel"].RouteCount)
	}
	if stats.Tiers["z3"].RouteCount != 3 {
		t.Errorf("z3 RouteCount = %d, want 3", stats.Tiers["z3"].RouteCount)
	}
}

// --- HTTP handler tests ---

func TestRouteHandler_ValidRequest(t *testing.T) {
	r := NewRouter()
	handler := r.RouteHandler()

	body := `{"query":{"complexity":{"constraint_count":5,"max_nesting_depth":2}}}`
	req := httptest.NewRequest(http.MethodPost, "/v1/orchestration/route", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var resp routeResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Result == nil {
		t.Fatal("result is nil")
	}
	if resp.Result.Tier != TierCEL {
		t.Errorf("tier = %q, want %q", resp.Result.Tier, TierCEL)
	}
}

func TestRouteHandler_InvalidJSON(t *testing.T) {
	r := NewRouter()
	handler := r.RouteHandler()

	req := httptest.NewRequest(http.MethodPost, "/v1/orchestration/route", strings.NewReader("not json"))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

func TestRouteHandler_BadAccuracy(t *testing.T) {
	r := NewRouter()
	handler := r.RouteHandler()

	body := `{"query":{"accuracy_required":200}}`
	req := httptest.NewRequest(http.MethodPost, "/v1/orchestration/route", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

func TestStatsHandler_ReturnsMetrics(t *testing.T) {
	r := NewRouter()

	// Generate some routes.
	_, _ = r.Route(VerificationRequest{MethodHint: "cel"})
	_, _ = r.Route(VerificationRequest{MethodHint: "z3"})

	handler := r.StatsHandler()
	req := httptest.NewRequest(http.MethodGet, "/v1/orchestration/stats", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var resp statsResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Result.TotalRoutes != 2 {
		t.Errorf("total_routes = %d, want 2", resp.Result.TotalRoutes)
	}
	if resp.Result.Tiers["cel"].RouteCount != 1 {
		t.Errorf("cel route_count = %d, want 1", resp.Result.Tiers["cel"].RouteCount)
	}
	if resp.Result.Tiers["z3"].RouteCount != 1 {
		t.Errorf("z3 route_count = %d, want 1", resp.Result.Tiers["z3"].RouteCount)
	}
}

func TestRegisterRoutes(t *testing.T) {
	r := NewRouter()
	mux := http.NewServeMux()
	r.RegisterRoutes(mux)

	// Verify that the routes are mounted by exercising them.
	t.Run("POST /v1/orchestration/route", func(t *testing.T) {
		body := `{"query":{}}`
		req := httptest.NewRequest(http.MethodPost, "/v1/orchestration/route", strings.NewReader(body))
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusOK, rec.Body.String())
		}
	})

	t.Run("GET /v1/orchestration/stats", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v1/orchestration/stats", nil)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusOK, rec.Body.String())
		}
	})
}

// --- Utility tests ---

func TestParseTier(t *testing.T) {
	tests := []struct {
		input string
		want  Tier
	}{
		{"cel", TierCEL},
		{"CEL", TierCEL},
		{"wazero", TierWazero},
		{"Wazero", TierWazero},
		{"z3", TierZ3},
		{"Z3", TierZ3},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got, err := ParseTier(tt.input)
			if err != nil {
				t.Fatalf("ParseTier(%q) error = %v", tt.input, err)
			}
			if got != tt.want {
				t.Errorf("ParseTier(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestParseTier_Unknown(t *testing.T) {
	_, err := ParseTier("unknown")
	if err == nil {
		t.Fatal("expected error for unknown tier, got nil")
	}
}

func TestFprintStats(t *testing.T) {
	r := NewRouter()
	_, _ = r.Route(VerificationRequest{MethodHint: "cel"})

	var buf strings.Builder
	FprintStats(&buf, r.Stats())

	out := buf.String()
	if !strings.Contains(out, "Total routes: 1") {
		t.Errorf("output = %q, want to contain 'Total routes: 1'", out)
	}
	if !strings.Contains(out, "cel:") {
		t.Errorf("output = %q, want to contain 'cel:'", out)
	}
}

func TestFormatInt(t *testing.T) {
	if got := formatInt(42); got != "42" {
		t.Errorf("formatInt(42) = %q, want %q", got, "42")
	}
}
