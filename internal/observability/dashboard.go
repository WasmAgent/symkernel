// dashboard.go — the per-policy performance dashboard.
//
// The dashboard is the human-facing summary of a Collector: it rolls the
// raw observation stream up into one PolicySummary per policy (latency
// distribution, error rate, resource usage) and renders both a JSON model
// for the symkernel HTTP API and a Grafana provisioning model that an
// operator can drop into a Grafana instance. The HTTP wiring lives in the
// Service type at the bottom of this file, which mounts the observability
// endpoints on a ServeMux the same way diagnostics/cache/audit do.

package observability

import (
	"encoding/json"
	"net/http"
	"time"
)

// LatencyStats is the latency rollup shown on the dashboard. All millisecond
// fields are rounded to three decimals; percentiles are approximated from
// the latency histogram.
type LatencyStats struct {
	Count    uint64   `json:"count"`
	MeanMs   float64  `json:"mean_ms"`
	MinMs    float64  `json:"min_ms"`
	MaxMs    float64  `json:"max_ms"`
	P50Ms    float64  `json:"p50_ms"`
	P95Ms    float64  `json:"p95_ms"`
	P99Ms    float64  `json:"p99_ms"`
	Buckets  []Bucket `json:"buckets"`
}

// Bucket is one latency-histogram bucket, exposed cumulatively (matching
// Prometheus convention) for direct plotting in Grafana.
type Bucket struct {
	// UpperBound is the bucket's upper latency bound in seconds; the
	// final bucket uses +Inf semantics (UpperBoundSeconds = -1 sentinel
	// rendered as "+Inf" in Prometheus text).
	UpperBoundSeconds float64 `json:"upper_bound_seconds"`

	// CumulativeCount is the number of observations at or below this
	// bound.
	CumulativeCount uint64 `json:"cumulative_count"`
}

// ResourceSummary is the per-policy resource rollup.
type ResourceSummary struct {
	// PeakMemoryBytes is the highest single-verification memory observed.
	PeakMemoryBytes uint64 `json:"peak_memory_bytes"`

	// TotalInstructions is the cumulative instruction count across all
	// observations of this policy.
	TotalInstructions uint64 `json:"total_instructions"`
}

// PolicySummary is the per-policy rollup rendered on the dashboard. It is
// the JSON shape returned by GET /v1/observability/dashboard.
type PolicySummary struct {
	PolicyID  string         `json:"policy_id"`
	Tier      Tier           `json:"tier"`
	Total     uint64         `json:"total"`
	Successes uint64         `json:"successes"`
	Failures  uint64         `json:"failures"`
	ErrorRate float64        `json:"error_rate"`
	Latency   LatencyStats   `json:"latency"`
	Resources ResourceSummary `json:"resources"`
	// ErrorsByCode breaks failures down by error code; omitted when empty.
	ErrorsByCode map[string]uint64 `json:"errors_by_code,omitempty"`
	UpdatedAt    time.Time         `json:"updated_at"`
}

// Dashboard is the full dashboard model: every policy the Collector has
// observed, plus an overall rollup. It is the JSON shape returned by
// GET /v1/observability/dashboard (under the "policies" key).
type Dashboard struct {
	GeneratedAt time.Time       `json:"generated_at"`
	PolicyCount int             `json:"policy_count"`
	Overall     PolicySummary   `json:"overall"`
	Policies    []PolicySummary `json:"policies"`
}

// summarize converts a raw policySeries into a PolicySummary. The latency
// distribution is copied out defensively so a later Record cannot mutate a
// summary the caller is holding.
func summarize(ps policySeries) PolicySummary {
	out := PolicySummary{
		PolicyID:  ps.policyID,
		Tier:      ps.tier,
		Total:     ps.total,
		Successes: ps.successes,
		Failures:  ps.failures,
		ErrorRate: errorRate(ps.total, ps.failures),
		Latency: LatencyStats{
			Count:  ps.lat.count,
			MeanMs: ms(ps.lat.mean()),
			MinMs:  ms(ps.lat.min),
			MaxMs:  ms(ps.lat.max),
			P50Ms:  ms(ps.lat.percentile(0.50)),
			P95Ms:  ms(ps.lat.percentile(0.95)),
			P99Ms:  ms(ps.lat.percentile(0.99)),
			Buckets: cumulativeBuckets(ps.lat.buckets),
		},
		Resources: ResourceSummary{
			PeakMemoryBytes:  ps.maxMemory,
			TotalInstructions: ps.totalInstructions,
		},
		ErrorsByCode: copyErrorCodes(ps.errorsByCode),
		UpdatedAt:    ps.lastUpdated,
	}
	return out
}

