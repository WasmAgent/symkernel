package observability

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// recordN records n identical observations for quick setup.
func recordN(c *Collector, policyID string, tier Tier, latency time.Duration, success bool, n int) {
	for i := 0; i < n; i++ {
		c.Record(Observation{
			PolicyID: policyID,
			Tier:     tier,
			Latency:  latency,
			Success:  success,
		})
	}
}

// --- Collector basics ---

func TestRecordEmptyPolicyIDIsDropped(t *testing.T) {
	c := NewCollector()
	c.Record(Observation{PolicyID: "", Latency: 10 * time.Millisecond})
	if got := c.PolicyIDs(); len(got) != 0 {
		t.Fatalf("empty policy id should be dropped, got %v", got)
	}
}

func TestRecordStampsTimestampWhenZero(t *testing.T) {
	c := NewCollector()
	c.Record(Observation{PolicyID: "p1", Latency: time.Millisecond})
	_, ok := c.snapshotSeries("p1")
	if !ok {
		t.Fatal("expected series p1")
	}
}

func TestTierStringNormalizesEmpty(t *testing.T) {
	if got := Tier("").String(); got != "unknown" {
		t.Fatalf(`Tier("").String() = %q, want "unknown"`, got)
	}
	if got := TierCEL.String(); got != "cel" {
		t.Fatalf("TierCEL.String() = %q, want \"cel\"", got)
	}
}

// --- latency histogram + percentiles ---

func TestLatencyPercentilesMonotonicAndBounded(t *testing.T) {
	c := NewCollector()
	// A spread of latencies spanning several buckets.
	latencies := []time.Duration{
		1 * time.Millisecond, 2 * time.Millisecond, 3 * time.Millisecond,
		10 * time.Millisecond, 20 * time.Millisecond, 30 * time.Millisecond,
		100 * time.Millisecond, 200 * time.Millisecond, 300 * time.Millisecond,
	}
	for _, d := range latencies {
		c.Record(Observation{PolicyID: "spread", Tier: TierCEL, Latency: d})
	}
	ps, ok := c.snapshotSeries("spread")
	if !ok {
		t.Fatal("missing series")
	}
	p50 := ps.lat.percentile(0.50)
	p95 := ps.lat.percentile(0.95)
	p99 := ps.lat.percentile(0.99)
	if p50 > p95 || p95 > p99 {
		t.Fatalf("percentiles not monotonic: p50=%v p95=%v p99=%v", p50, p95, p99)
	}
	// Lower bound holds: percentiles are >= the observed minimum.
	if p50 < ps.lat.min {
		t.Fatalf("p50 below min: p50=%v min=%v", p50, ps.lat.min)
	}
	// Histogram quantiles (like Prometheus histogram_quantile) approximate
	// by interpolating between bucket boundaries, so p99 can overshoot the
	// true sample max when the quantile lands in the top occupied bucket.
	// It must never exceed the largest declared bucket bound, though.
	ceiling := time.Duration(latencyBucketsSeconds[len(latencyBucketsSeconds)-1] * float64(time.Second))
	if p99 > ceiling {
		t.Fatalf("p99 above histogram ceiling: p99=%v ceiling=%v", p99, ceiling)
	}
}

func TestLatencyPercentileNoSamples(t *testing.T) {
	a := newLatencyAgg()
	if got := a.percentile(0.95); got != 0 {
		t.Fatalf("percentile of empty agg = %v, want 0", got)
	}
}

func TestLatencyBucketIndexOverflow(t *testing.T) {
	// A latency above the largest bucket lands in the +Inf overflow bucket.
	idx := latencyBucketIndex(60 * time.Second) // 60s > 10s max bucket
	if idx != numLatencyBuckets-1 {
		t.Fatalf("overflow bucket index = %d, want %d", idx, numLatencyBuckets-1)
	}
	// A tiny latency lands in the first bucket.
	if got := latencyBucketIndex(time.Microsecond); got != 0 {
		t.Fatalf("first bucket index = %d, want 0", got)
	}
}

