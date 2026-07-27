package verify

// This file implements a minimal WebAssembly binary decoder sufficient for
// symbolic execution of self-contained modules. It parses the type, import,
// function, export, and code sections into a module value that the symbolic
// engine interprets. Other sections (table, memory, global, element, data,
// custom) are recognised only enough to skip them: the symbolic engine
// reasons over a module's own functions and does not model imported APIs or
// linear memory.
//
// The decoder intentionally supports only the value type and opcode subset
// that the symbolic execution engine consumes (see symbolic.go). Unsupported
// opcodes surface as a typed error rather than silent misinterpretation.

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

const (
	wasmMagic   = "\x00asm"
	wasmVersion = 1
)

// WebAssembly section identifiers.
const (
	sectionCustom  = 0
	sectionType    = 1
	sectionImport  = 2
	sectionFunction = 3
	sectionTable   = 4
	sectionMemory  = 5
	sectionGlobal  = 6
	sectionExport  = 7
	sectionStart   = 8
	sectionElement = 9
	sectionCode    = 10
	sectionData    = 11
)

// valType identifies a WebAssembly value type. Only i32 is exercised by the
// symbolic engine; the others are parsed for completeness.
type valType byte

const (
	valI32 valType = 0x7f
	valI64 valType = 0x7e
	valF32 valType = 0x7d
	valF64 valType = 0x7c
)

func (t valType) String() string {
	switch t {
	case valI32:
		return "i32"
	case valI64:
		return "i64"
	case valF32:
		return "f32"
	case valF64:
		return "f64"
	default:
		return fmt.Sprintf("valType(%#x)", byte(t))
	}
}

// funcType is a function signature: ordered parameter and result types.
type funcType struct {
	params  []valType
	results []valType
}

// wasmExport is a single WebAssembly export. Only function exports (kind 0)
// are consumed by the symbolic engine.
type wasmExport struct {
	name  string
	kind  byte // 0 = function
	index uint32
}

// funcCode is a defined function's expanded local types and raw instruction
// bytes (locals vector followed by the expression body, excluding nothing —
// the trailing 0x0b end is included).
type funcCode struct {
	localTypes []valType
	body       []byte
}

// module is the decoded representation of a self-contained WASM module.
type module struct {
	types       []funcType
	funcTypes   []uint32 // per defined function: index into types
	exports     map[string]wasmExport
	codes       []funcCode
	importFuncs int // imported functions shift the function index space
}

// decodeWasm parses a WebAssembly binary module into a module value.
func decodeWasm(data []byte) (*module, error) {
	if len(data) < 8 {
		return nil, errors.New("wasm: module too short")
	}
	if string(data[:4]) != wasmMagic {
		return nil, errors.New("wasm: bad magic")
	}
	if binary.LittleEndian.Uint32(data[4:8]) != wasmVersion {
		return nil, errors.New("wasm: unsupported version")
	}
	r := &byteReader{b: data[8:]}
	m := &module{exports: map[string]wasmExport{}}
	for !r.eof() {
		id, err := r.byte()
		if err != nil {
			return nil, err
		}
		size, err := r.u32()
		if err != nil {
			return nil, err
		}
		payload, err := r.take(int(size))
		if err != nil {
			return nil, fmt.Errorf("wasm: section %d: %w", id, err)
		}
		sr := &byteReader{b: payload}
		switch id {
		case sectionType:
			if err := m.decodeTypes(sr); err != nil {
				return nil, fmt.Errorf("wasm: type section: %w", err)
			}
		case sectionImport:
			if err := m.decodeImports(sr); err != nil {
				return nil, fmt.Errorf("wasm: import section: %w", err)
			}
		case sectionFunction:
			if err := m.decodeFunctions(sr); err != nil {
				return nil, fmt.Errorf("wasm: function section: %w", err)
			}
		case sectionExport:
			if err := m.decodeExports(sr); err != nil {
				return nil, fmt.Errorf("wasm: export section: %w", err)
			}
		case sectionCode:
			if err := m.decodeCodes(sr); err != nil {
				return nil, fmt.Errorf("wasm: code section: %w", err)
			}
		default:
			// Skip sections the symbolic engine does not need.
		}
	}
	return m, nil
}

func (m *module) decodeTypes(r *byteReader) error {
	n, err := r.u32()
	if err != nil {
		return err
	}
	m.types = make([]funcType, n)
	for i := uint32(0); i < n; i++ {
		tag, err := r.byte()
		if err != nil {
			return err
		}
		if tag != 0x60 {
			return fmt.Errorf("type %d: bad functype tag %#x", i, tag)
		}
		params, err := r.valTypes()
		if err != nil {
			return err
		}
		results, err := r.valTypes()
		if err != nil {
			return err
		}
		m.types[i] = funcType{params: params, results: results}
	}
	return nil
}

func (m *module) decodeImports(r *byteReader) error {
	n, err := r.u32()
	if err != nil {
		return err
	}
	for i := uint32(0); i < n; i++ {
		if _, err := r.name(); err != nil {
			return err
		}
		if _, err := r.name(); err != nil {
			return err
		}
		kind, err := r.byte()
		if err != nil {
			return err
		}
		switch kind {
		case 0x00: // function import
			if _, err := r.u32(); err != nil {
				return err
			}
			m.importFuncs++
		case 0x01: // table import
			if err := skipTableType(r); err != nil {
				return err
			}
		case 0x02: // memory import
			if err := skipMemoryType(r); err != nil {
				return err
			}
		case 0x03: // global import
			if _, err := r.byte(); err != nil { // valtype
				return err
			}
			if _, err := r.byte(); err != nil { // mut
				return err
			}
		default:
			return fmt.Errorf("import %d: bad kind %#x", i, kind)
		}
	}
	return nil
}

