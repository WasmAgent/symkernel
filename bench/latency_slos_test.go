// Package bench provides SLO benchmark tests that measure p50/p95/p99
// latency across tiers (CEL, wazero, Z3) at multiple QPS levels (10, 100,
// 1000). Tests fail if latency regresses >15% above a persisted baseline.
//
// SLO targets (informational):
//
//	CEL:    p50 < 20ms
//	wazero: p95 < 100ms
//	Z3:     p99 < 500ms
//
// In short mode (go test -short) each sub-test uses 100 requests at
// 10/100 QPS. Run without -short for the full 10k-request sweep across
// all QPS levels.
//
// Baseline is auto-saved on the first run (no baseline file) and compared
// against on subsequent runs. Set SYMKERNEL_SLO_UPDATE_BASELINE=1 to
// re-record baselines. The baseline file is stored in the user cache
// directory so it does not affect git status.
package bench

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"

	cellib "github.com/WasmAgent/symkernel/internal/cel"
	"github.com/WasmAgent/symkernel/internal/sandbox"
	"github.com/WasmAgent/symkernel/internal/verify"
)

// ---------------------------------------------------------------------------
// Types
// ---------------------------------------------------------------------------

// sloTarget defines a latency SLO for a specific percentile.
type sloTarget struct {
	pct   float64 // e.g. 50, 95, 99
	maxMs float64 // maximum allowed latency in ms
}

// tierConfig holds the configuration for benchmarking a single verification tier.
type tierConfig struct {
	name    string
	makeReq func() []byte
	handler http.HandlerFunc
	slos    []sloTarget
}

// percentileResult holds the computed percentile values.
type percentileResult struct {
	p50, p95, p99 float64
}

// ---------------------------------------------------------------------------
// Percentile computation
// ---------------------------------------------------------------------------

// computePercentiles returns p50, p95, p99 from a slice of durations.
func computePercentiles(durations []time.Duration) percentileResult {
	if len(durations) == 0 {
		return percentileResult{}
	}
	ms := make([]float64, len(durations))
	for i, d := range durations {
		ms[i] = float64(d) / float64(time.Millisecond)
	}
	sort.Float64s(ms)
	return percentileResult{
		p50: ms[percentileIdx(len(ms), 50)],
		p95: ms[percentileIdx(len(ms), 95)],
		p99: ms[percentileIdx(len(ms), 99)],
	}
}

func percentileIdx(n int, pct float64) int {
	idx := int(math.Ceil(float64(n)*pct/100.0)) - 1
	if idx < 0 {
		return 0
	}
	if idx >= n {
		return n - 1
	}
	return idx
}

// ---------------------------------------------------------------------------
// Baseline persistence
// ---------------------------------------------------------------------------

// baselineFile returns the path to the baseline JSON file. It uses the
// user cache directory so the file does not appear in git status.
func baselineFile(t *testing.T) string {
	t.Helper()
	cacheDir, err := os.UserCacheDir()
	if err != nil {
		cacheDir = os.TempDir()
	}
	return filepath.Join(cacheDir, "symkernel-latency-slo-baseline.json")
}

// baselineEntry is a single persisted baseline measurement.
type baselineEntry struct {
	Tier string  `json:"tier"`
	QPS  int     `json:"qps"`
	P50  float64 `json:"p50"`
	P95  float64 `json:"p95"`
	P99  float64 `json:"p99"`
}

// baselineStore is the collection of baseline entries persisted to disk.
type baselineStore struct {
	Entries []baselineEntry `json:"entries"`
}

// loadBaseline reads the baseline from disk. It returns the store and a
// boolean indicating whether the file existed (false on first run).
func loadBaseline(t *testing.T) (baselineStore, bool) {
	t.Helper()
	var store baselineStore
	data, err := os.ReadFile(baselineFile(t))
	if err != nil {
		return store, false
	}
	if err := json.Unmarshal(data, &store); err != nil {
		t.Fatalf("parsing baseline file: %v", err)
	}
	return store, true
}

// saveBaseline writes the baseline store to disk.
func saveBaseline(t *testing.T, store baselineStore) {
	t.Helper()
	data, err := json.MarshalIndent(store, "", "  ")
	if err != nil {
		t.Fatalf("marshaling baseline: %v", err)
	}
	if err := os.WriteFile(baselineFile(t), data, 0644); err != nil {
		t.Fatalf("writing baseline file: %v", err)
	}
}

