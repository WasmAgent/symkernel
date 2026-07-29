// Package observability provides policy performance dashboards and alerting
// for the symkernel verification service, the observability tier called out
// by Milestone 11.
//
// A Collector records per-policy verification observations — latency,
// outcome (pass/fail), and resource usage (memory and instructions) — and
// exposes them three ways:
//
//   - as a Prometheus text exposition (PrometheusText) suitable for scraping
//     at /metrics,
//   - as a summarized dashboard (dashboard.go) consumed by operators and
//     Grafana provisioning,
//   - as degradation alerts (alerting.go) when latency, error rate, or
//     resource usage cross configured thresholds.
//
// The package is transport agnostic: any verifier that knows its policy id,
// tier, and outcome can feed it an Observation. Latency is bucketed into a
// fixed Prometheus-style histogram so memory use stays bounded regardless of
// request volume; percentiles (p50/p95/p99) are approximated from the
// histogram, matching Prometheus' histogram_quantile semantics.
package observability

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// Tier labels the verification tier an observation originated from. It
// mirrors the three-tier model (CEL rules, wazero sandbox, Z3 SMT) that
// symkernel exposes.
type Tier string

const (
	// TierCEL is the lightweight rules tier (cel-go).
	TierCEL Tier = "cel"
	// TierWasm is the hard-isolation tier (wazero sandbox).
	TierWasm Tier = "wasm"
	// TierZ3 is the formal-proof tier (Z3 SMT solver).
	TierZ3 Tier = "z3"
	// TierUnknown is used when the tier is not known, so a stray empty
	// string still produces a valid (if unhelpful) label.
	TierUnknown Tier = "unknown"
)

// String returns the canonical lower-case label for the tier, normalizing
// the empty string to "unknown".
func (t Tier) String() string {
	if t == "" {
		return string(TierUnknown)
	}
	return string(t)
}

// ResourceUsage captures the resource cost of a single verification. Fields
// are optional: a CEL evaluation records no instructions, while a wazero
// sandbox run records both memory and instructions.
type ResourceUsage struct {
	// MemoryBytes is the peak memory consumed during verification, in
	// bytes (e.g. the wazero guest's high-water mark).
	MemoryBytes uint64

	// InstructionCount is the number of instructions executed (e.g. the
	// wazero metering counter). Zero means "not measured".
	InstructionCount uint64
}

// Observation is a single verification event recorded by the Collector. It
// is the unit of input the dashboard, metrics, and alerts are built from.
type Observation struct {
	// PolicyID identifies the policy that was evaluated (e.g.
	// "rate-limit-v2"). Required.
	PolicyID string

	// Tier is the verification tier that produced the observation.
	Tier Tier

	// Latency is the wall-clock duration the verification took.
	Latency time.Duration

	// Success reports whether the verification completed without error.
	// A failed verification (policy rejected the input) is still a
	// Success if the verifier ran cleanly; set Success=false only when
	// the verifier itself erred or timed out.
	Success bool

	// ErrorCode is an optional machine-readable failure code (e.g.
	// "timeout", "trap", "malformed"). Populated only when Success is
	// false. It drives the errors-by-code breakdown.
	ErrorCode string

	// Resources is the resource cost of the verification.
	Resources ResourceUsage

	// Timestamp is when the observation was produced. If zero, Record
	// stamps it with the current time.
	Timestamp time.Time
}

// latencyBucketsSeconds are the histogram bucket upper bounds for
// verification latency, in seconds. They are the Prometheus default
// latency buckets (0.005s … 10s), which span the full symkernel SLO range:
// a CEL hit is sub-millisecond, a Z3 proof can run for seconds.
var latencyBucketsSeconds = []float64{
	0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10,
}

// numLatencyBuckets is the per-policy bucket array length: one slot per
// declared upper bound plus a final +Inf overflow bucket. It is a package
// variable (not a const) because len over a slice is not a constant
// expression.
var numLatencyBuckets = len(latencyBucketsSeconds) + 1

// latencyAgg accumulates the latency distribution for a single policy. The
// buckets slice holds non-cumulative counts: bucket[i] is the number of
// observations with latency in (bounds[i-1], bounds[i]], with bucket[len-1]
// the +Inf bucket capturing everything above the largest bound.
type latencyAgg struct {
	count   uint64
	sum     time.Duration
	min     time.Duration
	max     time.Duration
	buckets []uint64 // len == numLatencyBuckets
}