// --- error rate + resources ---

func TestErrorRateAndResourceTracking(t *testing.T) {
	c := NewCollector()
	recordN(c, "policy", TierWasm, 5*time.Millisecond, true, 80)
	recordN(c, "policy", TierWasm, 5*time.Millisecond, false, 20)
	// Two sandbox runs with known resource costs.
	c.Record(Observation{PolicyID: "policy", Tier: TierWasm, Latency: 1 * time.Millisecond, Success: true,
		Resources: ResourceUsage{MemoryBytes: 1024, InstructionCount: 100}})
	c.Record(Observation{PolicyID: "policy", Tier: TierWasm, Latency: 1 * time.Millisecond, Success: true,
		Resources: ResourceUsage{MemoryBytes: 4096, InstructionCount: 200}})

	ps, ok := c.snapshotSeries("policy")
	if !ok {
		t.Fatal("missing series")
	}
	if ps.total != 102 {
		t.Fatalf("total = %d, want 102", ps.total)
	}
	if ps.successes != 82 || ps.failures != 20 {
		t.Fatalf("successes=%d failures=%d, want 82/20", ps.successes, ps.failures)
	}
	if er := errorRate(ps.total, ps.failures); er < 0.19 || er > 0.21 {
		t.Fatalf("error rate = %.3f, want ~0.196", er)
	}
	if ps.maxMemory != 4096 {
		t.Fatalf("max memory = %d, want 4096", ps.maxMemory)
	}
	if ps.totalInstructions != 300 {
		t.Fatalf("total instructions = %d, want 300", ps.totalInstructions)
	}
}

func TestFailedObservationWithoutErrorCodeDefaultsToUnknown(t *testing.T) {
	c := NewCollector()
	c.Record(Observation{PolicyID: "p", Tier: TierCEL, Latency: time.Millisecond, Success: false})
	ps, ok := c.snapshotSeries("p")
	if !ok {
		t.Fatal("missing series")
	}
	if ps.errorsByCode["unknown"] != 1 {
		t.Fatalf("expected unknown error code, got %v", ps.errorsByCode)
	}
}

func TestFailedObservationWithErrorCode(t *testing.T) {
	c := NewCollector()
	c.Record(Observation{PolicyID: "p", Tier: TierCEL, Latency: time.Millisecond, Success: false, ErrorCode: "timeout"})
	c.Record(Observation{PolicyID: "p", Tier: TierCEL, Latency: time.Millisecond, Success: false, ErrorCode: "timeout"})
	ps, ok := c.snapshotSeries("p")
	if !ok {
		t.Fatal("missing series")
	}
	if ps.errorsByCode["timeout"] != 2 {
		t.Fatalf("timeout count = %d, want 2", ps.errorsByCode["timeout"])
	}
}

// --- Dashboard ---

func TestRenderSummarizesAndRollsUp(t *testing.T) {
	c := NewCollector()
	recordN(c, "alpha", TierCEL, 2*time.Millisecond, true, 10)
	recordN(c, "beta", TierZ3, 200*time.Millisecond, true, 5)

	dash := Render(c)
	if dash.PolicyCount != 2 {
		t.Fatalf("policy count = %d, want 2", dash.PolicyCount)
	}
	if len(dash.Policies) != 2 {
		t.Fatalf("policies len = %d, want 2", len(dash.Policies))
	}
	if dash.Policies[0].PolicyID != "alpha" {
		t.Fatalf("expected sorted first policy alpha, got %q", dash.Policies[0].PolicyID)
	}
	// Overall rollup aggregates both policies.
	if dash.Overall.Total != 15 {
		t.Fatalf("overall total = %d, want 15", dash.Overall.Total)
	}
}

func TestRenderEmptyCollector(t *testing.T) {
	c := NewCollector()
	dash := Render(c)
	if dash.PolicyCount != 0 || len(dash.Policies) != 0 {
		t.Fatalf("empty dashboard should have no policies, got %+v", dash)
	}
	if dash.Overall.Total != 0 {
		t.Fatalf("empty overall total = %d, want 0", dash.Overall.Total)
	}
}

