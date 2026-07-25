// alerting.go — degradation alerts over the per-policy metrics.
//
// An AlertEngine holds a set of AlertRules; Evaluate walks the rules against
// the live Collector snapshot and fires an Alert for every policy whose
// observed value crosses its threshold. The same rules can be exported as a
// Prometheus alerting-rule file (PrometheusRulesYAML) so an operator can run
// the alerts either inside symkernel (over the HTTP API) or in Prometheus +
// Alertmanager (the "integration with Grafana/Prometheus for alerting on
// policy degradation" the milestone calls for).
//
// Thresholds are deliberately conservative: a verification service degrades
// long before it fails, so the defaults flag a p95 latency regression and a
// rising error rate rather than a hard outage. Operators override them via
// NewAlertEngine with a custom rule set.

package observability

import (
	"bytes"
	"fmt"
	"time"

	"go.yaml.in/yaml/v3"
)

// Severity grades an alert's urgency. It maps to the Prometheus severity
// label and, in Grafana, to notification-routing policy.
type Severity string

const (
	// SeverityInfo is informational (e.g. a policy that just crossed a
	// soft threshold). Routed to a low-noise channel.
	SeverityInfo Severity = "info"
	// SeverityWarning means performance is degrading; investigate soon.
	SeverityWarning Severity = "warning"
	// SeverityCritical means the policy is failing its SLO now.
	SeverityCritical Severity = "critical"
)

// MetricKind names the metric an AlertRule evaluates. It selects which field
// of a PolicySummary the rule compares against its Threshold.
type MetricKind string

const (
	// MetricLatencyP95Ms compares the policy's p95 latency (ms).
	MetricLatencyP95Ms MetricKind = "latency_p95_ms"
	// MetricLatencyP99Ms compares the policy's p99 latency (ms).
	MetricLatencyP99Ms MetricKind = "latency_p99_ms"
	// MetricErrorRate compares the policy's failure fraction (0..1).
	MetricErrorRate MetricKind = "error_rate"
	// MetricPeakMemoryBytes compares the policy's peak memory (bytes).
	MetricPeakMemoryBytes MetricKind = "peak_memory_bytes"
)

// Comparison is the relational operator applied to observed vs threshold.
type Comparison string

const (
	// CompareGreaterThan fires when observed > threshold.
	CompareGreaterThan Comparison = ">"
	// CompareLessThan fires when observed < threshold (e.g. throughput
	// collapse if a future rule tracks it).
	CompareLessThan Comparison = "<"
)

// AlertRule defines a single degradation check. A rule fires for every
// policy whose metric crosses Threshold in the Comparison direction,
// provided the policy has at least MinSamples observations (so a policy
// that was just deployed does not alert on a single slow request).
type AlertRule struct {
	// ID is the stable rule identifier used in the fired Alert and the
	// Prometheus alert name. Required.
	ID string

	// Name is a human-readable summary.
	Name string

	// Metric selects which PolicySummary field is compared.
	Metric MetricKind

	// Comparison is the direction of the comparison (defaults to >).
	Comparison Comparison

	// Threshold is the value Metric is compared against, in the metric's
	// native unit (ms for latency, fraction for error rate, bytes for
	// memory).
	Threshold float64

	// For is the duration the condition must hold before the alert
	// fires in Prometheus; inside symkernel Evaluate fires immediately
	// and stamps For into the emitted Prometheus rule.
	For time.Duration

	// Severity grades the alert.
	Severity Severity

	// Message is the human-readable explanation included in a fired
	// Alert and the Prometheus annotation.
	Message string

	// MinSamples is the minimum observation count required before the
	// rule is evaluated for a policy. Zero means "always evaluate".
	MinSamples uint64
}

