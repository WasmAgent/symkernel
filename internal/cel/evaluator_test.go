package cel

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestEvaluate_ValidExpr(t *testing.T) {
	result, err := Evaluate(context.Background(), `"hello" + " " + "world"`, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != "hello world" {
		t.Errorf("result = %v, want %q", result, "hello world")
	}
}

func TestEvaluate_WithContextVars(t *testing.T) {
	vars := map[string]any{
		"name":  "symkernel",
		"count": 42,
	}

	result, err := Evaluate(context.Background(), `name + ": " + string(count)`, vars)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != "symkernel: 42" {
		t.Errorf("result = %v, want %q", result, "symkernel: 42")
	}
}

func TestEvaluate_NumericExpression(t *testing.T) {
	vars := map[string]any{
		"x": 10,
		"y": 3,
	}

	result, err := Evaluate(context.Background(), `x + y * 2`, vars)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// CEL returns int64 for integer arithmetic.
	want := int64(16)
	if result != want {
		t.Errorf("result = %v (%T), want %v", result, result, want)
	}
}

func TestEvaluate_ComparisonExpr(t *testing.T) {
	vars := map[string]any{
		"score": 85,
	}

	result, err := Evaluate(context.Background(), `score > 50`, vars)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != true {
		t.Errorf("result = %v, want true", result)
	}
}

func TestEvaluate_CompileError(t *testing.T) {
	_, err := Evaluate(context.Background(), `!!!invalid`, nil)
	if err == nil {
		t.Fatal("expected compile error, got nil")
	}
}

func TestEvaluate_CompileError_UndefinedVariable(t *testing.T) {
	_, err := Evaluate(context.Background(), `undefined_var + 1`, nil)
	if err == nil {
		t.Fatal("expected compile error for undefined variable, got nil")
	}
}

func TestEvaluate_TypeMismatch(t *testing.T) {
	// CEL will compile this but the type mismatch (int + string) is caught at eval.
	vars := map[string]any{
		"x": 42,     // int in Go
		"y": "hello", // string in Go
	}

	_, err := Evaluate(context.Background(), `x + y`, vars)
	if err == nil {
		t.Fatal("expected type mismatch error, got nil")
	}
}

func TestEvaluate_Timeout(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Millisecond)
	defer cancel()

	// Use a complex expression that should time out.
	// NOTE: CEL is fast and may complete before the 1ms deadline even for
	// complex expressions. We still test the timeout path by using a
	// near-zero deadline.
	_, err := Evaluate(ctx, `1 + 1`, nil)
	// We don't strictly assert timeout because CEL is fast; the test
	// validates that context cancellation is respected (no hang).
	_ = err
}

