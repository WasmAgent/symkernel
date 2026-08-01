package symbolic

import "testing"

func TestRecursionFixture(t *testing.T) {
	result := runNamedFixture(t, "recursion")
	assertFixtureResult(t, result, 8)
	if result.ExploredPaths != 3 {
		t.Errorf("recursion: explored paths = %d, want 3", result.ExploredPaths)
	}
}
