package tenant

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestTenantIDFromContextEmpty(t *testing.T) {
	got := TenantIDFromContext(t.Context())
	if got != "" {
		t.Fatalf("TenantIDFromContext(bg) = %q, want empty", got)
	}
}

func TestMiddlewareExtractsFromHeader(t *testing.T) {
	r := New(WithAllowedTenants("org-a,org-b"))
	var gotCtxID string
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCtxID = TenantIDFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	})

	handler := r.Middleware(next)

	req := httptest.NewRequest(http.MethodPost, "/v1/verify/cel", nil)
	req.Header.Set(TenantIDHeader, "org-a")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if gotCtxID != "org-a" {
		t.Fatalf("context tenant_id = %q, want %q", gotCtxID, "org-a")
	}
	if rec.Header().Get(TenantIDResponseHeader) != "org-a" {
		t.Fatalf("response header = %q, want %q", rec.Header().Get(TenantIDResponseHeader), "org-a")
	}
}

func TestMiddlewareExtractsFromJWTSub(t *testing.T) {
	r := New(WithAllowedTenants("tenant-123"))
	var gotCtxID string
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCtxID = TenantIDFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	})

	handler := r.Middleware(next)

	req := httptest.NewRequest(http.MethodPost, "/v1/verify/cel", nil)
	req.Header.Set("Authorization", "Bearer "+makeJWT(t, map[string]any{"sub": "tenant-123"}))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if gotCtxID != "tenant-123" {
		t.Fatalf("context tenant_id = %q, want %q", gotCtxID, "tenant-123")
	}
}

func TestMiddlewareJWTPrefersOverHeader(t *testing.T) {
	r := New(WithAllowedTenants("jwt-tenant"))
	var gotCtxID string
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCtxID = TenantIDFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	})

	handler := r.Middleware(next)

	req := httptest.NewRequest(http.MethodPost, "/v1/verify/cel", nil)
	req.Header.Set(TenantIDHeader, "header-tenant")
	req.Header.Set("Authorization", "Bearer "+makeJWT(t, map[string]any{"sub": "jwt-tenant"}))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if gotCtxID != "jwt-tenant" {
		t.Fatalf("context tenant_id = %q, want %q (JWT should win)", gotCtxID, "jwt-tenant")
	}
}

func TestMiddlewareRejectsDisallowedTenant(t *testing.T) {
	r := New(WithAllowedTenants("org-a"))
	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})

	handler := r.Middleware(next)

	req := httptest.NewRequest(http.MethodPost, "/v1/verify/cel", nil)
	req.Header.Set(TenantIDHeader, "org-b")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if called {
		t.Fatal("next handler should not be called for disallowed tenant")
	}
}

func TestMiddlewareOpenModeAllowsAny(t *testing.T) {
	r := New() // no allowlist → open mode
	var gotCtxID string
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCtxID = TenantIDFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	})

	handler := r.Middleware(next)

	req := httptest.NewRequest(http.MethodPost, "/v1/verify/cel", nil)
	req.Header.Set(TenantIDHeader, "any-random-tenant")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if gotCtxID != "any-random-tenant" {
		t.Fatalf("context tenant_id = %q, want %q", gotCtxID, "any-random-tenant")
	}
}

func TestMiddlewarePassesWithoutTenantID(t *testing.T) {
	r := New(WithAllowedTenants("org-a"))
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	handler := r.Middleware(next)

	req := httptest.NewRequest(http.MethodPost, "/v1/verify/cel", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (no tenant header should pass)", rec.Code)
	}
}

func TestMiddlewareRejectsDisallowedJWTTenant(t *testing.T) {
	r := New(WithAllowedTenants("allowed-tenant"))
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	handler := r.Middleware(next)

	req := httptest.NewRequest(http.MethodPost, "/v1/verify/cel", nil)
	req.Header.Set("Authorization", "Bearer "+makeJWT(t, map[string]any{"sub": "rogue-tenant"}))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
}

func TestMiddlewareInvalidJWT(t *testing.T) {
	r := New(WithAllowedTenants("org-a"))
	var gotCtxID string
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCtxID = TenantIDFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	})

	handler := r.Middleware(next)

	tests := []struct {
		name   string
		bearer string
		want   string
	}{
		{"not_jwt", "Bearer not-a-jwt", ""},
		{"empty_segments", "Bearer ..", ""},
		{"bad_base64", "Bearer x.y.z", ""},
		{"no_sub", "Bearer " + makeJWT(t, map[string]any{"iss": "test"}), ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotCtxID = ""
			req := httptest.NewRequest(http.MethodPost, "/v1/verify/cel", nil)
			req.Header.Set("Authorization", tt.bearer)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", rec.Code)
			}
			if gotCtxID != tt.want {
				t.Fatalf("tenant_id = %q, want %q", gotCtxID, tt.want)
			}
		})
	}
}

func TestParseAllowlist(t *testing.T) {
	tests := []struct {
		name string
		csv  string
		want map[string]bool
	}{
		{"empty", "", map[string]bool{}},
		{"single", "org-a", map[string]bool{"org-a": true}},
		{"multiple", "org-a, org-b,org-c", map[string]bool{"org-a": true, "org-b": true, "org-c": true}},
		{"trailing_comma", "org-a,", map[string]bool{"org-a": true}},
		{"spaces", "  org-a  ,  org-b  ", map[string]bool{"org-a": true, "org-b": true}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseAllowlist(tt.csv)
			if len(got) != len(tt.want) {
				t.Fatalf("len = %d, want %d", len(got), len(tt.want))
			}
			for k := range tt.want {
				if !got[k] {
					t.Fatalf("missing key %q", k)
				}
			}
		})
	}
}

// makeJWT builds a minimal unsigned JWT with the given claims for testing.
func makeJWT(t *testing.T, claims map[string]any) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("marshal claims: %v", err)
	}
	return header + "." + base64.RawURLEncoding.EncodeToString(payload) + "."
}