// Package orchestrator provides a verification routing engine that analyzes
// query complexity, cost targets, and accuracy requirements to automatically
// select the optimal verification tier: CEL, wazero, or Z3. It also exposes
// routing metrics via the GET /v1/orchestration/stats endpoint.
package orchestrator

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/WasmAgent/symkernel/internal/criterion"
	"github.com/WasmAgent/symkernel/internal/otel"
)

// Tier identifies a verification backend. Values are stable wire identifiers
// used in the routing decision and stats responses.
type Tier string

const (
	// TierCEL selects the CEL expression evaluator — fast, best for simple
	// boolean / format / content constraints with low complexity.
	TierCEL Tier = "cel"

	// TierWazero selects the wazero WebAssembly runtime — medium speed, for
	// Wasm-compiled verifiers requiring sandboxed execution.
	TierWazero Tier = "wazero"

	// TierZ3 selects the Z3 SMT solver — thorough but expensive, for
	// quantifier-heavy or complex logical constraints requiring SMT solving.
	TierZ3 Tier = "z3"
)

// Complexity describes the structural difficulty of a verification query.
type Complexity struct {
	// ConstraintCount is the number of individual constraints in the query.
	// Zero means "unknown".
	ConstraintCount int `json:"constraint_count"`

	// MaxNestingDepth is the maximum nesting depth of expressions or
	// constraint trees. Zero means "unknown".
	MaxNestingDepth int `json:"max_nesting_depth"`

	// HasQuantifiers is true when the query uses universal/existential
	// quantifiers (forall, exists) that typically require SMT solving.
	HasQuantifiers bool `json:"has_quantifiers"`
}

// VerificationRequest is the input to the Route function. It carries the
// structural complexity of the query along with cost and accuracy
// preferences so the router can select the optimal verification tier.
type VerificationRequest struct {
	// Complexity describes the structural difficulty of the verification
	// query. When all fields are zero-valued the router treats complexity
	// as unknown and uses a default heuristic.
	Complexity Complexity `json:"complexity"`

	// CostTargetMs is the caller's maximum acceptable latency in
	// milliseconds for the verification. Zero means "no preference".
	CostTargetMs int `json:"cost_target_ms"`

	// AccuracyRequired is the minimum confidence level (0–100) the caller
	// needs. Higher values push toward more thorough tiers (Z3). Zero
	// means "no preference" (default 80).
	AccuracyRequired int `json:"accuracy_required"`

	// MethodHint is an optional preference from the caller. When set to a
	// valid Tier value the router honours it unless it would violate the
	// CostTargetMs. Empty string means "auto-select".
	MethodHint string `json:"method_hint"`
}

// TierAlternative describes a tier that was considered but not selected,
// along with the reason it was not chosen.
type TierAlternative struct {
	// Tier is the verification backend that was not selected.
	Tier Tier `json:"tier"`

	// Reason is a human-readable explanation of why this tier was not
	// chosen.
	Reason string `json:"reason"`
}

// TierSelection is the output of the Route function, containing the chosen
// verification tier, the rationale, and the alternatives that were
// considered but not selected.
type TierSelection struct {
	// Tier is the selected verification backend.
	Tier Tier `json:"tier"`

	// Reason is a human-readable explanation of why this tier was chosen.
	Reason string `json:"reason"`

	// EstimatedCostMs is the router's estimated latency for the selected
	// tier based on the query complexity.
	EstimatedCostMs int `json:"estimated_cost_ms"`

	// Alternatives lists tiers that were considered but not selected, along
	// with the reason each was not chosen.
	Alternatives []TierAlternative `json:"alternatives,omitempty"`
}

// tierConfig holds per-tier routing thresholds and estimated costs.
type tierConfig struct {
	tier             Tier
	maxConstraintCount int
	maxNestingDepth    int
	baseCostMs         int
	accuracyCeiling    int
	label              string
}

var (
	// tierConfigs defines the tier selection cascade. The router walks the
	// list from fastest to most thorough and picks the first tier whose
	// thresholds cover the query's complexity.
	tierConfigs = []tierConfig{
		{
			tier:               TierCEL,
			maxConstraintCount: 20,
			maxNestingDepth:    3,
			baseCostMs:         5,
			accuracyCeiling:    85,
			label:              "CEL (fast expression evaluation)",
		},
		{
			tier:               TierWazero,
			maxConstraintCount: 100,
			maxNestingDepth:    10,
			baseCostMs:         50,
			accuracyCeiling:    95,
			label:              "wazero (sandboxed Wasm verification)",
		},
		{
			tier:               TierZ3,
			maxConstraintCount: 1 << 30, // effectively unlimited
			maxNestingDepth:    1 << 30,
			baseCostMs:         200,
			accuracyCeiling:    100,
			label:              "Z3 (SMT solver — maximum accuracy)",
		},
	}
)

