package taint

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/WasmAgent/symkernel/internal/z3"
)

type mockSolver struct {
	solution z3.Solution
	query    string
	model    map[string]any
}

func (s *mockSolver) Solve(_ context.Context, query string, model map[string]any) (z3.Solution, error) {
	s.query = query
	s.model = model
	return s.solution, nil
}

func TestAnalyzePropagatesTaintAndProvesPolicy(t *testing.T) {
	solver := &mockSolver{solution: z3.Solution{Sat: "unsat"}}
	result, err := AnalyzeWithSolver(context.Background(), Input{
		Sources: []string{"request"},
		Operations: []Operation{
			{Name: "normalize", Inputs: []string{"request"}, Outputs: []string{"normalized"}},
			{Name: "db.write", Inputs: []string{"normalized"}, Sensitive: true},
		},
		Constraints: []string{"(= x 1)"},
		Policies:    []string{"(> x 0)"},
	}, solver)
	if err != nil {
		t.Fatalf("AnalyzeWithSolver returned error: %v", err)
	}
	if !result.Safe {
		t.Fatalf("Safe = false, want true; result = %#v", result)
	}
	if result.Checked != 1 || len(result.Findings) != 1 {
		t.Fatalf("checked/findings = %d/%d, want 1/1", result.Checked, len(result.Findings))
	}
	finding := result.Findings[0]
	if finding.Sink != "db.write" || len(finding.Sources) != 1 || finding.Sources[0] != "request" {
		t.Errorf("finding = %#v, want db.write tainted by request", finding)
	}
	if !strings.Contains(solver.query, "(assert (= x 1))") || !strings.Contains(solver.query, "(assert (not (and (> x 0))))") {
		t.Errorf("query did not combine assumption and negated policy:\n%s", solver.query)
	}
	if solver.model["x"] != "Int" {
		t.Errorf("model[x] = %v, want inferred Int sort", solver.model["x"])
	}
}

func TestAnalyzeReportsTaintedPolicyViolation(t *testing.T) {
	result, err := AnalyzeWithSolver(context.Background(), Input{
		Sources:             []string{"untrusted_query"},
		Flows:               []Flow{{From: "untrusted_query", To: "db.write"}},
		SensitiveOperations: []string{"db.write"},
		Policies:            []string{"(= allowed 1)"},
	}, &mockSolver{solution: z3.Solution{Sat: "sat", Model: map[string]any{"allowed": "0"}}})
	if err != nil {
		t.Fatalf("AnalyzeWithSolver returned error: %v", err)
	}
	if result.Safe || len(result.Violations) != 1 {
		t.Fatalf("result = %#v, want one unsafe violation", result)
	}
	if result.Violations[0].Sat != "sat" || result.Violations[0].Model["allowed"] != "0" {
		t.Errorf("violation = %#v, want SAT counterexample", result.Violations[0])
	}
}

func TestInputUnmarshalFlowMap(t *testing.T) {
	var input Input
	if err := json.Unmarshal([]byte(`{"sources":["request"],"flows":{"request":["query"]}}`), &input); err != nil {
		t.Fatalf("unmarshal flow map: %v", err)
	}
	if got := input.FlowMap["request"]; len(got) != 1 || got[0] != "query" {
		t.Fatalf("FlowMap = %#v, want request -> query", input.FlowMap)
	}
}

func TestHandlerAcceptsOPAEnvelope(t *testing.T) {
	handler := HandlerWithSolver(&mockSolver{solution: z3.Solution{Sat: "unsat"}})
	req := httptest.NewRequest(http.MethodPost, "/v1/verify/taint", strings.NewReader(`{"input":{"sources":["request"],"flows":[{"from":"request","to":"sink"}],"sensitive_operations":["sink"],"policies":["(assert false)"]}}`))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	var result Result
	if err := json.NewDecoder(rec.Body).Decode(&result); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if !result.Safe || result.DecisionID == "" || result.Checked != 1 {
		t.Errorf("result = %#v, want safe checked result with decision ID", result)
	}
}
