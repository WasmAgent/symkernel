// Package verify provides the symbolic and SMT verification primitives used
// by symkerneld. This file implements the Milestone 3 symbolic execution
// engine: it decodes a base64 WebAssembly module, explores its execution
// paths by forking at conditional branches, accumulates each path's
// constraints as SMT-LIB v2, and solves them with Z3 to extract satisfying
// models.
package verify

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/google/uuid"
)

// SymbolicInput is the request payload for symbolic verification: a base64
// WebAssembly binary, the entry-point export to explore, and its arguments.
//
// Each entry in Args binds the corresponding entry-function parameter. A
// numeric entry fixes a concrete value; a nil or absent entry is treated as
// an unconstrained symbolic integer (named arg0, arg1, ...). Only i32
// parameters are supported.
type SymbolicInput struct {
	// WasmBinary is the base64-encoded WebAssembly module to explore.
	WasmBinary string `json:"wasmBinary"`
	// Entry names the export (function) to begin symbolic execution from.
	Entry string `json:"entry"`
	// Args are the initial argument values passed to Entry.
	Args []any `json:"args"`
}

// SymbolicPath describes one feasible execution path discovered during
// symbolic exploration: the guarding path constraint (SMT2) and a
// satisfying model.
type SymbolicPath struct {
	// Constraints is the SMT-LIB v2 formula concretising this path:
	// one declare-const per symbolic argument followed by one assert per
	// branch condition taken. It does not include (check-sat)/(get-model).
	Constraints string `json:"constraints"`
	// Model is a satisfying assignment for Constraints keyed by symbol, or
	// nil when the solver could not extract one.
	Model map[string]any `json:"model"`
}

// SymbolicResult holds the set of explored paths and bookkeeping fields.
type SymbolicResult struct {
	// Paths is the set of feasible execution paths discovered.
	Paths []SymbolicPath `json:"paths"`
	// Explored is the total number of paths considered (feasible or not).
	Explored int `json:"explored"`
	// DecisionID is the per-call UUID, following the GENAI_SEMCONV field
	// naming used across the WasmAgent ecosystem. Every response carries
	// one for traceability — see CLAUDE.md "Bot instructions".
	DecisionID string `json:"decision_id"`
}

// PathResult describes one feasible execution path: the SMT-LIB v2 path
// constraint and a satisfying model extracted by Z3. It is the per-path
// element type returned by ExplorePaths.
type PathResult struct {
	// Constraints is the SMT-LIB v2 formula concretising this path.
	Constraints string `json:"constraints"`
	// Model is a satisfying assignment for Constraints keyed by symbol, or
	// nil when the solver could not extract one.
	Model map[string]any `json:"model"`
}

// ErrPathBudget is returned when path exploration exceeds maxPaths leaves
// before exhausting every branch.
var ErrPathBudget = errors.New("symbolic: path budget exceeded")

// maxPaths bounds the number of leaves the engine explores before aborting,
// guarding against path explosion on adversarial inputs.
const maxPaths = 4096

// defaultSymbolicSolver is the Z3-backed solver Run uses to extract models.
// It is a variable so tests can substitute a stub, but callers must not
// mutate it concurrently.
var defaultSymbolicSolver Solver = &Z3Solver{}

// Run executes symbolic exploration of in.WasmBinary starting at in.Entry.
// It decodes the module, explores every execution path by forking at
// conditional branches, and asks Z3 for a satisfying model of each path's
// accumulated constraints. A DecisionID is always populated, even on error.
//
// When ctx is cancelled, Run returns ctx.Err() after emitting the DecisionID.
func Run(ctx context.Context, in SymbolicInput) (SymbolicResult, error) {
	return runWithSolver(ctx, defaultSymbolicSolver, in)
}

