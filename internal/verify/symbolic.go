// Package verify provides HTTP handlers for symkerneld verification endpoints.
package verify

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"

	"github.com/WasmAgent/symkernel/internal/z3"
	"github.com/google/uuid"
)

const (
	defaultSymbolicMaxDepth  = 100
	maxSymbolicMaxDepth      = 256
	maxSymbolicExplored      = 4096
	maxSymbolicRequestBytes  = 2 << 20
	maxSymbolicModuleBytes   = 1 << 20
	maxSymbolicEntrypointLen = 256
	maxWasmVectorItems       = 4096
	maxWasmParams            = 64
	maxWasmLocals            = 4096
	maxWasmInstructions      = 16384
	maxWasmControlDepth      = 256
)

// ErrNotImplemented is retained for source compatibility with callers of the
// original symbolic-verification scaffold. Run is implemented and no longer
// returns this sentinel.
var ErrNotImplemented = errors.New("symbolic verification not implemented")

// SymbolicInput is the request payload for POST /v1/verify/symbolic.
// Module, Entrypoint, MaxDepth, and PruneInfeasible are the v12 endpoint
// fields. The legacy fields below remain supported so existing Go callers can
// migrate without changing their request construction in one step. When both
// names are supplied, Run requires the module and entrypoint values to agree.
type SymbolicInput struct {
	Module          string `json:"module"`
	Entrypoint      string `json:"entrypoint"`
	MaxDepth        int    `json:"maxDepth"`
	PruneInfeasible bool   `json:"pruneInfeasible"`

	// WasmBinary is the legacy base64-encoded module field.
	// Deprecated: use Module.
	WasmBinary string `json:"wasmBinary,omitempty"`
	// Entry is the legacy exported function field.
	// Deprecated: use Entrypoint.
	Entry string `json:"entry,omitempty"`
	// Args are concrete legacy function arguments. They are still applied to
	// matching parameters when supplied.
	// Deprecated: prefer symbolic parameters through the v12 endpoint fields.
	Args []any `json:"args,omitempty"`
}

// SymbolicPath is one terminal path reached by the entrypoint.
type SymbolicPath struct {
	// Constraints is the SMT-LIB assertions guarding this path. It remains a
	// string for compatibility with callers that pass it to SolveConstraintsCtx.
	Constraints string `json:"constraints"`
	// Model is a satisfying assignment for Constraints keyed by symbol. It is
	// retained for compatibility with the original symbolic API.
	Model map[string]any `json:"model"`

	ID       string `json:"id"`
	Feasible bool   `json:"feasible"`
	Output   any    `json:"output"`

	constraints []string
}

// SymbolicResult is the response payload for symbolic verification.
type SymbolicResult struct {
	Paths      []SymbolicPath `json:"paths"`
	Explored   int            `json:"explored"`
	Pruned     int            `json:"pruned"`
	DecisionID string         `json:"decision_id"`
}

