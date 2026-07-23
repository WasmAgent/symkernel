package criterion

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func post(t *testing.T, body VerifyCriterionRequest) *httptest.ResponseRecorder {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/verify/criterion", bytes.NewReader(b))
	rr := httptest.NewRecorder()
	Handler()(rr, req)
	return rr
}

func celCriterion(id, expr string, ctx map[string]any) ConstraintIR {
	return ConstraintIR{
		ID:           id,
		Description:  "test constraint " + id,
		VerifyMethod: "cel_expr",
		Arg:          map[string]any{"expr": expr, "context": ctx},
		Level:        "hard",
		Priority:     50,
		Category:     "content",
	}
}

func TestHandler_CELPass(t *testing.T) {
	rr := post(t, VerifyCriterionRequest{
		Criterion: celCriterion("c1", "size(output) <= 5", map[string]any{"output": "abc"}),
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var resp VerifyCriterionResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.OK {
		t.Errorf("expected OK=true, got %+v", resp)
	}
	if resp.CriterionID != "c1" {
		t.Errorf("criterionId = %q, want c1", resp.CriterionID)
	}
	if resp.Hint != "" {
		t.Errorf("pass should carry no hint, got %q", resp.Hint)
	}
}

func TestHandler_CELViolationCarriesHint(t *testing.T) {
	rr := post(t, VerifyCriterionRequest{
		Criterion: celCriterion("c2", "size(output) <= 2", map[string]any{"output": "abcde"}),
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var resp VerifyCriterionResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.OK {
		t.Errorf("expected OK=false for violated constraint")
	}
	if resp.Hint != "test constraint c2" {
		t.Errorf("violation should carry the description as hint, got %q", resp.Hint)
	}
}

func TestHandler_UnknownMethod(t *testing.T) {
	c := celCriterion("c3", "true", nil)
	c.VerifyMethod = "not_a_real_method"
	rr := post(t, VerifyCriterionRequest{Criterion: c})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("unknown method should be 400, got %d (%s)", rr.Code, rr.Body.String())
	}
}

func TestHandler_MissingFields(t *testing.T) {
	// missing id
	rr := post(t, VerifyCriterionRequest{Criterion: ConstraintIR{VerifyMethod: "cel_expr"}})
	if rr.Code != http.StatusBadRequest {
		t.Errorf("missing id should be 400, got %d", rr.Code)
	}
	// missing verify_method
	rr = post(t, VerifyCriterionRequest{Criterion: ConstraintIR{ID: "x"}})
	if rr.Code != http.StatusBadRequest {
		t.Errorf("missing verify_method should be 400, got %d", rr.Code)
	}
}

func TestHandler_BadCELArg(t *testing.T) {
	c := ConstraintIR{ID: "c4", Description: "d", VerifyMethod: "cel_expr", Level: "hard", Category: "content"}
	c.Arg = map[string]any{"context": map[string]any{}} // no expr
	rr := post(t, VerifyCriterionRequest{Criterion: c})
	if rr.Code != http.StatusBadRequest {
		t.Errorf("missing expr should be 400, got %d (%s)", rr.Code, rr.Body.String())
	}
}
