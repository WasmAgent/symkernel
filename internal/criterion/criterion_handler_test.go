package criterion

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHandlerAcceptsSchemaBackedCriterion(t *testing.T) {
	body := `{"criterion":{"id":"must-use-json","description":"output must be JSON","verify_method":"cel_expr","arg":{"expr":"true"},"level":"hard","priority":100,"category":"format"}}`
	req := httptest.NewRequest(http.MethodPost, "/v1/verify/criterion", strings.NewReader(body))
	rec := httptest.NewRecorder()

	Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var got VerifyResponse
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !got.OK {
		t.Fatalf("ok = false, hint = %q", got.Hint)
	}
	if got.CriterionID != "must-use-json" {
		t.Fatalf("criterionId = %q, want %q", got.CriterionID, "must-use-json")
	}
}

func TestHandlerRejectsInvalidCriterion(t *testing.T) {
	body := `{"criterion":{"id":"bad","description":"bad category","verify_method":"cel_expr","level":"hard","priority":1,"category":"unknown"}}`
	req := httptest.NewRequest(http.MethodPost, "/v1/verify/criterion", strings.NewReader(body))
	rec := httptest.NewRecorder()

	Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var got VerifyResponse
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got.OK {
		t.Fatal("ok = true, want false")
	}
	if got.CriterionID != "bad" {
		t.Fatalf("criterionId = %q, want %q", got.CriterionID, "bad")
	}
	if !strings.Contains(got.Hint, "criterion.category") {
		t.Fatalf("hint = %q, want category validation message", got.Hint)
	}
}

func TestHandlerRejectsInvalidRepairMaxRounds(t *testing.T) {
	body := `{"criterion":{"id":"bad-repair","description":"bad repair","verify_method":"cel_expr","level":"hard","priority":1,"category":"format","repair":{"strategy":"patch","max_rounds":0}}}`
	req := httptest.NewRequest(http.MethodPost, "/v1/verify/criterion", strings.NewReader(body))
	rec := httptest.NewRecorder()

	Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var got VerifyResponse
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got.OK {
		t.Fatal("ok = true, want false")
	}
	if got.CriterionID != "bad-repair" {
		t.Fatalf("criterionId = %q, want %q", got.CriterionID, "bad-repair")
	}
	if !strings.Contains(got.Hint, "criterion.repair.max_rounds") {
		t.Fatalf("hint = %q, want repair max_rounds validation message", got.Hint)
	}
}

func TestHandlerRejectsNonPost(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/v1/verify/criterion", nil)
	rec := httptest.NewRecorder()

	Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusMethodNotAllowed)
	}
}

func TestHandlerRejectsTrailingJSON(t *testing.T) {
	body := `{"criterion":{"id":"t","description":"d","verify_method":"cel_expr","level":"hard","priority":1,"category":"format"}}{"extra":true}`
	req := httptest.NewRequest(http.MethodPost, "/v1/verify/criterion", strings.NewReader(body))
	rec := httptest.NewRecorder()

	Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d: %s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
}