// newLatencyAgg returns a zeroed latencyAgg with an allocated bucket slice.
func newLatencyAgg() latencyAgg {
	return latencyAgg{
		min:     0,
		max:     0,
		buckets: make([]uint64, numLatencyBuckets),
	}
}

// record adds a single latency sample to the aggregate.
func (a *latencyAgg) record(d time.Duration) {
	if a.count == 0 || d < a.min {
		a.min = d
	}
	if d > a.max {
		a.max = d
	}
	a.count++
	a.sum += d
	a.buckets[latencyBucketIndex(d)]++
}

// latencyBucketIndex returns the non-cumulative bucket index for a duration.
func latencyBucketIndex(d time.Duration) int {
	s := d.Seconds()
	for i, b := range latencyBucketsSeconds {
		if s <= b {
			return i
		}
	}
	return numLatencyBuckets - 1 // +Inf overflow bucket
}

// percentile approximates the q-quantile (0 <= q <= 1) of the latency
// distribution via linear interpolation between bucket boundaries, matching
// Prometheus' histogram_quantile behavior. Returns 0 if there are no
// samples.
func (a latencyAgg) percentile(q float64) time.Duration {
	if a.count == 0 {
		return 0
	}
	if q <= 0 {
		return a.min
	}
	if q >= 1 {
		return a.max
	}
	target := q * float64(a.count)
	cum := uint64(0)
	prevBound := 0.0
	for i, bound := range latencyBucketsSeconds {
		bucketCount := a.buckets[i]
		cumPrev := cum
		cum += bucketCount
		if float64(cum) >= target {
			if bucketCount == 0 {
				return secondsToDuration(bound)
			}
			// Linearly interpolate within this bucket.
			frac := (target - float64(cumPrev)) / float64(bucketCount)
			if frac < 0 {
				frac = 0
			} else if frac > 1 {
				frac = 1
			}
			return secondsToDuration(prevBound + frac*(bound-prevBound))
		}
		prevBound = bound
	}
	// Target falls in the +Inf bucket: return the observed max.
	return a.max
}

// secondsToDuration converts a float second value to a time.Duration,
// clamping negatives to zero.
func secondsToDuration(s float64) time.Duration {
	if s <= 0 {
		return 0
	}
	return time.Duration(s * float64(time.Second))
}

// policySeries is the full per-policy metric set maintained by the Collector.
type policySeries struct {
	policyID string
	tier     Tier

	lat latencyAgg

	total     uint64
	successes uint64
	failures  uint64

	errorsByCode map[string]uint64

	maxMemory         uint64
	totalInstructions uint64

	lastUpdated time.Time
}

// Collector aggregates per-policy verification metrics. It is safe for
// concurrent use: Record may be called from many goroutines (one per
// in-flight verification) while Snapshot/PrometheusText are read concurrently.
type Collector struct {
	mu     sync.RWMutex
	series map[string]*policySeries
}

// NewCollector creates an empty Collector.
func NewCollector() *Collector {
	return &Collector{series: make(map[string]*policySeries)}
}

// Record ingests a single Observation. It is safe for concurrent use. An
// observation with an empty PolicyID is dropped — there is no series to
// attribute it to — because the dashboard, metrics, and alerts are all
// per-policy.
func (c *Collector) Record(obs Observation) {
	if obs.PolicyID == "" {
		return
	}
	if obs.Timestamp.IsZero() {
		obs.Timestamp = time.Now().UTC()
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	ps := c.series[obs.PolicyID]
	if ps == nil {
		ps = &policySeries{
			policyID:     obs.PolicyID,
			tier:         obs.Tier,
			lat:          newLatencyAgg(),
			errorsByCode: make(map[string]uint64),
		}
		c.series[obs.PolicyID] = ps
	}
	// Keep the most-recently-seen tier so the dashboard reflects the
	// active policy owner even if a policy is re-keyed across tiers.
	ps.tier = obs.Tier

	ps.lat.record(obs.Latency)
	ps.total++
	if obs.Success {
		ps.successes++
	} else {
		ps.failures++
		if obs.ErrorCode != "" {
			ps.errorsByCode[obs.ErrorCode]++
		} else {
			ps.errorsByCode["unknown"]++
		}
	}
	if obs.Resources.MemoryBytes > ps.maxMemory {
		ps.maxMemory = obs.Resources.MemoryBytes
	}
	ps.totalInstructions += obs.Resources.InstructionCount
	ps.lastUpdated = obs.Timestamp
}

// snapshotSeries returns a defensive copy of the named series. The boolean
// reports whether the policy was known.
func (c *Collector) snapshotSeries(policyID string) (policySeries, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	ps, ok := c.series[policyID]
	if !ok {
		return policySeries{}, false
	}
	return *ps, true
}

// allSeries returns a stable snapshot of every series, sorted by policy id
// for deterministic output across exposition and tests.
func (c *Collector) allSeries() []policySeries {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]policySeries, 0, len(c.series))
	for _, ps := range c.series {
		out = append(out, *ps)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].policyID < out[j].policyID })
	return out
}

