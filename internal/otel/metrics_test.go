package otel

import (
	"strings"
	"testing"
)

func TestSMTMetrics_Counters(t *testing.T) {
	m := NewSMTMetrics()
	m.RecordSat(10, 3.0)
	m.RecordSat(20, 4.0)
	m.RecordUnsat(5, 2.0)
	m.RecordUnknown(100, 1.0)

	snap := m.Snapshot()
	if snap.SatCount != 2 {
		t.Errorf("SatCount = %d, want 2", snap.SatCount)
	}
	if snap.UnsatCount != 1 {
		t.Errorf("UnsatCount = %d, want 1", snap.UnsatCount)
	}
	if snap.UnknownCount != 1 {
		t.Errorf("UnknownCount = %d, want 1", snap.UnknownCount)
	}
}

func TestSMTMetrics_CacheCounters(t *testing.T) {
	m := NewSMTMetrics()
	m.RecordCacheHit()
	m.RecordCacheHit()
	m.RecordCacheMiss()

	snap := m.Snapshot()
	if snap.CacheHit != 2 {
		t.Errorf("CacheHit = %d, want 2", snap.CacheHit)
	}
	if snap.CacheMiss != 1 {
		t.Errorf("CacheMiss = %d, want 1", snap.CacheMiss)
	}
}

func TestSMTMetrics_Duration(t *testing.T) {
	m := NewSMTMetrics()
	for i := int64(1); i <= 10; i++ {
		m.RecordSat(i*10, 1.0) // 10, 20, ..., 100
	}

	snap := m.Snapshot()
	if snap.TotalSamples != 10 {
		t.Errorf("TotalSamples = %d, want 10", snap.TotalSamples)
	}
	if snap.MaxDurationMs != 100 {
		t.Errorf("MaxDurationMs = %d, want 100", snap.MaxDurationMs)
	}
	if snap.AvgDurationMs != 55.0 {
		t.Errorf("AvgDurationMs = %g, want 55.0", snap.AvgDurationMs)
	}
}

func TestSMTMetrics_EmptySnapshot(t *testing.T) {
	m := NewSMTMetrics()
	snap := m.Snapshot()
	if snap.TotalSamples != 0 {
		t.Errorf("TotalSamples = %d, want 0", snap.TotalSamples)
	}
	if snap.AvgDurationMs != 0 {
		t.Errorf("AvgDurationMs = %g, want 0", snap.AvgDurationMs)
	}
}

func TestSMTMetrics_PrometheusText(t *testing.T) {
	m := NewSMTMetrics()
	m.RecordSat(12, 5.0)
	m.RecordUnsat(7, 3.0)

	text := m.PrometheusText()
	mustContain := []string{
		"symkernel_smt_solver_sat_total",
		"symkernel_smt_solver_unsat_total",
		"symkernel_smt_solver_unknown_total",
		"symkernel_smt_solver_duration_ms",
		"symkernel_smt_constraint_complexity_score_avg",
		"symkernel_smt_cache_hit_total",
		"symkernel_smt_cache_miss_total",
		"# HELP",
		"# TYPE",
	}
	for _, s := range mustContain {
		if !strings.Contains(text, s) {
			t.Errorf("PrometheusText missing %q", s)
		}
	}
}

func TestConstraintComplexityScore(t *testing.T) {
	tests := []struct {
		constraints string
		want        float64
	}{
		{"", 0},
		{"(assert true)", 1},
		{"(declare-const x Int)\n(assert (> x 0))", 2},
		{"(assert (> x 0))\n(assert (< x 10))\n(assert (= (mod x 2) 0))", 3},
	}
	for _, tt := range tests {
		got := ConstraintComplexityScore(tt.constraints)
		if got != tt.want {
			t.Errorf("ConstraintComplexityScore(%q) = %g, want %g", tt.constraints, got, tt.want)
		}
	}
}

func TestRecordSolverResult(t *testing.T) {
	m := NewSMTMetrics()
	RecordSolverResult(m, "sat", 15, "(assert true)")
	RecordSolverResult(m, "unsat", 8, "(assert false)")
	RecordSolverResult(m, "unknown", 50, "")

	snap := m.Snapshot()
	if snap.SatCount != 1 {
		t.Errorf("SatCount = %d, want 1", snap.SatCount)
	}
	if snap.UnsatCount != 1 {
		t.Errorf("UnsatCount = %d, want 1", snap.UnsatCount)
	}
	if snap.UnknownCount != 1 {
		t.Errorf("UnknownCount = %d, want 1", snap.UnknownCount)
	}
}