// ExplorePaths is a convenience entry point matching the issue #246 API
// contract. It accepts raw WASM bytes (not base64), the entry function name,
// and concrete/symbolic arguments, and returns the set of feasible paths with
// their SMT-LIB v2 constraints and Z3 models. It delegates to Run internally.
func ExplorePaths(wasm []byte, entryFunc string, args []any) ([]PathResult, error) {
	in := SymbolicInput{
		WasmBinary: base64.StdEncoding.EncodeToString(wasm),
		Entry:      entryFunc,
		Args:       args,
	}
	res, err := Run(context.Background(), in)
	if err != nil {
		return nil, err
	}
	paths := make([]PathResult, len(res.Paths))
	for i, p := range res.Paths {
			paths[i] = PathResult(p)
	}
	return paths, nil
}

// runWithSolver is the testable core of Run, allowing a caller to inject a
// Solver (which may be nil to skip solving and return every leaf with a nil
// model). It is the seam the unit tests exercise.
func runWithSolver(ctx context.Context, solver Solver, in SymbolicInput) (SymbolicResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	res := SymbolicResult{DecisionID: uuid.NewString()}

	wasmBytes, err := base64.StdEncoding.DecodeString(in.WasmBinary)
	if err != nil {
		return res, fmt.Errorf("symbolic: decode wasm binary: %w", err)
	}
	mod, err := decodeWasm(wasmBytes)
	if err != nil {
		return res, err
	}
	infos, err := buildFuncInfos(mod)
	if err != nil {
		return res, err
	}
	entryIdx, err := resolveEntry(mod, in.Entry)
	if err != nil {
		return res, err
	}
	argVals, err := bindArgs(infos[entryIdx].params, in.Args)
	if err != nil {
		return res, err
	}

	leaves, explored, err := explorePaths(ctx, infos, mod.importFuncs, entryIdx, argVals)
	if err != nil {
		return res, err
	}

	res.Explored = explored
	for _, lf := range leaves {
		if lf.term == termTrap {
			continue // unreachable: an infeasible leaf with no model
		}
		script := buildSMT(lf)
		model, feasible := solvePath(ctx, solver, script, sortedSymbols(lf.symbols))
		if !feasible {
			continue
		}
		res.Paths = append(res.Paths, SymbolicPath{Constraints: script, Model: model})
	}
	return res, nil
}

// funcInfo is the per-function precomputed view the interpreter consumes.
type funcInfo struct {
	params    []valType
	results   []valType
	instrs    []instr
	numLocals int
}

func buildFuncInfos(m *module) ([]funcInfo, error) {
	if len(m.funcTypes) != len(m.codes) {
		return nil, fmt.Errorf("wasm: %d function decls but %d code bodies", len(m.funcTypes), len(m.codes))
	}
	infos := make([]funcInfo, len(m.funcTypes))
	for i := range m.funcTypes {
		ti := int(m.funcTypes[i])
		if ti >= len(m.types) {
			return nil, fmt.Errorf("wasm: function %d references missing type %d", i, ti)
		}
		instrs, err := decodeInstrs(m.codes[i].body)
		if err != nil {
			return nil, fmt.Errorf("wasm: function %d: %w", i, err)
		}
		infos[i] = funcInfo{
			params:    m.types[ti].params,
			results:   m.types[ti].results,
			instrs:    instrs,
			numLocals: len(m.codes[i].localTypes),
		}
	}
	return infos, nil
}

func resolveEntry(m *module, name string) (int, error) {
	if name == "" {
		return 0, errors.New("symbolic: entry export name is empty")
	}
	exp, ok := m.exports[name]
	if !ok {
		return 0, fmt.Errorf("symbolic: export %q not found", name)
	}
	if exp.kind != 0 {
		return 0, fmt.Errorf("symbolic: export %q is not a function", name)
	}
	if int(exp.index) < m.importFuncs {
		return 0, fmt.Errorf("symbolic: entry %q is an imported function", name)
	}
	return int(exp.index) - m.importFuncs, nil
}

// bindArgs translates the caller-supplied Args into initial symbolic values
// for the entry function's parameters. Numeric args are concrete; nil/absent
// args become symbolic terms named argN. Only i32 parameters are supported.
func bindArgs(params []valType, args []any) ([]symVal, error) {
	vals := make([]symVal, len(params))
	for i := range params {
		if params[i] != valI32 {
			return nil, fmt.Errorf("symbolic: unsupported parameter %d type %s (only i32)", i, params[i])
		}
		if i < len(args) && args[i] != nil {
			c, err := toConcrete(args[i])
			if err != nil {
				return nil, fmt.Errorf("symbolic: arg %d: %w", i, err)
			}
			vals[i] = concreteVal(c)
		} else {
			vals[i] = smtTerm(argName(i))
		}
	}
	return vals, nil
}