// Run symbolically executes the requested Wasm function. Unbound function
// parameters become fixed-width arg0, arg1, ... bitvector symbols. It
// deliberately supports the core integer/control-flow instruction set used
// for decisions; unsupported Wasm instructions fail explicitly instead of
// silently falling back to concrete execution.
func Run(ctx context.Context, in SymbolicInput) (SymbolicResult, error) {
	module := strings.TrimSpace(in.Module)
	if module == "" {
		module = strings.TrimSpace(in.WasmBinary)
	} else if legacy := strings.TrimSpace(in.WasmBinary); legacy != "" && legacy != module {
		return SymbolicResult{}, fmt.Errorf("module and wasmBinary disagree")
	}
	if module == "" {
		return SymbolicResult{}, fmt.Errorf("module is required")
	}
	entrypoint := strings.TrimSpace(in.Entrypoint)
	if entrypoint == "" {
		entrypoint = strings.TrimSpace(in.Entry)
	} else if legacy := strings.TrimSpace(in.Entry); legacy != "" && legacy != entrypoint {
		return SymbolicResult{}, fmt.Errorf("entrypoint and entry disagree")
	}
	if entrypoint == "" {
		return SymbolicResult{}, fmt.Errorf("entrypoint is required")
	}
	if len(entrypoint) > maxSymbolicEntrypointLen {
		return SymbolicResult{}, fmt.Errorf("entrypoint exceeds %d bytes", maxSymbolicEntrypointLen)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return SymbolicResult{}, err
	}
	if in.MaxDepth < 0 {
		return SymbolicResult{}, fmt.Errorf("maxDepth must not be negative")
	}
	if in.MaxDepth == 0 {
		in.MaxDepth = defaultSymbolicMaxDepth
	}
	if in.MaxDepth > maxSymbolicMaxDepth {
		return SymbolicResult{}, fmt.Errorf("maxDepth must not exceed %d", maxSymbolicMaxDepth)
	}
	maxEncodedModule := 4 * ((maxSymbolicModuleBytes + 2) / 3)
	if len(module) > maxEncodedModule {
		return SymbolicResult{}, fmt.Errorf("module exceeds %d decoded bytes", maxSymbolicModuleBytes)
	}
	wasm, err := base64.StdEncoding.DecodeString(module)
	if err != nil {
		return SymbolicResult{}, fmt.Errorf("decode module: %w", err)
	}
	if len(wasm) > maxSymbolicModuleBytes {
		return SymbolicResult{}, fmt.Errorf("module exceeds %d bytes", maxSymbolicModuleBytes)
	}
	fn, err := parseExportedFunction(wasm, entrypoint)
	if err != nil {
		return SymbolicResult{}, err
	}

	initial := symbolicState{locals: make([]symbolicValue, len(fn.params)+len(fn.locals))}
	model := make(map[string]any, len(fn.params))
	for i, typ := range fn.params {
		bits, ok := wasmIntegerWidth(typ)
		if !ok {
			return SymbolicResult{}, fmt.Errorf("entrypoint parameter %d has unsupported type 0x%x", i, typ)
		}
		if i < len(in.Args) && in.Args[i] != nil {
			value, err := concreteArgument(in.Args[i], bits)
			if err != nil {
				return SymbolicResult{}, fmt.Errorf("argument %d: %w", i, err)
			}
			initial.locals[i] = value
			continue
		}
		name := fmt.Sprintf("arg%d", i)
		initial.locals[i] = symbolicValue{expr: name, bits: bits}
		model[name] = fmt.Sprintf("BitVec_%d", bits)
	}
	for i, typ := range fn.locals {
		bits, ok := wasmIntegerWidth(typ)
		if !ok {
			return SymbolicResult{}, fmt.Errorf("entrypoint local %d has unsupported type 0x%x", i, typ)
		}
		initial.locals[len(fn.params)+i] = typedConstant(0, bits)
	}

	exec := executor{ctx: ctx, maxDepth: in.MaxDepth, prune: in.PruneInfeasible, model: model, explored: 1}
	states, err := exec.run(fn.body, []symbolicState{initial})
	if err != nil {
		return SymbolicResult{}, err
	}
	paths := make([]SymbolicPath, 0, len(states))
	for _, state := range states {
		feasible, pathModel, err := exec.feasibility(state.constraints)
		if err != nil {
			return SymbolicResult{}, err
		}
		if !feasible && in.PruneInfeasible {
			exec.pruned++
			continue
		}
		if len(state.stack) < len(fn.results) {
			return SymbolicResult{}, fmt.Errorf("entrypoint %q did not produce its declared results", entrypoint)
		}
		paths = append(paths, SymbolicPath{
			ID:          uuid.NewString(),
			Feasible:    feasible,
			Constraints: smt2Assertions(state.constraints),
			Model:       pathModel,
			Output:      state.output(fn.results),
			constraints: append([]string(nil), state.constraints...),
		})
	}
	return SymbolicResult{Paths: paths, Explored: exec.explored, Pruned: exec.pruned, DecisionID: uuid.NewString()}, nil
}

func smt2Assertions(constraints []string) string {
	var b strings.Builder
	for _, constraint := range constraints {
		fmt.Fprintf(&b, "(assert %s)\n", constraint)
	}
	return b.String()
}

type symbolicValue struct {
	expr    string
	known   *int64
	boolean bool
	bits    byte
}

func typedConstant(v int64, bits byte) symbolicValue {
	v = wrapSigned(v, bits)
	unsigned := uint64(v)
	if bits < 64 {
		unsigned &= (uint64(1) << bits) - 1
	}
	return symbolicValue{expr: fmt.Sprintf("(_ bv%d %d)", unsigned, bits), known: &v, bits: bits}
}