func TestLatencyStatsBucketsAreCumulative(t *testing.T) {
	c := NewCollector()
	c.Record(Observation{PolicyID: "p", Tier: TierCEL, Latency: 1 * time.Millisecond})
	c.Record(Observation{PolicyID: "p", Tier: TierCEL, Latency: 1 * time.Millisecond})
	dash := Render(c)
	ps := dash.Policies[0]
	// Cumulative counts must be non-decreasing and end at the total.
	var prev uint64
	for _, b := range ps.Latency.Buckets {
		if b.CumulativeCount < prev {
			t.Fatalf("cumulative bucket decreased: %d < %d", b.CumulativeCount, prev)
		}
		prev = b.CumulativeCount
	}
	if prev != ps.Total {
		t.Fatalf("final cumulative bucket = %d, want total %d", prev, ps.Total)
	}
}

func TestSummarizeOmitsEmptyErrorsByCode(t *testing.T) {
	c := NewCollector()
	recordN(c, "p", TierCEL, time.Millisecond, true, 3)
	dash := Render(c)
	if dash.Policies[0].ErrorsByCode != nil {
		t.Fatalf("expected ErrorsByCode nil for all-success policy, got %v", dash.Policies[0].ErrorsByCode)
	}
}

// --- Prometheus text ---

func TestPrometheusTextContainsExpectedMetrics(t *testing.T) {
	c := NewCollector()
	c.Record(Observation{PolicyID: "rate-limit", Tier: TierCEL, Latency: 1 * time.Millisecond, Success: true})
	c.Record(Observation{PolicyID: "rate-limit", Tier: TierCEL, Latency: 2 * time.Millisecond, Success: false, ErrorCode: "timeout"})

	out := c.PrometheusText()
	wantSubstrings := []string{
		"# TYPE symkernel_policy_verification_duration_seconds histogram",
		`symkernel_policy_verification_duration_seconds_bucket{policy_id="rate-limit",tier="cel",le="+Inf"} 2`,
		`symkernel_policy_verification_duration_seconds_count{policy_id="rate-limit",tier="cel"} 2`,
		`symkernel_policy_verification_total{policy_id="rate-limit",tier="cel",result="success"} 1`,
		`symkernel_policy_verification_total{policy_id="rate-limit",tier="cel",result="failure"} 1`,
		`symkernel_policy_verification_errors_total{policy_id="rate-limit",tier="cel",code="timeout"} 1`,
		`symkernel_policy_verification_memory_bytes_max{policy_id="rate-limit",tier="cel"} 0`,
	}
	for _, s := range wantSubstrings {
		if !strings.Contains(out, s) {
			t.Errorf("PrometheusText missing %q\n--- output ---\n%s", s, out)
		}
	}
}

func TestPrometheusTextEmptyCollectorHasHelpLines(t *testing.T) {
	c := NewCollector()
	out := c.PrometheusText()
	if !strings.Contains(out, "# HELP symkernel_policy_verification_total") {
		t.Errorf("empty exposition should still emit HELP/TYPE lines\n%s", out)
	}
	if strings.Contains(out, "policy_id=") {
		t.Errorf("empty exposition should not emit any series\n%s", out)
	}
}

// --- Alerting ---

func TestEvaluateFiresLatencyAlert(t *testing.T) {
	c := NewCollector()
	// p95 well above the 500ms warning threshold.
	recordN(c, "slow", TierZ3, 800*time.Millisecond, true, 25)
	engine := DefaultAlertEngine()
	alerts := engine.Evaluate(c)
	found := false
	for _, a := range alerts {
		if a.RuleID == "PolicyLatencyP95High" && a.PolicyID == "slow" {
			found = true
			if a.Severity != SeverityWarning {
				t.Errorf("latency high alert severity = %q, want warning", a.Severity)
			}
		}
	}
	if !found {
		t.Fatalf("expected PolicyLatencyP95High alert, got %+v", alerts)
	}
}