func toConcrete(v any) (int32, error) {
	switch x := v.(type) {
	case int:
		return int32(x), nil
	case int32:
		return x, nil
	case int64:
		return int32(x), nil
	case float64:
		// JSON numbers arrive as float64; accept integral values only.
		if x != float64(int64(x)) {
			return 0, fmt.Errorf("non-integral numeric arg %v", x)
		}
		return int32(int64(x)), nil
	default:
		return 0, fmt.Errorf("unsupported arg type %T", v)
	}
}

func argName(i int) string { return "arg" + strconv.Itoa(i) }

// ---- symbolic values --------------------------------------------------------

// symVal is a value on the symbolic operand stack. It is either concrete
// (a fixed int32) or an SMT-LIB v2 integer term over the declared symbols.
// All symbolic terms are Int-sorted: WASM i32 booleans (the result of i32.eq
// and friends) are encoded as 0/1 integers via ite, matching the i32
// representation and avoiding Bool/Int sort mixing in path conditions.
type symVal struct {
	concrete bool
	val      int32  // when concrete
	expr     string // SMT2 term when symbolic (Int sort)
}

func concreteVal(v int32) symVal { return symVal{concrete: true, val: v} }
func smtTerm(s string) symVal    { return symVal{concrete: false, expr: s} }

// term returns the SMT2 representation of v.
func (v symVal) term() string {
	if v.concrete {
		return smtInt(int64(v.val))
	}
	return v.expr
}

// smtInt renders an integer as an SMT-LIB v2 literal, using (- n) for
// negatives since SMT-LIB integer constants are non-negative.
func smtInt(v int64) string {
	if v < 0 {
		return "(- " + strconv.FormatInt(-v, 10) + ")"
	}
	return strconv.FormatInt(v, 10)
}

// binop folds a binary i32 operation when both operands are concrete, and
// otherwise emits the symbolic SMT2 term.
func binop(a, b symVal, concrete func(x, y int32) int32, sym func(at, bt string) string) symVal {
	if a.concrete && b.concrete {
		return concreteVal(concrete(a.val, b.val))
	}
	return smtTerm(sym(a.term(), b.term()))
}

func i32Add(a, b symVal) symVal {
	return binop(a, b,
		func(x, y int32) int32 { return x + y },
		func(at, bt string) string { return "(+ " + at + " " + bt + ")" })
}
func i32Sub(a, b symVal) symVal {
	return binop(a, b,
		func(x, y int32) int32 { return x - y },
		func(at, bt string) string { return "(- " + at + " " + bt + ")" })
}
func i32Mul(a, b symVal) symVal {
	return binop(a, b,
		func(x, y int32) int32 { return x * y },
		func(at, bt string) string { return "(* " + at + " " + bt + ")" })
}
func i32Eq(a, b symVal) symVal {
	return binop(a, b,
		func(x, y int32) int32 { if x == y { return 1 }; return 0 },
		func(at, bt string) string { return "(ite (= " + at + " " + bt + ") 1 0)" })
}
func i32Ne(a, b symVal) symVal {
	return binop(a, b,
		func(x, y int32) int32 { if x != y { return 1 }; return 0 },
		func(at, bt string) string { return "(ite (distinct " + at + " " + bt + ") 1 0)" })
}
func i32Cmp(a, b symVal, op func(x, y int32) bool, smtOp string) symVal {
	return binop(a, b,
		func(x, y int32) int32 { if op(x, y) { return 1 }; return 0 },
		func(at, bt string) string { return "(ite (" + smtOp + " " + at + " " + bt + ") 1 0)" })
}
func i32Eqz(a symVal) symVal {
	if a.concrete {
		if a.val == 0 {
			return concreteVal(1)
		}
		return concreteVal(0)
	}
	return smtTerm("(ite (= " + a.term() + " 0) 1 0)")
}