// Alert is a rule firing against a specific policy. It is the JSON shape
// returned by GET /v1/observability/alerts.
type Alert struct {
	RuleID    string    `json:"rule_id"`
	Name      string    `json:"name"`
	Severity  Severity  `json:"severity"`
	PolicyID  string    `json:"policy_id"`
	Tier      Tier      `json:"tier"`
	Metric    MetricKind `json:"metric"`
	Observed  float64   `json:"observed"`
	Threshold float64   `json:"threshold"`
	Unit      string    `json:"unit"`
	Message   string    `json:"message"`
	FiredAt   time.Time `json:"fired_at"`
}

// AlertEngine evaluates a rule set against a Collector. It is safe for
// concurrent use.
type AlertEngine struct {
	rules []AlertRule
	now   func() time.Time
}

// NewAlertEngine builds an engine from the given rules. If rules is empty
// the engine fires nothing — call DefaultRules for the standard set.
func NewAlertEngine(rules []AlertRule) *AlertEngine {
	return &AlertEngine{rules: rules, now: time.Now}
}

// DefaultRules returns the standard symkernel degradation rule set:
//
//   - p95 latency over 500ms (warning) and over 1s (critical)
//   - error rate over 5% (warning) and over 10% (critical)
//   - peak memory over 512MiB (critical)
//
// These are conservative starting points tuned to the per-tier SLOs (CEL
// sub-millisecond, wazero ~1ms, Z3 up to seconds); operators override them
// with NewAlertEngine.
func DefaultRules() []AlertRule {
	return []AlertRule{
		{
			ID:         "PolicyLatencyP95High",
			Name:       "Policy p95 latency is high",
			Metric:     MetricLatencyP95Ms,
			Comparison: CompareGreaterThan,
			Threshold:  500,
			For:        5 * time.Minute,
			Severity:   SeverityWarning,
			Message:    "p95 verification latency for {{policy_id}} exceeded 500ms — investigate a performance regression.",
			MinSamples: 20,
		},
		{
			ID:         "PolicyLatencyP95Critical",
			Name:       "Policy p95 latency is critical",
			Metric:     MetricLatencyP95Ms,
			Comparison: CompareGreaterThan,
			Threshold:  1000,
			For:        2 * time.Minute,
			Severity:   SeverityCritical,
			Message:    "p95 verification latency for {{policy_id}} exceeded 1s — the policy is failing its latency SLO.",
			MinSamples: 10,
		},
		{
			ID:         "PolicyErrorRateHigh",
			Name:       "Policy error rate is elevated",
			Metric:     MetricErrorRate,
			Comparison: CompareGreaterThan,
			Threshold:  0.05,
			For:        5 * time.Minute,
			Severity:   SeverityWarning,
			Message:    "More than 5% of verifications for {{policy_id}} are failing — check for malformed inputs or solver errors.",
			MinSamples: 50,
		},
		{
			ID:         "PolicyErrorRateCritical",
			Name:       "Policy error rate is critical",
			Metric:     MetricErrorRate,
			Comparison: CompareGreaterThan,
			Threshold:  0.10,
			For:        2 * time.Minute,
			Severity:   SeverityCritical,
			Message:    "More than 10% of verifications for {{policy_id}} are failing — the policy is likely broken.",
			MinSamples: 50,
		},
		{
			ID:         "PolicyPeakMemoryCritical",
			Name:       "Policy peak memory is critical",
			Metric:     MetricPeakMemoryBytes,
			Comparison: CompareGreaterThan,
			Threshold:  512 * 1024 * 1024,
			For:        5 * time.Minute,
			Severity:   SeverityCritical,
			Message:    "A verification for {{policy_id}} consumed over 512MiB — possible runaway sandbox allocation.",
			MinSamples: 1,
		},
	}
}

// DefaultAlertEngine returns an engine seeded with DefaultRules.
func DefaultAlertEngine() *AlertEngine {
	return NewAlertEngine(DefaultRules())
}

