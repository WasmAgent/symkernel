package main

import (
	"encoding/json"
	"net/http"
)

// healthResponse is the body returned by GET /v1/health.
type healthResponse struct {
	Status string `json:"status"`
}

// healthHandler reports liveness for symkerneld. It always responds with
// HTTP 200 and {"status":"ok"} once the process is serving requests.
func healthHandler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(healthResponse{Status: "ok"}) //nolint:errcheck // ResponseWriter write failure is unrecoverable
}