// ---- instruction model ------------------------------------------------------

type opcode byte

const (
	opUnreachable opcode = 0x00
	opNop         opcode = 0x01
	opBlock       opcode = 0x02
	opLoop        opcode = 0x03
	opIf          opcode = 0x04
	opElse        opcode = 0x05
	opEnd         opcode = 0x0b
	opBr          opcode = 0x0c
	opBrIf        opcode = 0x0d
	opReturn      opcode = 0x0f
	opCall        opcode = 0x10
	opDrop        opcode = 0x1a
	opLocalGet    opcode = 0x20
	opLocalSet    opcode = 0x21
	opLocalTee    opcode = 0x22
	opI32Const    opcode = 0x41
	opI32Eqz      opcode = 0x45
	opI32Eq       opcode = 0x46
	opI32Ne       opcode = 0x47
	opI32LtS      opcode = 0x48
	opI32GtS      opcode = 0x49
	opI32LeS      opcode = 0x4a
	opI32GeS      opcode = 0x4b
	opI32Add      opcode = 0x6a
	opI32Sub      opcode = 0x6b
	opI32Mul      opcode = 0x6c
)

// instr is one decoded WebAssembly instruction. Control-flow instructions
// carry precomputed jump targets so the interpreter can fork and resume.
type instr struct {
	op      opcode
	u32     uint32 // local/func/label index immediate
	i32     int32  // i32.const immediate
	arity   int    // block result arity
	elseIdx int    // opIf: matching opElse index, else -1
	endIdx  int    // opBlock/opLoop/opIf/opElse: matching opEnd index
	loopIdx int    // opLoop: own index (backward-branch target), else -1
}

// decodeInstrs turns a raw function body into a flat instruction list with
// matched control-flow targets. The function's terminating opEnd (with an
// empty control stack when reached) is recorded as a regular opEnd entry.
func decodeInstrs(body []byte) ([]instr, error) {
	r := &byteReader{b: body}
	var out []instr
	type frame struct {
		idx     int // index in out of the opening instr
		op      opcode
		elseIdx int // index of a matching opElse, else -1
	}
	var stack []frame

	for !r.eof() {
		opByte, err := r.byte()
		if err != nil {
			return nil, err
		}
		op := opcode(opByte)
		switch op {
		case opBlock, opLoop, opIf:
			arity, err := decodeBlocktype(r)
			if err != nil {
				return nil, err
			}
			idx := len(out)
			ins := instr{op: op, arity: arity, elseIdx: -1, endIdx: -1, loopIdx: -1}
			if op == opLoop {
				ins.loopIdx = idx
			}
			out = append(out, ins)
			stack = append(stack, frame{idx: idx, op: op, elseIdx: -1})
		case opElse:
			if len(stack) == 0 || stack[len(stack)-1].op != opIf {
				return nil, errors.New("wasm: else without matching if")
			}
			elseIdx := len(out)
			out = append(out, instr{op: opElse, endIdx: -1})
			out[stack[len(stack)-1].idx].elseIdx = elseIdx
			stack[len(stack)-1].elseIdx = elseIdx
		case opEnd:
			endIdx := len(out)
			out = append(out, instr{op: opEnd})
			if len(stack) > 0 {
				f := stack[len(stack)-1]
				stack = stack[:len(stack)-1]
				out[f.idx].endIdx = endIdx
				if f.elseIdx >= 0 {
					out[f.elseIdx].endIdx = endIdx
				}
			}
		case opBr, opBrIf, opCall, opLocalGet, opLocalSet, opLocalTee:
			imm, err := r.u32()
			if err != nil {
				return nil, err
			}
			out = append(out, instr{op: op, u32: imm, elseIdx: -1, endIdx: -1, loopIdx: -1})
		case opI32Const:
			imm, err := r.i32()
			if err != nil {
				return nil, err
			}
			out = append(out, instr{op: op, i32: imm, elseIdx: -1, endIdx: -1, loopIdx: -1})
		case opNop, opDrop, opReturn, opUnreachable,
			opI32Eqz, opI32Eq, opI32Ne, opI32LtS, opI32GtS, opI32LeS, opI32GeS,
			opI32Add, opI32Sub, opI32Mul:
			out = append(out, instr{op: op, elseIdx: -1, endIdx: -1, loopIdx: -1})
		default:
			return nil, fmt.Errorf("wasm: unsupported opcode 0x%02x", opByte)
		}
	}
	if len(stack) != 0 {
		return nil, errors.New("wasm: unterminated control frame")
	}
	return out, nil
}