// findBaselineEntry returns the baseline entry for the given tier and QPS,
// or nil if not found.
func findBaselineEntry(entries []baselineEntry, tier string, qps int) *baselineEntry {
	for i := range entries {
		if entries[i].Tier == tier && entries[i].QPS == qps {
			return &entries[i]
		}
	}
	return nil
}

// upsertBaselineEntry inserts or updates a baseline entry in the slice.
func upsertBaselineEntry(entries *[]baselineEntry, e baselineEntry) {
	for i := range *entries {
		if (*entries)[i].Tier == e.Tier && (*entries)[i].QPS == e.QPS {
			(*entries)[i] = e
			return
		}
	}
	*entries = append(*entries, e)
}

// ---------------------------------------------------------------------------
// SLO target validation (informational)
// ---------------------------------------------------------------------------

// checkSLOTargets logs the comparison of actual latencies against SLO
// targets. This is informational — the hard CI failure condition is
// regression against baseline (checkRegression).
func checkSLOTargets(t *testing.T, tier string, qps int, pcts percentileResult, targets []sloTarget) {
	t.Helper()
	for _, tgt := range targets {
		var actual float64
		switch tgt.pct {
		case 50:
			actual = pcts.p50
		case 95:
			actual = pcts.p95
		case 99:
			actual = pcts.p99
		default:
			continue
		}
		status := "PASS"
		if actual > tgt.maxMs {
			status = "EXCEEDS"
		}
		t.Logf("SLO %s @ %d QPS: p%d=%.2fms target < %.2fms [%s]",
			tier, qps, int(tgt.pct), actual, tgt.maxMs, status)
	}
}

// ---------------------------------------------------------------------------
// Regression check (hard CI failure)
// ---------------------------------------------------------------------------

// regressionThreshold is the maximum allowed regression above baseline (15%).
const regressionThreshold = 0.15

// checkRegression fails the test if any percentile regressed more than
// regressionThreshold above the baseline.
func checkRegression(t *testing.T, tier string, qps int, pcts percentileResult, baseline *baselineEntry) {
	t.Helper()
	if baseline == nil {
		return
	}
	checks := []struct {
		name   string
		actual float64
		base   float64
	}{
		{"p50", pcts.p50, baseline.P50},
		{"p95", pcts.p95, baseline.P95},
		{"p99", pcts.p99, baseline.P99},
	}
	for _, c := range checks {
		if c.base <= 0 {
			continue
		}
		pctChange := (c.actual - c.base) / c.base
		if pctChange > regressionThreshold {
			t.Errorf("%s @ %d QPS: %s=%.2fms regressed %.1f%% above baseline %.2fms (threshold %.0f%%)",
				tier, qps, c.name, c.actual, pctChange*100, c.base, regressionThreshold*100)
		}
	}
}

// ---------------------------------------------------------------------------
// Benchmark runner
// ---------------------------------------------------------------------------

// runSLOBenchmark sends totalRequests to the handler at the given QPS rate
// and returns the observed latencies for successful requests.
func runSLOBenchmark(t *testing.T, h http.HandlerFunc, makeReq func() []byte, qps, totalRequests int) []time.Duration {
	t.Helper()

	srv := httptest.NewServer(h)
	defer srv.Close()

	client := &http.Client{}
	interval := time.Second / time.Duration(qps)
	var (
		mu        sync.Mutex
		latencies []time.Duration
	)

	var wg sync.WaitGroup
	wg.Add(totalRequests)

	for i := 0; i < totalRequests; i++ {
		go func(idx int) {
			defer wg.Done()
			time.Sleep(time.Duration(idx) * interval)

			body := makeReq()
			start := time.Now()
			resp, err := client.Post(srv.URL, "application/json", bytes.NewReader(body))
			elapsed := time.Since(start)
			if err != nil {
				t.Errorf("request %d: %v", idx, err)
				return
			}
			io.Copy(io.Discard, resp.Body) //nolint:errcheck
			resp.Body.Close()

			if resp.StatusCode != http.StatusOK {
				t.Errorf("request %d: status %d", idx, resp.StatusCode)
				return
			}

			mu.Lock()
			latencies = append(latencies, elapsed)
			mu.Unlock()
		}(i)
	}

	wg.Wait()
	return latencies
}