func wrapSigned(v int64, bits byte) int64 {
	if bits == 32 {
		return int64(int32(v))
	}
	return v
}

func (v symbolicValue) zeroExpr() string {
	return fmt.Sprintf("(_ bv0 %d)", v.bits)
}

func concreteArgument(value any, bits byte) (symbolicValue, error) {
	var number int64
	switch v := value.(type) {
	case int:
		number = int64(v)
	case int8:
		number = int64(v)
	case int16:
		number = int64(v)
	case int32:
		number = int64(v)
	case int64:
		number = v
	case uint:
		number = int64(v)
	case uint8:
		number = int64(v)
	case uint16:
		number = int64(v)
	case uint32:
		number = int64(v)
	case uint64:
		number = int64(v)
	case float64:
		const minInt64 = -1 << 63
		if math.IsNaN(v) || math.IsInf(v, 0) || math.Trunc(v) != v ||
			v < float64(minInt64) || v >= -float64(minInt64) {
			return symbolicValue{}, fmt.Errorf("value %v is not an integer", v)
		}
		number = int64(v)
	case json.Number:
		parsed, err := v.Int64()
		if err == nil {
			number = parsed
			break
		}
		unsigned, unsignedErr := strconv.ParseUint(string(v), 10, 64)
		if unsignedErr != nil {
			return symbolicValue{}, fmt.Errorf("value %q is not an integer", v)
		}
		number = int64(unsigned)
	default:
		return symbolicValue{}, fmt.Errorf("unsupported concrete value %T", value)
	}
	return typedConstant(number, bits), nil
}

func (v symbolicValue) condition() string {
	if v.boolean {
		if v.known != nil {
			if *v.known == 0 {
				return "false"
			}
			return "true"
		}
		return v.expr
	}
	if v.known != nil {
		if *v.known == 0 {
			return "false"
		}
		return "true"
	}
	return "(not (= " + v.expr + " " + v.zeroExpr() + "))"
}

type symbolicState struct {
	locals      []symbolicValue
	stack       []symbolicValue
	constraints []string
	depth       int
}

func (s symbolicState) clone() symbolicState {
	s.locals = append([]symbolicValue(nil), s.locals...)
	s.stack = append([]symbolicValue(nil), s.stack...)
	s.constraints = append([]string(nil), s.constraints...)
	return s
}

func (s *symbolicState) pop() (symbolicValue, error) {
	if len(s.stack) == 0 {
		return symbolicValue{}, fmt.Errorf("wasm stack underflow")
	}
	v := s.stack[len(s.stack)-1]
	s.stack = s.stack[:len(s.stack)-1]
	return v, nil
}

func (s symbolicState) output(results []byte) any {
	if len(results) == 0 {
		return nil
	}
	values := make([]any, len(results))
	for i := range results {
		v := s.stack[len(s.stack)-len(results)+i]
		if v.known != nil {
			if results[i] == valueI32 {
				values[i] = int32(*v.known)
			} else {
				values[i] = *v.known
			}
		} else {
			values[i] = v.expr
		}
	}
	if len(values) == 1 {
		return values[0]
	}
	return values
}

type instruction struct {
	opcode    byte
	index     uint32
	value     int64
	then      []instruction
	otherwise []instruction
}

type executor struct {
	ctx      context.Context
	maxDepth int
	prune    bool
	model    map[string]any
	explored int
	pruned   int
}

func (e *executor) run(program []instruction, states []symbolicState) ([]symbolicState, error) {
	for _, in := range program {
		next := make([]symbolicState, 0, len(states))
		for _, state := range states {
			if err := e.ctx.Err(); err != nil {
				return nil, err
			}
			state.depth++
			if state.depth > e.maxDepth {
				e.pruned++
				continue
			}
			if in.opcode == 0x04 { // if
				condition, err := state.pop()
				if err != nil {
					return nil, err
				}
				for _, branch := range []struct {
					yes  bool
					code []instruction
				}{{true, in.then}, {false, in.otherwise}} {
					candidate := state.clone()
					constraint := condition.condition()
					if !branch.yes {
						constraint = "(not " + constraint + ")"
					}
					candidate.constraints = append(candidate.constraints, constraint)
					if e.explored >= maxSymbolicExplored {
						return nil, fmt.Errorf("symbolic execution exceeded the %d path limit", maxSymbolicExplored)
					}
					e.explored++
					if e.prune {
						ok, err := e.feasible(candidate.constraints)
						if err != nil {
							return nil, err
						}
						if !ok {
							e.pruned++
							continue
						}
					}
					finished, err := e.run(branch.code, []symbolicState{candidate})
					if err != nil {
						return nil, err
					}
					next = append(next, finished...)
				}
				continue
			}
			if err := executeInstruction(&state, in); err != nil {
				return nil, err
			}
			next = append(next, state)
		}
		states = next
	}
	return states, nil
}