// mean returns the average latency, or zero if there are no samples.
func (a latencyAgg) mean() time.Duration {
	if a.count == 0 {
		return 0
	}
	return a.sum / time.Duration(a.count)
}

// errorRate returns failures/total as a fraction in [0,1], guarding divide
// by zero.
func errorRate(total, failures uint64) float64 {
	if total == 0 {
		return 0
	}
	return float64(failures) / float64(total)
}

// cumulativeBuckets folds the non-cumulative bucket counts into the
// cumulative form the dashboard exposes.
func cumulativeBuckets(nonCum []uint64) []Bucket {
	out := make([]Bucket, 0, len(latencyBucketsSeconds)+1)
	cum := uint64(0)
	for i, bound := range latencyBucketsSeconds {
		cum += nonCum[i]
		out = append(out, Bucket{
			UpperBoundSeconds: bound,
			CumulativeCount:   cum,
		})
	}
	// +Inf bucket carries the full count.
	out = append(out, Bucket{
		UpperBoundSeconds: -1,
		CumulativeCount:   cum + nonCum[len(nonCum)-1],
	})
	return out
}

// copyErrorCodes returns a defensive copy of m, or nil if empty so the JSON
// field is omitted.
func copyErrorCodes(m map[string]uint64) map[string]uint64 {
	if len(m) == 0 {
		return nil
	}
	cp := make(map[string]uint64, len(m))
	for k, v := range m {
		cp[k] = v
	}
	return cp
}

// ms converts a duration to milliseconds rounded to three decimals.
func ms(d time.Duration) float64 {
	return float64(d.Microseconds()) / 1000.0
}

// Render builds a Dashboard from the Collector. The returned model is a
// point-in-time snapshot; later observations do not affect it.
func Render(c *Collector) Dashboard {
	series := c.allSeries()
	policies := make([]PolicySummary, 0, len(series))
	// overall is pre-initialized with an allocated latency bucket slice so
	// the empty-collector case still renders cleanly (cumulativeBuckets
	// indexes the slice unconditionally).
	overall := policySeries{lat: newLatencyAgg(), errorsByCode: make(map[string]uint64)}
	for _, ps := range series {
		policies = append(policies, summarize(ps))
		overall = mergeSeries(overall, ps)
	}
	overall.policyID = "__overall__"
	overall.tier = TierUnknown
	return Dashboard{
		GeneratedAt: time.Now().UTC(),
		PolicyCount: len(policies),
		Overall:     summarize(overall),
		Policies:    policies,
	}
}

// mergeSeries folds src into dst, producing the combined aggregate used for
// the dashboard's overall rollup. Latency is merged by re-adding the bucket
// counts and recomputing min/max/sum/count.
func mergeSeries(dst, src policySeries) policySeries {
	if dst.lat.buckets == nil {
		dst.lat = newLatencyAgg()
	}
	if dst.errorsByCode == nil {
		dst.errorsByCode = make(map[string]uint64)
	}
	if src.lat.count > 0 {
		if dst.lat.count == 0 || src.lat.min < dst.lat.min {
			dst.lat.min = src.lat.min
		}
		if src.lat.max > dst.lat.max {
			dst.lat.max = src.lat.max
		}
		dst.lat.count += src.lat.count
		dst.lat.sum += src.lat.sum
		for i := range dst.lat.buckets {
			dst.lat.buckets[i] += src.lat.buckets[i]
		}
	}
	dst.total += src.total
	dst.successes += src.successes
	dst.failures += src.failures
	for code, n := range src.errorsByCode {
		dst.errorsByCode[code] += n
	}
	if src.maxMemory > dst.maxMemory {
		dst.maxMemory = src.maxMemory
	}
	dst.totalInstructions += src.totalInstructions
	if src.lastUpdated.After(dst.lastUpdated) {
		dst.lastUpdated = src.lastUpdated
	}
	return dst
}

// --- Grafana provisioning model ---