// ---------------------------------------------------------------------------
// Wazero sandbox HTTP handler
// ---------------------------------------------------------------------------

// helloWorldWasmB64 is a base64-encoded minimal Wasm module that writes
// "hello\n" to stdout via WASI fd_write and exits cleanly. Used as the
// wazero tier benchmark payload.
var helloWorldWasmB64 string

func init() {
	helloWorldWasmB64 = base64.StdEncoding.EncodeToString([]byte{
		0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00, 0x01, 0x0c, 0x02, 0x60,
		0x04, 0x7f, 0x7f, 0x7f, 0x7f, 0x01, 0x7f, 0x60, 0x00, 0x00, 0x02, 0x23,
		0x01, 0x16, 0x77, 0x61, 0x73, 0x69, 0x5f, 0x73, 0x6e, 0x61, 0x70, 0x73,
		0x68, 0x6f, 0x74, 0x5f, 0x70, 0x72, 0x65, 0x76, 0x69, 0x65, 0x77, 0x31,
		0x08, 0x66, 0x64, 0x5f, 0x77, 0x72, 0x69, 0x74, 0x65, 0x00, 0x00, 0x03,
		0x02, 0x01, 0x01, 0x05, 0x03, 0x01, 0x00, 0x01, 0x07, 0x13, 0x02, 0x06,
		0x6d, 0x65, 0x6d, 0x6f, 0x72, 0x79, 0x02, 0x00, 0x06, 0x5f, 0x73, 0x74,
		0x61, 0x72, 0x74, 0x00, 0x01, 0x0a, 0x1d, 0x01, 0x1b, 0x00, 0x41, 0x00,
		0x41, 0x20, 0x36, 0x02, 0x00, 0x41, 0x04, 0x41, 0x06, 0x36, 0x02, 0x00,
		0x41, 0x01, 0x41, 0x00, 0x41, 0x01, 0x41, 0x10, 0x10, 0x00, 0x1a, 0x0b,
		0x0b, 0x0c, 0x01, 0x00, 0x41, 0x20, 0x0b, 0x06, 0x68, 0x65, 0x6c, 0x6c,
		0x6f, 0x0a,
	})
}

