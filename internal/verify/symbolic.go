// Package verify provides the symbolic and SMT verification primitives used
// by symkerneld. The symbolic types in this file define the Milestone 3
// contract that endpoint handlers wire against; Run is a stub until the
// full Z3-backed symbolic exploration engine lands.
package verify

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/google/uuid"
)

// ErrNotImplemented is returned by Run until the Z3-backed symbolic
// exploration engine is implemented. Endpoint handlers should still wire
// Run so the contract is exercised end-to-end while the engine matures.
var ErrNotImplemented = errors.New("symbolic verification not implemented")

// SymbolicInput is the request payload for symbolic verification: a base64
// WebAssembly binary, the entry-point export to explore, and its arguments.
type SymbolicInput struct {
	// WasmBinary is the base64-encoded WebAssembly module to explore.
	WasmBinary string `json:"wasmBinary"`
	// Entry names the export (function) to begin symbolic execution from.
	Entry string `json:"entry"`
	// Args are the initial argument values passed to Entry.
	Args []any `json:"args"`
}

// SymbolicPath describes one feasible execution path discovered during
// symbolic exploration: the guarding path constraint (SMT2) and a
// satisfying model.
type SymbolicPath struct {
	// Constraints is the SMT2 path constraint that guards this path.
	Constraints string `json:"constraints"`
	// Model is a satisfying assignment for Constraints, keyed by symbol.
	Model map[string]any `json:"model"`
}

// SymbolicResult holds the set of explored paths and bookkeeping fields.
type SymbolicResult struct {
	// Paths is the set of feasible execution paths discovered.
	Paths []SymbolicPath `json:"paths"`
	// Explored is the total number of paths considered (feasible or not).
	Explored int `json:"explored"`
	// DecisionID is the per-call UUID, following the GENAI_SEMCONV field
	// naming used across the WasmAgent ecosystem. Every response carries
	// one for traceability — see CLAUDE.md "Bot instructions".
	DecisionID string `json:"decision_id"`
}

// Run executes symbolic exploration of in.WasmBinary starting at in.Entry.
//
// The engine is not yet implemented: Run always returns ErrNotImplemented
// alongside a result whose DecisionID is populated with a fresh UUID, so
// endpoint handlers can call it today and surface a decision_id to callers
// while the Z3-backed implementation lands. When ctx is cancelled the stub
// still returns the same sentinel rather than ctx.Err(), since no work is
// performed.
func Run(ctx context.Context, in SymbolicInput) (SymbolicResult, error) {
	_ = ctx // no work performed by the stub; reserved for the real engine
	_ = in
	return SymbolicResult{DecisionID: uuid.NewString()}, ErrNotImplemented
}

// symbolicPlaceholderResponse is the fixed acknowledgement body returned by
// SymbolicHandler while the symbolic execution engine is being built.
type symbolicPlaceholderResponse struct {
	Message string `json:"message"`
}

// SymbolicHandler returns an http.HandlerFunc for the POST /v1/verify/symbolic
// endpoint.
//
// It is a placeholder: the route contract, middleware wiring, and content
// type are exercised end-to-end, but no symbolic exploration is performed.
// It always responds 200 OK with
// {"message": "Symbolic execution endpoint placeholder"} so callers can detect
// that the route is mounted while the Z3-backed engine behind Run matures. It
// is a prerequisite for the full symbolic execution logic (issue #245): once
// Run is implemented, this handler will decode a SymbolicInput, call Run, and
// shape the SymbolicResult into the response.
func SymbolicHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(symbolicPlaceholderResponse{
			Message: "Symbolic execution endpoint placeholder",
		})
	}
}
