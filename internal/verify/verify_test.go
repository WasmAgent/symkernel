package verify

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"
)

// mockSolver is a test stub that returns predetermined results.
type mockSolver struct {
	result Result
	err    error
}

func (m *mockSolver) Solve(_ context.Context, _ string) (Result, error) {
	return m.result, m.err
}

// blockingSolver blocks until the context is cancelled, simulating a
// solver that takes too long (used for timeout tests).
type blockingSolver struct{}

func (s *blockingSolver) Solve(ctx context.Context, _ string) (Result, error) {
	<-ctx.Done()
	return Result{}, ctx.Err()
}

func TestHandler_Sat(t *testing.T) {
	mock := &mockSolver{
		result: Result{Sat: "sat", Model: map[string]any{"x": "6"}},
	}
	handler := Handler(mock)

	body := `{"input":{"constraints_smt2":"(declare-const x Int) (assert (> x 5))"}}`
	req := httptest.NewRequest(http.MethodPost, "/v1/verify/z3", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var resp opaResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	if resp.Result.Sat != "sat" {
		t.Errorf("sat = %q, want %q", resp.Result.Sat, "sat")
	}
	if resp.Result.Model == nil {
		t.Fatal("model is nil, want non-nil")
	}
	if resp.Result.Model["x"] != "6" {
		t.Errorf("model[x] = %v, want %q", resp.Result.Model["x"], "6")
	}
}

func TestHandler_Unsat(t *testing.T) {
	mock := &mockSolver{
		result: Result{Sat: "unsat", Model: nil},
	}
	handler := Handler(mock)

	body := `{"input":{"constraints_smt2":"(assert false)"}}`
	req := httptest.NewRequest(http.MethodPost, "/v1/verify/z3", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var resp opaResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	if resp.Result.Sat != "unsat" {
		t.Errorf("sat = %q, want %q", resp.Result.Sat, "unsat")
	}
	if resp.Result.Model != nil {
		t.Errorf("model = %v, want nil", resp.Result.Model)
	}
}

func TestHandler_Timeout(t *testing.T) {
	handler := Handler(&blockingSolver{})

	body := `{"input":{"constraints_smt2":"(assert true)","timeout_ms":1}}`
	req := httptest.NewRequest(http.MethodPost, "/v1/verify/z3", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		handler.ServeHTTP(rec, req)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("timeout test did not complete within 5 seconds")
	}

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var resp opaResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	if resp.Result.Sat != "unknown" {
		t.Errorf("sat = %q, want %q", resp.Result.Sat, "unknown")
	}
	if resp.Result.Model != nil {
		t.Errorf("model = %v, want nil", resp.Result.Model)
	}
}

func TestHandler_SolverError(t *testing.T) {
	handler := Handler(&mockSolver{err: fmt.Errorf("z3: executable not found")})

	body := `{"input":{"constraints_smt2":"(assert true)"}}`
	req := httptest.NewRequest(http.MethodPost, "/v1/verify/z3", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
	}
}

func TestHandler_InvalidJSON(t *testing.T) {
	handler := Handler(&mockSolver{})

	req := httptest.NewRequest(http.MethodPost, "/v1/verify/z3", strings.NewReader("not json"))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

func TestHandler_EmptyConstraints(t *testing.T) {
	handler := Handler(&mockSolver{})

	body := `{"input":{"constraints_smt2":""}}`
	req := httptest.NewRequest(http.MethodPost, "/v1/verify/z3", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

func TestHandler_DefaultTimeout(t *testing.T) {
	// Verify that a zero timeout_ms falls back to the default.
	var capturedTimeout time.Duration
	captureSolver := &mockSolver{
		result: Result{Sat: "sat", Model: nil},
	}

	// We can't easily inspect the context timeout from the mock, so we
	// verify indirectly: the request should succeed (default timeout is
	// 2000ms, plenty of time for an instant mock).
	handler := Handler(captureSolver)

	body := `{"input":{"constraints_smt2":"(assert true)","timeout_ms":0}}`
	req := httptest.NewRequest(http.MethodPost, "/v1/verify/z3", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var resp opaResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	if resp.Result.Sat != "sat" {
		t.Errorf("sat = %q, want %q", resp.Result.Sat, "sat")
	}
	_ = capturedTimeout // avoid unused variable lint
}

func TestParseModel(t *testing.T) {
	tests := []struct {
		name  string
		lines []string
		want  map[string]any
	}{
		{
			name:  "simple binding",
			lines: []string{"((x 6))"},
			want:  map[string]any{"x": "6"},
		},
		{
			name:  "multiple bindings",
			lines: []string{"((x 6))", "((y 10))"},
			want:  map[string]any{"x": "6", "y": "10"},
		},
		{
			name:  "empty input",
			lines: nil,
			want:  nil,
		},
		{
			name:  "model envelope markers",
			lines: []string{"(model", "((x 1))", ")"},
			want:  map[string]any{"x": "1"},
		},
		{
			name:  "blank lines ignored",
			lines: []string{"", "((x 2))", ""},
			want:  map[string]any{"x": "2"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseModel(tt.lines)
			if tt.want == nil {
				if got != nil {
					t.Errorf("parseModel() = %v, want nil", got)
				}
				return
			}
			if len(got) != len(tt.want) {
				t.Errorf("parseModel() len = %d, want %d", len(got), len(tt.want))
				return
			}
			for k, v := range tt.want {
				if got[k] != v {
					t.Errorf("parseModel()[%q] = %v, want %v", k, got[k], v)
				}
			}
		})
	}
}

// verifyCommand is the YAML structure for .claude-bot/verify.yml entries.
type verifyCommand struct {
	Name    string `yaml:"name"`
	Cmd     string `yaml:"cmd"`
	Timeout string `yaml:"timeout"`
	Level   int    `yaml:"level"`
}

// verifyConfig is the top-level YAML structure for .claude-bot/verify.yml.
type verifyConfig struct {
	Commands []verifyCommand `yaml:"commands"`
}

func TestVerifyYAML_StaticcheckAllEntry(t *testing.T) {
	path := "../../.claude-bot/verify.yml"
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	var cfg verifyConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}

	var found bool
	for _, c := range cfg.Commands {
		if c.Name == "staticcheck-all" {
			found = true
			if c.Level != 4 {
				t.Errorf("staticcheck-all level = %d, want 4", c.Level)
			}
			if !strings.Contains(c.Cmd, "staticcheck -checks all") {
				t.Errorf("staticcheck-all cmd = %q, want to contain 'staticcheck -checks all'", c.Cmd)
			}
			if strings.Contains(c.Cmd, "head -") {
				t.Errorf("staticcheck-all cmd = %q, must not truncate output with 'head'", c.Cmd)
			}
			if c.Timeout == "" {
				t.Error("staticcheck-all timeout is empty, want a non-empty timeout")
			}
			break
		}
	}
	if !found {
		t.Error("staticcheck-all command not found in verify.yml")
	}
}
