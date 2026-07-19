package sandbox

import (
	"bytes"
	"encoding/base64"
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
