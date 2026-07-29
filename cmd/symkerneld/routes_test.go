package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestRegisterRoutes mounts the full route table on a fresh mux and asserts:
//   - GET /v1/health responds 200 {"status":"ok"} (the scaffold contract).
//   - An unregistered path returns 404, proving the mux discriminates
//     registered routes (RegisterRoutes does not wildcard-mount).
//   - POST /v1/verify/z3 is mounted: its handler rejects an empty body with
//     400 rather than falling through to the mux 404.
//
// This exercises the RegisterRoutes(mux *http.ServeMux) extension point that
// endpoint issues rely on to add routes without rewriting main.
func TestRegisterRoutes(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	RegisterRoutes(mux)

	// Health route is mounted and serves the documented contract.
	hreq := httptest.NewRequest(http.MethodGet, "/v1/health", nil)
	hrec := httptest.NewRecorder()
	mux.ServeHTTP(hrec, hreq)

	if hrec.Code != http.StatusOK {
		t.Fatalf("GET /v1/health status = %d, want %d; body = %s", hrec.Code, http.StatusOK, hrec.Body.String())
	}
	var body healthResponse
	if err := json.NewDecoder(hrec.Body).Decode(&body); err != nil {
		t.Fatalf("decode health body: %v; body = %s", err, hrec.Body.String())
	}
	if body.Status != "ok" {
		t.Errorf("health status = %q, want %q", body.Status, "ok")
	}

	// An unregistered path returns 404, proving registered routes discriminate.
	ureq := httptest.NewRequest(http.MethodGet, "/v1/does-not-exist", nil)
	urec := httptest.NewRecorder()
	mux.ServeHTTP(urec, ureq)
	if urec.Code != http.StatusNotFound {
		t.Errorf("GET /v1/does-not-exist status = %d, want %d", urec.Code, http.StatusNotFound)
	}

	// The POST /v1/verify/z3 route is mounted: an empty body is rejected by
	// the handler (400) rather than falling through to a mux 404.
	zreq := httptest.NewRequest(http.MethodPost, "/v1/verify/z3", nil)
	zrec := httptest.NewRecorder()
	mux.ServeHTTP(zrec, zreq)
	if zrec.Code == http.StatusNotFound {
		t.Errorf("POST /v1/verify/z3 status = 404; route not mounted by RegisterRoutes")
	}

	// The POST /v1/verify/symbolic placeholder route is mounted and responds
	// 200 with its placeholder body, proving RegisterRoutes wired it.
	sreq := httptest.NewRequest(http.MethodPost, "/v1/verify/symbolic", nil)
	srec := httptest.NewRecorder()
	mux.ServeHTTP(srec, sreq)
	if srec.Code != http.StatusOK {
		t.Fatalf("POST /v1/verify/symbolic status = %d, want %d; route not mounted or wrong handler; body = %s", srec.Code, http.StatusOK, srec.Body.String())
	}
	var sbody struct {
		Message string `json:"message"`
	}
	if err := json.NewDecoder(srec.Body).Decode(&sbody); err != nil {
		t.Fatalf("decode symbolic placeholder body: %v; body = %s", err, srec.Body.String())
	}
	if sbody.Message != "Symbolic execution endpoint placeholder" {
		t.Errorf("symbolic message = %q, want %q", sbody.Message, "Symbolic execution endpoint placeholder")
	}
}
