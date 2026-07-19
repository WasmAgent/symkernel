package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestMiddlewareAllowsMatchingBearerToken(t *testing.T) {
	t.Setenv(clientTokenEnv, "test-token")

	handler := Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
}

func TestMiddlewareRejectsInvalidAuthorization(t *testing.T) {
	tests := []struct {
		name          string
		envToken      string
		authorization string
	}{
		{
			name:          "mismatch",
			envToken:      "test-token",
			authorization: "Bearer wrong-token",
		},
		{
			name:          "missing header",
			envToken:      "test-token",
			authorization: "",
		},
		{
			name:          "wrong scheme",
			envToken:      "test-token",
			authorization: "Basic test-token",
		},
		{
			name:          "unset expected token",
			envToken:      "",
			authorization: "Bearer test-token",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(clientTokenEnv, tt.envToken)

			called := false
			handler := Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				called = true
				w.WriteHeader(http.StatusOK)
			}))

			req := httptest.NewRequest(http.MethodGet, "/", nil)
			if tt.authorization != "" {
				req.Header.Set("Authorization", tt.authorization)
			}
			rec := httptest.NewRecorder()

			handler.ServeHTTP(rec, req)

			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
			}
			if called {
				t.Fatal("next handler was called")
			}
		})
	}
}