func (e *executor) feasible(constraints []string) (bool, error) {
	feasible, _, err := e.feasibility(constraints)
	return feasible, err
}

func (e *executor) feasibility(constraints []string) (bool, map[string]any, error) {
	if len(constraints) == 0 {
		return true, nil, nil
	}
	var b strings.Builder
	for _, c := range constraints {
		fmt.Fprintf(&b, "(assert %s)\n", c)
	}
	solution, err := z3.SolveConstraintsCtx(e.ctx, b.String(), e.model)
	if err != nil {
		return false, nil, fmt.Errorf("check path feasibility: %w", err)
	}
	switch solution.Sat {
	case "sat":
		return true, solution.Model, nil
	case "unsat":
		return false, nil, nil
	default:
		return false, nil, fmt.Errorf("check path feasibility: z3 returned %q", solution.Sat)
	}
}

func executeInstruction(s *symbolicState, in instruction) error {
	switch in.opcode {
	case 0x20: // local.get
		if int(in.index) >= len(s.locals) {
			return fmt.Errorf("local index %d out of range", in.index)
		}
		s.stack = append(s.stack, s.locals[in.index])
	case 0x21, 0x22: // local.set / local.tee
		v, err := s.pop()
		if err != nil {
			return err
		}
		if int(in.index) >= len(s.locals) {
			return fmt.Errorf("local index %d out of range", in.index)
		}
		s.locals[in.index] = v
		if in.opcode == 0x22 {
			s.stack = append(s.stack, v)
		}
	case 0x41:
		s.stack = append(s.stack, typedConstant(in.value, 32))
	case 0x42:
		s.stack = append(s.stack, typedConstant(in.value, 64))
	case 0x45, 0x50: // i32.eqz / i64.eqz
		v, err := s.pop()
		if err != nil {
			return err
		}
		if (in.opcode == 0x45 && v.bits != 32) || (in.opcode == 0x50 && v.bits != 64) {
			return fmt.Errorf("eqz operand has width %d", v.bits)
		}
		var known *bool
		if v.known != nil {
			value := *v.known == 0
			known = &value
		}
		s.stack = append(s.stack, boolValue("(= "+v.expr+" "+v.zeroExpr()+")", known))
	case 0x46, 0x47, 0x48, 0x49, 0x4a, 0x4b, 0x4c, 0x4d, 0x4e, 0x4f,
		0x51, 0x52, 0x53, 0x54, 0x55, 0x56, 0x57, 0x58, 0x59, 0x5a:
		b, err := s.pop()
		if err != nil {
			return err
		}
		a, err := s.pop()
		if err != nil {
			return err
		}
		if a.bits == 0 || a.bits != b.bits {
			return fmt.Errorf("comparison operands have incompatible widths %d and %d", a.bits, b.bits)
		}
		op, ok := comparisonOperator(in.opcode)
		if !ok {
			return fmt.Errorf("unsupported comparison opcode 0x%x", in.opcode)
		}
		var known *bool
		if a.known != nil && b.known != nil {
			value := compare(in.opcode, *a.known, *b.known, a.bits)
			known = &value
		}
		s.stack = append(s.stack, boolValue("("+op+" "+a.expr+" "+b.expr+")", known))
	case 0x6a, 0x6b, 0x6c, 0x7c, 0x7d, 0x7e: // i32/i64 add/sub/mul
		b, err := s.pop()
		if err != nil {
			return err
		}
		a, err := s.pop()
		if err != nil {
			return err
		}
		bits, ok := arithmeticWidth(in.opcode)
		if !ok || a.bits != bits || b.bits != bits {
			return fmt.Errorf("arithmetic operands have invalid widths %d and %d", a.bits, b.bits)
		}
		op := map[byte]string{0x6a: "bvadd", 0x6b: "bvsub", 0x6c: "bvmul", 0x7c: "bvadd", 0x7d: "bvsub", 0x7e: "bvmul"}[in.opcode]
		if a.known != nil && b.known != nil {
			s.stack = append(s.stack, typedConstant(wrappedArithmetic(*a.known, *b.known, in.opcode, bits), bits))
		} else {
			s.stack = append(s.stack, symbolicValue{expr: "(" + op + " " + a.expr + " " + b.expr + ")", bits: bits})
		}
	case 0x1a:
		_, err := s.pop()
		return err // drop
	default:
		return fmt.Errorf("unsupported symbolic wasm opcode 0x%x", in.opcode)
	}
	return nil
}