// decodeBlocktype reads a blocktype, returning its result arity. Only the
// empty (0x40) and single-value forms are supported; type-index (multi-value)
// blocktypes are consumed but treated as arity 0.
func decodeBlocktype(r *byteReader) (int, error) {
	b, err := r.peekByte()
	if err != nil {
		return 0, err
	}
	switch valType(b) {
	case 0x40:
		_, _ = r.byte()
		return 0, nil
	case valI32, valI64, valF32, valF64:
		_, _ = r.byte()
		return 1, nil
	default:
		if _, err := r.i32(); err != nil { // s33 type index (signed LEB128)
			return 0, err
		}
		return 0, nil
	}
}

// ---- path state -------------------------------------------------------------

type termKind byte

const (
	termAlive termKind = iota
	termDone // returned or reached function end: a feasible leaf
	termTrap // unreachable executed: an infeasible leaf
)

// blockFrame is one entry on an activation's control stack.
type blockFrame struct {
	loopIdx int // backward-branch target (loop), else -1
	endIdx  int // forward-branch target: the matching opEnd index
	arity   int
}

// activation is one call frame: the function's instructions, an instruction
// pointer, locals, and a control (block) stack. The operand stack is shared
// across activations on the path state, matching WebAssembly call semantics.
type activation struct {
	instrs []instr
	ip     int
	locals []symVal
	blocks []blockFrame
}

// symState is one in-progress execution path under exploration.
type symState struct {
	acts        []activation
	stack       []symVal
	constraints []string
	symbols     map[string]bool
	term        termKind
}

func (s *symState) cur() *activation { return &s.acts[len(s.acts)-1] }

func (s *symState) push(v symVal) { s.stack = append(s.stack, v) }

func (s *symState) pop() (symVal, bool) {
	if len(s.stack) == 0 {
		return symVal{}, false
	}
	n := len(s.stack)
	v := s.stack[n-1]
	s.stack = s.stack[:n-1]
	return v, true
}

func (s *symState) addConstraint(c string) { s.constraints = append(s.constraints, c) }

// clone deep-copies the path state so the two branches of a fork diverge.
// Instruction slices are shared read-only.
func (s *symState) clone() *symState {
	nb := *s
	nb.acts = append([]activation(nil), s.acts...)
	for i := range nb.acts {
		nb.acts[i].locals = append([]symVal(nil), s.acts[i].locals...)
		nb.acts[i].blocks = append([]blockFrame(nil), s.acts[i].blocks...)
	}
	nb.stack = append([]symVal(nil), s.stack...)
	nb.constraints = append([]string(nil), s.constraints...)
	if len(s.symbols) > 0 {
		syms := make(map[string]bool, len(s.symbols))
		for k, v := range s.symbols {
			syms[k] = v
		}
		nb.symbols = syms
	}
	return &nb
}

// ---- exploration ------------------------------------------------------------

