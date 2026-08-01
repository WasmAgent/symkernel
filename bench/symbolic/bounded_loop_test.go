package symbolic

import "testing"

func TestBoundedLoopFixture(t *testing.T) {
	result := runNamedFixture(t, "bounded-loop")
	assertFixtureResult(t, result, 4)
	if result.ExploredPaths != 2 {
		t.Errorf("bounded-loop: explored paths = %d, want 2", result.ExploredPaths)
	}
}
