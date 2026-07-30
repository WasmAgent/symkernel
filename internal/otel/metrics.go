// Package otel provides OpenTelemetry span instrumentation for HTTP handlers,
// and SMT solver metrics (solver_duration_ms, sat/unsat/unknown counts,
// constraint_complexity_score, cache hit/miss) exported in Prometheus text
// format at GET /metrics.
package otel

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
)

// SMTMetrics holds atomic counters for SMT solver telemetry. It is safe for
// concurrent use by multiple goroutines. Use NewSMTMetrics to create an
// instance, then call the Record* methods from solver invocation sites.
type SMTMetrics struct {
	satCount     atomic.Int64
	unsatCount   atomic.Int64
	unknownCount atomic.Int64
	cacheHit     atomic.Int64
	cacheMiss    atomic.Int64

	mu       sync.Mutex
	durations []int64 // solver_duration_ms samples (unbounded; reset on Snapshot)
	complexity []float64 // constraint_complexity_score samples
}

// NewSMTMetrics creates and returns a ready-to-use SMTMetrics.
func NewSMTMetrics() *SMTMetrics {
	return &SMTMetrics{}
}

// RecordSat increments the sat counter and records the solver duration and
// optional constraint complexity score.
func (m *SMTMetrics) RecordSat(durationMs int64, complexityScore float64) {
	m.satCount.Add(1)
	m.record(durationMs, complexityScore)
}

// RecordUnsat increments the unsat counter.
func (m *SMTMetrics) RecordUnsat(durationMs int64, complexityScore float64) {
	m.unsatCount.Add(1)
	m.record(durationMs, complexityScore)
}

// RecordUnknown increments the unknown counter.
func (m *SMTMetrics) RecordUnknown(durationMs int64, complexityScore float64) {
	m.unknownCount.Add(1)
	m.record(durationMs, complexityScore)
}

// RecordCacheHit increments the cache-hit counter.
func (m *SMTMetrics) RecordCacheHit() {
	m.cacheHit.Add(1)
}

// RecordCacheMiss increments the cache-miss counter.
func (m *SMTMetrics) RecordCacheMiss() {
	m.cacheMiss.Add(1)
}

// record appends a duration and complexity sample under the mutex.
func (m *SMTMetrics) record(durationMs int64, complexityScore float64) {
	m.mu.Lock()
	m.durations = append(m.durations, durationMs)
	m.complexity = append(m.complexity, complexityScore)
	m.mu.Unlock()
}

// SMTSnapshot is a point-in-time view of SMTMetrics.
type SMTSnapshot struct {
	SatCount         int64
	UnsatCount       int64
	UnknownCount     int64
	CacheHit         int64
	CacheMiss        int64
	AvgDurationMs    float64
	P50DurationMs    int64
	P95DurationMs    int64
	P99DurationMs    int64
	MaxDurationMs    int64
	TotalSamples     int
	AvgComplexity    float64
}

// Snapshot returns a consistent point-in-time snapshot of current counters
// and duration percentiles.
func (m *SMTMetrics) Snapshot() SMTSnapshot {
	m.mu.Lock()
	durs := make([]int64, len(m.durations))
	copy(durs, m.durations)
	comps := make([]float64, len(m.complexity))
	copy(comps, m.complexity)
	m.mu.Unlock()

	snap := SMTSnapshot{
		SatCount:     m.satCount.Load(),
		UnsatCount:   m.unsatCount.Load(),
		UnknownCount: m.unknownCount.Load(),
		CacheHit:     m.cacheHit.Load(),
		CacheMiss:    m.cacheMiss.Load(),
		TotalSamples: len(durs),
	}

	if len(durs) > 0 {
		sorted := make([]int64, len(durs))
		copy(sorted, durs)
		sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })

		var sum int64
		for _, d := range sorted {
			sum += d
		}
		snap.AvgDurationMs = float64(sum) / float64(len(sorted))
		snap.MaxDurationMs = sorted[len(sorted)-1]
		snap.P50DurationMs = percentile(sorted, 50)
		snap.P95DurationMs = percentile(sorted, 95)
		snap.P99DurationMs = percentile(sorted, 99)
	}
	if len(comps) > 0 {
		var sum float64
		for _, c := range comps {
			sum += c
		}
		snap.AvgComplexity = sum / float64(len(comps))
	}
	return snap
}