// GrafanaDashboard is a minimal Grafana dashboard provisioning model. It is
// not the full Grafana schema — only enough to render the symkernel policy
// panels and a policy-id template variable. Operators paste the marshalled
// JSON into Grafana's "Import" flow or drop it under provisioning/dashboards.
type GrafanaDashboard struct {
	UID           string          `json:"uid"`
	Title         string          `json:"title"`
	Tags          []string        `json:"tags"`
	Timezone      string          `json:"timezone"`
	SchemaVersion int             `json:"schema_version"`
	Refresh       string          `json:"refresh"`
	Time          grafanaTime     `json:"time"`
	Templating    grafanaTemplating `json:"templating"`
	Panels        []GrafanaPanel  `json:"panels"`
}

type grafanaTime struct {
	From string `json:"from"`
	To   string `json:"to"`
}

type grafanaTemplating struct {
	List []grafanaVariable `json:"list"`
}

// grafanaVariable is a query variable backed by the policy_id label.
type grafanaVariable struct {
	Name      string            `json:"name"`
	Type      string            `json:"type"`
	Label     string            `json:"label"`
	IncludeAll bool             `json:"includeAll"`
	Multi     bool              `json:"multi"`
	Query     grafanaVarQuery   `json:"query"`
	Current   map[string]string `json:"current"`
}

type grafanaVarQuery struct {
	Query   string `json:"query"`
	Format  string `json:"format"`
}

// GrafanaPanel is one dashboard panel. The PromQL expressions reference the
// Prometheus metric names emitted by Collector.PrometheusText and are
// scoped to the selected policy via the $policy_id variable.
type GrafanaPanel struct {
	ID      int          `json:"id"`
	Title   string       `json:"title"`
	Type    string       `json:"type"`
	GridPos grafanaGrid  `json:"gridPos"`
	Targets []grafanaTarget `json:"targets"`
}

type grafanaGrid struct {
	H int `json:"h"`
	W int `json:"w"`
	X int `json:"x"`
	Y int `json:"y"`
}

type grafanaTarget struct {
	Expr      string `json:"expr"`
	LegendFmt string `json:"legendFormat"`
	RefID     string `json:"refId"`
}

// GrafanaDashboardJSON builds a Grafana provisioning dashboard that graphs
// per-policy latency, error rate, and resource usage from the Prometheus
// metrics this package emits. title overrides the dashboard title; pass ""
// for the default.
func GrafanaDashboardJSON(title string) GrafanaDashboard {
	if title == "" {
		title = "symkernel — Policy Performance"
	}
	return GrafanaDashboard{
		UID:           "symkernel-policy-perf",
		Title:         title,
		Tags:          []string{"symkernel", "verification"},
		Timezone:      "browser",
		SchemaVersion: 39,
		Refresh:       "10s",
		Time:          grafanaTime{From: "now-1h", To: "now"},
		Templating: grafanaTemplating{
			List: []grafanaVariable{{
				Name:       "policy_id",
				Type:       "query",
				Label:      "Policy",
				IncludeAll: true,
				Multi:      true,
				Query: grafanaVarQuery{
					Query:  `label_values(symkernel_policy_verification_duration_seconds_count, policy_id)`,
					Format: "string",
				},
				Current: map[string]string{"text": "All", "value": "$__all"},
			}},
		},
		Panels: []GrafanaPanel{
			{
				ID: 1, Title: "Verification Latency (p50 / p95 / p99)", Type: "timeseries",
				GridPos: grafanaGrid{H: 8, W: 12, X: 0, Y: 0},
				Targets: []grafanaTarget{
					{Expr: `histogram_quantile(0.50, sum by (le) (rate(symkernel_policy_verification_duration_seconds_bucket{policy_id=~"$policy_id"}[5m])))`, LegendFmt: "p50", RefID: "A"},
					{Expr: `histogram_quantile(0.95, sum by (le) (rate(symkernel_policy_verification_duration_seconds_bucket{policy_id=~"$policy_id"}[5m])))`, LegendFmt: "p95", RefID: "B"},
					{Expr: `histogram_quantile(0.99, sum by (le) (rate(symkernel_policy_verification_duration_seconds_bucket{policy_id=~"$policy_id"}[5m])))`, LegendFmt: "p99", RefID: "C"},
				},
			},
			{
				ID: 2, Title: "Error Rate", Type: "timeseries",
				GridPos: grafanaGrid{H: 8, W: 12, X: 12, Y: 0},
				Targets: []grafanaTarget{
					{Expr: `sum by (policy_id) (rate(symkernel_policy_verification_total{policy_id=~"$policy_id",result="failure"}[5m])) / clamp_min(sum by (policy_id) (rate(symkernel_policy_verification_total{policy_id=~"$policy_id"}[5m])), 1)`, LegendFmt: "{{policy_id}}", RefID: "A"},
				},
			},
			{
				ID: 3, Title: "Peak Memory (bytes)", Type: "timeseries",
				GridPos: grafanaGrid{H: 8, W: 12, X: 0, Y: 8},
				Targets: []grafanaTarget{
					{Expr: `max by (policy_id) (symkernel_policy_verification_memory_bytes_max{policy_id=~"$policy_id"})`, LegendFmt: "{{policy_id}}", RefID: "A"},
				},
			},
			{
				ID: 4, Title: "Throughput (ops/s)", Type: "timeseries",
				GridPos: grafanaGrid{H: 8, W: 12, X: 12, Y: 8},
				Targets: []grafanaTarget{
					{Expr: `sum by (policy_id) (rate(symkernel_policy_verification_duration_seconds_count{policy_id=~"$policy_id"}[5m]))`, LegendFmt: "{{policy_id}}", RefID: "A"},
				},
			},
		},
	}
}

