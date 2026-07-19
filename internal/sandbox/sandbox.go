// Package sandbox provides a wazero-based WebAssembly runtime wrapper that
// executes Wasm modules inside an isolated environment. By default, filesystem
// and network access are denied and memory usage is capped.
package sandbox

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"time"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
)

// wasmPageSize is the WebAssembly page size in bytes (64 KiB).
const wasmPageSize = 65536

// SandboxResult captures the observable side-effects of a sandboxed execution.
type SandboxResult struct {
	// Stdout contains bytes the guest wrote to file descriptor 1.
	Stdout []byte

	// Stderr contains bytes the guest wrote to file descriptor 2.
	Stderr []byte

	// ExitCode is the exit code returned by the guest via WASI proc_exit,
	// or 0 if the module's _start function returned normally.
	ExitCode int32
}

// Run decodes the base64-encoded Wasm module in wasmModuleB64 and executes it
// inside a fresh wazero runtime subject to the supplied constraints.
//
// Parameters:
//   - wasmModuleB64: base64 encoding of a .wasm binary.
//   - args: optional key-value pairs forwarded to the guest as CLI arguments
//     ("key=value" form). Nil or empty results in no arguments.
//   - memLimitMB: maximum guest memory in megabytes. The runtime translates
//     this to Wasm pages (1 page = 64 KiB) and enforces it via
//     wazero RuntimeConfig.WithMemoryLimitPages.
//   - timeoutMs: wall-clock deadline in milliseconds. Zero or negative values
//     disable the timeout.
//
// Security defaults:
//   - Filesystem access: denied (no FSConfig / DirMount).
//   - Network access: denied (wasi_snapshot_preview1 has no socket support).
//   - Environment variables: none inherited.
//   - Stdin: closed immediately.
func Run(wasmModuleB64 string, args map[string]any, memLimitMB int, timeoutMs int) (SandboxResult, error) {
	wasmBytes, err := base64.StdEncoding.DecodeString(wasmModuleB64)
	if err != nil {
		return SandboxResult{}, fmt.Errorf("sandbox: base64 decode: %w", err)
	}

	var stdoutBuf, stderrBuf limitedBuffer

	// Convert memory limit from MB to Wasm pages.
	pages := memLimitMBToPages(memLimitMB)

	ctx := context.Background()
	if timeoutMs > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(timeoutMs)*time.Millisecond)
		defer cancel()
	}

	rConfig := wazero.NewRuntimeConfig().
		WithMemoryLimitPages(pages).
		WithCloseOnContextDone(true)

	r := wazero.NewRuntimeWithConfig(ctx, rConfig)
	defer r.Close(ctx) //nolint:errcheck // best-effort cleanup

	// Instantiate WASI so that standard Wasm binaries (e.g. TinyGo) can
	// resolve imports. No filesystem or network mounts are configured, which
	// denies those capabilities by default.
	wasi_snapshot_preview1.MustInstantiate(ctx, r)

	// Build module configuration with sandboxed I/O.
	mConfig := wazero.NewModuleConfig().
		WithStdout(&stdoutBuf).
		WithStderr(&stderrBuf).
		WithStartFunctions("_start")

	// Forward args as positional CLI arguments.
	mConfig = mConfig.WithArgs(buildArgs(args)...)

	mod, err := r.InstantiateWithConfig(ctx, wasmBytes, mConfig)
	if err != nil {
		// If the module exited via WASI proc_exit, extract the exit code.
		return SandboxResult{
			Stdout:  stdoutBuf.Bytes(),
			Stderr:  stderrBuf.Bytes(),
			ExitCode: exitCodeFromErr(err),
		}, err
	}

	// Close the instantiated module (not the whole runtime) to release
	// sandbox resources.
	_ = mod.Close(ctx) //nolint:errcheck // best-effort

	return SandboxResult{
		Stdout:  stdoutBuf.Bytes(),
		Stderr:  stderrBuf.Bytes(),
		ExitCode: 0,
	}, nil
}

// memLimitMBToPages converts a megabyte value to WebAssembly 64 KiB pages.
// It returns at least 1 page so that even a zero-limit still allows minimal
// allocation required by the Wasm spec.
func memLimitMBToPages(mb int) uint32 {
	if mb <= 0 {
		return 1
	}
	pages := uint32(mb) * 1024 * 1024 / wasmPageSize
	if pages == 0 {
		return 1
	}
	return pages
}

// buildArgs converts a map of arguments into a slice of "key=value" strings
// suitable for WASI argv.
func buildArgs(args map[string]any) []string {
	if len(args) == 0 {
		return nil
	}
	result := make([]string, 0, len(args))
	for k, v := range args {
		result = append(result, fmt.Sprintf("%s=%v", k, v))
	}
	return result
}

// exitCodeFromErr extracts a WASI exit code from the error returned by
// wazero when a module calls proc_exit. For all other errors it returns 0.
func exitCodeFromErr(err error) int32 {
	if err == nil {
		return 0
	}
	type exitCoder interface {
		ExitCode() uint32
	}
	if ec, ok := err.(exitCoder); ok {
		return int32(ec.ExitCode())
	}
	return 0
}

// limitedBuffer is a byte buffer that implements io.Writer with an
// optional size cap to prevent unbounded memory usage from a misbehaving
// guest. Once the cap is reached, writes are silently discarded.
type limitedBuffer struct {
	buf    []byte
	maxCap int // zero means unlimited
}

func (b *limitedBuffer) Write(p []byte) (n int, err error) {
	if b.maxCap > 0 && len(b.buf) >= b.maxCap {
		return len(p), nil // silently discard
	}
	want := len(p)
	if b.maxCap > 0 {
		remaining := b.maxCap - len(b.buf)
		if int64(remaining) < int64(want) {
			want = remaining
		}
	}
	b.buf = append(b.buf, p[:want]...)
	return len(p), nil
}

func (b *limitedBuffer) Bytes() []byte { return b.buf }

func (b *limitedBuffer) ReadFrom(r io.Reader) (int64, error) {
	return io.Copy(b, r)
}