// percentile returns the p-th percentile value from a sorted slice.
func percentile(sorted []int64, p int) int64 {
	if len(sorted) == 0 {
		return 0
	}
	idx := (p * len(sorted)) / 100
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

// PrometheusText renders the SMT metrics snapshot in Prometheus text format
// (version 0.0.4), suitable for inclusion in a /metrics handler response.
func (m *SMTMetrics) PrometheusText() string {
	snap := m.Snapshot()
	var b strings.Builder

	b.WriteString("# HELP symkernel_smt_solver_sat_total Total number of SAT results from the SMT solver.\n")
	b.WriteString("# TYPE symkernel_smt_solver_sat_total counter\n")
	fmt.Fprintf(&b, "symkernel_smt_solver_sat_total %d\n", snap.SatCount)

	b.WriteString("# HELP symkernel_smt_solver_unsat_total Total number of UNSAT results from the SMT solver.\n")
	b.WriteString("# TYPE symkernel_smt_solver_unsat_total counter\n")
	fmt.Fprintf(&b, "symkernel_smt_solver_unsat_total %d\n", snap.UnsatCount)

	b.WriteString("# HELP symkernel_smt_solver_unknown_total Total number of UNKNOWN results from the SMT solver.\n")
	b.WriteString("# TYPE symkernel_smt_solver_unknown_total counter\n")
	fmt.Fprintf(&b, "symkernel_smt_solver_unknown_total %d\n", snap.UnknownCount)

	b.WriteString("# HELP symkernel_smt_solver_duration_ms SMT solver duration in milliseconds.\n")
	b.WriteString("# TYPE symkernel_smt_solver_duration_ms summary\n")
	fmt.Fprintf(&b, `symkernel_smt_solver_duration_ms{quantile="0.5"} %d`+"\n", snap.P50DurationMs)
	fmt.Fprintf(&b, `symkernel_smt_solver_duration_ms{quantile="0.95"} %d`+"\n", snap.P95DurationMs)
	fmt.Fprintf(&b, `symkernel_smt_solver_duration_ms{quantile="0.99"} %d`+"\n", snap.P99DurationMs)
	fmt.Fprintf(&b, "symkernel_smt_solver_duration_ms_sum %d\n", sumInt64(snap))
	fmt.Fprintf(&b, "symkernel_smt_solver_duration_ms_count %d\n", snap.TotalSamples)

	b.WriteString("# HELP symkernel_smt_constraint_complexity_score_avg Average constraint complexity score (clause count proxy).\n")
	b.WriteString("# TYPE symkernel_smt_constraint_complexity_score_avg gauge\n")
	fmt.Fprintf(&b, "symkernel_smt_constraint_complexity_score_avg %g\n", snap.AvgComplexity)

	b.WriteString("# HELP symkernel_smt_cache_hit_total Total number of SMT result cache hits.\n")
	b.WriteString("# TYPE symkernel_smt_cache_hit_total counter\n")
	fmt.Fprintf(&b, "symkernel_smt_cache_hit_total %d\n", snap.CacheHit)

	b.WriteString("# HELP symkernel_smt_cache_miss_total Total number of SMT result cache misses.\n")
	b.WriteString("# TYPE symkernel_smt_cache_miss_total counter\n")
	fmt.Fprintf(&b, "symkernel_smt_cache_miss_total %d\n", snap.CacheMiss)

	return b.String()
}

// sumInt64 returns the approximate sum from the snapshot avg × count.
func sumInt64(s SMTSnapshot) int64 {
	if s.TotalSamples == 0 {
		return 0
	}
	return int64(s.AvgDurationMs * float64(s.TotalSamples))
}

// MetricsHandler returns an http.HandlerFunc that responds with the
// concatenated Prometheus text output of one or more SMTMetrics instances.
// It is intended to be mounted at /metrics alongside the observability
// collector's existing endpoint.
func MetricsHandler(metrics ...*SMTMetrics) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		var b strings.Builder
		for _, m := range metrics {
			b.WriteString(m.PrometheusText())
		}
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		fmt.Fprint(w, b.String())
	}
}

// ConstraintComplexityScore computes a lightweight proxy for constraint
// complexity: it counts the number of top-level SMTLIB2 s-expressions
// (assertion forms) in the constraints string. This is intentionally simple
// to keep the hot path cheap; more sophisticated scoring can be added later.
func ConstraintComplexityScore(constraints string) float64 {
	count := 0
	depth := 0
	topLevel := false
	for _, ch := range constraints {
		switch ch {
		case '(':
			depth++
			if depth == 1 {
				topLevel = true
			}
		case ')':
			if depth == 1 && topLevel {
				count++
				topLevel = false
			}
			depth--
		}
	}
	return float64(count)
}

// RecordSolverResult records an SMT solver outcome in m, computing the
// complexity score automatically from the constraints string.
func RecordSolverResult(m *SMTMetrics, sat string, durationMs int64, constraints string) {
	score := ConstraintComplexityScore(constraints)
	switch sat {
	case "sat":
		m.RecordSat(durationMs, score)
	case "unsat":
		m.RecordUnsat(durationMs, score)
	default:
		m.RecordUnknown(durationMs, score)
	}
}

// GlobalSMTMetrics is the package-level SMTMetrics instance used by the
// symkerneld HTTP server. Handlers that invoke the Z3 solver should record
// results here via RecordSolverResult.
var GlobalSMTMetrics = NewSMTMetrics()

// RegisterMetricsRoute mounts a GET /metrics handler on mux that exports
// GlobalSMTMetrics in Prometheus text format. It is called by routes.go.
func RegisterMetricsRoute(mux *http.ServeMux) {
	mux.Handle("GET /metrics", MetricsHandler(GlobalSMTMetrics))
}


