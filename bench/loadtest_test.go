// Package bench holds repo-level invariants for the Z3 load-test
// infrastructure. The Cloudflare Containers deploy (issue #43) requires
// bench/load-test-infra.md to exist and be non-empty — this is the shell
// acceptance criterion `test -s bench/load-test-infra.md`; the test below
// pins it so the invariant cannot silently regress.
package bench

import (
	"os"
	"path/filepath"
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