func wrappedArithmetic(a, b int64, opcode, bits byte) int64 {
	var value uint64
	switch opcode {
	case 0x6a, 0x7c:
		value = uint64(a) + uint64(b)
	case 0x6b, 0x7d:
		value = uint64(a) - uint64(b)
	default:
		value = uint64(a) * uint64(b)
	}
	if bits == 32 {
		return int64(int32(uint32(value)))
	}
	return int64(value)
}

func boolValue(expr string, value *bool) symbolicValue {
	if value != nil {
		known := int64(0)
		if *value {
			known = 1
		}
		return symbolicValue{expr: fmt.Sprintf("%t", *value), known: &known, boolean: true}
	}
	return symbolicValue{expr: expr, boolean: true}
}
func comparisonOperator(op byte) (string, bool) {
	operators := map[byte]string{
		0x46: "=", 0x47: "distinct", 0x48: "bvslt", 0x49: "bvult",
		0x4a: "bvsgt", 0x4b: "bvugt", 0x4c: "bvsle", 0x4d: "bvule",
		0x4e: "bvsge", 0x4f: "bvuge", 0x51: "=", 0x52: "distinct",
		0x53: "bvslt", 0x54: "bvult", 0x55: "bvsgt", 0x56: "bvugt",
		0x57: "bvsle", 0x58: "bvule", 0x59: "bvsge", 0x5a: "bvuge",
	}
	operator, ok := operators[op]
	return operator, ok
}

func arithmeticWidth(op byte) (byte, bool) {
	switch op {
	case 0x6a, 0x6b, 0x6c:
		return 32, true
	case 0x7c, 0x7d, 0x7e:
		return 64, true
	default:
		return 0, false
	}
}

func compare(op byte, a, b int64, bits byte) bool {
	unsignedA, unsignedB := uint64(a), uint64(b)
	if bits < 64 {
		mask := (uint64(1) << bits) - 1
		unsignedA &= mask
		unsignedB &= mask
	}
	switch op {
	case 0x46, 0x51:
		return a == b
	case 0x47, 0x52:
		return a != b
	case 0x48, 0x53:
		return a < b
	case 0x49, 0x54:
		return unsignedA < unsignedB
	case 0x4a, 0x55:
		return a > b
	case 0x4b, 0x56:
		return unsignedA > unsignedB
	case 0x4c, 0x57:
		return a <= b
	case 0x4d, 0x58:
		return unsignedA <= unsignedB
	case 0x4e, 0x59:
		return a >= b
	default:
		return unsignedA >= unsignedB
	}
}

func wasmIntegerWidth(typ byte) (byte, bool) {
	switch typ {
	case valueI32:
		return 32, true
	case valueI64:
		return 64, true
	default:
		return 0, false
	}
}

const (
	valueI32 byte = 0x7f
	valueI64 byte = 0x7e
)

type wasmFunction struct {
	params, results, locals []byte
	body                    []instruction
}

