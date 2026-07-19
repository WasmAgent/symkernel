// Package repair assembles structured prompts for LLM-driven repair of
// programs that trapped during sandboxed execution. It does NOT call the LLM
// itself — the caller injects the returned RepairPrompt into its model of
// choice (e.g. an OpenAI or Anthropic chat completion). Keeping the model
// invocation out of this package means repair is fully deterministic and
// trivially testable without network access.
package repair

import (
	"errors"
	"fmt"
	"strings"

	"github.com/WasmAgent/symkernel/internal/sandbox"
)

// ErrEmptyProgram is returned by Repair when the supplied program string is
// blank — there is nothing for an LLM to edit.
var ErrEmptyProgram = errors.New("repair: program is empty")

// ErrNoTrap is returned by Repair when the supplied SandboxResult carries no
// trap information. A clean exit (no trap) is not a repair signal, so Repair
// refuses rather than emitting a misleading "fix this" prompt.
var ErrNoTrap = errors.New("repair: trap result has no trap to repair")

// systemPrompt is the fixed instruction message that frames the repair task
// for the LLM. It sets the model's role, the inputs it will receive, and the
// required output format (the corrected program only, no prose).
const systemPrompt = `You are a WebAssembly repair agent. You receive a program that trapped during sandboxed execution along with the categorized trap kind, the runtime error message, and any captured stdout/stderr. Diagnose the trap and return a corrected program that avoids it while preserving the original intent. Output ONLY the fixed program source — no prose, no fences, no explanation.`

// RepairPrompt is the structured prompt handed to an LLM to produce a fixed
// version of a program whose sandboxed execution trapped. The System and User
// fields are model-ready chat messages; the remaining fields expose the raw
// inputs for callers that compose their own prompts.
type RepairPrompt struct {
	// System is the instruction (system-role) message framing the task.
	System string `json:"system"`

	// User is the assembled user-role message combining the program, trap
	// details, and captured I/O into a single coherent instruction.
	User string `json:"user"`

	// Program is the original (failing) program source, embedded verbatim
	// so the model can edit it directly.
	Program string `json:"program"`

	// TrapKind is the categorized trap kind from SandboxResult.Trap.Kind
	// (e.g. "unreachable", "memory_oob", "stack_overflow", "timeout").
	TrapKind string `json:"trap_kind"`

	// TrapMessage is the verbatim runtime error message produced by the
	// sandbox runtime, which may include the wasm stack trace.
	TrapMessage string `json:"trap_message"`

	// Stdout is the captured stdout from the failing run.
	Stdout string `json:"stdout"`

	// Stderr is the captured stderr from the failing run.
	Stderr string `json:"stderr"`

	// ExitCode is the exit code observed on the failing run.
	ExitCode int32 `json:"exit_code"`
}

// Repair assembles a structured prompt from a trapped program and its
// SandboxResult for an LLM-driven fix. It does NOT call any LLM — the caller
// injects the returned RepairPrompt into its own model invocation.
//
// Parameters:
//   - program: the original program source (e.g. WAT source, or base64 of the
//     Wasm module) that trapped during sandboxed execution.
//   - trapResult: the SandboxResult returned by sandbox.Run for the failing
//     execution. Its Trap field must be non-nil; if it is nil, Repair returns
//     ErrNoTrap because there is no trap to diagnose.
func Repair(program string, trapResult sandbox.SandboxResult) (RepairPrompt, error) {
	if strings.TrimSpace(program) == "" {
		return RepairPrompt{}, ErrEmptyProgram
	}
	if trapResult.Trap == nil {
		return RepairPrompt{}, ErrNoTrap
	}

	p := RepairPrompt{
		System:      systemPrompt,
		Program:     program,
		TrapKind:    string(trapResult.Trap.Kind),
		TrapMessage: trapResult.Trap.Message,
		Stdout:      string(trapResult.Stdout),
		Stderr:      string(trapResult.Stderr),
		ExitCode:    trapResult.ExitCode,
	}
	p.User = assembleUserPrompt(p)
	return p, nil
}

// assembleUserPrompt combines the structured fields into a single user-role
// instruction message. Captured stdout/stderr sections are only included when
// present, keeping the prompt focused on the available evidence.
func assembleUserPrompt(p RepairPrompt) string {
	var b strings.Builder
	b.WriteString("The following program trapped during sandboxed execution. Diagnose and fix it.\n\n")
	fmt.Fprintf(&b, "## Trap kind\n%s\n\n", p.TrapKind)
	if p.TrapMessage != "" {
		fmt.Fprintf(&b, "## Runtime error\n%s\n\n", p.TrapMessage)
	}
	fmt.Fprintf(&b, "## Exit code\n%d\n\n", p.ExitCode)
	if p.Stdout != "" {
		fmt.Fprintf(&b, "## Captured stdout\n%s\n\n", p.Stdout)
	}
	if p.Stderr != "" {
		fmt.Fprintf(&b, "## Captured stderr\n%s\n\n", p.Stderr)
	}
	b.WriteString("## Program to fix\n")
	b.WriteString(p.Program)
	b.WriteString("\n")
	return b.String()
}
