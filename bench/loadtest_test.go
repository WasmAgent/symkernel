// Package bench holds repo-level invariants for the load-test
// infrastructure. The Cloudflare Containers deploy (issue #43) requires
// bench/load-test-infra.md to exist and be non-empty — this is the shell
// acceptance criterion `test -s bench/load-test-infra.md`; the test below
// pins it so the invariant cannot silently regress.
//
// Milestone 6 adds composition-tier benchmarks (issue #114): three k6 scripts
// in bench/k6/ that compare single-tier CEL vs. three-tier chains under cache
// cold/warm conditions, measure fallback impact on tail latency (p95, p99),
// and emit trace examples for each path.
package bench

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// repoRoot returns the repository root (the directory containing go.mod)
// by walking up from the test process's working directory. go test runs
// each package with its directory as the CWD, so for package bench that
// is one level below the root.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("os.Getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("go.mod not found walking up from %s", dir)
		}
		dir = parent
	}
}

// TestLoadTestInfraDocExists mirrors the shell check
// `test -s bench/load-test-infra.md`: the file must exist and have a
// non-zero size. This is an acceptance criterion for issue #43.
func TestLoadTestInfraDocExists(t *testing.T) {
	path := filepath.Join(repoRoot(t), "bench", "load-test-infra.md")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("bench/load-test-infra.md: %v (acceptance criterion `test -s` requires the file to exist)", err)
	}
	if info.Size() == 0 {
		t.Fatal("bench/load-test-infra.md is empty (acceptance criterion `test -s` requires non-zero size)")
	}
}

// k6CompositionBenchmarks are the three k6 scripts added in Milestone 6
// (issue #114). Each entry maps the file name to a list of substring patterns
// that must appear in the file for it to be considered valid.
var k6CompositionBenchmarks = []struct {
	file    string
	patterns []string
}{
	{
		file: "composition-tier-benchmarks.js",
		patterns: []string{
			"import http from 'k6/http'",
			"cel_only",
			"three_tier_any_pass",
			"three_tier_all_pass",
			"three_tier_short_circuit",
			"v1/verify/composed",
			"p(95)",
			"p(99)",
			"trace",
		},
	},
	{
		file: "composition-cache-cold-warm.js",
		patterns: []string{
			"import http from 'k6/http'",
			"coldRequest",
			"warmRequest",
			"v1/admin/cache/invalidate",
			"v1/admin/cache/stats",
			"v1/verify/composed",
			"cold_cache_duration",
			"warm_cache_duration",
			"trace",
		},
	},
	{
		file: "composition-fallback-tail-latency.js",
		patterns: []string{
			"import http from 'k6/http'",
			"fallbackAnyPass",
			"fallbackAllPass",
			"fallbackShortCircuit",
			"v1/verify/composed",
			"fallback_any_pass_duration",
			"fallback_all_pass_duration",
			"fallback_short_circuit_duration",
			"p(95)",
			"p(99)",
			"trace",
		},
	},
}

// TestLatencySLOHarnessExists verifies that the SLO benchmark harness file
// exists under bench/ and contains the expected structural patterns
// (percentile computation, baseline persistence, tier configurations,
// regression checking). This pins the acceptance criteria for issue #140.
func TestLatencySLOHarnessExists(t *testing.T) {
	root := repoRoot(t)
	path := filepath.Join(root, "bench", "latency_slos_test.go")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	content := string(data)
	patterns := []string{
		"computePercentiles",
		"baselineStore",
		"regressionThreshold",
		"celTierConfig",
		"wazeroTierConfig",
		"z3TierConfig",
		"TestLatencySLOs",
		"runSLOBenchmark",
		"checkRegression",
	}
	for _, pat := range patterns {
		if !strings.Contains(content, pat) {
			t.Errorf("latency_slos_test.go is missing expected pattern %q", pat)
		}
	}
}

// TestCompositionBenchmarksExist verifies that the three Milestone 6 k6
// composition-tier benchmark scripts exist under bench/k6/ and contain the
// expected structural patterns (scenarios, metrics, endpoints, trace output).
// This pins the acceptance criteria from issue #114 so they cannot silently
// regress.
func TestCompositionBenchmarksExist(t *testing.T) {
	root := repoRoot(t)
	k6Dir := filepath.Join(root, "bench", "k6")

	for _, tc := range k6CompositionBenchmarks {
		t.Run(tc.file, func(t *testing.T) {
			path := filepath.Join(k6Dir, tc.file)
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("reading %s: %v", path, err)
			}
			content := string(data)
			for _, pat := range tc.patterns {
				if !strings.Contains(content, pat) {
					t.Errorf("file %s is missing expected pattern %q", tc.file, pat)
				}
			}
		})
	}
}
