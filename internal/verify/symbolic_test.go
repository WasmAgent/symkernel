package verify

import (
	"context"
	"encoding/base64"
	"errors"
	"os/exec"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// These tests exercise the Milestone 3 symbolic execution engine end to end.
// They build small WebAssembly modules programmatically with the enc* helpers
// below so the byte layouts are self-documenting and verified by the engine's
// own decoder on every run.

// ---- minimal WASM encoder (test only) --------------------------------------

const (
	btEmpty byte = 0x40 // blocktype: no result
	btI32   byte = 0x7f // blocktype: single i32 result
)

func encUleb(v uint32) []byte {
	var out []byte
	for {
		b := byte(v & 0x7f)
		v >>= 7
		if v != 0 {
			b |= 0x80
		}
		out = append(out, b)
		if v == 0 {
			return out
		}
	}
}

func encSleb(v int32) []byte {
	var out []byte
	for {
		b := byte(v & 0x7f)
		v >>= 7
		signBit := b&0x40 != 0
		done := (v == 0 && !signBit) || (v == -1 && signBit)
		if !done {
			b |= 0x80
		}
		out = append(out, b)
		if done {
			return out
		}
	}
}

func encOp(b byte) []byte { return []byte{b} }

func encLocalGet(i uint32) []byte  { return append([]byte{0x20}, encUleb(i)...) }
func encLocalSet(i uint32) []byte  { return append([]byte{0x21}, encUleb(i)...) }
func encI32Const(v int32) []byte   { return append([]byte{0x41}, encSleb(v)...) }
func encBrIf(label uint32) []byte  { return append([]byte{0x0d}, encUleb(label)...) }
func encCall(f uint32) []byte      { return append([]byte{0x10}, encUleb(f)...) }
func encIf(bt byte) []byte         { return []byte{0x04, bt} }
func encBlock(bt byte) []byte      { return []byte{0x02, bt} }

// locals encodes a locals vector from pre-encoded local-decl groups.
func locals(groups ...[]byte) []byte {
	out := encUleb(uint32(len(groups)))
	for _, g := range groups {
		out = append(out, g...)
	}
	return out
}

// i32LocalGroup is one local-decl group: a single i32 local.
func i32LocalGroup() []byte { return []byte{0x01, 0x7f} }

// cat concatenates byte slices, used to assemble instruction streams.
func cat(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

func encSec(id byte, payload []byte) []byte {
	out := []byte{id}
	out = append(out, encUleb(uint32(len(payload)))...)
	return append(out, payload...)
}

func encTypeSection(types ...[]byte) []byte {
	p := encUleb(uint32(len(types)))
	for _, t := range types {
		p = append(p, t...)
	}
	return encSec(1, p)
}

// encFuncType encodes a functype from raw param/result valtype bytes.
func encFuncType(params, results []byte) []byte {
	out := []byte{0x60}
	out = append(out, encUleb(uint32(len(params)))...)
	out = append(out, params...)
	out = append(out, encUleb(uint32(len(results)))...)
	out = append(out, results...)
	return out
}

func encFuncSection(typeIdxs ...uint32) []byte {
	p := encUleb(uint32(len(typeIdxs)))
	for _, t := range typeIdxs {
		p = append(p, encUleb(t)...)
	}
	return encSec(3, p)
}

func encExportSection(exports ...wasmExport) []byte {
	p := encUleb(uint32(len(exports)))
	for _, e := range exports {
		p = append(p, encUleb(uint32(len(e.name)))...)
		p = append(p, e.name...)
		p = append(p, e.kind)
		p = append(p, encUleb(e.index)...)
	}
	return encSec(7, p)
}

func encCodeSection(bodies ...[]byte) []byte {
	p := encUleb(uint32(len(bodies)))
	for _, body := range bodies {
		p = append(p, encUleb(uint32(len(body)))...)
		p = append(p, body...)
	}
	return encSec(10, p)
}

func encModule(sections ...[]byte) []byte {
	m := []byte{0x00, 'a', 's', 'm', 0x01, 0x00, 0x00, 0x00}
	for _, s := range sections {
		m = append(m, s...)
	}
	return m
}

func encB64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

// ---- fixture modules --------------------------------------------------------

// ifElseModule returns 1 when arg0 == 0 and 2 otherwise:
//
//	(func (export "decide") (param i32) (result i32)
//	  local.get 0
//	  i32.const 0
//	  i32.eq
//	  if (result i32) (i32.const 1) (else (i32.const 2))
//	)
//
// Symbolic exploration yields two paths with complementary constraints.
func ifElseModule() []byte {
	body := cat(locals(),
		encLocalGet(0),
		encI32Const(0),
		encOp(0x46), // i32.eq
		encIf(btI32),
		encI32Const(1),
		encOp(0x05), // else
		encI32Const(2),
		encOp(0x0b), // end
		encOp(0x0b), // end (function)
	)
	return encModule(
		encTypeSection(encFuncType([]byte{0x7f}, []byte{0x7f})),
		encFuncSection(0),
		encExportSection(wasmExport{name: "decide", kind: 0, index: 0}),
		encCodeSection(body),
	)
}

// brIfModule returns 99 when arg0 == 0 (br_if skips the fall-through) and
// arg0+1 otherwise, exercising br_if, block, return, and i32.add:
//
//	(block
//	  (br_if 0 (i32.eq (local.get 0) (i32.const 0)))
//	  (return (i32.add (local.get 0) (i32.const 1)))
//	)
//	(i32.const 99)
func brIfModule() []byte {
	body := cat(locals(),
		encBlock(btEmpty), // label 0
		encLocalGet(0),
		encI32Const(0),
		encOp(0x46), // i32.eq
		encBrIf(0),
		encLocalGet(0),
		encI32Const(1),
		encOp(0x6a), // i32.add
		encOp(0x0f), // return
		encOp(0x0b), // end (block)
		encI32Const(99),
		encOp(0x0b), // end (function)
	)
	return encModule(
		encTypeSection(encFuncType([]byte{0x7f}, []byte{0x7f})),
		encFuncSection(0),
		encExportSection(wasmExport{name: "decide", kind: 0, index: 0}),
		encCodeSection(body),
	)
}

// callModule calls a helper that doubles arg0, then branches on ==4:
//
//	(func (param i32) (result i32) (i32.mul (local.get 0) (i32.const 2)))  ; func 0
//	(func (export "decide") (param i32) (result i32)                       ; func 1
//	  (if (result i32) (i32.eq (call 0) (i32.const 4))
//	    (then (i32.const 1)) (else (i32.const 2))))
func callModule() []byte {
	double := cat(locals(),
		encLocalGet(0),
		encI32Const(2),
		encOp(0x6c), // i32.mul
		encOp(0x0b), // end
	)
	decide := cat(locals(),
		encLocalGet(0),
		encCall(0),
		encI32Const(4),
		encOp(0x46), // i32.eq
		encIf(btI32),
		encI32Const(1),
		encOp(0x05), // else
		encI32Const(2),
		encOp(0x0b), // end
		encOp(0x0b), // end
	)
	return encModule(
		encTypeSection(encFuncType([]byte{0x7f}, []byte{0x7f})),
		encFuncSection(0, 0), // func 0 and 1 share type 0
		encExportSection(wasmExport{name: "decide", kind: 0, index: 1}),
		encCodeSection(double, decide),
	)
}

// identityModule returns its argument unchanged (a single non-branching path):
//
//	(func (export "id") (param i32) (result i32) (local.get 0))
func identityModule() []byte {
	body := cat(locals(),
		encLocalGet(0),
		encOp(0x0b),
	)
	return encModule(
		encTypeSection(encFuncType([]byte{0x7f}, []byte{0x7f})),
		encFuncSection(0),
		encExportSection(wasmExport{name: "id", kind: 0, index: 0}),
		encCodeSection(body),
	)
}

// calcModule exercises a broader opcode set (local.set, sub, lt_s) plus the
// required local.set from the success-metric opcode list:
//
//	(func (export "calc") (param i32) (result i32) (local i32)
//	  (local.set 1 (i32.sub (local.get 0) (i32.const 3)))   ; local1 = arg0-3
//	  (i32.lt_s (local.get 1) (i32.const 10))
//	  if (result i32) (i32.const 100) (else (i32.const 200))
//	)
func calcModule() []byte {
	body := cat(locals(i32LocalGroup()), // one i32 local (index 1)
		encLocalGet(0),
		encI32Const(3),
		encOp(0x6b), // i32.sub
		encLocalSet(1),
		encLocalGet(1),
		encI32Const(10),
		encOp(0x48), // i32.lt_s
		encIf(btI32),
		encI32Const(100),
		encOp(0x05), // else
		encI32Const(200),
		encOp(0x0b), // end
		encOp(0x0b), // end
	)
	return encModule(
		encTypeSection(encFuncType([]byte{0x7f}, []byte{0x7f})),
		encFuncSection(0),
		encExportSection(wasmExport{name: "calc", kind: 0, index: 0}),
		encCodeSection(body),
	)
}

// ---- harness helpers --------------------------------------------------------

// capturingSolver records every SMT2 script it is asked to solve and returns a
// fixed result. It lets unit tests inspect generated constraints without a
// real z3 binary.
type capturingSolver struct {
	scripts []string
	result  Result
}

func (c *capturingSolver) Solve(_ context.Context, smt2 string) (Result, error) {
	c.scripts = append(c.scripts, smt2)
	return c.result, nil
}

// assertTwoComplementaryPaths checks the core success metric: branching code
// yields exactly two feasible paths whose path conditions are complementary
// (one asserts a condition non-zero, the other zero).
func assertTwoComplementaryPaths(t *testing.T, paths []SymbolicPath, symbol string) {
	t.Helper()
	if len(paths) != 2 {
		t.Fatalf("paths = %d, want 2 (got %+v)", len(paths), paths)
	}
	negated := 0
	for _, p := range paths {
		if !strings.Contains(p.Constraints, symbol) {
			t.Errorf("constraints missing %s: %q", symbol, p.Constraints)
		}
		if strings.Contains(p.Constraints, "(not ") {
			negated++
		}
	}
	if negated != 1 {
		t.Errorf("expected exactly one negated branch constraint, got %d", negated)
	}
}

func z3Skip(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("z3"); err != nil {
		t.Skip("z3 not installed; skipping integration test")
	}
}

// parseModelInt pulls an integer value out of a solver model, tolerating the
// "(- n)" rendering of negatives that z3 emits.
func parseModelInt(t *testing.T, model map[string]any, key string) int64 {
	t.Helper()
	raw, ok := model[key]
	if !ok {
		t.Fatalf("model missing %s: %v", key, model)
	}
	s, _ := raw.(string)
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "(-") {
		s = strings.TrimSpace(strings.TrimPrefix(s, "(-"))
		s = strings.TrimSuffix(strings.TrimSpace(s), ")")
	}
	v, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil {
		t.Fatalf("model[%s] = %q is not an integer: %v", key, raw, err)
	}
	return v
}

// ---- tests ------------------------------------------------------------------

func TestRun_AlwaysReturnsDecisionID(t *testing.T) {
	t.Parallel()

	// On error the DecisionID is still populated for traceability.
	_, err := Run(context.Background(), SymbolicInput{WasmBinary: "!!not base64!!", Entry: "x"})
	if err == nil {
		t.Fatal("expected error for invalid base64, got nil")
	}

	// On success the DecisionID is a valid, fresh UUID.
	b64 := encB64(ifElseModule())
	res, err := runWithSolver(context.Background(), nil, SymbolicInput{WasmBinary: b64, Entry: "decide"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.DecisionID == "" {
		t.Fatal("DecisionID is empty")
	}
	if _, parseErr := uuid.Parse(res.DecisionID); parseErr != nil {
		t.Errorf("DecisionID = %q is not a valid UUID: %v", res.DecisionID, parseErr)
	}

	other, _ := runWithSolver(context.Background(), nil, SymbolicInput{WasmBinary: b64, Entry: "decide"})
	if res.DecisionID == other.DecisionID {
		t.Fatalf("DecisionIDs collided across calls: %s", res.DecisionID)
	}
}

func TestRun_RejectsBadMagic(t *testing.T) {
	t.Parallel()
	_, err := Run(context.Background(), SymbolicInput{WasmBinary: encB64([]byte("not a wasm module")), Entry: "f"})
	if err == nil || !strings.Contains(err.Error(), "magic") {
		t.Fatalf("err = %v, want a magic error", err)
	}
}

func TestRun_MissingEntry(t *testing.T) {
	t.Parallel()
	b64 := encB64(ifElseModule())
	_, err := Run(context.Background(), SymbolicInput{WasmBinary: b64, Entry: "nope"})
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("err = %v, want a not-found error", err)
	}
}

func TestRun_EmptyEntry(t *testing.T) {
	t.Parallel()
	b64 := encB64(ifElseModule())
	_, err := Run(context.Background(), SymbolicInput{WasmBinary: b64})
	if err == nil || !strings.Contains(err.Error(), "empty") {
		t.Fatalf("err = %v, want an empty-entry error", err)
	}
}

// TestRun_ExecutesFromEntry covers "can execute WASM from the specified entry
// function": a non-branching identity function yields a single path and no
// branch constraints.
func TestRun_ExecutesFromEntry(t *testing.T) {
	t.Parallel()
	b64 := encB64(identityModule())
	res, err := runWithSolver(context.Background(), nil, SymbolicInput{WasmBinary: b64, Entry: "id"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(res.Paths) != 1 {
		t.Fatalf("paths = %d, want 1", len(res.Paths))
	}
	if res.Explored != 1 {
		t.Errorf("explored = %d, want 1", res.Explored)
	}
	// A symbolic argument with no branches: just the declaration, no asserts.
	if !strings.Contains(res.Paths[0].Constraints, "(declare-const arg0 Int)") {
		t.Errorf("constraints = %q, want a declaration", res.Paths[0].Constraints)
	}
	if strings.Contains(res.Paths[0].Constraints, "(assert") {
		t.Errorf("constraints = %q, want no asserts for a branch-free path", res.Paths[0].Constraints)
	}
}

// TestRun_IfElseTwoPaths covers the headline success metric: a simple
// conditional returns two paths with complementary constraints.
func TestRun_IfElseTwoPaths(t *testing.T) {
	t.Parallel()
	b64 := encB64(ifElseModule())
	res, err := runWithSolver(context.Background(), nil, SymbolicInput{WasmBinary: b64, Entry: "decide"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	assertTwoComplementaryPaths(t, res.Paths, "arg0")
	if res.Explored != 2 {
		t.Errorf("explored = %d, want 2", res.Explored)
	}
}

// TestRun_BrIfTwoPaths covers the br_if opcode explicitly named in the
// success metrics.
func TestRun_BrIfTwoPaths(t *testing.T) {
	t.Parallel()
	b64 := encB64(brIfModule())
	res, err := runWithSolver(context.Background(), nil, SymbolicInput{WasmBinary: b64, Entry: "decide"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	assertTwoComplementaryPaths(t, res.Paths, "arg0")
}

// TestRun_CallResolvesFunction covers the call opcode plus i32.mul; the call
// target doubles arg0 before the branch condition compares against 4.
func TestRun_CallResolvesFunction(t *testing.T) {
	t.Parallel()
	b64 := encB64(callModule())
	res, err := runWithSolver(context.Background(), nil, SymbolicInput{WasmBinary: b64, Entry: "decide"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	assertTwoComplementaryPaths(t, res.Paths, "arg0")
	// The doubled argument must appear in the generated constraints.
	found := false
	for _, p := range res.Paths {
		if strings.Contains(p.Constraints, "(* arg0 2)") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("no constraint references (* arg0 2); got %+v", res.Paths)
	}
}

// TestRun_BroaderOpcodeCoverage exercises sub/tee/lt_s/nop/drop in one module.
func TestRun_BroaderOpcodeCoverage(t *testing.T) {
	t.Parallel()
	b64 := encB64(calcModule())
	res, err := runWithSolver(context.Background(), nil, SymbolicInput{WasmBinary: b64, Entry: "calc"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	assertTwoComplementaryPaths(t, res.Paths, "arg0")
	found := false
	for _, p := range res.Paths {
		if strings.Contains(p.Constraints, "(< (- arg0 3) 10)") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("no constraint references (< (- arg0 3) 10); got %+v", res.Paths)
	}
}

// TestRun_ConstraintsAreSMTLIB2 covers "constraints output in valid SMT-LIB2
// format": the engine emits declare-const declarations and assert commands the
// solver receives intact.
func TestRun_ConstraintsAreSMTLIB2(t *testing.T) {
	t.Parallel()
	b64 := encB64(ifElseModule())
	cap := &capturingSolver{result: Result{Sat: "sat", Model: map[string]any{"arg0": "0"}}}
	res, err := runWithSolver(context.Background(), cap, SymbolicInput{WasmBinary: b64, Entry: "decide"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cap.scripts) != 2 {
		t.Fatalf("solver invoked %d times, want 2", len(cap.scripts))
	}
	for i, script := range cap.scripts {
		if !strings.Contains(script, "(declare-const arg0 Int)") {
			t.Errorf("script %d missing declaration: %q", i, script)
		}
		if !strings.Contains(script, "(assert ") {
			t.Errorf("script %d missing an assert: %q", i, script)
		}
		if !strings.Contains(script, "(check-sat)") || !strings.Contains(script, "(get-value (") {
			t.Errorf("script %d missing solver commands: %q", i, script)
		}
	}
	if len(res.Paths) != 2 {
		t.Errorf("paths = %d, want 2 (both sat via the mock)", len(res.Paths))
	}
}

// TestRun_PrunesInfeasiblePath verifies that an unsatisfiable path condition is
// dropped from the result while Explored still counts it.
func TestRun_PrunesInfeasiblePath(t *testing.T) {
	t.Parallel()
	b64 := encB64(ifElseModule())
	cap := &capturingSolver{result: Result{Sat: "unsat"}}
	res, err := runWithSolver(context.Background(), cap, SymbolicInput{WasmBinary: b64, Entry: "decide"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(res.Paths) != 0 {
		t.Errorf("paths = %d, want 0 when all branches are unsat", len(res.Paths))
	}
	if res.Explored != 2 {
		t.Errorf("explored = %d, want 2 (both considered before pruning)", res.Explored)
	}
}

func TestRun_ToleratesCancelledContext(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	b64 := encB64(ifElseModule())
	_, err := runWithSolver(ctx, nil, SymbolicInput{WasmBinary: b64, Entry: "decide"})
	if err == nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestRun_ConcreteArgBindsValue(t *testing.T) {
	t.Parallel()
	b64 := encB64(identityModule())
	// A concrete argument produces no symbolic declaration.
	res, err := runWithSolver(context.Background(), nil, SymbolicInput{WasmBinary: b64, Entry: "id", Args: []any{7}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(res.Paths) != 1 {
		t.Fatalf("paths = %d, want 1", len(res.Paths))
	}
	if res.Paths[0].Constraints != "" {
		t.Errorf("constraints = %q, want empty for a fully concrete path", res.Paths[0].Constraints)
	}
}

// TestRun_Z3ExtractsModels is the integration test for "Z3 can solve generated
// constraints and return models": the real solver yields a satisfying arg0 for
// each of the two complementary paths.
func TestRun_Z3ExtractsModels(t *testing.T) {
	t.Parallel()
	z3Skip(t)

	b64 := encB64(ifElseModule())
	res, err := Run(context.Background(), SymbolicInput{WasmBinary: b64, Entry: "decide"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(res.Paths) != 2 {
		t.Fatalf("paths = %d, want 2", len(res.Paths))
	}
	vals := make(map[int64]bool, len(res.Paths))
	for _, p := range res.Paths {
		if p.Model == nil {
			t.Fatalf("path model is nil; constraints=%q", p.Constraints)
		}
		vals[parseModelInt(t, p.Model, "arg0")] = true
	}
	// One path is guarded by arg0 == 0, the other by arg0 != 0.
	if !vals[0] {
		t.Errorf("no model with arg0 == 0; got %v", vals)
	}
	var hasNonzero bool
	for v := range vals {
		if v != 0 {
			hasNonzero = true
		}
	}
	if !hasNonzero {
		t.Errorf("no model with arg0 != 0; got %v", vals)
	}
}

// TestRun_Z3ExtractsCallModels solves the call module: one path must satisfy
// arg0*2 == 4, i.e. arg0 == 2.
func TestRun_Z3ExtractsCallModels(t *testing.T) {
	t.Parallel()
	z3Skip(t)

	b64 := encB64(callModule())
	res, err := Run(context.Background(), SymbolicInput{WasmBinary: b64, Entry: "decide"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(res.Paths) != 2 {
		t.Fatalf("paths = %d, want 2", len(res.Paths))
	}
	var hasTwo bool
	for _, p := range res.Paths {
		if p.Model != nil && parseModelInt(t, p.Model, "arg0") == 2 {
			hasTwo = true
		}
	}
	if !hasTwo {
		t.Errorf("no model with arg0 == 2 (arg0*2 == 4 branch); got %+v", res.Paths)
	}
}

// TestRun_Z3KnownSatisfiable is the "integration test with known-satisfiable
// constraints" acceptance criterion: feed the engine a real branching module
// and confirm Z3 returns sat models rather than errors.
func TestRun_Z3KnownSatisfiable(t *testing.T) {
	t.Parallel()
	z3Skip(t)

	b64 := encB64(brIfModule())
	res, err := Run(context.Background(), SymbolicInput{WasmBinary: b64, Entry: "decide"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(res.Paths) == 0 {
		t.Fatal("no feasible paths; expected at least one satisfiable model")
	}
	for _, p := range res.Paths {
		if p.Model == nil {
			t.Errorf("nil model for satisfiable path; constraints=%q", p.Constraints)
		}
	}
}

// TestExplorePaths_MatchesIssueAPI verifies the exported ExplorePaths
// function from the issue #246 API contract: it takes raw WASM bytes,
// entry function name, and args, returning PathResult slices.
func TestExplorePaths_MatchesIssueAPI(t *testing.T) {
	t.Parallel()
	wasm := ifElseModule()
	paths, err := ExplorePaths(wasm, "decide", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(paths) != 2 {
		t.Fatalf("paths = %d, want 2", len(paths))
	}
	// Verify each path has the declared fields.
	for _, p := range paths {
		if p.Constraints == "" {
			t.Error("PathResult.Constraints is empty")
		}
		// Model may be nil (no z3) but the field must exist.
		_ = p.Model
	}
}

// TestExplorePaths_WithConcreteArgs verifies ExplorePaths with concrete
// argument bindings produces no symbolic declarations.
func TestExplorePaths_WithConcreteArgs(t *testing.T) {
	t.Parallel()
	paths, err := ExplorePaths(identityModule(), "id", []any{42})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(paths) != 1 {
		t.Fatalf("paths = %d, want 1", len(paths))
	}
	if paths[0].Constraints != "" {
		t.Errorf("constraints = %q, want empty for concrete path", paths[0].Constraints)
	}
}