func parseExportedFunction(wasm []byte, entrypoint string) (wasmFunction, error) {
	if len(wasm) < 8 || string(wasm[:4]) != "\x00asm" || string(wasm[4:8]) != "\x01\x00\x00\x00" {
		return wasmFunction{}, fmt.Errorf("invalid wasm module")
	}
	types := [][]byte{}
	functions := []uint32{}
	codes := [][]byte{}
	exports := map[string]uint32{}
	imported := uint32(0)
	p := 8
	for p < len(wasm) {
		id := wasm[p]
		p++
		n, next, err := readU32(wasm, p)
		if err != nil {
			return wasmFunction{}, err
		}
		p = next
		if int(n) > len(wasm)-p {
			return wasmFunction{}, fmt.Errorf("truncated wasm section")
		}
		section := wasm[p : p+int(n)]
		p += int(n)
		switch id {
		case 1:
			var err error
			types, err = parseTypes(section)
			if err != nil {
				return wasmFunction{}, err
			}
		case 2:
			imported, err = parseImportedFunctionCount(section)
			if err != nil {
				return wasmFunction{}, err
			}
		case 3:
			functions, err = parseU32Vector(section)
			if err != nil {
				return wasmFunction{}, err
			}
		case 7:
			exports, err = parseExports(section)
			if err != nil {
				return wasmFunction{}, err
			}
		case 10:
			codes, err = parseCodes(section)
			if err != nil {
				return wasmFunction{}, err
			}
		}
	}
	idx, ok := exports[entrypoint]
	if !ok {
		return wasmFunction{}, fmt.Errorf("entrypoint %q is not an exported function", entrypoint)
	}
	if idx < imported || int(idx-imported) >= len(functions) || int(idx-imported) >= len(codes) {
		return wasmFunction{}, fmt.Errorf("entrypoint %q uses an unsupported imported function", entrypoint)
	}
	typeIndex := functions[idx-imported]
	if int(typeIndex) >= len(types) {
		return wasmFunction{}, fmt.Errorf("invalid entrypoint type")
	}
	typeSig := types[typeIndex]
	paramsCount := int(typeSig[0])
	resultsOffset := 1 + paramsCount
	params := typeSig[1:resultsOffset]
	results := typeSig[resultsOffset+1:]
	locals, body, err := parseCode(codes[idx-imported])
	if err != nil {
		return wasmFunction{}, err
	}
	return wasmFunction{params: params, results: results, locals: locals, body: body}, nil
}

// parseImportedFunctionCount returns the number of function imports in an
// import section. Only function imports occupy function-index space; tables,
// memories, and globals must still be consumed but do not offset local
// function indices.
func parseImportedFunctionCount(b []byte) (uint32, error) {
	count, p, err := readU32(b, 0)
	if err != nil {
		return 0, err
	}
	if count > maxWasmVectorItems || int(count) > len(b)-p {
		return 0, fmt.Errorf("too many wasm imports")
	}
	var functions uint32
	for range count {
		for range 2 { // module and import names
			n, q, err := readU32(b, p)
			if err != nil || int(n) > len(b)-q {
				return 0, fmt.Errorf("invalid wasm import")
			}
			p = q + int(n)
		}
		if p >= len(b) {
			return 0, fmt.Errorf("truncated wasm import")
		}
		switch b[p] {
		case 0: // function: type index
			functions++
			_, p, err = readU32(b, p+1)
		case 1: // table: reference type followed by limits
			if p+1 >= len(b) {
				return 0, fmt.Errorf("truncated wasm table import")
			}
			p, err = skipWasmLimits(b, p+2)
		case 2: // memory: limits
			p, err = skipWasmLimits(b, p+1)
		case 3: // global: value type and mutability
			if p+2 >= len(b) {
				return 0, fmt.Errorf("truncated wasm global import")
			}
			p += 3
		default:
			return 0, fmt.Errorf("invalid wasm import kind")
		}
		if err != nil {
			return 0, err
		}
	}
	if p != len(b) {
		return 0, fmt.Errorf("invalid wasm import section")
	}
	return functions, nil
}

func skipWasmLimits(b []byte, p int) (int, error) {
	flags, p, err := readU32(b, p)
	if err != nil {
		return 0, err
	}
	if flags > 1 {
		return 0, fmt.Errorf("unsupported wasm limits")
	}
	_, p, err = readU32(b, p)
	if err != nil || flags == 0 {
		return p, err
	}
	_, p, err = readU32(b, p)
	return p, err
}

