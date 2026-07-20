// Package diagnostics provides verification explainability for failed
// verifications. It stores structured WhyFailed explanations keyed by
// decision_id and exposes them via GET /v1/diagnostics/<decision_id>.
package diagnostics

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
)

// WhyFailed is a structured explanation for a single constraint violation
// discovered during verification. It captures the constraint name, the actual
// observed value, the configured limit, and a human-readable remediation hint.
type WhyFailed struct {
	// Constraint is the name of the constraint that was violated (e.g.
	// "max_memory", "max_instructions").
	Constraint string `json:"constraint"`

	// Actual is the observed value that triggered the violation. It may be
	// a number, string, or other JSON-compatible value depending on the
	// constraint type.
	Actual any `json:"actual"`

	// Limit is the threshold defined by the constraint that was exceeded.
	// It may be a number, string, or other JSON-compatible value.
	Limit any `json:"limit"`

	// Remediation is a human-readable suggestion for resolving the
	// violation (e.g. "reduce_allocation_or_increase_limit").
	Remediation string `json:"remediation"`
}

// DiagnosticRecord holds the full set of WhyFailed explanations for a single
// verification decision. It is returned by GET /v1/diagnostics/<decision_id>.
type DiagnosticRecord struct {
	// DecisionID is the unique identifier of the verification decision.
	DecisionID string `json:"decision_id"`

	// Reasons lists every constraint violation that caused the verification
	// to fail. Empty (but present) means the decision passed or has no
	// diagnostics recorded.
	Reasons []WhyFailed `json:"reasons"`
}

// Store holds in-memory diagnostics keyed by decision_id. It is safe for
// concurrent use.
type Store struct {
	mu    sync.RWMutex
	items map[string][]WhyFailed
}

// New creates a new diagnostics Store.
func New() *Store {
	return &Store{
		items: make(map[string][]WhyFailed),
	}
}

// Record associates a set of WhyFailed explanations with the given decision_id.
// If reasons is empty the decision is still recorded (allowing callers to
// distinguish "no diagnostics" from "unknown decision").
func (s *Store) Record(decisionID string, reasons []WhyFailed) {
	s.mu.Lock()
	defer s.mu.Unlock()

	cp := make([]WhyFailed, len(reasons))
	copy(cp, reasons)
	s.items[decisionID] = cp
}

// Lookup retrieves the WhyFailed explanations for a decision. The returned
// slice is owned by the caller. The boolean reports whether the decision_id
// was found at all.
func (s *Store) Lookup(decisionID string) ([]WhyFailed, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	reasons, ok := s.items[decisionID]
	if !ok {
		return nil, false
	}
	cp := make([]WhyFailed, len(reasons))
	copy(cp, reasons)
	return cp, true
}

// --- HTTP handlers ---

// RegisterRoutes registers the diagnostics endpoints on the given ServeMux.
// It mounts:
//
//	GET /v1/diagnostics/{decision_id} — structured WhyFailed explanation
func (s *Store) RegisterRoutes(mux *http.ServeMux) {
	mux.Handle("GET /v1/diagnostics/{decision_id}", s.lookupHandler())
}

// lookupHandler handles GET /v1/diagnostics/{decision_id}. It returns a JSON
// DiagnosticRecord with the stored WhyFailed explanations.
func (s *Store) lookupHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		decisionID := r.PathValue("decision_id")
		if decisionID == "" {
			http.Error(w, "missing decision_id in path", http.StatusBadRequest)
			return
		}

		reasons, ok := s.Lookup(decisionID)
		if !ok {
			http.Error(w, fmt.Sprintf("decision %q not found", decisionID), http.StatusNotFound)
			return
		}

		rec := DiagnosticRecord{
			DecisionID: decisionID,
			Reasons:    reasons,
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(rec) //nolint:errcheck // partial write to ResponseWriter is unrecoverable
	}
}