// Evaluate walks every rule against every policy in the Collector snapshot
// and returns the alerts that fired, sorted by severity (critical first)
// then policy id. It does not mutate the Collector.
func (e *AlertEngine) Evaluate(c *Collector) []Alert {
	now := e.now()
	var alerts []Alert
	for _, ps := range c.allSeries() {
		summary := summarize(ps)
		for _, rule := range e.rules {
			if rule.MinSamples > 0 && summary.Latency.Count < rule.MinSamples {
				// MinSamples guards on the latency sample count,
				// which is also the verification count for a
				// policy (one latency sample per verification).
				continue
			}
			observed, ok := metricValue(summary, rule.Metric)
			if !ok {
				continue
			}
			if !compare(rule.Comparison, observed, rule.Threshold) {
				continue
			}
			alerts = append(alerts, Alert{
				RuleID:    rule.ID,
				Name:      rule.Name,
				Severity:  rule.Severity,
				PolicyID:  summary.PolicyID,
				Tier:      summary.Tier,
				Metric:    rule.Metric,
				Observed:  observed,
				Threshold: rule.Threshold,
				Unit:      metricUnit(rule.Metric),
				Message:   renderMessage(rule.Message, summary.PolicyID, observed),
				FiredAt:   now,
			})
		}
	}
	sortAlerts(alerts)
	return alerts
}

// metricValue extracts the metric the rule watches from a PolicySummary.
// The ok return is false for a metric the summary does not carry (none
// today, but guards future MetricKinds).
func metricValue(s PolicySummary, m MetricKind) (float64, bool) {
	switch m {
	case MetricLatencyP95Ms:
		return s.Latency.P95Ms, true
	case MetricLatencyP99Ms:
		return s.Latency.P99Ms, true
	case MetricErrorRate:
		return s.ErrorRate, true
	case MetricPeakMemoryBytes:
		return float64(s.Resources.PeakMemoryBytes), true
	default:
		return 0, false
	}
}

// metricUnit returns the display unit for a metric, included in fired
// alerts so dashboards can format Observed without guessing.
func metricUnit(m MetricKind) string {
	switch m {
	case MetricLatencyP95Ms, MetricLatencyP99Ms:
		return "ms"
	case MetricErrorRate:
		return "ratio"
	case MetricPeakMemoryBytes:
		return "bytes"
	default:
		return ""
	}
}

// compare applies the relational operator.
func compare(op Comparison, observed, threshold float64) bool {
	switch op {
	case CompareLessThan:
		return observed < threshold
	default: // CompareGreaterThan (and unset, which defaults to >)
		return observed > threshold
	}
}

// renderMessage substitutes {{policy_id}} and {{observed}} into the rule's
// message template. It is deliberately tiny (no full template engine) so
// the dependency surface stays at the standard library.
func renderMessage(msg, policyID string, observed float64) string {
	out := msg
	out = replaceAll(out, "{{policy_id}}", policyID)
	out = replaceAll(out, "{{observed}}", fmt.Sprintf("%.4g", observed))
	return out
}

// replaceAll is a strings.ReplaceAll alias kept local so this file does not
// import strings solely for one call.
func replaceAll(s, old, new string) string {
	var b []byte
	for i := 0; i < len(s); {
		if i+len(old) <= len(s) && s[i:i+len(old)] == old {
			b = append(b, new...)
			i += len(old)
			continue
		}
		b = append(b, s[i])
		i++
	}
	return string(b)
}

// alertOrder is the severity ranking used to sort fired alerts (critical
// first so dashboards surface the worst degradation).
var alertOrder = map[Severity]int{
	SeverityCritical: 0,
	SeverityWarning:  1,
	SeverityInfo:     2,
}

func sortAlerts(a []Alert) {
	// Simple insertion sort: the alert count is small (rules × policies),
	// and avoiding a sort.Slice import keeps the dependency list tight.
	for i := 1; i < len(a); i++ {
		for j := i; j > 0 && alertLess(a[j], a[j-1]); j-- {
			a[j], a[j-1] = a[j-1], a[j]
		}
	}
}

