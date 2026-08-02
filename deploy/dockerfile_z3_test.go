package deploy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestDockerfileZ3 covers the Milestone 12 deploy deliverable: the
// extended multi-stage build in deploy/Dockerfile.z3 must contain a
// dedicated Z3 library compilation stage, static linking for distroless
// compatibility, and the optional BUILD_Z3_FROM_SOURCE knob for version
// pinning.
func TestDockerfileZ3(t *testing.T) {
	content, err := os.ReadFile(filepath.Join("Dockerfile.z3"))
	if err != nil {
		t.Fatalf("deploy/Dockerfile.z3 must exist: %v", err)
	}
	dockerfile := string(content)

	required := []string{
		"BUILD_Z3_FROM_SOURCE",        // optional source-build knob
		"Z3_VERSION",                  // pinned Z3 release version
		"Z3_SHA256",                   // optional tarball checksum verification
		"FROM debian:bookworm AS z3",  // dedicated Z3 library compilation stage
		"-DBUILD_LIBZ3_SHARED=OFF",    // build a static libz3.a
		"-static",                     // static linking flags
		"distroless",                  // distroless compatibility
		"FINAL_IMAGE",                 // selectable final base image
	}
	for _, needle := range required {
		if !strings.Contains(dockerfile, needle) {
			t.Errorf("deploy/Dockerfile.z3 must contain %q", needle)
		}
	}

	// The Dockerfile must be multi-stage: the Z3 compilation stage, the
	// Go builder stage, and the final image stage.
	if got := strings.Count(dockerfile, "\nFROM "); got < 3 {
		t.Errorf("deploy/Dockerfile.z3 must define at least 3 stages, found %d FROM directives", got)
	}
}
