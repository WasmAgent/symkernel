package repair

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/WasmAgent/symkernel/internal/sandbox"
)

// mockTrapResult builds a SandboxResult carrying a trap, simulating what
// sandbox.Run returns when a guest triggers a trap. It is the "mock trap"
// required by the milestone bullet's unit-test coverage.
func mockTrapResult(kind sandbox.TrapKind, msg string) sandbox.SandboxResult {
	return sandbox.SandboxResult{
		Stdout:   []byte("partial output\n"),
		Stderr:   []byte("warning: low memory\n"),
		ExitCode: 0,
		Trap:     &sandbox.TrapInfo{Kind: kind, Message: msg},
	}
}

// TestRepair_UnreachableTrap is the primary success case: a representative
// trap (unreachable) with stdout/stderr populated. It verifies every
// structured field propagates verbatim and the assembled user prompt
// references every input.
func TestRepair_UnreachableTrap(t *testing.T) {
	program := `(module (func (export "_start") unreachable))`
	result := mockTrapResult(
		sandbox.TrapKindUnreachable,
		"wasm error: unreachable\nwasm stack trace:\t.handle",
	)

	prompt, err := Repair(program, result)
	if err != nil {
		t.Fatalf("Repair returned error: %v", err)
	}

	// Structured fields propagate verbatim from the inputs.
	if prompt.Program != program {
		t.Errorf("Program = %q, want %q", prompt.Program, program)
	}
	if prompt.TrapKind != string(sandbox.TrapKindUnreachable) {
		t.Errorf("TrapKind = %q, want %q", prompt.TrapKind, sandbox.TrapKindUnreachable)
	}
	if prompt.TrapMessage != result.Trap.Message {
		t.Errorf("TrapMessage = %q, want %q", prompt.TrapMessage, result.Trap.Message)
	}
	if prompt.Stdout != "partial output\n" {
		t.Errorf("Stdout = %q, want %q", prompt.Stdout, "partial output\n")
	}
	if prompt.Stderr != "warning: low memory\n" {
		t.Errorf("Stderr = %q, want %q", prompt.Stderr, "warning: low memory\n")
	}
	if prompt.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", prompt.ExitCode)
	}

	// System prompt frames the repair task.
	if prompt.System == "" {
		t.Error("System prompt is empty")
	}

	// Assembled user prompt references every input.
	for _, want := range []string{
		"unreachable",
		result.Trap.Message,
		program,
		"partial output",
		"warning: low memory",
		"## Program to fix",
	} {
		if !strings.Contains(prompt.User, want) {
			t.Errorf("User prompt missing %q; got:\n%s", want, prompt.User)
		}
	}
}

// TestRepair_DoesNotCallLLM guards the contract that Repair is side-effect
// free: it must not reach out to any model or network, and must return
// promptly with a populated prompt. (If it ever did call an LLM, this test
// would have no way to satisfy that dependency in CI.)
func TestRepair_DoesNotCallLLM(t *testing.T) {
	result := mockTrapResult(sandbox.TrapKindMemoryOOB, "wasm error: out of bounds memory access")
	prompt, err := Repair("module-source", result)
	if err != nil {
		t.Fatalf("Repair returned error: %v", err)
	}
	if prompt.User == "" {
		t.Fatal("User prompt is empty")
	}
	if prompt.System == "" {
		t.Fatal("System prompt is empty")
	}
}

// TestRepair_TableDriven covers every trap kind surfaced by the sandbox's
// trap → error protocol, asserting the kind flows through into both the
// structured field and the assembled prompt.
func TestRepair_TableDriven(t *testing.T) {
	tests := []struct {
		name string
		kind sandbox.TrapKind
	}{
		{"unreachable", sandbox.TrapKindUnreachable},
		{"memory_oob", sandbox.TrapKindMemoryOOB},
		{"stack_overflow", sandbox.TrapKindStackOverflow},
		{"timeout", sandbox.TrapKindTimeout},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := mockTrapResult(tt.kind, "msg for "+string(tt.kind))
			prompt, err := Repair("prog-"+tt.name, result)
			if err != nil {
				t.Fatalf("Repair returned error: %v", err)
			}
			if prompt.TrapKind != string(tt.kind) {
				t.Errorf("TrapKind = %q, want %q", prompt.TrapKind, tt.kind)
			}
			if !strings.Contains(prompt.User, string(tt.kind)) {
				t.Errorf("User prompt missing trap kind %q", tt.kind)
			}
		})
	}
}

func TestRepair_EmptyProgram(t *testing.T) {
	result := mockTrapResult(sandbox.TrapKindUnreachable, "boom")
	_, err := Repair("   ", result)
	if !errors.Is(err, ErrEmptyProgram) {
		t.Fatalf("err = %v, want ErrEmptyProgram", err)
	}
}

// TestRepair_NoTrap asserts that a clean exit (Trap == nil) is rejected: there
// is no trap to diagnose, so emitting a repair prompt would be misleading.
func TestRepair_NoTrap(t *testing.T) {
	result := sandbox.SandboxResult{Stdout: []byte("ok"), ExitCode: 0}
	_, err := Repair("module-source", result)
	if !errors.Is(err, ErrNoTrap) {
		t.Fatalf("err = %v, want ErrNoTrap", err)
	}
}

// TestRepair_OmitsEmptyStreams verifies that when stdout/stderr are empty the
// prompt omits their section headers, keeping the message focused on the
// available evidence.
func TestRepair_OmitsEmptyStreams(t *testing.T) {
	result := sandbox.SandboxResult{
		Trap: &sandbox.TrapInfo{Kind: sandbox.TrapKindTimeout, Message: "deadline exceeded"},
	}
	prompt, err := Repair("prog", result)
	if err != nil {
		t.Fatalf("Repair returned error: %v", err)
	}
	if strings.Contains(prompt.User, "## Captured stdout") {
		t.Errorf("prompt should omit empty stdout section; got:\n%s", prompt.User)
	}
	if strings.Contains(prompt.User, "## Captured stderr") {
		t.Errorf("prompt should omit empty stderr section; got:\n%s", prompt.User)
	}
	// The trap section is still present even when streams are empty.
	if !strings.Contains(prompt.User, "## Trap kind") {
		t.Errorf("prompt missing trap kind section; got:\n%s", prompt.User)
	}
}

// TestRepairPrompt_JSONRoundTrip ensures RepairPrompt serializes cleanly so a
// caller can log, persist, or forward it to its LLM client as JSON.
func TestRepairPrompt_JSONRoundTrip(t *testing.T) {
	result := mockTrapResult(sandbox.TrapKindUnreachable, "boom")
	prompt, err := Repair("program-source", result)
	if err != nil {
		t.Fatalf("Repair returned error: %v", err)
	}

	data, err := json.Marshal(prompt)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var got RepairPrompt
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.TrapKind != prompt.TrapKind {
		t.Errorf("TrapKind round-trip = %q, want %q", got.TrapKind, prompt.TrapKind)
	}
	if got.Program != prompt.Program {
		t.Errorf("Program round-trip = %q, want %q", got.Program, prompt.Program)
	}
	if got.User != prompt.User {
		t.Errorf("User round-trip mismatch")
	}
}
