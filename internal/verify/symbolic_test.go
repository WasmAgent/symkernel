package verify

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestRun_ExecutesEntrypoint(t *testing.T) {
	t.Parallel()

	result, err := Run(context.Background(), SymbolicInput{
		Module:     wasmReturningI32(42),
		Entrypoint: "main",
		MaxDepth:   100,
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Explored != 1 || result.Pruned != 0 {
		t.Errorf("bookkeeping = explored:%d pruned:%d, want 1 and 0", result.Explored, result.Pruned)
	}
	if len(result.Paths) != 1 {
		t.Fatalf("paths = %d, want 1", len(result.Paths))
	}
	path := result.Paths[0]
	if !path.Feasible || len(path.Constraints) != 0 || path.Output != int32(42) {
		t.Errorf("path = %+v, want feasible empty-constraint path with output 42", path)
	}
	if _, err := uuid.Parse(path.ID); err != nil {
		t.Errorf("path ID = %q is not a UUID: %v", path.ID, err)
	}
	if _, err := uuid.Parse(result.DecisionID); err != nil {
		t.Errorf("decision ID = %q is not a UUID: %v", result.DecisionID, err)
	}
}

func TestRun_RejectsInvalidInput(t *testing.T) {
	t.Parallel()

	for _, input := range []SymbolicInput{
		{},
		{Module: "not base64", Entrypoint: "main"},
		{Module: wasmReturningI32(1), Entrypoint: "missing"},
		{Module: wasmReturningI32(1), Entrypoint: "main", MaxDepth: -1},
		{Module: wasmReturningI32(1), Entrypoint: "main", MaxDepth: maxSymbolicMaxDepth + 1},
	} {
		if _, err := Run(context.Background(), input); err == nil {
			t.Errorf("Run(%+v) error = nil, want validation error", input)
		}
	}
}

func TestSymbolicComparisonConditionPreservesBoolExpression(t *testing.T) {
	t.Parallel()

	state := symbolicState{locals: []symbolicValue{{expr: "arg0"}}}
	for _, instruction := range []instruction{
		{opcode: 0x20, index: 0},
		{opcode: 0x41, value: 0},
		{opcode: 0x46},
	} {
		if err := executeInstruction(&state, instruction); err != nil {
			t.Fatalf("executeInstruction(%#x) error = %v", instruction.opcode, err)
		}
	}
	condition, err := state.pop()
	if err != nil {
		t.Fatalf("pop() error = %v", err)
	}
	if got, want := condition.condition(), "(= arg0 0)"; got != want {
		t.Errorf("condition() = %q, want %q", got, want)
	}
}

func TestRun_EnforcesGlobalPathLimit(t *testing.T) {
	t.Parallel()

	_, err := Run(context.Background(), SymbolicInput{
		Module:     wasmSequentialBranches(12),
		Entrypoint: "main",
		MaxDepth:   maxSymbolicMaxDepth,
	})
	if err == nil || !strings.Contains(err.Error(), "path limit") {
		t.Fatalf("Run() error = %v, want global path limit error", err)
	}
}

func TestRun_ExploresBranchesAndHonorsControls(t *testing.T) {
	t.Parallel()

	input := SymbolicInput{Module: wasmNestedBranch(), Entrypoint: "main", MaxDepth: 100}
	withoutPruning, err := Run(context.Background(), input)
	if err != nil {
		t.Fatalf("Run() without pruning error = %v", err)
	}
	if withoutPruning.Explored != 5 || withoutPruning.Pruned != 0 || len(withoutPruning.Paths) != 3 {
		t.Fatalf("without pruning = %+v, want five explored and three returned paths", withoutPruning)
	}
	var infeasible int
	for _, path := range withoutPruning.Paths {
		if !path.Feasible {
			infeasible++
		}
	}
	if infeasible != 1 {
		t.Errorf("infeasible paths = %d, want 1", infeasible)
	}

	input.PruneInfeasible = true
	pruned, err := Run(context.Background(), input)
	if err != nil {
		t.Fatalf("Run() with pruning error = %v", err)
	}
	if pruned.Explored != 5 || pruned.Pruned != 1 || len(pruned.Paths) != 2 {
		t.Errorf("with pruning = %+v, want 5 explored, 1 pruned, 2 paths", pruned)
	}

	input.MaxDepth = 1
	depthLimited, err := Run(context.Background(), input)
	if err != nil {
		t.Fatalf("Run() with depth limit error = %v", err)
	}
	if len(depthLimited.Paths) != 0 || depthLimited.Pruned == 0 {
		t.Errorf("depth-limited result = %+v, want no completed paths and pruned work", depthLimited)
	}
}

func TestSymbolicHandler(t *testing.T) {
	t.Parallel()

	body := `{"module":"` + wasmReturningI32(7) + `","entrypoint":"main","maxDepth":100,"pruneInfeasible":true}`
	req := httptest.NewRequest(http.MethodPost, "/v1/verify/symbolic", strings.NewReader(body))
	rec := httptest.NewRecorder()

	SymbolicHandler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if contentType := rec.Header().Get("Content-Type"); contentType != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", contentType)
	}
	var result SymbolicResult
	if err := json.NewDecoder(rec.Body).Decode(&result); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(result.Paths) != 1 || result.Paths[0].Output != float64(7) {
		t.Errorf("response = %+v, want one path with output 7", result)
	}
}

func TestSymbolicHandler_RejectsInvalidRequest(t *testing.T) {
	t.Parallel()

	for _, body := range []string{
		`{"module":"not-base64","entrypoint":"main"}`,
		`{"module":"` + wasmReturningI32(1) + `","entrypoint":"main"} {}`,
	} {
		req := httptest.NewRequest(http.MethodPost, "/v1/verify/symbolic", strings.NewReader(body))
		rec := httptest.NewRecorder()

		SymbolicHandler().ServeHTTP(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Errorf("status = %d, want %d; body = %s", rec.Code, http.StatusBadRequest, rec.Body.String())
		}
	}
}

func TestSymbolicHandler_RejectsOversizedRequest(t *testing.T) {
	t.Parallel()

	req := httptest.NewRequest(http.MethodPost, "/v1/verify/symbolic", strings.NewReader(strings.Repeat("x", maxSymbolicRequestBytes+1)))
	rec := httptest.NewRecorder()

	SymbolicHandler().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

func TestWasmParserRejectsOversizedVectors(t *testing.T) {
	t.Parallel()

	tooMany := []byte{0xff, 0xff, 0xff, 0xff, 0x0f}
	if _, err := parseTypes(tooMany); err == nil {
		t.Error("parseTypes() error = nil, want vector limit error")
	}
	if _, err := parseU32Vector(tooMany); err == nil {
		t.Error("parseU32Vector() error = nil, want vector limit error")
	}
	if _, err := parseExports(tooMany); err == nil {
		t.Error("parseExports() error = nil, want vector limit error")
	}
	if _, err := parseCodes(tooMany); err == nil {
		t.Error("parseCodes() error = nil, want vector limit error")
	}
}

func wasmReturningI32(value byte) string {
	wasm := []byte{
		0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00,
		0x01, 0x05, 0x01, 0x60, 0x00, 0x01, 0x7f,
		0x03, 0x02, 0x01, 0x00,
		0x07, 0x08, 0x01, 0x04, 0x6d, 0x61, 0x69, 0x6e, 0x00, 0x00,
		0x0a, 0x06, 0x01, 0x04, 0x00, 0x41, value, 0x0b,
	}
	return base64.StdEncoding.EncodeToString(wasm)
}

func wasmNestedBranch() string {
	wasm := []byte{
		0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00,
		0x01, 0x06, 0x01, 0x60, 0x01, 0x7f, 0x01, 0x7f,
		0x03, 0x02, 0x01, 0x00,
		0x07, 0x08, 0x01, 0x04, 0x6d, 0x61, 0x69, 0x6e, 0x00, 0x00,
		0x0a, 0x16, 0x01, 0x14, 0x00,
		0x20, 0x00, 0x04, 0x7f,
		0x20, 0x00, 0x04, 0x7f, 0x41, 0x01, 0x05, 0x41, 0x02, 0x0b,
		0x05, 0x41, 0x03, 0x0b, 0x0b,
	}
	return base64.StdEncoding.EncodeToString(wasm)
}

func wasmSequentialBranches(count int) string {
	body := []byte{0x00} // no locals
	for range count {
		body = append(body, 0x20, 0x00, 0x04, 0x7f, 0x41, 0x01, 0x05, 0x41, 0x02, 0x0b)
	}
	body = append(body, 0x0b)

	code := append([]byte{0x01}, wasmU32(uint32(len(body)))...)
	code = append(code, body...)
	wasm := []byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00}
	wasm = append(wasm, wasmSection(1, []byte{0x01, 0x60, 0x01, 0x7f, 0x01, 0x7f})...)
	wasm = append(wasm, wasmSection(3, []byte{0x01, 0x00})...)
	wasm = append(wasm, wasmSection(7, []byte{0x01, 0x04, 'm', 'a', 'i', 'n', 0x00, 0x00})...)
	wasm = append(wasm, wasmSection(10, code)...)
	return base64.StdEncoding.EncodeToString(wasm)
}

func wasmSection(id byte, payload []byte) []byte {
	section := append([]byte{id}, wasmU32(uint32(len(payload)))...)
	return append(section, payload...)
}

func wasmU32(value uint32) []byte {
	var out []byte
	for {
		b := byte(value & 0x7f)
		value >>= 7
		if value != 0 {
			b |= 0x80
		}
		out = append(out, b)
		if value == 0 {
			return out
		}
	}
}
