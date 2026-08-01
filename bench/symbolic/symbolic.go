// Package symbolic runs the curated WebAssembly symbolic-execution benchmark
// corpus. It validates concrete execution with wazero and times the matching
// Z3 path-feasibility queries, including the solver decision-cache hit ratio.
package symbolic

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/WasmAgent/symkernel/internal/z3"
	"github.com/tetratelabs/wazero"
)

const manifestName = "manifest.json"

// FixtureRoot is the repository-relative location of the curated corpus.
const FixtureRoot = "wasmagent/symbolic-fixtures"

// Result is one row in the symbolic benchmark report.
type Result struct {
	Fixture           string
	ExploredPaths     int
	PotentialPaths    int
	PathExplosionRate float64
	SolverTime        time.Duration
	CacheHitRatio     float64
}

type manifest struct {
	Fixtures []fixture `json:"fixtures"`
}

type fixture struct {
	Name         string      `json:"name"`
	Module       string      `json:"module"`
	Entry        string      `json:"entry"`
	Arguments    []uint64    `json:"arguments"`
	BranchPoints int         `json:"branch_points"`
	Paths        []pathQuery `json:"paths"`
}

type pathQuery struct {
	Constraints string         `json:"constraints"`
	Model       map[string]any `json:"model"`
}

// Run executes every fixture in root and measures its declared SMT path
// conditions. Every condition is solved twice: the first pass measures solver
// work, and the second pass records the decision-cache behaviour.
func Run(ctx context.Context, root string) ([]Result, error) {
	corpus, err := loadManifest(root)
	if err != nil {
		return nil, err
	}

	results := make([]Result, 0, len(corpus.Fixtures))
	for _, f := range corpus.Fixtures {
		result, err := runFixture(ctx, root, f)
		if err != nil {
			return nil, fmt.Errorf("symbolic benchmark %q: %w", f.Name, err)
		}
		results = append(results, result)
	}
	return results, nil
}

func loadManifest(root string) (manifest, error) {
	var corpus manifest
	data, err := os.ReadFile(filepath.Join(root, manifestName))
	if err != nil {
		return corpus, fmt.Errorf("read manifest: %w", err)
	}
	if err := json.Unmarshal(data, &corpus); err != nil {
		return corpus, fmt.Errorf("decode manifest: %w", err)
	}
	if len(corpus.Fixtures) == 0 {
		return corpus, fmt.Errorf("manifest has no fixtures")
	}
	return corpus, nil
}

func runFixture(ctx context.Context, root string, f fixture) (Result, error) {
	if f.Name == "" || f.Module == "" || f.Entry == "" || len(f.Paths) == 0 {
		return Result{}, fmt.Errorf("incomplete fixture definition")
	}
	if f.BranchPoints < 0 || f.BranchPoints >= 63 {
		return Result{}, fmt.Errorf("branch_points must be between 0 and 62")
	}
	if err := executeModule(ctx, filepath.Join(root, f.Module), f.Entry, f.Arguments); err != nil {
		return Result{}, err
	}

	cacheBefore := z3.CacheStats()
	start := time.Now()
	feasible, err := solvePaths(ctx, f.Paths)
	if err != nil {
		return Result{}, err
	}
	solverTime := time.Since(start)
	if _, err := solvePaths(ctx, f.Paths); err != nil {
		return Result{}, err
	}
	cacheAfter := z3.CacheStats()

	potentialPaths := 1 << f.BranchPoints
	accesses := cacheAfter.Hits - cacheBefore.Hits + cacheAfter.Misses - cacheBefore.Misses
	cacheHits := cacheAfter.Hits - cacheBefore.Hits
	cacheHitRatio := 0.0
	if accesses > 0 {
		cacheHitRatio = float64(cacheHits) / float64(accesses)
	}

	return Result{
		Fixture:           f.Name,
		ExploredPaths:     feasible,
		PotentialPaths:    potentialPaths,
		PathExplosionRate: float64(feasible) / float64(potentialPaths),
		SolverTime:        solverTime,
		CacheHitRatio:     cacheHitRatio,
	}, nil
}

func executeModule(ctx context.Context, modulePath, entry string, arguments []uint64) error {
	encoded, err := os.ReadFile(modulePath)
	if err != nil {
		return fmt.Errorf("read module: %w", err)
	}
	wasm, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(encoded)))
	if err != nil {
		return fmt.Errorf("decode module: %w", err)
	}

	runtime := wazero.NewRuntime(ctx)
	defer runtime.Close(ctx) //nolint:errcheck // context controls runtime teardown
	module, err := runtime.Instantiate(ctx, wasm)
	if err != nil {
		return fmt.Errorf("instantiate module: %w", err)
	}
	defer module.Close(ctx) //nolint:errcheck // runtime close releases module resources
	fn := module.ExportedFunction(entry)
	if fn == nil {
		return fmt.Errorf("export %q not found", entry)
	}
	if _, err := fn.Call(ctx, arguments...); err != nil {
		return fmt.Errorf("call %q: %w", entry, err)
	}
	return nil
}

func solvePaths(ctx context.Context, paths []pathQuery) (int, error) {
	feasible := 0
	for _, path := range paths {
		solution, err := z3.SolveConstraintsCtx(ctx, path.Constraints, path.Model)
		if err != nil {
			return 0, err
		}
		if solution.Sat == "sat" {
			feasible++
		}
	}
	return feasible, nil
}

// FormatTable renders benchmark results as a Markdown table suitable for CI
// logs and benchmark reports.
func FormatTable(results []Result) string {
	var b strings.Builder
	b.WriteString("| Fixture | Paths explored | Potential paths | Path explosion rate | Solver time | Cache hit ratio |\n")
	b.WriteString("| --- | ---: | ---: | ---: | ---: | ---: |\n")
	for _, result := range results {
		fmt.Fprintf(&b, "| %s | %d | %d | %.2f%% | %s | %.2f%% |\n",
			result.Fixture,
			result.ExploredPaths,
			result.PotentialPaths,
			result.PathExplosionRate*100,
			result.SolverTime.Round(time.Microsecond),
			result.CacheHitRatio*100,
		)
	}
	return b.String()
}

// ValidateResult verifies invariants that callers can use when turning the
// report into a regression gate.
func ValidateResult(result Result) error {
	if result.Fixture == "" || result.PotentialPaths < 1 {
		return fmt.Errorf("invalid benchmark result")
	}
	if result.ExploredPaths < 0 || result.ExploredPaths > result.PotentialPaths {
		return fmt.Errorf("explored paths outside potential-path range")
	}
	if math.IsNaN(result.PathExplosionRate) || result.PathExplosionRate < 0 || result.PathExplosionRate > 1 {
		return fmt.Errorf("invalid path explosion rate")
	}
	if math.IsNaN(result.CacheHitRatio) || result.CacheHitRatio < 0 || result.CacheHitRatio > 1 {
		return fmt.Errorf("invalid cache hit ratio")
	}
	return nil
}