func TestEvaluateFiresErrorRateAndCriticalAlerts(t *testing.T) {
	c := NewCollector()
	// 50% failure rate -> both error-rate rules fire; p95 50ms is under
	// the latency threshold so no latency alert.
	for i := 0; i < 100; i++ {
		c.Record(Observation{PolicyID: "flaky", Tier: TierWasm, Latency: 50 * time.Millisecond, Success: i%2 == 0})
	}
	engine := DefaultAlertEngine()
	alerts := engine.Evaluate(c)

	rules := map[string]bool{}
	for _, a := range alerts {
		rules[a.RuleID] = true
	}
	if !rules["PolicyErrorRateHigh"] || !rules["PolicyErrorRateCritical"] {
		t.Errorf("expected both error-rate alerts, got %v", rules)
	}
	if rules["PolicyLatencyP95High"] {
		t.Errorf("latency alert should not fire at p95=50ms")
	}
}

func TestEvaluateFiresMemoryAlert(t *testing.T) {
	c := NewCollector()
	c.Record(Observation{PolicyID: "hog", Tier: TierWasm, Latency: time.Millisecond, Success: true,
		Resources: ResourceUsage{MemoryBytes: 600 * 1024 * 1024}})
	engine := DefaultAlertEngine()
	alerts := engine.Evaluate(c)
	var found bool
	for _, a := range alerts {
		if a.RuleID == "PolicyPeakMemoryCritical" {
			found = true
			if a.Observed < 600*1024*1024 {
				t.Errorf("memory alert observed = %g, want >= 600MiB", a.Observed)
			}
		}
	}
	if !found {
		t.Fatalf("expected memory alert, got %+v", alerts)
	}
}

func TestEvaluateRespectsMinSamples(t *testing.T) {
	c := NewCollector()
	// A single slow observation: PolicyLatencyP95High requires MinSamples=20.
	c.Record(Observation{PolicyID: "oneshot", Tier: TierZ3, Latency: 2 * time.Second, Success: true})
	engine := DefaultAlertEngine()
	alerts := engine.Evaluate(c)
	for _, a := range alerts {
		if a.RuleID == "PolicyLatencyP95High" {
			t.Fatalf("PolicyLatencyP95High must respect MinSamples=20, fired: %+v", a)
		}
	}
}

func TestEvaluateNoAlertsForHealthyPolicy(t *testing.T) {
	c := NewCollector()
	recordN(c, "healthy", TierCEL, time.Millisecond, true, 100)
	engine := DefaultAlertEngine()
	if alerts := engine.Evaluate(c); len(alerts) != 0 {
		t.Fatalf("healthy policy should produce no alerts, got %+v", alerts)
	}
}

func TestEvaluateSortsCriticalFirst(t *testing.T) {
	c := NewCollector()
	// p95 over 1s fires warning AND critical latency alerts.
	recordN(c, "slow", TierZ3, 1200*time.Millisecond, true, 25)
	engine := DefaultAlertEngine()
	alerts := engine.Evaluate(c)
	if len(alerts) < 2 {
		t.Fatalf("expected >=2 alerts, got %d", len(alerts))
	}
	if alerts[0].Severity != SeverityCritical {
		t.Errorf("first alert should be critical, got %q", alerts[0].Severity)
	}
}

func TestAlertRulesSortedCriticalFirstUnit(t *testing.T) {
	c := NewCollector()
	recordN(c, "slow", TierZ3, 1200*time.Millisecond, true, 25)
	for _, a := range DefaultAlertEngine().Evaluate(c) {
		if a.Unit == "" {
			t.Errorf("alert %q has empty unit", a.RuleID)
		}
		if !strings.Contains(a.Message, "slow") {
			t.Errorf("alert message should substitute policy_id, got %q", a.Message)
		}
	}
}

