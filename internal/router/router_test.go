package router

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// mockHandler returns a HandlerFunc that records the number of invocations
// and responds with a fixed JSON payload.
func mockHandler(counter *uint64, tier Tier, result any) HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		atomic.AddUint64(counter, 1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":    true,
			"tier":  string(tier),
			"result": result,
		})
	}
}

func TestClassifyRequest_CEL(t *testing.T) {
	input := map[string]any{"expr": "x > 10", "context": map[string]any{"x": 5}}
	c, err := classifyRequest(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.Tier != TierCEL {
		t.Errorf("expected tier %q, got %q", TierCEL, c.Tier)
	}
}

func TestClassifyRequest_Wazero(t *testing.T) {
	input := map[string]any{"wasm_module_b64": "AGFzbQAAAQIZAgMBAA==", "args": map[string]any{}}
	c, err := classifyRequest(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.Tier != TierWazero {
		t.Errorf("expected tier %q, got %q", TierWazero, c.Tier)
	}
}

func TestClassifyRequest_Z3(t *testing.T) {
	input := map[string]any{"constraints_smt2": "(declare-const x Int) (assert (> x 0))"}
	c, err := classifyRequest(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.Tier != TierZ3 {
		t.Errorf("expected tier %q, got %q", TierZ3, c.Tier)
	}
}

func TestClassifyRequest_Precedence(t *testing.T) {
	// When multiple signals are present, wazero > z3 > cel.
	tests := []struct {
		name     string
		input    map[string]any
		expected Tier
	}{
		{
			name:     "wazero beats z3",
			input:    map[string]any{"wasm_module_b64": "AAA=", "constraints_smt2": "(check-sat)"},
			expected: TierWazero,
		},
		{
			name:     "wazero beats cel",
			input:    map[string]any{"wasm_module_b64": "AAA=", "expr": "x > 0"},
			expected: TierWazero,
		},
		{
			name:     "z3 beats cel",
			input:    map[string]any{"constraints_smt2": "(check-sat)", "expr": "x > 0"},
			expected: TierZ3,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := classifyRequest(tt.input)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if c.Tier != tt.expected {
				t.Errorf("expected tier %q, got %q", tt.expected, c.Tier)
			}
		})
	}
}

func TestClassifyRequest_NoRecognisedFields(t *testing.T) {
	input := map[string]any{"foo": "bar"}
	_, err := classifyRequest(input)
	if err == nil {
		t.Fatal("expected error for unrecognised request")
	}
}

func TestClassifyRequest_EmptyInput(t *testing.T) {
	input := map[string]any{}
	_, err := classifyRequest(input)
	if err == nil {
		t.Fatal("expected error for empty input")
	}
}

func TestVerifyHandler_CEL(t *testing.T) {
	var celCount uint64
	r := New()
	defer r.Stop()
	r.RegisterHandler(TierCEL, mockHandler(&celCount, TierCEL, map[string]any{"value": true}))

	body := unifiedRequest{Input: map[string]any{"expr": "x > 10", "context": map[string]any{"x": 15}}}
	b, _ := json.Marshal(body)

	req := httptest.NewRequest("POST", "/v1/verify", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	r.VerifyHandler()(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}
	if resp["tier"] != "cel" {
		t.Errorf("expected tier %q in response, got %v", "cel", resp["tier"])
	}

	if atomic.LoadUint64(&celCount) != 1 {
		t.Errorf("expected CEL handler called once, got %d", celCount)
	}
}

func TestVerifyHandler_Wazero(t *testing.T) {
	var wasmCount uint64
	r := New()
	defer r.Stop()
	r.RegisterHandler(TierWazero, mockHandler(&wasmCount, TierWazero, map[string]any{"exit_code": 0}))

	body := unifiedRequest{Input: map[string]any{"wasm_module_b64": "AAA=", "memory_limit_mb": 64}}
	b, _ := json.Marshal(body)

	req := httptest.NewRequest("POST", "/v1/verify", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	r.VerifyHandler()(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", rec.Code, rec.Body.String())
	}

	if atomic.LoadUint64(&wasmCount) != 1 {
		t.Errorf("expected wazero handler called once, got %d", wasmCount)
	}
}

func TestVerifyHandler_Z3(t *testing.T) {
	var z3Count uint64
	r := New()
	defer r.Stop()
	r.RegisterHandler(TierZ3, mockHandler(&z3Count, TierZ3, map[string]any{"sat": "unsat"}))

	body := unifiedRequest{Input: map[string]any{"constraints_smt2": "(declare-const x Int) (assert (= x 1) (assert (= x 2)))"}}
	b, _ := json.Marshal(body)

	req := httptest.NewRequest("POST", "/v1/verify", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	r.VerifyHandler()(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", rec.Code, rec.Body.String())
	}

	if atomic.LoadUint64(&z3Count) != 1 {
		t.Errorf("expected z3 handler called once, got %d", z3Count)
	}
}

func TestVerifyHandler_UnrecognisedRequest(t *testing.T) {
	r := New()
	defer r.Stop()

	body := unifiedRequest{Input: map[string]any{"unknown_field": "value"}}
	b, _ := json.Marshal(body)

	req := httptest.NewRequest("POST", "/v1/verify", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	r.VerifyHandler()(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected status 400, got %d", rec.Code)
	}
}

func TestVerifyHandler_NoInput(t *testing.T) {
	r := New()
	defer r.Stop()

	b, _ := json.Marshal(map[string]any{})

	req := httptest.NewRequest("POST", "/v1/verify", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	r.VerifyHandler()(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected status 400, got %d", rec.Code)
	}
}

func TestVerifyHandler_MalformedJSON(t *testing.T) {
	r := New()
	defer r.Stop()

	req := httptest.NewRequest("POST", "/v1/verify", bytes.NewReader([]byte("not json")))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	r.VerifyHandler()(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected status 400, got %d", rec.Code)
	}
}

func TestVerifyHandler_NoHandlerRegistered(t *testing.T) {
	r := New()
	defer r.Stop()
	// Register only CEL handler; send a Z3 request.
	var celCount uint64
	r.RegisterHandler(TierCEL, mockHandler(&celCount, TierCEL, nil))

	body := unifiedRequest{Input: map[string]any{"constraints_smt2": "(check-sat)"}}
	b, _ := json.Marshal(body)

	req := httptest.NewRequest("POST", "/v1/verify", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	r.VerifyHandler()(rec, req)

	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("expected status 501, got %d", rec.Code)
	}
}

func TestStatsHandler(t *testing.T) {
	var celCount, z3Count uint64
	r := New()
	defer r.Stop()
	r.RegisterHandler(TierCEL, mockHandler(&celCount, TierCEL, nil))
	r.RegisterHandler(TierZ3, mockHandler(&z3Count, TierZ3, nil))

	// Route a CEL request to increment stats.
	celBody := unifiedRequest{Input: map[string]any{"expr": "true"}}
	celB, _ := json.Marshal(celBody)
	celReq := httptest.NewRequest("POST", "/v1/verify", bytes.NewReader(celB))
	celReq.Header.Set("Content-Type", "application/json")
	celRec := httptest.NewRecorder()
	r.VerifyHandler()(celRec, celReq)

	// Route a Z3 request to increment stats.
	z3Body := unifiedRequest{Input: map[string]any{"constraints_smt2": "(check-sat)"}}
	z3B, _ := json.Marshal(z3Body)
	z3Req := httptest.NewRequest("POST", "/v1/verify", bytes.NewReader(z3B))
	z3Req.Header.Set("Content-Type", "application/json")
	z3Rec := httptest.NewRecorder()
	r.VerifyHandler()(z3Rec, z3Req)

	// Fetch stats.
	req := httptest.NewRequest("GET", "/v1/router/stats", nil)
	rec := httptest.NewRecorder()
	r.StatsHandler()(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", rec.Code)
	}

	var statsResp statsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &statsResp); err != nil {
		t.Fatalf("failed to unmarshal stats response: %v", err)
	}

	if statsResp.Result.TotalClassifications != 2 {
		t.Errorf("expected 2 total classifications, got %d", statsResp.Result.TotalClassifications)
	}
	if statsResp.Result.RouteCounts["cel"] != 1 {
		t.Errorf("expected 1 cel classification, got %d", statsResp.Result.RouteCounts["cel"])
	}
	if statsResp.Result.RouteCounts["z3"] != 1 {
		t.Errorf("expected 1 z3 classification, got %d", statsResp.Result.RouteCounts["z3"])
	}
	// decision_id is set by the otel middleware; in unit tests the context
	// has no decision_id, so we only verify the result shape is correct.
}

func TestRegisterRoutes(t *testing.T) {
	r := New()
	defer r.Stop()

	mux := http.NewServeMux()
	r.RegisterRoutes(mux)

	// Verify that routes are registered by sending to stats.
	req := httptest.NewRequest("GET", "/v1/router/stats", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /v1/router/stats should be registered, got status %d", rec.Code)
	}

	var statsResp statsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &statsResp); err != nil {
		t.Fatalf("failed to unmarshal stats response: %v", err)
	}
	// decision_id is set by otel middleware; unit tests only verify
	// the result shape is present.
}

func TestConcurrentRequests(t *testing.T) {
	var celCount uint64
	r := New(WithWorkerCount(4))
	defer r.Stop()
	r.RegisterHandler(TierCEL, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddUint64(&celCount, 1)
		// Simulate work.
		time.Sleep(5 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"ok": true}) //nolint:errcheck
	})

	const concurrency = 20
	results := make(chan int, concurrency)

	for i := 0; i < concurrency; i++ {
		go func() {
			body := unifiedRequest{Input: map[string]any{"expr": "true"}}
			b, _ := json.Marshal(body)
			req := httptest.NewRequest("POST", "/v1/verify", bytes.NewReader(b))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			r.VerifyHandler()(rec, req)
			results <- rec.Code
		}()
	}

	for i := 0; i < concurrency; i++ {
		code := <-results
		if code != http.StatusOK {
			t.Errorf("expected status 200, got %d", code)
		}
	}

	if atomic.LoadUint64(&celCount) != concurrency {
		t.Errorf("expected %d handler invocations, got %d", concurrency, celCount)
	}

	// Verify stats reflect pool usage.
	req := httptest.NewRequest("GET", "/v1/router/stats", nil)
	rec := httptest.NewRecorder()
	r.StatsHandler()(rec, req)

	var statsResp statsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &statsResp); err != nil {
		t.Fatalf("failed to unmarshal stats response: %v", err)
	}
	if statsResp.Result.PoolStats["cel"].TotalJobs != uint64(concurrency) {
		t.Errorf("expected %d total jobs, got %d", concurrency, statsResp.Result.PoolStats["cel"].TotalJobs)
	}
}

func TestWorkerPoolOptions(t *testing.T) {
	r := New(WithWorkerCount(2), WithQueueLen(4))
	defer r.Stop()

	if r.pools[TierCEL] == nil || r.pools[TierWazero] == nil || r.pools[TierZ3] == nil {
		t.Fatal("expected all tier pools to be initialised")
	}
}