// --- HTTP service ---

// Service is the HTTP façade over a Collector and AlertEngine. It mounts the
// observability read endpoints on a ServeMux, mirroring the RegisterRoutes
// pattern used by diagnostics, cache, and audit. It performs no mutation;
// callers feed the Collector out-of-band (typically from the verify
// handlers) and read the dashboard, metrics, and alerts over HTTP.
type Service struct {
	collector *Collector
	alerts    *AlertEngine
}

// NewService wires a Collector to the default AlertEngine and returns a
// Service ready to have its routes registered. Pass nil for c to get a
// fresh Collector.
func NewService(c *Collector) *Service {
	if c == nil {
		c = NewCollector()
	}
	return &Service{collector: c, alerts: DefaultAlertEngine()}
}

// NewServiceWithAlerts is like NewService but with a custom AlertEngine,
// for operators that want different degradation thresholds.
func NewServiceWithAlerts(c *Collector, e *AlertEngine) *Service {
	if c == nil {
		c = NewCollector()
	}
	if e == nil {
		e = DefaultAlertEngine()
	}
	return &Service{collector: c, alerts: e}
}

// Collector exposes the underlying Collector so callers (e.g. a verify
// handler) can Record observations.
func (s *Service) Collector() *Collector { return s.collector }

// RegisterRoutes mounts the observability endpoints on the given ServeMux:
//
//	GET /v1/observability/dashboard  — per-policy performance summary (JSON)
//	GET /v1/observability/metrics    — Prometheus text exposition
//	GET /v1/observability/alerts     — currently-firing degradation alerts
//	GET /v1/observability/grafana    — Grafana dashboard provisioning model
func (s *Service) RegisterRoutes(mux *http.ServeMux) {
	mux.Handle("GET /v1/observability/dashboard", s.dashboardHandler())
	mux.Handle("GET /v1/observability/metrics", s.metricsHandler())
	mux.Handle("GET /v1/observability/alerts", s.alertsHandler())
	mux.Handle("GET /v1/observability/grafana", s.grafanaHandler())
}

func (s *Service) dashboardHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, Render(s.collector))
	}
}

func (s *Service) metricsHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		_, _ = w.Write([]byte(s.collector.PrometheusText()))
	}
}

func (s *Service) alertsHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		alerts := s.alerts.Evaluate(s.collector)
		writeJSON(w, http.StatusOK, alertsResponse{Alerts: alerts})
	}
}

func (s *Service) grafanaHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, GrafanaDashboardJSON(""))
	}
}

// alertsResponse wraps the alerts slice so the JSON is an object, allowing
// future fields to be added without breaking the response shape.
type alertsResponse struct {
	Alerts []Alert `json:"alerts"`
}

// writeJSON encodes v as JSON with a 2-space indent and the given status.
// It is best-effort on the final encode error (a partial write to a
// ResponseWriter is unrecoverable, matching the diagnostics handler).
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}
