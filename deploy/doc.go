// Package deploy contains the symkernel deployment artifacts, including
// the extended multi-stage Z3 Dockerfile variant (Dockerfile.z3) that
// adds a dedicated Z3 library compilation stage and can statically link
// Z3 for distroless compatibility via the optional BUILD_Z3_FROM_SOURCE
// build knob.
package deploy
