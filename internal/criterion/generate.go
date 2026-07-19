package criterion

//go:generate bash -c "cd ../.. && make check-schemas"

// generate.go anchors the //go:generate directive that validates the pinned
// schema files in schemas/ against the upstream wasmagent-js source of truth.
// This ensures the Go types in this package remain in sync with the canonical
// JSON Schema definitions.
//
// The actual types in types.go, constraint_ir.go, and constraint_violation.go
// are hand-maintained but must be kept structurally identical to the schemas
// validated here. Regenerating from scratch requires introducing a
// JSON-Schema-to-Go codegen tool (tracked as a future milestone).
