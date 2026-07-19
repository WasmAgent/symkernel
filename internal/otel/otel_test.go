package otel

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
)

func TestInitProviderReturnsShutdown(t *testing.T) {
	shutdown, err := InitProvider(context.Background(), nil)
	if err != nil {
		t.Fatalf("InitProvider(nil) error = %v", err)
	}
	if shutdown == nil {
		t.Fatal("InitProvider(nil) returned nil shutdown func")
	}
	if err := shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown() error = %v", err)
	}
}

func TestInitProviderWithWriter(t *testing.T) {
	var buf bytes.Buffer
	shutdown, err := InitProvider(context.Background(), &buf)
	if err != nil {
		t.Fatalf("InitProvider(&buf) error = %v", err)
	}
	if shutdown == nil {
		t.Fatal("returned nil shutdown func")
	}
	if err := shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown() error = %v", err)
	}
}

func TestDecisionIDFromContextEmpty(t *testing.T) {
	got := DecisionIDFromContext(context.Background())
	if got != "" {
		t.Fatalf("DecisionIDFromContext(bg) = %q, want empty", got)
	}
}

func TestMiddlewareSetsDecisionID(t *testing.T) {
	// Set up a provider so the tracer works (nil writer = io.Discard).
	shutdown, err := InitProvider(context.Background(), nil)
	if err != nil {
		t.Fatalf("InitProvider: %v", err)
	}
	t.Cleanup(func() {
		if err := shutdown(context.Background()); err != nil {
			t.Errorf("shutdown: %v", err)
		}
	})

	var gotCtxID string
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCtxID = DecisionIDFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	})

	handler := Middleware(next)

	req := httptest.NewRequest(http.MethodPost, "/v1/verify/cel", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	headerID := rec.Header().Get(DecisionIDHeader)
	if headerID == "" {
		t.Fatal("X-Decision-Id response header is empty")
	}
	if _, err := uuid.Parse(headerID); err != nil {
		t.Fatalf("X-Decision-Id = %q, not a valid UUID: %v", headerID, err)
	}
	if gotCtxID == "" {
		t.Fatal("decision_id not propagated to handler context")
	}
	if gotCtxID != headerID {
		t.Fatalf("context ID = %q, header ID = %q; want equal", gotCtxID, headerID)
	}
}

func TestMiddlewareRecordsErrorStatus(t *testing.T) {
	shutdown, err := InitProvider(context.Background(), nil)
	if err != nil {
		t.Fatalf("InitProvider: %v", err)
	}
	t.Cleanup(func() {
		if err := shutdown(context.Background()); err != nil {
			t.Errorf("shutdown: %v", err)
		}
	})

	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	})

	handler := Middleware(next)
	req := httptest.NewRequest(http.MethodGet, "/missing", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
	// decision_id should still be set on error responses.
	if rec.Header().Get(DecisionIDHeader) == "" {
		t.Fatal("X-Decision-Id header missing on error response")
	}
}

func TestMiddlewareDifferentRoutes(t *testing.T) {
	shutdown, err := InitProvider(context.Background(), nil)
	if err != nil {
		t.Fatalf("InitProvider: %v", err)
	}
	t.Cleanup(func() {
		if err := shutdown(context.Background()); err != nil {
			t.Errorf("shutdown: %v", err)
		}
	})

	routes := []struct {
		method string
		path   string
	}{
		{http.MethodPost, "/v1/verify/cel"},
		{http.MethodPost, "/v1/verify/criterion"},
		{http.MethodPost, "/v1/sandbox/run"},
		{http.MethodPost, "/v1/verify/z3"},
	}

	for _, rt := range routes {
		t.Run(rt.method+" "+rt.path, func(t *testing.T) {
			var gotID string
			next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotID = DecisionIDFromContext(r.Context())
				w.WriteHeader(http.StatusOK)
			})

			handler := Middleware(next)
			req := httptest.NewRequest(rt.method, rt.path, nil)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", rec.Code)
			}
			if gotID == "" {
				t.Fatal("decision_id not set")
			}
			if _, err := uuid.Parse(gotID); err != nil {
				t.Fatalf("decision_id = %q, not a UUID: %v", gotID, err)
			}
		})
	}
}

func TestMiddlewareUniqueDecisionIDs(t *testing.T) {
	shutdown, err := InitProvider(context.Background(), nil)
	if err != nil {
		t.Fatalf("InitProvider: %v", err)
	}
	t.Cleanup(func() {
		if err := shutdown(context.Background()); err != nil {
			t.Errorf("shutdown: %v", err)
		}
	})

	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	handler := Middleware(next)

	ids := make(map[string]bool)
	for i := 0; i < 100; i++ {
		req := httptest.NewRequest(http.MethodPost, "/", nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		id := rec.Header().Get(DecisionIDHeader)
		if ids[id] {
			t.Fatalf("duplicate decision_id %q at iteration %d", id, i)
		}
		ids[id] = true
	}
}