// explorePaths walks every execution path from the entry function, forking at
// each conditional branch (opIf, opBrIf). It returns the terminal leaves and
// the total number of paths considered.
func explorePaths(ctx context.Context, infos []funcInfo, importFuncs, entryIdx int, argVals []symVal) ([]*symState, int, error) {
	entry := infos[entryIdx]
	locals := make([]symVal, len(entry.params)+entry.numLocals)
	symbols := map[string]bool{}
	for i, v := range argVals {
		locals[i] = v
		if !v.concrete {
			symbols[v.expr] = true
		}
	}
	for i := len(argVals); i < len(locals); i++ {
		locals[i] = concreteVal(0)
	}
	start := &symState{
		acts:    []activation{{instrs: entry.instrs, ip: 0, locals: locals}},
		symbols: symbols,
	}

	worklist := []*symState{start}
	var leaves []*symState
	explored := 0
	for len(worklist) > 0 {
		if err := ctx.Err(); err != nil {
			return leaves, explored, err
		}
		s := worklist[len(worklist)-1]
		worklist = worklist[:len(worklist)-1]

		if s.term != termAlive {
			leaves = append(leaves, s)
			explored++
			continue
		}
		forks, terminated, err := step(s, infos, importFuncs)
		if err != nil {
			return leaves, explored, err
		}
		if terminated {
			leaves = append(leaves, s)
			explored++
		} else {
			worklist = append(worklist, s)
		}
		worklist = append(worklist, forks...)
		if explored+len(worklist) > maxPaths {
			return leaves, explored, ErrPathBudget
		}
	}
	return leaves, explored, nil
}