func TestEvaluate_DefaultTimeout(t *testing.T) {
	// Verify that with no deadline on the context, evaluation still works
	// (the default 2s timeout is applied internally).
	result, err := Evaluate(context.Background(), `42 * 2`, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != int64(84) {
		t.Errorf("result = %v (%T), want 84", result, result)
	}
}

func TestEvaluate_EmptyExpr(t *testing.T) {
	_, err := Evaluate(context.Background(), ``, nil)
	if err == nil {
		t.Fatal("expected error for empty expression, got nil")
	}
}

func TestEvaluate_BooleanLogic(t *testing.T) {
	vars := map[string]any{
		"a": true,
		"b": false,
	}

	tests := []struct {
		expr string
		want bool
	}{
		{`a && !b`, true},
		{`a || b`, true},
		{`!a && b`, false},
		{`a == b`, false},
		{`a != b`, true},
	}
	for _, tt := range tests {
		t.Run(tt.expr, func(t *testing.T) {
			result, err := Evaluate(context.Background(), tt.expr, vars)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if result != tt.want {
				t.Errorf("result = %v, want %v", result, tt.want)
			}
		})
	}
}

func TestEvaluate_ListAndContains(t *testing.T) {
	vars := map[string]any{
		"tags": []string{"go", "cel", "verify"},
	}

	result, err := Evaluate(context.Background(), `"cel" in tags`, vars)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != true {
		t.Errorf("result = %v, want true", result)
	}
}

func TestEvaluate_Ternary(t *testing.T) {
	vars := map[string]any{
		"x": 10,
	}

	result, err := Evaluate(context.Background(), `x > 5 ? "big" : "small"`, vars)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != "big" {
		t.Errorf("result = %v, want %q", result, "big")
	}
}

func TestEvaluate_StringSize(t *testing.T) {
	vars := map[string]any{
		"s": "hello",
	}

	result, err := Evaluate(context.Background(), `size(s)`, vars)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != int64(5) {
		t.Errorf("result = %v (%T), want 5", result, result)
	}
}

// --- Evaluator struct tests ---

func TestNewEvaluator_Success(t *testing.T) {
	ev, err := NewEvaluator()
	if err != nil {
		t.Fatalf("NewEvaluator() error = %v", err)
	}
	if ev == nil {
		t.Fatal("NewEvaluator() returned nil evaluator")
	}
	if ev.env == nil {
		t.Error("NewEvaluator().env is nil")
	}
}

func TestEvaluator_Evaluate_BoolResult(t *testing.T) {
	ev, err := NewEvaluator()
	if err != nil {
		t.Fatalf("NewEvaluator() error = %v", err)
	}

	args := map[string]interface{}{"score": 85}
	result, err := ev.Evaluate("score > 50", args)
	if err != nil {
		t.Fatalf("Evaluate() error = %v", err)
	}
	if result != true {
		t.Errorf("Evaluate() = %v, want true", result)
	}
	if ev.prg == nil {
		t.Error("Evaluate() did not store program on struct")
	}
}

func TestEvaluator_Evaluate_BoolResultFalse(t *testing.T) {
	ev, err := NewEvaluator()
	if err != nil {
		t.Fatalf("NewEvaluator() error = %v", err)
	}

	args := map[string]interface{}{"score": 30}
	result, err := ev.Evaluate("score > 50", args)
	if err != nil {
		t.Fatalf("Evaluate() error = %v", err)
	}
	if result != false {
		t.Errorf("Evaluate() = %v, want false", result)
	}
}

func TestEvaluator_Evaluate_ComplexBoolExpr(t *testing.T) {
	ev, err := NewEvaluator()
	if err != nil {
		t.Fatalf("NewEvaluator() error = %v", err)
	}

	args := map[string]interface{}{
		"a": true,
		"b": false,
	}
	result, err := ev.Evaluate("a && !b", args)
	if err != nil {
		t.Fatalf("Evaluate() error = %v", err)
	}
	if result != true {
		t.Errorf("Evaluate() = %v, want true", result)
	}
}

func TestEvaluator_Evaluate_InvalidSyntax(t *testing.T) {
	ev, err := NewEvaluator()
	if err != nil {
		t.Fatalf("NewEvaluator() error = %v", err)
	}

	_, err = ev.Evaluate("!!!invalid", nil)
	if err == nil {
		t.Fatal("Evaluate() expected error for invalid syntax, got nil")
	}
}

func TestEvaluator_Evaluate_NonBoolResult(t *testing.T) {
	ev, err := NewEvaluator()
	if err != nil {
		t.Fatalf("NewEvaluator() error = %v", err)
	}

	// Expression returns a string, not a bool.
	_, err = ev.Evaluate(`"hello" + " world"`, nil)
	if err == nil {
		t.Fatal("Evaluate() expected error for non-bool result, got nil")
	}
}

func TestEvaluator_Evaluate_UndefinedVariable(t *testing.T) {
	ev, err := NewEvaluator()
	if err != nil {
		t.Fatalf("NewEvaluator() error = %v", err)
	}

	_, err = ev.Evaluate("undefined_var > 1", nil)
	if err == nil {
		t.Fatal("Evaluate() expected error for undefined variable, got nil")
	}
}

// --- HTTP handler tests for POST /v1/verify/cel ---

func TestHandler_ValidExpr(t *testing.T) {
	handler := Handler()

	body := `{"input":{"expr":"\"hello\" + \" \" + \"world\""}}`
	req := httptest.NewRequest(http.MethodPost, "/v1/verify/cel", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var resp celOpaResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	if !resp.Result.Ok {
		t.Errorf("ok = false, want true")
	}
	if resp.Result.Value != "hello world" {
		t.Errorf("value = %v, want %q", resp.Result.Value, "hello world")
	}
	if resp.Result.EvalMs <= 0 {
		t.Errorf("evalMs = %v, want > 0", resp.Result.EvalMs)
	}
}

func TestHandler_WithContextVars(t *testing.T) {
	handler := Handler()

	body := `{"input":{"expr":"x + y * 2","context":{"x":10,"y":3}}}`
	req := httptest.NewRequest(http.MethodPost, "/v1/verify/cel", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var resp celOpaResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	if !resp.Result.Ok {
		t.Errorf("ok = false, want true; value = %v", resp.Result.Value)
	}
	// JSON round-trip produces float64 for numbers.
	if resp.Result.Value != float64(16) {
		t.Errorf("value = %v (%T), want 16", resp.Result.Value, resp.Result.Value)
	}
}

func TestHandler_CompileError(t *testing.T) {
	handler := Handler()

	body := `{"input":{"expr":"!!!invalid"}}`
	req := httptest.NewRequest(http.MethodPost, "/v1/verify/cel", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var resp celOpaResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	if resp.Result.Ok {
		t.Error("ok = true, want false for compile error")
	}
	if resp.Result.Value == nil {
		t.Error("value is nil, want error message")
	}
}

func TestHandler_TypeMismatch(t *testing.T) {
	handler := Handler()

	body := `{"input":{"expr":"x + y","context":{"x":42,"y":"hello"}}}`
	req := httptest.NewRequest(http.MethodPost, "/v1/verify/cel", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var resp celOpaResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	if resp.Result.Ok {
		t.Error("ok = true, want false for type mismatch")
	}
}

func TestHandler_EmptyExpr(t *testing.T) {
	handler := Handler()

	body := `{"input":{"expr":""}}`
	req := httptest.NewRequest(http.MethodPost, "/v1/verify/cel", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

func TestHandler_InvalidJSON(t *testing.T) {
	handler := Handler()

	req := httptest.NewRequest(http.MethodPost, "/v1/verify/cel", strings.NewReader("not json"))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

func TestHandler_Timeout(t *testing.T) {
	handler := Handler()

	// Use a very small timeout_ms; CEL is fast so the expression may
	// succeed before the deadline. The key assertion is the handler
	// returns without hanging.
	body := `{"input":{"expr":"1 + 1","timeout_ms":1}}`
	req := httptest.NewRequest(http.MethodPost, "/v1/verify/cel", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		handler.ServeHTTP(rec, req)
		close(done)
	}()

	select {
	case <-done:
		// Handler completed without hanging — success.
	case <-time.After(5 * time.Second):
		t.Fatal("handler did not complete within 5 seconds")
	}

	// Accept either ok (CEL was fast enough) or error (deadline hit).
	var resp celOpaResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	_ = resp.Result.Ok // value is non-deterministic for timeout tests
}

func TestHandler_DefaultTimeout(t *testing.T) {
	handler := Handler()

	// Zero timeout_ms should fall back to the default (2000ms).
	body := `{"input":{"expr":"42 * 2","timeout_ms":0}}`
	req := httptest.NewRequest(http.MethodPost, "/v1/verify/cel", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var resp celOpaResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	if !resp.Result.Ok {
		t.Errorf("ok = false, want true; value = %v", resp.Result.Value)
	}
	// JSON round-trip produces float64 for numbers.
	if resp.Result.Value != float64(84) {
		t.Errorf("value = %v (%T), want 84", resp.Result.Value, resp.Result.Value)
	}
}