// Router analyzes VerificationRequests and selects the best verification
// tier. It is safe for concurrent use. Use NewRouter to create an instance.
type Router struct {
	mu sync.RWMutex

	// routeCounts tracks the number of Route calls per tier.
	routeCounts map[Tier]*uint64

	// routeLatencyMs tracks cumulative latency samples per tier.
	routeLatencyMs map[Tier]*uint64

	// routeSamples tracks the number of latency samples per tier.
	routeSamples map[Tier]*uint64
}

// NewRouter creates a Router with initialised metric counters.
func NewRouter() *Router {
	r := &Router{
		routeCounts:    make(map[Tier]*uint64),
		routeLatencyMs: make(map[Tier]*uint64),
		routeSamples:   make(map[Tier]*uint64),
	}
	for _, tc := range tierConfigs {
		r.routeCounts[tc.tier] = new(uint64)
		r.routeLatencyMs[tc.tier] = new(uint64)
		r.routeSamples[tc.tier] = new(uint64)
	}
	return r
}

// selectTier contains the core routing logic without metrics recording or
// alternatives computation. It is unexported; use Route instead.
func (r *Router) selectTier(query VerificationRequest) (*TierSelection, error) {
	// Validate input.
	accuracy := query.AccuracyRequired
	if accuracy == 0 {
		accuracy = 80 // default accuracy requirement
	}
	if accuracy < 0 || accuracy > 100 {
		return nil, fmt.Errorf("orchestrator: accuracy_required must be 0–100, got %d", query.AccuracyRequired)
	}

	// Honour explicit method hint when it's valid and within cost budget.
	if query.MethodHint != "" {
		t := Tier(query.MethodHint)
		if t != TierCEL && t != TierWazero && t != TierZ3 {
			return nil, fmt.Errorf("orchestrator: invalid method_hint %q, must be one of cel, wazero, z3", query.MethodHint)
		}
		tc := configForTier(t)
		if query.CostTargetMs == 0 || tc.baseCostMs <= query.CostTargetMs {
			return &TierSelection{
				Tier:            t,
				Reason:          fmt.Sprintf("method_hint=%q honoured (base cost %dms within budget)", query.MethodHint, tc.baseCostMs),
				EstimatedCostMs: tc.baseCostMs,
			}, nil
		}
		// Hint violates cost budget — fall through to auto-select.
	}

	// Auto-select: walk tiers from fastest to most thorough.
	cx := query.Complexity
	for _, tc := range tierConfigs {
		// Skip tiers that can't meet the accuracy requirement.
		if accuracy > tc.accuracyCeiling {
			continue
		}
		// Skip tiers whose base cost exceeds the caller's budget.
		if query.CostTargetMs > 0 && tc.baseCostMs > query.CostTargetMs {
			continue
		}
		// Select the first tier whose structural thresholds cover the query.
		fits := true
		if cx.ConstraintCount > 0 && cx.ConstraintCount > tc.maxConstraintCount {
			fits = false
		}
		if cx.MaxNestingDepth > 0 && cx.MaxNestingDepth > tc.maxNestingDepth {
			fits = false
		}
		if cx.HasQuantifiers && tc.tier == TierCEL {
			fits = false
		}
		if fits {
			reason := fmt.Sprintf("auto-selected %s: constraint_count=%d, nesting_depth=%d, quantifiers=%v, accuracy=%d",
				tc.label, cx.ConstraintCount, cx.MaxNestingDepth, cx.HasQuantifiers, accuracy)
			return &TierSelection{
				Tier:            tc.tier,
				Reason:          reason,
				EstimatedCostMs: tc.baseCostMs,
			}, nil
		}
	}

	// Fallback: Z3 always qualifies (unlimited thresholds, accuracy=100).
	tc := configForTier(TierZ3)
	return &TierSelection{
		Tier:            TierZ3,
		Reason:          "fallback to Z3 (no other tier fits constraints and budget)",
		EstimatedCostMs: tc.baseCostMs,
	}, nil
}

// Route analyzes the given VerificationRequest and returns the optimal
// TierSelection with alternatives. It records routing metrics on the
// Router for later retrieval via Stats.
func (r *Router) Route(query VerificationRequest) (*TierSelection, error) {
	sel, err := r.selectTier(query)
	if err != nil {
		return nil, err
	}
	r.recordRoute(sel.Tier, sel.EstimatedCostMs)
	sel.Alternatives = r.computeAlternatives(query, sel.Tier)
	return sel, nil
}