// step advances state s by a single instruction. It mutates s in place (s
// continues down one branch of any fork) and returns any sibling branches
// created by a fork. terminated reports whether s reached a terminal state.
func step(s *symState, infos []funcInfo, importFuncs int) (forks []*symState, terminated bool, err error) {
	a := s.cur()
	if a.ip >= len(a.instrs) {
		// Ran off the end of a body without an explicit end: treat as done.
		s.term = termDone
		return nil, true, nil
	}
	ins := a.instrs[a.ip]

	switch ins.op {
	case opNop:
		a.ip++
	case opUnreachable:
		s.term = termTrap
		return nil, true, nil
	case opBlock, opLoop:
		a.blocks = append(a.blocks, blockFrame{loopIdx: ins.loopIdx, endIdx: ins.endIdx, arity: ins.arity})
		a.ip++
	case opIf:
		cond, ok := s.pop()
		if !ok {
			return nil, false, errors.New("wasm: if on empty stack")
		}
		falseBranch := s.clone()
		// True branch (cond != 0) continues on s.
		s.addConstraint(condTrue(cond))
		a.blocks = append(a.blocks, blockFrame{loopIdx: -1, endIdx: ins.endIdx, arity: ins.arity})
		a.ip++
		// False branch (cond == 0).
		fb := falseBranch.cur()
		falseBranch.addConstraint(condFalse(cond))
		if ins.elseIdx >= 0 {
			fb.blocks = append(fb.blocks, blockFrame{loopIdx: -1, endIdx: ins.endIdx, arity: ins.arity})
			fb.ip = ins.elseIdx + 1
		} else {
			fb.ip = ins.endIdx + 1
		}
		return []*symState{falseBranch}, false, nil
	case opElse:
		// Reached by falling through a then-body: exit the if block.
		if len(a.blocks) == 0 {
			return nil, false, errors.New("wasm: else with empty block stack")
		}
		a.blocks = a.blocks[:len(a.blocks)-1]
		a.ip = ins.endIdx + 1
	case opEnd:
		if len(a.blocks) > 0 {
			a.blocks = a.blocks[:len(a.blocks)-1]
			a.ip++
		} else if len(s.acts) == 1 {
			s.term = termDone
			return nil, true, nil
		} else {
			// Returning from a callee: results remain on the shared stack.
			s.acts = s.acts[:len(s.acts)-1]
		}
	case opBr:
		if err := s.branchTo(ins.u32); err != nil {
			return nil, false, err
		}
	case opBrIf:
		cond, ok := s.pop()
		if !ok {
			return nil, false, errors.New("wasm: br_if on empty stack")
		}
		falseBranch := s.clone()
		// Taken branch (cond != 0) continues on s.
		s.addConstraint(condTrue(cond))
		if err := s.branchTo(ins.u32); err != nil {
			return nil, false, err
		}
		// Not-taken branch (cond == 0).
		falseBranch.addConstraint(condFalse(cond))
		falseBranch.cur().ip++
		return []*symState{falseBranch}, false, nil
	case opReturn:
		if len(s.acts) == 1 {
			s.term = termDone
			return nil, true, nil
		}
		s.acts = s.acts[:len(s.acts)-1]
	case opCall:
		idx := int(ins.u32)
		if idx < importFuncs {
			return nil, false, fmt.Errorf("symbolic: call to imported function %d is unsupported", idx)
		}
		defined := idx - importFuncs
		if defined >= len(infos) {
			return nil, false, fmt.Errorf("wasm: call to undefined function %d", idx)
		}
		callee := infos[defined]
		args := make([]symVal, len(callee.params))
		for i := len(args) - 1; i >= 0; i-- {
			v, ok := s.pop()
			if !ok {
				return nil, false, fmt.Errorf("wasm: call %d: missing argument %d", idx, i)
			}
			args[i] = v
		}
		calleeLocals := make([]symVal, len(callee.params)+callee.numLocals)
		copy(calleeLocals, args)
		for i := len(args); i < len(calleeLocals); i++ {
			calleeLocals[i] = concreteVal(0)
		}
		s.cur().ip++ // caller resumes after the call
		s.acts = append(s.acts, activation{instrs: callee.instrs, ip: 0, locals: calleeLocals})
	case opDrop:
		if _, ok := s.pop(); !ok {
			return nil, false, errors.New("wasm: drop on empty stack")
		}
		a.ip++
	case opLocalGet:
		v, err := readLocal(s, ins.u32)
		if err != nil {
			return nil, false, err
		}
		s.push(v)
		a.ip++
	case opLocalSet:
		v, ok := s.pop()
		if !ok {
			return nil, false, errors.New("wasm: local.set on empty stack")
		}
		if err := writeLocal(s, ins.u32, v); err != nil {
			return nil, false, err
		}
		a.ip++
	case opLocalTee:
		if len(s.stack) == 0 {
			return nil, false, errors.New("wasm: local.tee on empty stack")
		}
		if err := writeLocal(s, ins.u32, s.stack[len(s.stack)-1]); err != nil {
			return nil, false, err
		}
		a.ip++
	case opI32Const:
		s.push(concreteVal(ins.i32))
		a.ip++
	case opI32Eqz:
		v, ok := s.pop()
		if !ok {
			return nil, false, errStack("i32.eqz")
		}
		s.push(i32Eqz(v))
		a.ip++
	case opI32Eq:
		b, a2, ok := pop2(s)
		if !ok {
			return nil, false, errStack("i32.eq")
		}
		s.push(i32Eq(a2, b))
		a.ip++
	case opI32Ne:
		b, a2, ok := pop2(s)
		if !ok {
			return nil, false, errStack("i32.ne")
		}
		s.push(i32Ne(a2, b))
		a.ip++
	case opI32LtS:
		b, a2, ok := pop2(s)
		if !ok {
			return nil, false, errStack("i32.lt_s")
		}
		s.push(i32Cmp(a2, b, func(x, y int32) bool { return x < y }, "<"))
		a.ip++
	case opI32GtS:
		b, a2, ok := pop2(s)
		if !ok {
			return nil, false, errStack("i32.gt_s")
		}
		s.push(i32Cmp(a2, b, func(x, y int32) bool { return x > y }, ">"))
		a.ip++
	case opI32LeS:
		b, a2, ok := pop2(s)
		if !ok {
			return nil, false, errStack("i32.le_s")
		}
		s.push(i32Cmp(a2, b, func(x, y int32) bool { return x <= y }, "<="))
		a.ip++
	case opI32GeS:
		b, a2, ok := pop2(s)
		if !ok {
			return nil, false, errStack("i32.ge_s")
		}
		s.push(i32Cmp(a2, b, func(x, y int32) bool { return x >= y }, ">="))
		a.ip++
	case opI32Add:
		b, a2, ok := pop2(s)
		if !ok {
			return nil, false, errStack("i32.add")
		}
		s.push(i32Add(a2, b))
		a.ip++
	case opI32Sub:
		b, a2, ok := pop2(s)
		if !ok {
			return nil, false, errStack("i32.sub")
		}
		s.push(i32Sub(a2, b))
		a.ip++
	case opI32Mul:
		b, a2, ok := pop2(s)
		if !ok {
			return nil, false, errStack("i32.mul")
		}
		s.push(i32Mul(a2, b))
		a.ip++
	default:
		return nil, false, fmt.Errorf("wasm: unhandled opcode 0x%02x", byte(ins.op))
	}
	return nil, false, nil
}

