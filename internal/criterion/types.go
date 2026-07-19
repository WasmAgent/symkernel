// Package criterion defines Go types that mirror the wasmagent-js compliance
// schemas pinned in the top-level schemas/ directory (constraint-ir.schema.json,
// constraint-violation.schema.json). Every exported type is derived from one of
// those JSON Schema definitions so that there is no hand-maintained duplication
// between the wire contract and the Go representation.
//
// Run "go generate ./internal/criterion" to re-derive the types (or to validate
// that they still match the schema files).
package criterion

// Level is the enforcement strictness of a constraint. Values match the
// "level" enum in constraint-ir.schema.json and constraint-violation.schema.json.
type Level string

const (
	// LevelHard means the constraint MUST be satisfied; a violation is a
	// blocking failure.
	LevelHard Level = "hard"

	// LevelSoft means the constraint SHOULD be satisfied; a violation is
	// recorded but does not block.
	LevelSoft Level = "soft"
)

// Category classifies the semantic domain of a constraint. Values match
// the "category" enum shared by constraint-ir.schema.json and
// constraint-violation.schema.json.
type Category string

const (
	// CategoryFormat covers structural formatting rules (indentation, line
	// length, file naming, etc.).
	CategoryFormat Category = "format"

	// CategoryContent covers correctness of the generated output with respect
	// to the task specification.
	CategoryContent Category = "content"

	// CategoryStyle covers stylistic preferences (tone, voice, phrasing).
	CategoryStyle Category = "style"

	// CategoryTool covers constraints on tool selection or usage patterns.
	CategoryTool Category = "tool"

	// CategoryState covers constraints about agent state or session invariants.
	CategoryState Category = "state"

	// CategorySecurity covers security-sensitive constraints (no secrets, no
	// injection, sandbox boundaries).
	CategorySecurity Category = "security"

	// CategorySemantic covers higher-level semantic correctness (logical
	// consistency, factual accuracy).
	CategorySemantic Category = "semantic"
)

// RepairStrategy selects how a failed constraint should be repaired. Values
// match the "strategy" enum in constraint-ir.schema.json.
type RepairStrategy string

const (
	// RepairPatch applies a targeted diff to fix the violation in-place.
	RepairPatch RepairStrategy = "patch"

	// RepairInsertSection inserts a missing section at a specified region.
	RepairInsertSection RepairStrategy = "insert_section"

	// RepairRegenerateRegion regenerates the contents of a specified region
	// from scratch.
	RepairRegenerateRegion RepairStrategy = "regenerate_region"

	// RepairFull discards the entire artifact and regenerates it.
	RepairFull RepairStrategy = "full"
)

// DetectedAt identifies the verification phase that caught the violation.
// Values match the "detected_at" enum in constraint-violation.schema.json.
type DetectedAt string

const (
	// DetectedPreDecode means the violation was caught before the artifact
	// was decoded (e.g. schema validation, structural checks).
	DetectedPreDecode DetectedAt = "pre_decode"

	// DetectedPostDecode means the violation was caught after decoding but
	// before any tool call was issued.
	DetectedPostDecode DetectedAt = "post_decode"

	// DetectedPostToolCall means the violation was caught after a tool call
	// returned (e.g. the tool output broke a constraint).
	DetectedPostToolCall DetectedAt = "post_tool_call"
)