// sandboxRunHandler returns an http.HandlerFunc that accepts a JSON body
// {"wasm_module_b64":"..."} and runs it through the wazero sandbox.
func sandboxRunHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			WasmModuleB64 string `json:"wasm_module_b64"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, fmt.Sprintf("invalid request: %v", err), http.StatusBadRequest)
			return
		}
		result, _ := sandbox.Run(req.WasmModuleB64, nil, 8, 0)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(result) //nolint:errcheck
	}
}

// ---------------------------------------------------------------------------
// Mock Z3 solver
// ---------------------------------------------------------------------------

// mockZ3Solver is a test double that returns "unsat" immediately without
// requiring the z3 binary. This ensures the Z3 tier always participates
// in the SLO benchmark regardless of host tooling availability.
type mockZ3Solver struct{}

func (m *mockZ3Solver) Solve(_ context.Context, _ string) (verify.Result, error) {
	return verify.Result{Sat: "unsat", Model: nil}, nil
}

// ---------------------------------------------------------------------------
// Tier configurations
// ---------------------------------------------------------------------------

// celTierConfig returns the CEL tier configuration with p50 < 20ms SLO target.
func celTierConfig() tierConfig {
	return tierConfig{
		name: "cel",
		makeReq: func() []byte {
			req := map[string]any{
				"input": map[string]any{
					"expr": "x > 0 && y < 100",
					"context": map[string]any{
						"x": 42,
						"y": 50,
					},
				},
			}
			body, _ := json.Marshal(req)
			return body
		},
		handler: cellib.Handler(),
		slos: []sloTarget{
			{pct: 50, maxMs: 20},
		},
	}
}

// wazeroTierConfig returns the wazero sandbox tier configuration with
// p95 < 100ms SLO target.
func wazeroTierConfig() tierConfig {
	return tierConfig{
		name: "wazero",
		makeReq: func() []byte {
			req := map[string]any{
				"wasm_module_b64": helloWorldWasmB64,
			}
			body, _ := json.Marshal(req)
			return body
		},
		handler: sandboxRunHandler(),
		slos: []sloTarget{
			{pct: 95, maxMs: 100},
		},
	}
}

// z3TierConfig returns the Z3 tier configuration with p99 < 500ms SLO target.
// Uses a mock solver so the tier always participates regardless of whether
// the z3 binary is installed.
func z3TierConfig() tierConfig {
	return tierConfig{
		name: "z3",
		makeReq: func() []byte {
			req := map[string]any{
				"input": map[string]any{
					"constraints_smt2": "(declare-const x Int) (assert (> x 0))",
					"timeout_ms":       2000,
				},
			}
			body, _ := json.Marshal(req)
			return body
		},
		handler: verify.Handler(&mockZ3Solver{}),
		slos: []sloTarget{
			{pct: 99, maxMs: 500},
		},
	}
}

// ---------------------------------------------------------------------------
// Main test
// ---------------------------------------------------------------------------

// TestLatencySLOs runs the SLO benchmark harness: measures p50/p95/p99
// latency across 10k requests at 10/100/1000 QPS for each tier (CEL,
// wazero, Z3), validates SLO targets, and checks for regressions >15%
// above a persisted baseline.
//
// In short mode (go test -short) each sub-test uses 100 requests at
// 10/100 QPS. Run without -short for the full 10k-request sweep.
//
// The baseline is auto-saved on the first run (no baseline file exists)
// so subsequent runs can detect regressions. Set
// SYMKERNEL_SLO_UPDATE_BASELINE=1 to re-record baselines.
func TestLatencySLOs(t *testing.T) {
	qpsLevels := []int{10, 100, 1000}
	totalRequests := 10000

	if testing.Short() {
		totalRequests = 100
		qpsLevels = []int{10, 100}
	}

	tiers := []tierConfig{
		celTierConfig(),
		wazeroTierConfig(),
		z3TierConfig(),
	}

	baseline, baselineExists := loadBaseline(t)
	// Start with existing entries so we preserve any entries not re-measured
	// in this run (e.g. if a tier is added later).
	updatedEntries := make([]baselineEntry, len(baseline.Entries))
	copy(updatedEntries, baseline.Entries)

	for _, tc := range tiers {
		t.Run(tc.name, func(t *testing.T) {
			for _, qps := range qpsLevels {
				t.Run(fmt.Sprintf("%dqps", qps), func(t *testing.T) {
					latencies := runSLOBenchmark(t, tc.handler, tc.makeReq, qps, totalRequests)
					if len(latencies) == 0 {
						t.Fatal("no successful requests")
					}

					pcts := computePercentiles(latencies)
					t.Logf("%s @ %d QPS (%d requests): p50=%.2fms p95=%.2fms p99=%.2fms",
						tc.name, qps, len(latencies), pcts.p50, pcts.p95, pcts.p99)

					// Validate SLO targets (informational).
					checkSLOTargets(t, tc.name, qps, pcts, tc.slos)

					// Check regression against baseline (hard CI failure).
					// Skipped in short mode — sample size too small for
					// statistically meaningful comparison.
					if !testing.Short() {
						bl := findBaselineEntry(baseline.Entries, tc.name, qps)
						checkRegression(t, tc.name, qps, pcts, bl)
					}

					// Record measurement for baseline persistence.
					upsertBaselineEntry(&updatedEntries, baselineEntry{
						Tier: tc.name,
						QPS:  qps,
						P50:  pcts.p50,
						P95:  pcts.p95,
						P99:  pcts.p99,
					})
				})
			}
		})
	}

	// In short mode (100 requests) variance is too high for meaningful
	// regression detection — always re-save the baseline and skip
	// regression checks.  Regression checking only fires in full mode
	// (10 000 requests) where the sample size provides statistical
	// significance.
	shortMode := testing.Short()
	if shortMode {
		// Overwrite baseline with this run's measurements.
		if len(updatedEntries) > 0 {
			saveBaseline(t, baselineStore{Entries: updatedEntries})
			t.Log("baseline saved (short mode — no regression check)")
		}
		return
	}

	// Full mode: regression is the hard CI gate.  Save baseline on first
	// run or when explicitly requested.
	shouldSave := !baselineExists || os.Getenv("SYMKERNEL_SLO_UPDATE_BASELINE") == "1"
	if shouldSave && len(updatedEntries) > 0 {
		saveBaseline(t, baselineStore{Entries: updatedEntries})
		t.Log("baseline saved")
	}
}