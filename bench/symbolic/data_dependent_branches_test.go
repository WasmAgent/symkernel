package symbolic

import "testing"

func TestDataDependentBranchesFixture(t *testing.T) {
	result := runNamedFixture(t, "data-dependent-branches")
	assertFixtureResult(t, result, 4)
	if result.ExploredPaths != 4 {
		t.Errorf("data-dependent-branches: explored paths = %d, want 4", result.ExploredPaths)
	}
}
