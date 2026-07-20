package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestNormalizeContext(t *testing.T) {
	m := map[string]any{
		"int":   3.0,    // whole float64 → int64
		"float": 3.14,   // fractional stays
		"str":   "hello", // non-float stays
		"zero":  0.0,    // zero → int64
		"neg":   -5.0,   // negative whole → int64
	}
	normalizeContext(m)

	if v, ok := m["int"]; !ok || v != int64(3) {
		t.Errorf("int: got %T %v, want int64 3", v, v)
	}
	if v, ok := m["float"]; !ok || v != 3.14 {
		t.Errorf("float: got %T %v, want float64 3.14", v, v)
	}
	if v, ok := m["str"]; !ok || v != "hello" {
		t.Errorf("str: got %T %v, want string hello", v, v)
	}
	if v, ok := m["zero"]; !ok || v != int64(0) {
		t.Errorf("zero: got %T %v, want int64 0", v, v)
	}
	if v, ok := m["neg"]; !ok || v != int64(-5) {
		t.Errorf("neg: got %T %v, want int64 -5", v, v)
	}
}

// buildSymk builds the symk binary and returns its path.
func buildSymk(t *testing.T) string {
	t.Helper()
	tmpDir := t.TempDir()
	bin := filepath.Join(tmpDir, "symk")
	cmd := exec.Command("go", "build", "-o", bin, "github.com/WasmAgent/symkernel/cmd/symk")
	cmd.Dir = filepath.Join("..", "..")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	return bin
}

func TestVerifyCELWithExpr(t *testing.T) {
	bin := buildSymk(t)

	tests := []struct {
		name     string
		args     []string
		wantCode int
		wantOut  string
	}{
		{
			name:     "simple expression passes",
			args:     []string{"verify", "cel", "--expr", "1 + 1 == 2"},
			wantCode: 0,
			wantOut:  "PASS",
		},
		{
			name:     "false expression still passes eval",
			args:     []string{"verify", "cel", "--expr", "1 + 1 == 3"},
			wantCode: 0,
			wantOut:  "PASS",
		},
		{
			name:     "invalid syntax fails",
			args:     []string{"verify", "cel", "--expr", "???"},
			wantCode: 1,
			wantOut:  "FAIL",
		},
		{
			name:     "no expr flag fails",
			args:     []string{"verify", "cel"},
			wantCode: 1,
		},
		{
			name:     "unknown command fails",
			args:     []string{"foobar"},
			wantCode: 1,
		},
		{
			name:     "no args shows usage",
			args:     []string{},
			wantCode: 1,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command(bin, tc.args...)
			out, err := cmd.CombinedOutput()
			code := exitCode(err)
			if code != tc.wantCode {
				t.Errorf("exit code = %d, want %d; output: %s", code, tc.wantCode, string(out))
			}
			if tc.wantOut != "" && !strings.Contains(string(out), tc.wantOut) {
				t.Errorf("output missing %q; got:\n%s", tc.wantOut, string(out))
			}
		})
	}
}

func TestVerifyCELWithContextFile(t *testing.T) {
	bin := buildSymk(t)

	// Create a temp context file with {"age": 25}.
	tmpDir := t.TempDir()
	ctxFile := filepath.Join(tmpDir, "ctx.json")
	if err := os.WriteFile(ctxFile, []byte(`{"age": 25}`), 0644); err != nil {
		t.Fatalf("write context file: %v", err)
	}

	tests := []struct {
		name     string
		expr     string
		wantCode int
		wantOut  string
	}{
		{
			name:     "age > 18 passes",
			expr:     "age > 18",
			wantCode: 0,
			wantOut:  "PASS",
		},
		{
			name:     "age > 30 evaluates to false",
			expr:     "age > 30",
			wantCode: 0,
			wantOut:  "PASS",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command(bin, "verify", "cel", "--expr", tc.expr, "--context", ctxFile)
			out, err := cmd.CombinedOutput()
			code := exitCode(err)
			if code != tc.wantCode {
				t.Errorf("exit code = %d, want %d; output: %s", code, tc.wantCode, string(out))
			}
			if tc.wantOut != "" && !strings.Contains(string(out), tc.wantOut) {
				t.Errorf("output missing %q; got:\n%s", tc.wantOut, string(out))
			}
		})
	}
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	if exitErr, ok := err.(*exec.ExitError); ok {
		return exitErr.ExitCode()
	}
	return -1
}