func (m *module) decodeFunctions(r *byteReader) error {
	n, err := r.u32()
	if err != nil {
		return err
	}
	m.funcTypes = make([]uint32, n)
	for i := uint32(0); i < n; i++ {
		idx, err := r.u32()
		if err != nil {
			return err
		}
		m.funcTypes[i] = idx
	}
	return nil
}

func (m *module) decodeExports(r *byteReader) error {
	n, err := r.u32()
	if err != nil {
		return err
	}
	for i := uint32(0); i < n; i++ {
		name, err := r.name()
		if err != nil {
			return err
		}
		kind, err := r.byte()
		if err != nil {
			return err
		}
		idx, err := r.u32()
		if err != nil {
			return err
		}
		m.exports[name] = wasmExport{name: name, kind: kind, index: idx}
	}
	return nil
}

func (m *module) decodeCodes(r *byteReader) error {
	n, err := r.u32()
	if err != nil {
		return err
	}
	m.codes = make([]funcCode, n)
	for i := uint32(0); i < n; i++ {
		size, err := r.u32()
		if err != nil {
			return err
		}
		body, err := r.take(int(size))
		if err != nil {
			return err
		}
		br := &byteReader{b: body}
		localCount, err := br.u32()
		if err != nil {
			return fmt.Errorf("code %d: locals: %w", i, err)
		}
		var localTypes []valType
		for j := uint32(0); j < localCount; j++ {
			cnt, err := br.u32()
			if err != nil {
				return fmt.Errorf("code %d: local decl: %w", i, err)
			}
			vt, err := br.byte()
			if err != nil {
				return fmt.Errorf("code %d: local type: %w", i, err)
			}
			for k := uint32(0); k < cnt; k++ {
				localTypes = append(localTypes, valType(vt))
			}
		}
		m.codes[i] = funcCode{localTypes: localTypes, body: br.remaining()}
	}
	return nil
}

// skipTableType advances r past a table type (element type + limits).
func skipTableType(r *byteReader) error {
	if _, err := r.byte(); err != nil { // element type
		return err
	}
	return skipLimits(r)
}

// skipMemoryType advances r past a memory type (limits).
func skipMemoryType(r *byteReader) error {
	return skipLimits(r)
}

// skipLimits advances r past a limits structure (min, optional max).
func skipLimits(r *byteReader) error {
	flags, err := r.byte()
	if err != nil {
		return err
	}
	if _, err := r.u32(); err != nil { // min
		return err
	}
	if flags&0x01 != 0 {
		if _, err := r.u32(); err != nil { // max
			return err
		}
	}
	return nil
}

// byteReader is a tiny cursor over a byte slice with LEB128 decoders.
type byteReader struct {
	b   []byte
	pos int
}

func (r *byteReader) eof() bool { return r.pos >= len(r.b) }

func (r *byteReader) remaining() []byte { return r.b[r.pos:] }

func (r *byteReader) byte() (byte, error) {
	if r.pos >= len(r.b) {
		return 0, io.ErrUnexpectedEOF
	}
	v := r.b[r.pos]
	r.pos++
	return v, nil
}

func (r *byteReader) peekByte() (byte, error) {
	if r.pos >= len(r.b) {
		return 0, io.ErrUnexpectedEOF
	}
	return r.b[r.pos], nil
}

func (r *byteReader) take(n int) ([]byte, error) {
	if n < 0 || r.pos+n > len(r.b) {
		return nil, io.ErrUnexpectedEOF
	}
	s := r.b[r.pos : r.pos+n]
	r.pos += n
	return s, nil
}

func (r *byteReader) name() (string, error) {
	n, err := r.u32()
	if err != nil {
		return "", err
	}
	b, err := r.take(int(n))
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func (r *byteReader) valTypes() ([]valType, error) {
	n, err := r.u32()
	if err != nil {
		return nil, err
	}
	out := make([]valType, n)
	for i := uint32(0); i < n; i++ {
		v, err := r.byte()
		if err != nil {
			return nil, err
		}
		out[i] = valType(v)
	}
	return out, nil
}

// u32 decodes an unsigned LEB128 integer.
func (r *byteReader) u32() (uint32, error) {
	var result uint32
	var shift uint
	for {
		b, err := r.byte()
		if err != nil {
			return 0, err
		}
		result |= uint32(b&0x7f) << shift
		if b&0x80 == 0 {
			return result, nil
		}
		shift += 7
		if shift >= 32 {
			return 0, errors.New("leb128: u32 overflow")
		}
	}
}

// i32 decodes a signed LEB128 integer.
func (r *byteReader) i32() (int32, error) {
	var result int32
	var shift uint
	for {
		b, err := r.byte()
		if err != nil {
			return 0, err
		}
		result |= int32(b&0x7f) << shift
		if b&0x80 == 0 {
			if b&0x40 != 0 {
				result |= int32(-1) << shift // sign-extend
			}
			return result, nil
		}
		shift += 7
		if shift > 32 {
			return 0, errors.New("leb128: i32 overflow")
		}
	}
}