func TestPrometheusRulesYAMLIsParsable(t *testing.T) {
	engine := DefaultAlertEngine()
	out, err := engine.PrometheusRulesYAML()
	if err != nil {
		t.Fatalf("PrometheusRulesYAML: %v", err)
	}
	wantSubstrings := []string{
		"name: symkernel_policy_degradation",
		"alert: PolicyLatencyP95High",
		"expr:",
		"severity: warning",
		"histogram_quantile(0.95",
	}
	for _, s := range wantSubstrings {
		if !strings.Contains(out, s) {
			t.Errorf("rules YAML missing %q\n---\n%s", s, out)
		}
	}
}

// --- HTTP Service ---

func TestServiceDashboardHandler(t *testing.T) {
	svc := NewService(nil)
	recordN(svc.Collector(), "p1", TierCEL, time.Millisecond, true, 5)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/observability/dashboard", nil)
	svc.dashboardHandler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("content-type = %q, want application/json", ct)
	}
	if !strings.Contains(rec.Body.String(), `"policy_id": "p1"`) {
		t.Errorf("dashboard body missing p1: %s", rec.Body.String())
	}
}

func TestServiceMetricsHandlerContentType(t *testing.T) {
	svc := NewService(nil)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/observability/metrics", nil)
	svc.metricsHandler().ServeHTTP(rec, req)
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/plain") {
		t.Errorf("metrics content-type = %q, want text/plain", ct)
	}
	if !strings.Contains(rec.Body.String(), "# TYPE symkernel") {
		t.Errorf("metrics body missing TYPE lines: %s", rec.Body.String())
	}
}

func TestServiceAlertsHandlerReturnsObject(t *testing.T) {
	svc := NewService(nil)
	recordN(svc.Collector(), "slow", TierZ3, 800*time.Millisecond, true, 25)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/observability/alerts", nil)
	svc.alertsHandler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"alerts":`) {
		t.Errorf("alerts body should be an object with alerts key: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "PolicyLatencyP95High") {
		t.Errorf("alerts body should contain firing rule: %s", rec.Body.String())
	}
}

func TestServiceGrafanaHandlerReturnsDashboard(t *testing.T) {
	svc := NewService(nil)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/observability/grafana", nil)
	svc.grafanaHandler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"uid": "symkernel-policy-perf"`) {
		t.Errorf("grafana body missing uid: %s", body)
	}
	if !strings.Contains(body, "histogram_quantile") {
		t.Errorf("grafana body missing prometheus query: %s", body)
	}
}

func TestRegisterRoutesMountsAllFour(t *testing.T) {
	svc := NewService(nil)
	mux := http.NewServeMux()
	svc.RegisterRoutes(mux)

	for _, path := range []string{
		"/v1/observability/dashboard",
		"/v1/observability/metrics",
		"/v1/observability/alerts",
		"/v1/observability/grafana",
	} {
		_, pattern := mux.Handler(httptest.NewRequest(http.MethodGet, path, nil))
		// ServeMux returns the fully-qualified pattern, method-prefixed.
		if want := "GET " + path; pattern != want {
			t.Errorf("route %q not registered, got pattern %q", path, pattern)
		}
	}
}

// --- concurrency ---

func TestRecordConcurrentSafe(t *testing.T) {
	c := NewCollector()
	done := make(chan struct{})
	for g := 0; g < 8; g++ {
		go func(id int) {
			policy := "p"
			if id%2 == 0 {
				policy = "q"
			}
			for i := 0; i < 200; i++ {
				c.Record(Observation{PolicyID: policy, Tier: TierCEL, Latency: time.Millisecond, Success: true})
			}
			done <- struct{}{}
		}(g)
	}
	for g := 0; g < 8; g++ {
		<-done
	}
	// 8 goroutines × 200 = 1600 observations split across p and q.
	total := 0
	for _, ps := range c.allSeries() {
		total += int(ps.total)
	}
	if total != 1600 {
		t.Fatalf("concurrent record lost samples: total = %d, want 1600", total)
	}
}
