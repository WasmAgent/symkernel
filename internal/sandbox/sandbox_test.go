package sandbox

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// helloWorldWasm is a minimal Wasm module that writes "hello\n" to stdout
// via WASI fd_write and exits cleanly.
var helloWorldWasm = []byte{
	0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00, 0x01, 0x0c, 0x02, 0x60,
	0x04, 0x7f, 0x7f, 0x7f, 0x7f, 0x01, 0x7f, 0x60, 0x00, 0x00, 0x02, 0x23,
	0x01, 0x16, 0x77, 0x61, 0x73, 0x69, 0x5f, 0x73, 0x6e, 0x61, 0x70, 0x73,
	0x68, 0x6f, 0x74, 0x5f, 0x70, 0x72, 0x65, 0x76, 0x69, 0x65, 0x77, 0x31,
	0x08, 0x66, 0x64, 0x5f, 0x77, 0x72, 0x69, 0x74, 0x65, 0x00, 0x00, 0x03,
	0x02, 0x01, 0x01, 0x05, 0x03, 0x01, 0x00, 0x01, 0x07, 0x13, 0x02, 0x06,
	0x6d, 0x65, 0x6d, 0x6f, 0x72, 0x79, 0x02, 0x00, 0x06, 0x5f, 0x73, 0x74,
	0x61, 0x72, 0x74, 0x00, 0x01, 0x0a, 0x1d, 0x01, 0x1b, 0x00, 0x41, 0x00,
	0x41, 0x20, 0x36, 0x02, 0x00, 0x41, 0x04, 0x41, 0x06, 0x36, 0x02, 0x00,
	0x41, 0x01, 0x41, 0x00, 0x41, 0x01, 0x41, 0x10, 0x10, 0x00, 0x1a, 0x0b,
	0x0b, 0x0c, 0x01, 0x00, 0x41, 0x20, 0x0b, 0x06, 0x68, 0x65, 0x6c, 0x6c,
	0x6f, 0x0a,
}

// unreachableWasm executes the "unreachable" instruction in _start.
var unreachableWasm = []byte{
	0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00, // magic + version
	0x01, 0x04, 0x01, 0x60, 0x00, 0x00, // type[0] = () -> ()
	0x03, 0x02, 0x01, 0x00, // func[0] = type[0]
	0x07, 0x0a, 0x01, 0x06, 0x5f, 0x73, 0x74, 0x61, 0x72, 0x74, 0x00, 0x00, // export "_start" = func 0
	0x0a, 0x05, 0x01, 0x03, 0x00, 0x00, 0x0b, // code: unreachable; end
}

// oobWasm loads 4 bytes from address 0 + offset 100000 of a single 64 KiB
// page, which is out of bounds and traps.
var oobWasm = []byte{
	0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00,
	0x01, 0x04, 0x01, 0x60, 0x00, 0x00, // type[0] = () -> ()
	0x03, 0x02, 0x01, 0x00, // func[0] = type[0]
	0x05, 0x03, 0x01, 0x00, 0x01, // memory[0] min=1 page
	0x07, 0x0a, 0x01, 0x06, 0x5f, 0x73, 0x74, 0x61, 0x72, 0x74, 0x00, 0x00,
	0x0a, 0x0c, 0x01, 0x0a, 0x00, // code: locals=0
	0x41, 0x00, // i32.const 0
	0x28, 0x02, 0xa0, 0x8d, 0x06, // i32.load align=2 offset=100000
	0x1a, // drop
	0x0b, // end
}

// stackOverflowWasm is a function that calls itself, exhausting the call stack.
var stackOverflowWasm = []byte{
	0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00,
	0x01, 0x04, 0x01, 0x60, 0x00, 0x00, // type[0] = () -> ()
	0x03, 0x02, 0x01, 0x00, // func[0] = type[0]
	0x07, 0x0a, 0x01, 0x06, 0x5f, 0x73, 0x74, 0x61, 0x72, 0x74, 0x00, 0x00,
	0x0a, 0x06, 0x01, 0x04, 0x00, 0x10, 0x00, 0x0b, // code: locals=0, call 0, end
}

// infiniteLoopWasm loops forever via `loop ... br 0 ... end`, used to exercise
// the sandbox timeout path.
var infiniteLoopWasm = []byte{
	0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00,
	0x01, 0x04, 0x01, 0x60, 0x00, 0x00, // type[0] = () -> ()
	0x03, 0x02, 0x01, 0x00, // func[0] = type[0]
	0x07, 0x0a, 0x01, 0x06, 0x5f, 0x73, 0x74, 0x61, 0x72, 0x74, 0x00, 0x00,
	0x0a, 0x09, 0x01, 0x07, 0x00, // code: locals=0
	0x03, 0x40, // loop (empty blocktype)
	0x0c, 0x00, // br 0 (branch to loop start)
	0x0b, // end loop
	0x0b, // end func
}