// recordRoute increments routing metrics for the given tier.
func (r *Router) recordRoute(t Tier, costMs int) {
	atomic.AddUint64(r.routeCounts[t], 1)
	atomic.AddUint64(r.routeLatencyMs[t], uint64(costMs))
	atomic.AddUint64(r.routeSamples[t], 1)
}

// computeAlternatives returns the list of tiers that were considered but
// not selected, with human-readable reasons for each.
func (r *Router) computeAlternatives(query VerificationRequest, selected Tier) []TierAlternative {
	accuracy := query.AccuracyRequired
	if accuracy == 0 {
		accuracy = 80
	}
	cx := query.Complexity
	var alts []TierAlternative

	for _, tc := range tierConfigs {
		if tc.tier == selected {
			continue
		}
		var reasons []string
		if accuracy > tc.accuracyCeiling {
			reasons = append(reasons, fmt.Sprintf("accuracy requirement %d exceeds ceiling %d", accuracy, tc.accuracyCeiling))
		}
		if query.CostTargetMs > 0 && tc.baseCostMs > query.CostTargetMs {
			reasons = append(reasons, fmt.Sprintf("base cost %dms exceeds budget %dms", tc.baseCostMs, query.CostTargetMs))
		}
		if cx.ConstraintCount > 0 && cx.ConstraintCount > tc.maxConstraintCount {
			reasons = append(reasons, fmt.Sprintf("constraint count %d exceeds max %d", cx.ConstraintCount, tc.maxConstraintCount))
		}
		if cx.MaxNestingDepth > 0 && cx.MaxNestingDepth > tc.maxNestingDepth {
			reasons = append(reasons, fmt.Sprintf("nesting depth %d exceeds max %d", cx.MaxNestingDepth, tc.maxNestingDepth))
		}
		if cx.HasQuantifiers && tc.tier == TierCEL {
			reasons = append(reasons, "cannot handle quantifiers (forall/exists)")
		}
		if len(reasons) == 0 {
			reasons = append(reasons, "lower priority than selected tier")
		}
		alts = append(alts, TierAlternative{
			Tier:   tc.tier,
			Reason: strings.Join(reasons, "; "),
		})
	}
	return alts
}

// Stats returns a snapshot of routing metrics. It is safe to call from
// concurrent goroutines.
func (r *Router) Stats() Stats {
	r.mu.RLock()
	defer r.mu.RUnlock()

	s := Stats{Tiers: make(map[string]TierStats)}
	for _, tc := range tierConfigs {
		count := atomic.LoadUint64(r.routeCounts[tc.tier])
		samples := atomic.LoadUint64(r.routeSamples[tc.tier])
		var avgMs float64
		if samples > 0 {
			avgMs = float64(atomic.LoadUint64(r.routeLatencyMs[tc.tier])) / float64(samples)
		}
		s.Tiers[string(tc.tier)] = TierStats{
			RouteCount: count,
			AvgCostMs:  avgMs,
		}
		s.TotalRoutes += count
	}
	return s
}

// Stats is the JSON response body for GET /v1/orchestration/stats.
type Stats struct {
	// TotalRoutes is the cumulative number of Route calls.
	TotalRoutes uint64 `json:"total_routes"`

	// Tiers breaks down metrics per verification tier.
	Tiers map[string]TierStats `json:"tiers"`
}

// TierStats holds per-tier routing metrics.
type TierStats struct {
	// RouteCount is the number of times this tier was selected.
	RouteCount uint64 `json:"route_count"`

	// AvgCostMs is the average estimated cost (ms) for this tier.
	AvgCostMs float64 `json:"avg_cost_ms"`
}

// --- HTTP handlers ---

// routeRequest wraps the JSON request body for POST /v1/orchestration/route.
type routeRequest struct {
	Query VerificationRequest `json:"query"`
}

// routeResponse wraps the OPA-envelope response body for route requests.
type routeResponse struct {
	Result     *TierSelection `json:"result"`
	DecisionID string        `json:"decision_id"`
}

// RouteHandler returns an http.HandlerFunc for the POST /v1/orchestration/route
// endpoint. It accepts a VerificationRequest in an OPA-envelope and returns the
// TierSelection with a decision_id.
func (r *Router) RouteHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		var body routeRequest
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			http.Error(w, fmt.Sprintf("invalid request: %v", err), http.StatusBadRequest)
			return
		}

		sel, err := r.Route(body.Query)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		resp := routeResponse{
			Result:     sel,
			DecisionID: otel.DecisionIDFromContext(req.Context()),
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp) //nolint:errcheck // partial write to ResponseWriter is unrecoverable
	}
}

