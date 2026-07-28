// Package bench also pins the policy-compliance fixture corpus shipped in
// bench/policy-compliance-fixtures/ (issue #249). The tests here serve two
// purposes:
//
//  1. Structural: every fixture file parses, there are exactly six of them,
//     each declares a complexity in {simple, moderate, complex}, and each
//     carries the documentation and dual (CEL + SMT-LIB2) representations the
//     acceptance criteria require.
//
//  2. Runnable: each fixture's CEL expression is executed against every one of
//     its sample contexts through the real internal/cel evaluator — the
//     concrete verification engine shared by /v1/verify/cel and
//     /v1/verify/criterion — and the boolean verdict is asserted to match the
//     fixture's documented expected_cel. This is the "tasks can be executed by
//     the concrete testing framework" acceptance criterion, pinned so a broken
//     expression cannot land silently.
package bench

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/WasmAgent/symkernel/internal/cel"
)

// fixturePath returns the absolute path to the policy-compliance-fixtures
// directory under the repository root.
func fixturePath(t *testing.T) string {
	t.Helper()
	return filepath.Join(repoRoot(t), "bench", "policy-compliance-fixtures")
}

// fixtureTask is the subset of a fixture file's JSON schema that these tests
// inspect. Field tags mirror the on-disk shape documented in the fixtures
// README.
type fixtureTask struct {
	ID               string `json:"id"`
	Title            string `json:"title"`
	Category         string `json:"category"`
	Complexity       string `json:"complexity"`
	Description      string `json:"description"`
	ExpectedBehavior string `json:"expected_behavior"`
	PolicyRules      []string `json:"policy_rules"`
	Paths            []struct {
		ID        string `json:"id"`
		Condition string `json:"condition"`
		Verdict   string `json:"verdict"`
		Feasible  bool   `json:"feasible"`
	} `json:"paths"`
	Representations struct {
		CEL struct {
			Endpoint      string         `json:"endpoint"`
			VerifyMethod  string         `json:"verify_method"`
			Expr          string         `json:"expr"`
			ContextTemplate map[string]any `json:"context_template"`
		} `json:"cel"`
		SMT2 struct {
			Endpoint          string `json:"endpoint"`
			Declarations      string `json:"declarations"`
			PolicyAssertion   string `json:"policy_assertion"`
			ExpectedCheckSat  string `json:"expected_check_sat"`
		} `json:"smt2"`
	} `json:"representations"`
	Samples []struct {
		Name         string         `json:"name"`
		Context      map[string]any `json:"context"`
		ExpectedCEL  *bool          `json:"expected_cel"`
		ExpectedPath string         `json:"expected_path"`
	} `json:"samples"`
}

// loadFixtures reads and parses every *.json file in the fixtures directory,
// returning the tasks keyed by their file stem. It fatals if the directory
// cannot be read or any file fails to parse.
func loadFixtures(t *testing.T) map[string]fixtureTask {
	t.Helper()
	dir := fixturePath(t)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read fixtures dir %s: %v", dir, err)
	}
	out := make(map[string]fixtureTask)
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		var task fixtureTask
		if err := json.Unmarshal(raw, &task); err != nil {
			t.Fatalf("parse %s: %v", e.Name(), err)
		}
		stem := strings.TrimSuffix(e.Name(), ".json")
		out[stem] = task
	}
	return out
}

// TestFixtures_CorpusShape enforces the directory-level acceptance criteria:
// the fixtures directory exists, contains exactly six task files, the set
// matches the six documented task ids, and the complexity range spans
// simple → complex.
func TestFixtures_CorpusShape(t *testing.T) {
	tasks := loadFixtures(t)

	want := map[string]bool{ // expected id → seen?
		"auth-minimum-age":   false,
		"rbac-role-check":    false,
		"resource-quota":     false,
		"time-window-access": false,
		"tiered-rate-limit":  false,
		"data-residency-geo": false,
	}
	if len(tasks) != len(want) {
		t.Errorf("expected exactly %d fixture files, got %d", len(want), len(tasks))
	}

	complexities := map[string]bool{}
	for stem, task := range tasks {
		if _, ok := want[stem]; !ok {
			t.Errorf("unexpected fixture file %q", stem)
			continue
		}
		want[stem] = true
		if stem != task.ID {
			t.Errorf("%s: file stem does not match id %q", stem, task.ID)
		}
		complexities[task.Complexity] = true
	}
	for id, seen := range want {
		if !seen {
			t.Errorf("expected fixture %q is missing from the directory", id)
		}
	}
	// The acceptance criteria require a range of complexity, not a single
	// band. The corpus deliberately spans all three bands.
	for _, c := range []string{"simple", "moderate", "complex"} {
		if !complexities[c] {
			t.Errorf("corpus is missing the %q complexity band (need simple→complex range)", c)
		}
	}
}

