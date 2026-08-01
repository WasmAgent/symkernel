package symbolic

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestSymbolicFixtureCorpus(t *testing.T) {
	root := filepath.Join("..", "..", FixtureRoot)
	corpus, err := loadManifest(root)
	if err != nil {
		t.Fatalf("loadManifest: %v", err)
	}
	if len(corpus.Fixtures) != 3 {
		t.Fatalf("fixture count = %d, want 3", len(corpus.Fixtures))
	}

	seen := make(map[string]bool, len(corpus.Fixtures))
	for _, fixture := range corpus.Fixtures {
		if seen[fixture.Name] {
			t.Errorf("duplicate fixture %q", fixture.Name)
		}
		seen[fixture.Name] = true
		if fixture.BranchPoints <= 0 || len(fixture.Paths) == 0 {
			t.Errorf("%s: missing branch/path declarations", fixture.Name)
		}

		encoded, err := os.ReadFile(filepath.Join(root, fixture.Module))
		if err != nil {
			t.Errorf("%s: read module: %v", fixture.Name, err)
			continue
		}
		if _, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(encoded))); err != nil {
			t.Errorf("%s: invalid base64 module: %v", fixture.Name, err)
		}
	}

	manifestData, err := os.ReadFile(filepath.Join(root, manifestName))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(manifestData, &decoded); err != nil {
		t.Fatalf("manifest is not JSON: %v", err)
	}
}

func runNamedFixture(t *testing.T, name string) Result {
	t.Helper()
	if _, err := exec.LookPath("z3"); err != nil {
		t.Skip("z3 not on PATH")
	}

	root := filepath.Join("..", "..", FixtureRoot)
	corpus, err := loadManifest(root)
	if err != nil {
		t.Fatalf("loadManifest: %v", err)
	}
	for _, fixture := range corpus.Fixtures {
		if fixture.Name != name {
			continue
		}
		result, err := runFixture(context.Background(), root, fixture)
		if err != nil {
			t.Fatalf("runFixture: %v", err)
		}
		return result
	}
	t.Fatalf("fixture %q not found", name)
	return Result{}
}

func assertFixtureResult(t *testing.T, result Result, wantPotentialPaths int) {
	t.Helper()
	if err := ValidateResult(result); err != nil {
		t.Fatalf("%s: %v", result.Fixture, err)
	}
	if result.ExploredPaths == 0 {
		t.Fatalf("%s: explored no feasible paths", result.Fixture)
	}
	if result.PotentialPaths != wantPotentialPaths {
		t.Errorf("%s: potential paths = %d, want %d", result.Fixture, result.PotentialPaths, wantPotentialPaths)
	}
	if result.SolverTime <= 0 {
		t.Errorf("%s: solver time = %s, want positive duration", result.Fixture, result.SolverTime)
	}
	if result.CacheHitRatio <= 0 {
		t.Errorf("%s: cache hit ratio = %f, want a warm-cache hit", result.Fixture, result.CacheHitRatio)
	}
}

func TestFormatTable(t *testing.T) {
	table := FormatTable([]Result{{
		Fixture:           "bounded-loop",
		ExploredPaths:     2,
		PotentialPaths:    4,
		PathExplosionRate: 0.5,
		CacheHitRatio:     0.5,
	}})
	for _, column := range []string{"Fixture", "Path explosion rate", "Solver time", "Cache hit ratio", "bounded-loop"} {
		if !strings.Contains(table, column) {
			t.Errorf("table missing %q:\n%s", column, table)
		}
	}
}

func BenchmarkCuratedFixtures(b *testing.B) {
	if _, err := exec.LookPath("z3"); err != nil {
		b.Skip("z3 not on PATH")
	}

	root := filepath.Join("..", "..", FixtureRoot)
	var results []Result
	b.ResetTimer()
	for range b.N {
		var err error
		results, err = Run(context.Background(), root)
		if err != nil {
			b.Fatalf("Run: %v", err)
		}
	}
	b.StopTimer()
	b.Logf("\n%s", FormatTable(results))
}