func alertLess(x, y Alert) bool {
	ox, ok := alertOrder[x.Severity]
	if !ok {
		ox = 3
	}
	oy, ok := alertOrder[y.Severity]
	if !ok {
		oy = 3
	}
	if ox != oy {
		return ox < oy
	}
	if x.PolicyID != y.PolicyID {
		return x.PolicyID < y.PolicyID
	}
	return x.RuleID < y.RuleID
}

// --- Prometheus alerting-rule export ---

// promRuleGroup is the YAML model for a Prometheus alerting-rules file. It
// marshals to the "groups:" document Prometheus and Alertmanager consume.
type promRuleGroup struct {
	Name  string        `yaml:"name"`
	Rules []promRule    `yaml:"rules"`
}

type promRule struct {
	Alert       string            `yaml:"alert"`
	Expr        string            `yaml:"expr"`
	For         string            `yaml:"for"`
	Labels      map[string]string `yaml:"labels"`
	Annotations map[string]string `yaml:"annotations"`
}

// promQLFor renders the PromQL expression for a rule against the metric
// names emitted by Collector.PrometheusText.
func promQLFor(r AlertRule) string {
	switch r.Metric {
	case MetricLatencyP95Ms:
		return fmt.Sprintf(
			`histogram_quantile(0.95, sum by (policy_id, le) (rate(symkernel_policy_verification_duration_seconds_bucket[5m]))) > %g`,
			r.Threshold/1000)
	case MetricLatencyP99Ms:
		return fmt.Sprintf(
			`histogram_quantile(0.99, sum by (policy_id, le) (rate(symkernel_policy_verification_duration_seconds_bucket[5m]))) > %g`,
			r.Threshold/1000)
	case MetricErrorRate:
		return fmt.Sprintf(
			`sum by (policy_id) (rate(symkernel_policy_verification_total{result="failure"}[5m])) / clamp_min(sum by (policy_id) (rate(symkernel_policy_verification_total[5m])), 1) > %g`,
			r.Threshold)
	case MetricPeakMemoryBytes:
		return fmt.Sprintf(
			`max by (policy_id) (symkernel_policy_verification_memory_bytes_max) > %g`,
			r.Threshold)
	default:
		return ""
	}
}

// PrometheusRulesYAML renders the engine's rules as a Prometheus alerting-
// rules file, ready to drop into a Prometheus server or hand to
// `promtool check rules`. The returned YAML has one group,
// "symkernel_policy_degradation", containing one alert per rule.
func (e *AlertEngine) PrometheusRulesYAML() (string, error) {
	group := promRuleGroup{Name: "symkernel_policy_degradation"}
	for _, r := range e.rules {
		expr := promQLFor(r)
		if expr == "" {
			continue
		}
		rule := promRule{
			Alert: r.ID,
			Expr:  expr,
			For:   promFor(r.For),
			Labels: map[string]string{
				"severity": string(r.Severity),
			},
			Annotations: map[string]string{
				"summary":   r.Name,
				"message":   renderMessage(r.Message, "{{policy_id}}", r.Threshold),
				"metric":    string(r.Metric),
				"threshold": fmt.Sprintf("%g", r.Threshold),
			},
		}
		group.Rules = append(group.Rules, rule)
	}

	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode([]promRuleGroup{group}); err != nil {
		return "", fmt.Errorf("observability: encode prometheus rules: %w", err)
	}
	if err := enc.Close(); err != nil {
		return "", fmt.Errorf("observability: close prometheus rules encoder: %w", err)
	}
	return buf.String(), nil
}

// promFor formats a duration as Prometheus' "for" field (e.g. "5m", "30s"),
// clamping zero to "0s".
func promFor(d time.Duration) string {
	if d <= 0 {
		return "0s"
	}
	return d.String()
}