func TestRun_InvalidBase64(t *testing.T) {
	_, err := Run("!!!not-base64!!!", nil, 1, 0)
	if err == nil {
		t.Fatal("expected error for invalid base64")
	}
	if !strings.Contains(err.Error(), "base64 decode") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRun_InvalidWasm(t *testing.T) {
	b64 := base64.StdEncoding.EncodeToString([]byte("not a wasm module"))
	_, err := Run(b64, nil, 1, 0)
	if err == nil {
		t.Fatal("expected error for invalid wasm binary")
	}
}

func TestRun_HelloWorld(t *testing.T) {
	b64 := base64.StdEncoding.EncodeToString(helloWorldWasm)
	result, err := Run(b64, nil, 8, 0) // 8 MB to cover WASI internal memory
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !bytes.Contains(result.Stdout, []byte("hello\n")) {
		t.Fatalf("expected stdout to contain 'hello\\n', got %q", string(result.Stdout))
	}
	if result.ExitCode != 0 {
		t.Fatalf("expected exit code 0, got %d", result.ExitCode)
	}
}

func TestRun_WithArgs(t *testing.T) {
	b64 := base64.StdEncoding.EncodeToString(helloWorldWasm)
	result, err := Run(b64, map[string]any{"name": "test", "mode": "sandbox"}, 8, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// The hello-world module doesn't use args, but we verify it doesn't crash.
	if result.ExitCode != 0 {
		t.Fatalf("expected exit code 0, got %d", result.ExitCode)
	}
}

func TestRun_Timeout(t *testing.T) {
	b64 := base64.StdEncoding.EncodeToString(helloWorldWasm)
	// Very short timeout — the module should still complete as it's tiny.
	result, err := Run(b64, nil, 8, 5000)
	if err != nil {
		t.Fatalf("unexpected error with 5s timeout: %v", err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("expected exit code 0, got %d", result.ExitCode)
	}
}

func TestRun_LargeMemoryLimit(t *testing.T) {
	b64 := base64.StdEncoding.EncodeToString(helloWorldWasm)
	result, err := Run(b64, nil, 256, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("expected exit code 0, got %d", result.ExitCode)
	}
}

func TestMemLimitMBToPages(t *testing.T) {
	tests := []struct {
		mb     int
		expect uint32
	}{
		{0, 1},
		{-1, 1},
		{1, 16},       // 1 MB = 16 pages
		{64, 1024},    // 64 MB = 1024 pages
		{256, 4096},   // 256 MB = 4096 pages
	}
	for _, tt := range tests {
		got := memLimitMBToPages(tt.mb)
		if got != tt.expect {
			t.Errorf("memLimitMBToPages(%d) = %d, want %d", tt.mb, got, tt.expect)
		}
	}
}

func TestBuildArgs(t *testing.T) {
	got := buildArgs(map[string]any{"key": "value", "num": 42})
	if len(got) != 2 {
		t.Fatalf("expected 2 args, got %d", len(got))
	}
	fmt.Println(got) // just ensure no crash
}

func TestBuildArgs_Nil(t *testing.T) {
	got := buildArgs(nil)
	if got != nil {
		t.Fatalf("expected nil for nil args, got %v", got)
	}
}

func TestLimitedBuffer(t *testing.T) {
	var buf limitedBuffer
	buf.maxCap = 10

	n, err := buf.Write([]byte("hello"))
	if err != nil || n != 5 {
		t.Fatalf("Write(hello) = %d, %v; want 5, nil", n, err)
	}

	n, err = buf.Write([]byte(" world!!"))
	if err != nil || n != 8 {
		t.Fatalf("Write(world!!) = %d, %v; want 8, nil", n, err)
	}

	// Buffer should be capped at 10 bytes (5 from first, 5 from second).
	if got := string(buf.Bytes()); got != "hello worl" {
		t.Fatalf("Bytes() = %q, want %q", got, "hello worl")
	}
}

func TestLimitedBuffer_Unlimited(t *testing.T) {
	var buf limitedBuffer
	data := strings.Repeat("x", 1000)
	n, err := buf.Write([]byte(data))
	if err != nil || n != 1000 {
		t.Fatalf("Write = %d, %v; want 1000, nil", n, err)
	}
	if len(buf.Bytes()) != 1000 {
		t.Fatalf("expected 1000 bytes, got %d", len(buf.Bytes()))
	}
}

// TestTrapInfo_JSON verifies the structured error protocol payload serializes
// to the exact {"kind":"<trap_kind>","message":"..."} shape required by the
// milestone bullet.
func TestTrapInfo_JSON(t *testing.T) {
	got, err := json.Marshal(TrapInfo{Kind: TrapKindUnreachable, Message: "boom"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := `{"kind":"unreachable","message":"boom"}`
	if string(got) != want {
		t.Errorf("json = %s, want %s", string(got), want)
	}
}

// TestTrapFromErr is the table-driven mapping test covering every trap kind
// plus the non-trap error cases that must leave Trap unset.
func TestTrapFromErr(t *testing.T) {
	tests := []struct {
		name      string
		err       error
		wantKind  TrapKind
		wantInMsg string
	}{
		{name: "nil", err: nil},
		{
			name:      "unreachable",
			err:       errors.New("module[] function[_start] failed: wasm error: unreachable\nwasm stack trace:"),
			wantKind:  TrapKindUnreachable,
			wantInMsg: "unreachable",
		},
		{
			name:      "memory_oob",
			err:       errors.New("wasm error: out of bounds memory access"),
			wantKind:  TrapKindMemoryOOB,
			wantInMsg: "out of bounds memory access",
		},
		{
			name:      "stack_overflow",
			err:       errors.New("wasm error: stack overflow"),
			wantKind:  TrapKindStackOverflow,
			wantInMsg: "stack overflow",
		},
		{
			name:      "timeout",
			err:       fmt.Errorf("module closed with %w", context.DeadlineExceeded),
			wantKind:  TrapKindTimeout,
			wantInMsg: "deadline exceeded",
		},
		{name: "non_trap_proc_exit", err: errors.New("module closed with exit_code(42)")},
		{name: "non_trap_invalid_module", err: errors.New("invalid magic number")},
		{name: "non_trap_base64_decode", err: errors.New("sandbox: base64 decode: illegal base64 data")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := trapFromErr(tt.err)
			wantTrap := tt.wantKind != ""
			if !wantTrap {
				if got != nil {
					t.Fatalf("expected no trap, got %+v", got)
				}
				return
			}
			if got == nil {
				t.Fatalf("expected trap kind %q, got nil", tt.wantKind)
			}
			if got.Kind != tt.wantKind {
				t.Errorf("kind = %q, want %q", got.Kind, tt.wantKind)
			}
			if tt.wantInMsg != "" && !strings.Contains(got.Message, tt.wantInMsg) {
				t.Errorf("message %q does not contain %q", got.Message, tt.wantInMsg)
			}
		})
	}
}

// runTrapModule is a helper that executes a raw wasm module and asserts a trap
// of the given kind was produced.
func runTrapModule(t *testing.T, wasm []byte, memLimitMB, timeoutMs int, wantKind TrapKind) {
	t.Helper()
	b64 := base64.StdEncoding.EncodeToString(wasm)
	result, err := Run(b64, nil, memLimitMB, timeoutMs)
	if err == nil {
		t.Fatalf("expected error for %q trap, got nil", wantKind)
	}
	if result.Trap == nil {
		t.Fatalf("expected Trap to be set (%q), got nil; err=%v", wantKind, err)
	}
	if result.Trap.Kind != wantKind {
		t.Fatalf("trap kind = %q (msg=%q), want %q", result.Trap.Kind, result.Trap.Message, wantKind)
	}
}

func TestRun_TrapUnreachable(t *testing.T) {
	runTrapModule(t, unreachableWasm, 8, 0, TrapKindUnreachable)
}

func TestRun_TrapMemoryOOB(t *testing.T) {
	runTrapModule(t, oobWasm, 8, 0, TrapKindMemoryOOB)
}

func TestRun_TrapStackOverflow(t *testing.T) {
	runTrapModule(t, stackOverflowWasm, 8, 0, TrapKindStackOverflow)
}

func TestRun_TrapTimeout(t *testing.T) {
	// The infinite-loop guest must be killed by the 200 ms deadline and
	// surfaced as a timeout trap rather than running forever.
	runTrapModule(t, infiniteLoopWasm, 8, 200, TrapKindTimeout)
}

// TestRun_HelloWorldHasNoTrap guards the happy path: a clean run leaves Trap
// unset so callers can distinguish traps from normal execution.
func TestRun_HelloWorldHasNoTrap(t *testing.T) {
	b64 := base64.StdEncoding.EncodeToString(helloWorldWasm)
	result, err := Run(b64, nil, 8, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Trap != nil {
		t.Fatalf("expected no trap on clean run, got %+v", result.Trap)
	}
}