// TestFixtures_DocumentationAndStructure validates the per-task acceptance
// criteria: documentation fields are populated, each task declares multiple
// reachable paths (branch coverage), and both the CEL and SMT-LIB2
// representations are present and well-formed.
func TestFixtures_DocumentationAndStructure(t *testing.T) {
	tasks := loadFixtures(t)

	// Stable ordering for readable failures.
	stems := make([]string, 0, len(tasks))
	for s := range tasks {
		stems = append(stems, s)
	}
	sort.Strings(stems)

	for _, stem := range stems {
		task := tasks[stem]
		t.Run(stem, func(t *testing.T) {
			if task.Title == "" || task.Description == "" || task.ExpectedBehavior == "" {
				t.Errorf("missing documentation: title/description/expected_behavior must all be set")
			}
			if len(task.PolicyRules) == 0 {
				t.Errorf("policy_rules must list at least one rule")
			}
			switch task.Complexity {
			case "simple", "moderate", "complex":
			default:
				t.Errorf("complexity %q is not one of simple|moderate|complex", task.Complexity)
			}

			// Path coverage: at least one allow and one deny, all feasible.
			if len(task.Paths) < 2 {
				t.Errorf("paths must enumerate at least 2 branches for coverage, got %d", len(task.Paths))
			}
			sawAllow, sawDeny := false, false
			known := map[string]bool{}
			for _, p := range task.Paths {
				if p.ID == "" || p.Condition == "" {
					t.Errorf("path entry missing id/condition: %+v", p)
				}
				switch p.Verdict {
				case "allow":
					sawAllow = true
				case "deny":
					sawDeny = true
				default:
					t.Errorf("path %q has verdict %q, want allow|deny", p.ID, p.Verdict)
				}
				if !p.Feasible {
					t.Errorf("path %q must be feasible (witness-reachable) for meaningful coverage", p.ID)
				}
				known[p.ID] = true
			}
			if !sawAllow || !sawDeny {
				t.Errorf("paths must include at least one allow AND one deny branch")
			}

			// CEL representation: non-empty expression addressed to the
			// criterion endpoint via cel_expr.
			cel := task.Representations.CEL
			if cel.Endpoint == "" || cel.VerifyMethod != "cel_expr" || cel.Expr == "" {
				t.Errorf("representations.cel must set endpoint, verify_method=cel_expr, and a non-empty expr")
			}
			if len(cel.ContextTemplate) == 0 {
				t.Errorf("representations.cel.context_template must declare the input variables")
			}

			// SMT-LIB2 representation: declarations + a policy assertion that
			// encodes the allow path, plus the documented check-sat result.
			smt := task.Representations.SMT2
			if !strings.Contains(smt.Declarations, "declare-const") {
				t.Errorf("representations.smt2.declarations must declare at least one constant: %q", smt.Declarations)
			}
			if !strings.HasPrefix(strings.TrimSpace(smt.PolicyAssertion), "(assert") {
				t.Errorf("representations.smt2.policy_assertion must be an (assert ...) form: %q", smt.PolicyAssertion)
			}
			if smt.ExpectedCheckSat != "sat" && smt.ExpectedCheckSat != "unsat" {
				t.Errorf("representations.smt2.expected_check_sat must be sat|unsat, got %q", smt.ExpectedCheckSat)
			}

			// Samples: golden cases that name an expected verdict and a path
			// documented above.
			if len(task.Samples) == 0 {
				t.Fatalf("samples must contain at least one golden case")
			}
			for _, s := range task.Samples {
				if s.Name == "" {
					t.Errorf("sample missing name")
				}
				if s.ExpectedCEL == nil {
					t.Errorf("sample %q missing expected_cel", s.Name)
				}
				if !known[s.ExpectedPath] {
					t.Errorf("sample %q references unknown expected_path %q", s.Name, s.ExpectedPath)
				}
			}
		})
	}
}

// TestFixtures_ConcreteCEL executes every fixture's CEL expression against
// every sample context through the real internal/cel evaluator and asserts the
// boolean verdict matches the documented expected_cel. This proves the corpus
// is runnable by the concrete testing framework (the CEL engine behind
// /v1/verify/cel and /v1/verify/criterion).
func TestFixtures_ConcreteCEL(t *testing.T) {
	tasks := loadFixtures(t)

	stems := make([]string, 0, len(tasks))
	for s := range tasks {
		stems = append(stems, s)
	}
	sort.Strings(stems)

	ctx := context.Background()
	for _, stem := range stems {
		task := tasks[stem]
		expr := task.Representations.CEL.Expr
		if expr == "" {
			t.Fatalf("%s: missing CEL expr", stem)
		}
		t.Run(stem, func(t *testing.T) {
			for _, s := range task.Samples {
				if s.ExpectedCEL == nil {
					t.Fatalf("%s/%s: sample missing expected_cel", stem, s.Name)
				}
				t.Run(s.Name, func(t *testing.T) {
					// JSON decodes every number as float64, but CEL type
					// inference then maps it to DoubleType and rejects
					// comparisons against int literals (e.g. age >= 18).
					// Convert whole numbers back to int64, mirroring the
					// production /v1/verify/cel handler's normalizeContext.
					vars := normalizeNumbers(s.Context)
					got, err := cel.Evaluate(ctx, expr, vars)
					if err != nil {
						t.Fatalf("%s/%s: cel.Evaluate: %v", stem, s.Name, err)
					}
					ok, ok2 := got.(bool)
					if !ok2 {
						t.Fatalf("%s/%s: CEL expr returned %T, want bool", stem, s.Name, got)
					}
					if ok != *s.ExpectedCEL {
						t.Errorf("%s/%s: CEL verdict = %v, want %v (path %q)",
							stem, s.Name, ok, *s.ExpectedCEL, s.ExpectedPath)
					}
				})
			}
		})
	}
}

// normalizeNumbers returns a copy of m in which whole-number float64 values
// (the result of encoding/json's default number decoding) are converted to
// int64 so that CEL infers IntType rather than DoubleType. This mirrors
// internal/cel.normalizeContext — the same preparation the production
// /v1/verify/cel handler applies to inbound request contexts.
func normalizeNumbers(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		if f, ok := v.(float64); ok {
			if f == math.Trunc(f) && !math.IsInf(f, 0) && !math.IsNaN(f) {
				out[k] = int64(f)
				continue
			}
		}
		out[k] = v
	}
	return out
}