func pop2(s *symState) (top, below symVal, ok bool) {
	if len(s.stack) < 2 {
		return symVal{}, symVal{}, false
	}
	top = s.stack[len(s.stack)-1]
	below = s.stack[len(s.stack)-2]
	s.stack = s.stack[:len(s.stack)-2]
	return top, below, true
}

func errStack(op string) error { return fmt.Errorf("wasm: %s on short stack", op) }

func readLocal(s *symState, idx uint32) (symVal, error) {
	locals := s.cur().locals
	if int(idx) >= len(locals) {
		return symVal{}, fmt.Errorf("wasm: local.get out-of-range index %d", idx)
	}
	return locals[idx], nil
}

func writeLocal(s *symState, idx uint32, v symVal) error {
	locals := s.cur().locals
	if int(idx) >= len(locals) {
		return fmt.Errorf("wasm: local.set out-of-range index %d", idx)
	}
	locals[idx] = v
	return nil
}

// branchTo performs an unconditional branch to the given label depth on the
// current activation. Forward branches land on the target block's opEnd
// (which then pops the frame); backward branches (loops) re-enter the loop.
func (s *symState) branchTo(label uint32) error {
	a := s.cur()
	if int(label) >= len(a.blocks) {
		return fmt.Errorf("wasm: branch label %d out of range (depth %d)", label, len(a.blocks))
	}
	target := a.blocks[len(a.blocks)-1-int(label)]
	if target.arity > 0 {
		// Keep the top `arity` values as the block result; drop the rest.
		if len(s.stack) >= target.arity {
			s.stack = append([]symVal{}, s.stack[len(s.stack)-target.arity:]...)
		}
	}
	a.blocks = a.blocks[:len(a.blocks)-int(label)]
	if target.loopIdx >= 0 {
		a.ip = target.loopIdx + 1 // re-enter loop body
	} else {
		a.ip = target.endIdx // opEnd will pop the target frame
	}
	return nil
}

// condTrue/condFalse render the path condition asserting a branch's i32
// condition is non-zero (taken) or zero (not taken).
func condTrue(c symVal) string  { return "(not (= " + c.term() + " 0))" }
func condFalse(c symVal) string { return "(= " + c.term() + " 0)" }

// ---- constraint generation & solving ---------------------------------------

// buildSMT renders the SMT-LIB v2 formula for a path: one declare-const per
// distinct symbol followed by one assert per accumulated path condition.
func buildSMT(s *symState) string {
	var b strings.Builder
	for _, name := range sortedSymbols(s.symbols) {
		fmt.Fprintf(&b, "(declare-const %s Int)\n", name)
	}
	for _, c := range s.constraints {
		fmt.Fprintf(&b, "(assert %s)\n", c)
	}
	return b.String()
}

func sortedSymbols(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// solvePath asks the solver whether a path's constraints are satisfiable and,
// if so, returns a satisfying model. It queries the declared symbols with
// (get-value ...) rather than (get-model) so the binding format ((var val))
// is stable across z3 versions (z3 >=4.8 emits define-fun models that the
// shared parser does not understand). A nil solver means "do not solve": the
// path is assumed feasible and returned without a model.
func solvePath(ctx context.Context, solver Solver, script string, symbols []string) (map[string]any, bool) {
	if solver == nil {
		return nil, true
	}
	var b strings.Builder
	b.WriteString(script)
	b.WriteString("(check-sat)\n")
	if len(symbols) > 0 {
		b.WriteString("(get-value (")
		for _, s := range symbols {
			b.WriteString(s)
			b.WriteByte(' ')
		}
		b.WriteString("))\n")
	}
	result, err := solver.Solve(ctx, b.String())
	if err != nil {
		// Solver unavailable (e.g. z3 not installed): assume feasible without
		// a model rather than dropping the path or failing the whole run.
		return nil, true
	}
	if result.Sat == "sat" {
		return result.Model, true
	}
	return nil, false
}
