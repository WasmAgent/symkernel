// Package verify provides HTTP handlers for symkerneld verification endpoints.
package verify

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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

// SymbolicInput is the request payload for POST /v1/verify/symbolic.
type SymbolicInput struct {
	Module          string `json:"module"`
	Entrypoint      string `json:"entrypoint"`
	MaxDepth        int    `json:"maxDepth"`
	PruneInfeasible bool   `json:"pruneInfeasible"`
}

// SymbolicPath is one terminal path reached by the entrypoint.
type SymbolicPath struct {
	ID          string   `json:"id"`
	Feasible    bool     `json:"feasible"`
	Constraints []string `json:"constraints"`
	Output      any      `json:"output"`
}

// SymbolicResult is the response payload for symbolic verification.
type SymbolicResult struct {
	Paths      []SymbolicPath `json:"paths"`
	Explored   int            `json:"explored"`
	Pruned     int            `json:"pruned"`
	DecisionID string         `json:"decision_id"`
}

// Run symbolically executes the requested Wasm function. Function parameters
// become arg0, arg1, ... integer symbols. It deliberately supports the core
// integer/control-flow instruction set used for decisions; unsupported Wasm
// instructions fail explicitly instead of silently falling back to concrete
// execution.
func Run(ctx context.Context, in SymbolicInput) (SymbolicResult, error) {
	if strings.TrimSpace(in.Module) == "" {
		return SymbolicResult{}, fmt.Errorf("module is required")
	}
	if strings.TrimSpace(in.Entrypoint) == "" {
		return SymbolicResult{}, fmt.Errorf("entrypoint is required")
	}
	if len(in.Entrypoint) > maxSymbolicEntrypointLen {
		return SymbolicResult{}, fmt.Errorf("entrypoint exceeds %d bytes", maxSymbolicEntrypointLen)
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
	if len(in.Module) > maxEncodedModule {
		return SymbolicResult{}, fmt.Errorf("module exceeds %d decoded bytes", maxSymbolicModuleBytes)
	}
	wasm, err := base64.StdEncoding.DecodeString(in.Module)
	if err != nil {
		return SymbolicResult{}, fmt.Errorf("decode module: %w", err)
	}
	if len(wasm) > maxSymbolicModuleBytes {
		return SymbolicResult{}, fmt.Errorf("module exceeds %d bytes", maxSymbolicModuleBytes)
	}
	fn, err := parseExportedFunction(wasm, in.Entrypoint)
	if err != nil {
		return SymbolicResult{}, err
	}

	initial := symbolicState{locals: make([]symbolicValue, len(fn.params)+len(fn.locals))}
	model := make(map[string]any, len(fn.params))
	for i, typ := range fn.params {
		if typ != valueI32 && typ != valueI64 {
			return SymbolicResult{}, fmt.Errorf("entrypoint parameter %d has unsupported type 0x%x", i, typ)
		}
		name := fmt.Sprintf("arg%d", i)
		initial.locals[i] = symbolicValue{expr: name}
		model[name] = "Int"
	}

	exec := executor{ctx: ctx, maxDepth: in.MaxDepth, prune: in.PruneInfeasible, model: model, explored: 1}
	states, err := exec.run(fn.body, []symbolicState{initial})
	if err != nil {
		return SymbolicResult{}, err
	}
	paths := make([]SymbolicPath, 0, len(states))
	for _, state := range states {
		feasible, err := exec.feasible(state.constraints)
		if err != nil {
			return SymbolicResult{}, err
		}
		if !feasible && in.PruneInfeasible {
			exec.pruned++
			continue
		}
		if len(state.stack) < len(fn.results) {
			return SymbolicResult{}, fmt.Errorf("entrypoint %q did not produce its declared results", in.Entrypoint)
		}
		paths = append(paths, SymbolicPath{
			ID: uuid.NewString(), Feasible: feasible, Constraints: state.constraints,
			Output: state.output(fn.results),
		})
	}
	return SymbolicResult{Paths: paths, Explored: exec.explored, Pruned: exec.pruned, DecisionID: uuid.NewString()}, nil
}

type symbolicValue struct {
	expr    string
	known   *int64
	boolean bool
}

func constant(v int64) symbolicValue { return symbolicValue{expr: fmt.Sprintf("%d", v), known: &v} }

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
	return "(not (= " + v.expr + " 0))"
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
	if len(constraints) == 0 {
		return true, nil
	}
	var b strings.Builder
	for _, c := range constraints {
		fmt.Fprintf(&b, "(assert %s)\n", c)
	}
	solution, err := z3.SolveConstraintsCtx(e.ctx, b.String(), e.model)
	if err != nil {
		return false, fmt.Errorf("check path feasibility: %w", err)
	}
	switch solution.Sat {
	case "sat":
		return true, nil
	case "unsat":
		return false, nil
	default:
		return false, fmt.Errorf("check path feasibility: z3 returned %q", solution.Sat)
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
	case 0x41, 0x42:
		s.stack = append(s.stack, constant(in.value)) // i32/i64.const
	case 0x45: // i32.eqz
		v, err := s.pop()
		if err != nil {
			return err
		}
		var known *bool
		if v.known != nil {
			value := *v.known == 0
			known = &value
		}
		s.stack = append(s.stack, boolValue("(= "+v.expr+" 0)", known))
	case 0x46, 0x47, 0x48, 0x4a, 0x4c, 0x4e: // eq, ne, lt_s, gt_s, le_s, ge_s
		b, err := s.pop()
		if err != nil {
			return err
		}
		a, err := s.pop()
		if err != nil {
			return err
		}
		op := map[byte]string{0x46: "=", 0x47: "distinct", 0x48: "<", 0x4a: ">", 0x4c: "<=", 0x4e: ">="}[in.opcode]
		var known *bool
		if a.known != nil && b.known != nil {
			value := compare(in.opcode, *a.known, *b.known)
			known = &value
		}
		s.stack = append(s.stack, boolValue("("+op+" "+a.expr+" "+b.expr+")", known))
	case 0x6a, 0x6b, 0x6c: // i32.add/sub/mul
		b, err := s.pop()
		if err != nil {
			return err
		}
		a, err := s.pop()
		if err != nil {
			return err
		}
		op := map[byte]string{0x6a: "+", 0x6b: "-", 0x6c: "*"}[in.opcode]
		if a.known != nil && b.known != nil {
			switch in.opcode {
			case 0x6a:
				s.stack = append(s.stack, constant(*a.known+*b.known))
			case 0x6b:
				s.stack = append(s.stack, constant(*a.known-*b.known))
			default:
				s.stack = append(s.stack, constant(*a.known**b.known))
			}
			return nil
		}
		s.stack = append(s.stack, symbolicValue{expr: "(" + op + " " + a.expr + " " + b.expr + ")"})
	case 0x1a:
		_, err := s.pop()
		return err // drop
	default:
		return fmt.Errorf("unsupported symbolic wasm opcode 0x%x", in.opcode)
	}
	return nil
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
func compare(op byte, a, b int64) bool {
	switch op {
	case 0x46:
		return a == b
	case 0x47:
		return a != b
	case 0x48:
		return a < b
	case 0x4a:
		return a > b
	case 0x4c:
		return a <= b
	default:
		return a >= b
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
			count, _, err := readU32(section, 0)
			if err != nil {
				return wasmFunction{}, err
			}
			if count > maxWasmVectorItems || imported > maxWasmVectorItems-count {
				return wasmFunction{}, fmt.Errorf("too many wasm imports")
			}
			imported += count // imported functions are unsupported below
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
		locals = append(locals, bytesRepeat(b[p], int(n))...)
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
func bytesRepeat(v byte, n int) []byte {
	r := make([]byte, n)
	for i := range r {
		r[i] = v
	}
	return r
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
			in.then, p, _, err = parseInstructionsWithBudget(b, p, instructionCount, controlDepth+1)
			if err != nil {
				return nil, p, 0, err
			}
			if p > 0 && b[p-1] == 0x05 {
				in.otherwise, p, _, err = parseInstructionsWithBudget(b, p, instructionCount, controlDepth+1)
				if err != nil {
					return nil, p, 0, err
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
		_ = json.NewEncoder(w).Encode(result)
	}
}