func parseTypes(b []byte) ([][]byte, error) {
	count, p, err := readU32(b, 0)
	if err != nil {
		return nil, err
	}
	if count > maxWasmVectorItems || int(count) > len(b) {
		return nil, fmt.Errorf("too many wasm types")
	}
	out := make([][]byte, 0, int(count))
	for range count {
		if p >= len(b) || b[p] != 0x60 {
			return nil, fmt.Errorf("invalid wasm function type")
		}
		p++
		pc, n, err := readU32(b, p)
		if err != nil {
			return nil, err
		}
		p = n
		if pc > maxWasmParams || int(pc) > len(b)-p {
			return nil, fmt.Errorf("truncated function params")
		}
		sig := []byte{byte(pc)}
		sig = append(sig, b[p:p+int(pc)]...)
		p += int(pc)
		rc, n, err := readU32(b, p)
		if err != nil {
			return nil, err
		}
		p = n
		if rc > maxWasmParams || int(rc) > len(b)-p {
			return nil, fmt.Errorf("truncated function results")
		}
		sig = append(sig, byte(rc))
		sig = append(sig, b[p:p+int(rc)]...)
		p += int(rc)
		out = append(out, sig)
	}
	return out, nil
}
func parseU32Vector(b []byte) ([]uint32, error) {
	count, p, err := readU32(b, 0)
	if err != nil {
		return nil, err
	}
	if count > maxWasmVectorItems || int(count) > len(b)-p {
		return nil, fmt.Errorf("too many wasm vector items")
	}
	out := make([]uint32, 0, int(count))
	for range count {
		v, n, err := readU32(b, p)
		if err != nil {
			return nil, err
		}
		p = n
		out = append(out, v)
	}
	return out, nil
}
func parseExports(b []byte) (map[string]uint32, error) {
	count, p, err := readU32(b, 0)
	if err != nil {
		return nil, err
	}
	if count > maxWasmVectorItems || int(count) > len(b)-p {
		return nil, fmt.Errorf("too many wasm exports")
	}
	out := make(map[string]uint32, int(count))
	for range count {
		n, q, err := readU32(b, p)
		if err != nil || int(n) > len(b)-q {
			return nil, fmt.Errorf("invalid wasm export")
		}
		name := string(b[q : q+int(n)])
		p = q + int(n)
		if p >= len(b) {
			return nil, fmt.Errorf("truncated wasm export")
		}
		kind := b[p]
		p++
		idx, q, err := readU32(b, p)
		if err != nil {
			return nil, err
		}
		p = q
		if kind == 0 {
			out[name] = idx
		}
	}
	return out, nil
}
func parseCodes(b []byte) ([][]byte, error) {
	count, p, err := readU32(b, 0)
	if err != nil {
		return nil, err
	}
	if count > maxWasmVectorItems || int(count) > len(b)-p {
		return nil, fmt.Errorf("too many wasm code bodies")
	}
	out := make([][]byte, 0, int(count))
	for range count {
		n, q, err := readU32(b, p)
		if err != nil || int(n) > len(b)-q {
			return nil, fmt.Errorf("invalid wasm code")
		}
		out = append(out, b[q:q+int(n)])
		p = q + int(n)
	}
	return out, nil
}
func parseCode(b []byte) ([]byte, []instruction, error) {
	groups, p, err := readU32(b, 0)
	if err != nil {
		return nil, nil, err
	}
	if groups > maxWasmVectorItems || int(groups) > len(b)-p {
		return nil, nil, fmt.Errorf("too many wasm local groups")
	}
	var locals []byte
	for range groups {
		n, q, err := readU32(b, p)
		if err != nil || q >= len(b) || n > maxWasmLocals || uint64(len(locals))+uint64(n) > maxWasmLocals {
			return nil, nil, fmt.Errorf("invalid wasm locals")
		}
		p = q
		locals = append(locals, bytes.Repeat([]byte{b[p]}, int(n))...)
		p++
	}
	body, p, stop, err := parseInstructions(b, p)
	if err != nil {
		return nil, nil, err
	}
	if stop != 0x0b || p != len(b) {
		return nil, nil, fmt.Errorf("invalid wasm function body")
	}
	return locals, body, nil
}
func parseInstructions(b []byte, p int) ([]instruction, int, byte, error) {
	return parseInstructionsWithBudget(b, p, new(int), 0)
}