// PolicyIDs returns the sorted set of policy ids the Collector has observed.
func (c *Collector) PolicyIDs() []string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	ids := make([]string, 0, len(c.series))
	for id := range c.series {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// PrometheusText exposes the collected metrics in the Prometheus text
// exposition format (version 0.0.4), suitable for scraping at /metrics.
// It emits, per policy:
//
//	symkernel_policy_verification_duration_seconds_bucket{...}   (cumulative)
//	symkernel_policy_verification_duration_seconds_sum{...}
//	symkernel_policy_verification_duration_seconds_count{...}
//	symkernel_policy_verification_total{...,result=success|failure}
//	symkernel_policy_verification_errors_total{...,code=...}
//	symkernel_policy_verification_memory_bytes_max{...}
//	symkernel_policy_verification_instructions_total{...}
func (c *Collector) PrometheusText() string {
	series := c.allSeries()
	var b strings.Builder
	b.WriteString("# HELP symkernel_policy_verification_duration_seconds Verification latency distribution.\n")
	b.WriteString("# TYPE symkernel_policy_verification_duration_seconds histogram\n")
	b.WriteString("# HELP symkernel_policy_verification_total Total verifications by policy and outcome.\n")
	b.WriteString("# TYPE symkernel_policy_verification_total counter\n")
	b.WriteString("# HELP symkernel_policy_verification_errors_total Verification errors by policy and code.\n")
	b.WriteString("# TYPE symkernel_policy_verification_errors_total counter\n")
	b.WriteString("# HELP symkernel_policy_verification_memory_bytes_max Peak memory consumed by a verification, in bytes.\n")
	b.WriteString("# TYPE symkernel_policy_verification_memory_bytes_max gauge\n")
	b.WriteString("# HELP symkernel_policy_verification_instructions_total Cumulative instructions executed across verifications.\n")
	b.WriteString("# TYPE symkernel_policy_verification_instructions_total counter\n")

	for _, ps := range series {
		labels := fmt.Sprintf(`policy_id=%q,tier=%q`, ps.policyID, ps.tier.String())

		// Cumulative histogram buckets.
		cum := uint64(0)
		for i, bound := range latencyBucketsSeconds {
			cum += ps.lat.buckets[i]
			fmt.Fprintf(&b,
				`symkernel_policy_verification_duration_seconds_bucket{%s,le="%g"} %d`+"\n",
				labels, bound, cum)
		}
		// +Inf bucket equals the total count.
		fmt.Fprintf(&b,
			`symkernel_policy_verification_duration_seconds_bucket{%s,le="+Inf"} %d`+"\n",
			labels, ps.lat.count)
		fmt.Fprintf(&b,
			`symkernel_policy_verification_duration_seconds_sum{%s} %g`+"\n",
			labels, ps.lat.sum.Seconds())
		fmt.Fprintf(&b,
			`symkernel_policy_verification_duration_seconds_count{%s} %d`+"\n",
			labels, ps.lat.count)

		fmt.Fprintf(&b,
			`symkernel_policy_verification_total{%s,result="success"} %d`+"\n",
			labels, ps.successes)
		fmt.Fprintf(&b,
			`symkernel_policy_verification_total{%s,result="failure"} %d`+"\n",
			labels, ps.failures)

		// Errors by code, sorted for stable output.
		for _, code := range sortedKeys(ps.errorsByCode) {
			fmt.Fprintf(&b,
				`symkernel_policy_verification_errors_total{%s,code=%q} %d`+"\n",
				labels, code, ps.errorsByCode[code])
		}

		fmt.Fprintf(&b,
			`symkernel_policy_verification_memory_bytes_max{%s} %d`+"\n",
			labels, ps.maxMemory)
		fmt.Fprintf(&b,
			`symkernel_policy_verification_instructions_total{%s} %d`+"\n",
			labels, ps.totalInstructions)
	}
	return b.String()
}

// sortedKeys returns the keys of m sorted lexicographically.
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
