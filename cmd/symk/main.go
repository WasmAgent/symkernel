// symk is a CLI tool for local development against the symkernel
// verification kernel. It provides offline CEL evaluation using the
// embedded CEL evaluator.
//
// Usage:
//
//	symk verify cel --expr "input.age > 18" --context ctx.json
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"os"
	"time"

	"github.com/WasmAgent/symkernel/internal/cel"
)

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	switch os.Args[1] {
	case "verify":
		cmdVerify(os.Args[2:])
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n", os.Args[1])
		printUsage()
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Fprintln(os.Stderr, "symk — symkernel local development CLI")
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "Usage:")
	fmt.Fprintln(os.Stderr, "  symk verify cel --expr <expr> [--context <file>]")
}

// cmdVerify implements the "verify cel" subcommand. It parses a CEL
// expression and an optional JSON context file, evaluates the expression
// using the embedded CEL evaluator, and prints PASS/FAIL with the result.
func cmdVerify(args []string) {
	if len(args) < 2 || args[0] != "cel" {
		fmt.Fprintln(os.Stderr, "usage: symk verify cel --expr <expr> [--context <file>]")
		os.Exit(1)
	}

	fs := flag.NewFlagSet("verify cel", flag.ExitOnError)
	expr := fs.String("expr", "", "CEL expression to evaluate")
	contextFile := fs.String("context", "", "JSON file containing context variables")
	if err := fs.Parse(args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "error parsing flags: %v\n", err)
		os.Exit(1)
	}

	if *expr == "" {
		fmt.Fprintln(os.Stderr, "error: --expr is required")
		os.Exit(1)
	}

	var vars map[string]any
	if *contextFile != "" {
		data, err := os.ReadFile(*contextFile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error reading context file: %v\n", err)
			os.Exit(1)
		}
		if err := json.Unmarshal(data, &vars); err != nil {
			fmt.Fprintf(os.Stderr, "error parsing context JSON: %v\n", err)
			os.Exit(1)
		}
		// Normalise float64 to int64 for whole numbers (same as server handler).
		normalizeContext(vars)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	start := time.Now()
	result, err := cel.Evaluate(ctx, *expr, vars)
	elapsed := time.Since(start)

	if err != nil {
		fmt.Printf("FAIL  %s\n", elapsed.Round(time.Microsecond))
		fmt.Fprintf(os.Stderr, "  error: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("PASS  %s\n", elapsed.Round(time.Microsecond))
	fmt.Printf("  result: %v\n", result)
}

// normalizeContext converts JSON-decoded float64 values to int64 when they
// have no fractional part, matching the server-side normalization in
// internal/cel/evaluator.go.
func normalizeContext(m map[string]any) {
	for k, v := range m {
		if f, ok := v.(float64); ok {
			if f == math.Trunc(f) && !math.IsInf(f, 0) && !math.IsNaN(f) {
				m[k] = int64(f)
			}
		}
	}
}