func parseInstructionsWithBudget(b []byte, p int, instructionCount *int, controlDepth int) ([]instruction, int, byte, error) {
	var out []instruction
	for p < len(b) {
		op := b[p]
		p++
		if op == 0x0b || op == 0x05 {
			return out, p, op, nil
		}
		in := instruction{opcode: op}
		(*instructionCount)++
		if *instructionCount > maxWasmInstructions {
			return nil, p, 0, fmt.Errorf("too many wasm instructions")
		}
		var err error
		switch op {
		case 0x04:
			if controlDepth >= maxWasmControlDepth {
				return nil, p, 0, fmt.Errorf("wasm control nesting exceeds %d", maxWasmControlDepth)
			}
			if p >= len(b) {
				return nil, p, 0, fmt.Errorf("truncated if")
			}
			p++
			var stop byte
			in.then, p, stop, err = parseInstructionsWithBudget(b, p, instructionCount, controlDepth+1)
			if err != nil {
				return nil, p, 0, err
			}
			if stop == 0x05 {
				in.otherwise, p, stop, err = parseInstructionsWithBudget(b, p, instructionCount, controlDepth+1)
				if err != nil {
					return nil, p, 0, err
				}
				if stop != 0x0b {
					return nil, p, 0, fmt.Errorf("invalid wasm if: else must end with end opcode")
				}
			}
		case 0x20, 0x21, 0x22:
			in.index, p, err = readU32(b, p)
		case 0x41, 0x42:
			in.value, p, err = readS64(b, p)
		}
		if err != nil {
			return nil, p, 0, err
		}
		out = append(out, in)
	}
	return nil, p, 0, fmt.Errorf("unterminated wasm instructions")
}
func readU32(b []byte, p int) (uint32, int, error) {
	var v uint32
	for i := 0; i < 5; i++ {
		if p >= len(b) {
			return 0, p, fmt.Errorf("truncated wasm integer")
		}
		x := b[p]
		p++
		v |= uint32(x&127) << uint(7*i)
		if x&128 == 0 {
			return v, p, nil
		}
	}
	return 0, p, fmt.Errorf("invalid wasm integer")
}
func readS64(b []byte, p int) (int64, int, error) {
	var v int64
	var shift uint
	for {
		if p >= len(b) || shift >= 64 {
			return 0, p, fmt.Errorf("invalid wasm signed integer")
		}
		x := b[p]
		p++
		v |= int64(x&127) << shift
		shift += 7
		if x&128 == 0 {
			if shift < 64 && x&64 != 0 {
				v |= ^int64(0) << shift
			}
			return v, p, nil
		}
	}
}

// SymbolicHandler returns the POST /v1/verify/symbolic endpoint handler.
func SymbolicHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		r.Body = http.MaxBytesReader(w, r.Body, maxSymbolicRequestBytes)
		var input SymbolicInput
		decoder := json.NewDecoder(r.Body)
		decoder.UseNumber()
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&input); err != nil {
			http.Error(w, fmt.Sprintf("invalid request: %v", err), http.StatusBadRequest)
			return
		}
		if err := decoder.Decode(&struct{}{}); err != io.EOF {
			http.Error(w, "invalid request: multiple JSON values", http.StatusBadRequest)
			return
		}
		result, err := Run(r.Context(), input)
		if err != nil {
			http.Error(w, fmt.Sprintf("invalid symbolic execution request: %v", err), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(symbolicHTTPResponse(result))
	}
}

// symbolicHTTPResponse keeps the v12 HTTP contract independent of the
// long-standing Go SymbolicPath API. In particular, the endpoint returns one
// expression per constraints array element while Go callers retain the single
// SMT-LIB constraint string required by existing solver integrations.
type symbolicHTTPPath struct {
	ID          string   `json:"id"`
	Feasible    bool     `json:"feasible"`
	Constraints []string `json:"constraints"`
	Output      any      `json:"output"`
}

type symbolicHTTPResult struct {
	Paths      []symbolicHTTPPath `json:"paths"`
	Explored   int                `json:"explored"`
	Pruned     int                `json:"pruned"`
	DecisionID string             `json:"decision_id"`
}

func symbolicHTTPResponse(result SymbolicResult) symbolicHTTPResult {
	paths := make([]symbolicHTTPPath, len(result.Paths))
	for i, path := range result.Paths {
		paths[i] = symbolicHTTPPath{
			ID:          path.ID,
			Feasible:    path.Feasible,
			Constraints: append([]string(nil), path.constraints...),
			Output:      path.Output,
		}
	}
	return symbolicHTTPResult{
		Paths:      paths,
		Explored:   result.Explored,
		Pruned:     result.Pruned,
		DecisionID: result.DecisionID,
	}
}
