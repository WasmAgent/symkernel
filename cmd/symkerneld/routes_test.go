package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
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

	// The POST /v1/verify/symbolic route is mounted: an empty request is
	// rejected by its handler rather than falling through to the mux 404.
	sreq := httptest.NewRequest(http.MethodPost, "/v1/verify/symbolic", nil)
	srec := httptest.NewRecorder()
	mux.ServeHTTP(srec, sreq)
	if srec.Code != http.StatusBadRequest {
		t.Errorf("POST /v1/verify/symbolic status = %d, want %d; body = %s", srec.Code, http.StatusBadRequest, srec.Body.String())
	}
}

func TestRegisterRoutes_SymbolicExecution(t *testing.T) {
	// This is a Wasm module which exports main() -> i32 and returns 7.
	const module = "AGFzbQEAAAABBQFgAAF/AwIBAAcIAQRtYWluAAAKBgEEAEEHCw=="

	mux := http.NewServeMux()
	RegisterRoutes(mux)

	req := httptest.NewRequest(http.MethodPost, "/v1/verify/symbolic", strings.NewReader(
		`{"module":"`+module+`","entrypoint":"main","maxDepth":100,"pruneInfeasible":true}`,
	))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("POST /v1/verify/symbolic status = %d, want %d; body = %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var response struct {
		Paths []struct {
			Feasible    bool     `json:"feasible"`
			Constraints []string `json:"constraints"`
			Output      float64  `json:"output"`
		} `json:"paths"`
		Explored int `json:"explored"`
		Pruned   int `json:"pruned"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&response); err != nil {
		t.Fatalf("decode symbolic response: %v", err)
	}
	if response.Explored != 1 || response.Pruned != 0 || len(response.Paths) != 1 {
		t.Fatalf("symbolic response = %+v, want one explored feasible path", response)
	}
	path := response.Paths[0]
	if !path.Feasible || len(path.Constraints) != 0 || path.Output != 7 {
		t.Errorf("symbolic path = %+v, want feasible path with output 7", path)
	}
}
