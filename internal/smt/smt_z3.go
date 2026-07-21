//go:build z3

// Package smt provides SMT-LIB2 solving through Z3.
package smt

/*
#cgo LDFLAGS: -lz3
#include <stdlib.h>
#include <string.h>
#include <z3.h>
*/
import "C"

import (
	"fmt"
	"unsafe"

	z3 "github.com/aclements/go-z3/z3"
)

var _ = z3.NewContextConfig

// Solve parses and solves an SMT-LIB2 program with Z3.
func Solve(smt2 string, timeoutMs int) (SMTResult, error) {
	cfg := C.Z3_mk_config()
	if cfg == nil {
		return SMTResult{}, fmt.Errorf("smt: failed to create z3 config")
	}
	defer C.Z3_del_config(cfg)

	setConfig(cfg, "model", "true")
	setConfig(cfg, "unsat_core", "true")

	ctx := C.Z3_mk_context(cfg)
	if ctx == nil {
		return SMTResult{}, fmt.Errorf("smt: failed to create z3 context")
	}
	defer C.Z3_del_context(ctx)

	solver := C.Z3_mk_solver(ctx)
	if solver == nil {
		return SMTResult{}, fmt.Errorf("smt: failed to create z3 solver")
	}
	C.Z3_solver_inc_ref(ctx, solver)
	defer C.Z3_solver_dec_ref(ctx, solver)

	if timeoutMs > 0 {
		params := C.Z3_mk_params(ctx)
		C.Z3_params_inc_ref(ctx, params)
		defer C.Z3_params_dec_ref(ctx, params)

		timeoutName := C.CString("timeout")
		defer C.free(unsafe.Pointer(timeoutName))
		timeoutKey := C.Z3_mk_string_symbol(ctx, timeoutName)
		C.Z3_params_set_uint(ctx, params, timeoutKey, C.uint(timeoutMs))
		C.Z3_solver_set_params(ctx, solver, params)
	}

	input := C.CString(smt2)
	defer C.free(unsafe.Pointer(input))

	astVector := C.Z3_parse_smtlib2_string(ctx, input, 0, nil, nil, 0, nil, nil)
	if astVector == nil {
		return SMTResult{}, fmt.Errorf("smt: failed to parse smt2")
	}
	C.Z3_ast_vector_inc_ref(ctx, astVector)
	defer C.Z3_ast_vector_dec_ref(ctx, astVector)

	size := C.Z3_ast_vector_size(ctx, astVector)
	for i := C.uint(0); i < size; i++ {
		ast := C.Z3_ast_vector_get(ctx, astVector, i)
		C.Z3_solver_assert(ctx, solver, ast)
	}

	switch C.Z3_solver_check(ctx, solver) {
	case C.Z3_L_TRUE:
		model := C.Z3_solver_get_model(ctx, solver)
		return SMTResult{Sat: "sat", Model: parseModel(C.GoString(C.Z3_model_to_string(ctx, model)))}, nil
	case C.Z3_L_FALSE:
		return SMTResult{Sat: "unsat", UnsatCore: parseUnsatCore(ctx, solver)}, nil
	case C.Z3_L_UNDEF:
		return SMTResult{Sat: "unknown"}, nil
	default:
		return SMTResult{}, fmt.Errorf("smt: unexpected z3 satisfiability result")
	}
}

func setConfig(cfg C.Z3_config, key, value string) {
	cKey := C.CString(key)
	defer C.free(unsafe.Pointer(cKey))
	cValue := C.CString(value)
	defer C.free(unsafe.Pointer(cValue))
	C.Z3_set_param_value(cfg, cKey, cValue)
}

func parseUnsatCore(ctx C.Z3_context, solver C.Z3_solver) []string {
	core := C.Z3_solver_get_unsat_core(ctx, solver)
	size := C.Z3_ast_vector_size(ctx, core)
	if size == 0 {
		return nil
	}

	out := make([]string, 0, size)
	for i := C.uint(0); i < size; i++ {
		ast := C.Z3_ast_vector_get(ctx, core, i)
		out = append(out, C.GoString(C.Z3_ast_to_string(ctx, ast)))
	}
	return out
}