// statsResponse wraps the OPA-envelope response body for stats requests.
type statsResponse struct {
	Result     Stats  `json:"result"`
	DecisionID string `json:"decision_id"`
}

// StatsHandler returns an http.HandlerFunc for the GET /v1/orchestration/stats
// endpoint. It returns current routing metrics in an OPA-envelope.
func (r *Router) StatsHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		stats := r.Stats()
		resp := statsResponse{
			Result:     stats,
			DecisionID: otel.DecisionIDFromContext(req.Context()),
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp) //nolint:errcheck // partial write to ResponseWriter is unrecoverable
	}
}

// --- Orchestrated verification endpoint ---

// orchestratedRequest wraps the JSON request body for POST
// /v1/verify/orchestrated. It extends the routing query with an optional
// criterion constraint for downstream verification integration.
type orchestratedRequest struct {
	Query     VerificationRequest     `json:"query"`
	Criterion *criterion.ConstraintIR `json:"criterion,omitempty"`
}

// orchestratedResponse wraps the OPA-envelope response body for orchestrated
// verification requests.
type orchestratedResponse struct {
	Result     *orchestratedResult `json:"result"`
	DecisionID string              `json:"decision_id"`
}

// orchestratedResult is the routing result for an orchestrated verification,
// containing the tier selection with alternatives and an optional criterion
// reference for downstream verification.
type orchestratedResult struct {
	TierSelection `json:",inline"`

	// CriterionID echoes the criterion ID when a constraint was supplied in
	// the request, enabling downstream verification pipelines to correlate
	// the routing decision with the original constraint.
	CriterionID string `json:"criterion_id,omitempty"`
}

// OrchestratedVerifyHandler returns an http.HandlerFunc for the POST
// /v1/verify/orchestrated endpoint. It accepts a verification query with an
// optional criterion constraint, routes it to the optimal tier, and returns
// the selection with alternatives.
func (r *Router) OrchestratedVerifyHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		var body orchestratedRequest
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			http.Error(w, fmt.Sprintf("invalid request: %v", err), http.StatusBadRequest)
			return
		}

		sel, err := r.Route(body.Query)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		result := &orchestratedResult{
			TierSelection: *sel,
		}
		if body.Criterion != nil {
			result.CriterionID = body.Criterion.ID
		}

		resp := orchestratedResponse{
			Result:     result,
			DecisionID: otel.DecisionIDFromContext(req.Context()),
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp) //nolint:errcheck // partial write to ResponseWriter is unrecoverable
	}
}

// RegisterRoutes registers the orchestrator endpoints on the given ServeMux.
// It mounts:
//
//	POST /v1/orchestration/route    — verification routing
//	GET  /v1/orchestration/stats    — routing metrics
//	POST /v1/verify/orchestrated    — orchestrated verification with alternatives
func (r *Router) RegisterRoutes(mux *http.ServeMux) {
	mux.Handle("POST /v1/orchestration/route", r.RouteHandler())
	mux.Handle("GET /v1/orchestration/stats", r.StatsHandler())
	mux.Handle("POST /v1/verify/orchestrated", r.OrchestratedVerifyHandler())
}

// ParseTier parses a tier string into a Tier value. Returns an error for
// unrecognised values.
func ParseTier(s string) (Tier, error) {
	switch strings.ToLower(s) {
	case string(TierCEL):
		return TierCEL, nil
	case string(TierWazero):
		return TierWazero, nil
	case string(TierZ3):
		return TierZ3, nil
	default:
		return "", fmt.Errorf("orchestrator: unknown tier %q", s)
	}
}

// configForTier returns the tierConfig for the given Tier, or panics if the
// tier is not registered. This is an internal helper called only after
// validation.
func configForTier(t Tier) tierConfig {
	for _, tc := range tierConfigs {
		if tc.tier == t {
			return tc
		}
	}
	// Should never happen if ParseTier / validation gate is used.
	panic("orchestrator: configForTier called with unregistered tier " + t)
}

// FprintStats writes human-readable stats to w. It is a convenience wrapper
// used in health-check or admin endpoints. Exported for testing.
func FprintStats(w *strings.Builder, s Stats) {
	fmt.Fprintf(w, "Total routes: %d\n", s.TotalRoutes)
	for name, ts := range s.Tiers {
		fmt.Fprintf(w, "  %s: %d routes, avg %.1f ms\n", name, ts.RouteCount, ts.AvgCostMs)
	}
}

// formatInt is a helper that exists only so the tests can verify stats
// rendering without importing fmt directly.
func formatInt(v uint64) string {
	return strconv.FormatUint(v, 10)
}
