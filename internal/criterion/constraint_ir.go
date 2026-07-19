package criterion

// ConstraintIR is the Go mirror of constraint-ir.schema.json — a typed,
// repairable, prioritised superset of the wasmagent-js @wasmagent/core
// Criterion. Phase-0 contract; fields may be added during alpha.
type ConstraintIR struct {
	// ID is the unique identifier for this constraint. Required; minLength 1.
	ID string `json:"id"`

	// Description is a human-readable explanation of what the constraint
	// enforces. Required.
	Description string `json:"description"`

	// VerifyMethod names the verifier to use (e.g. "cel_expr"). It is an open
	// string union — built-in verifiers register their own method names at
	// runtime. Required; minLength 1.
	VerifyMethod string `json:"verify_method"`

	// Arg carries verifier-specific parameters. Its shape is determined by
	// VerifyMethod at runtime. Omitted when empty.
	Arg any `json:"arg,omitempty"`

	// Path is an optional locator (file path, section ID, etc.) that scopes
	// the constraint to a region of the artifact.
	Path string `json:"path,omitempty"`

	// Level is the enforcement strictness (hard or soft). Required.
	Level Level `json:"level"`

	// Priority ranks this constraint inside its priority hierarchy band.
	// Higher wins. Conventional band 0–100. Required.
	Priority float64 `json:"priority"`

	// Category classifies the semantic domain of the constraint. Required.
	Category Category `json:"category"`

	// Repair holds optional repair instructions. When nil the constraint is
	// not repairable (violations can only be reported, not auto-fixed).
	Repair *RepairDirective `json:"repair,omitempty"`
}

// RepairDirective describes how to fix a violation of this constraint.
// It maps to the optional "repair" object in constraint-ir.schema.json.
type RepairDirective struct {
	// Strategy selects the repair approach. Required.
	Strategy RepairStrategy `json:"strategy"`

	// TargetRegion is an optional locator identifying the region to repair.
	TargetRegion string `json:"target_region,omitempty"`

	// MaxRounds is the maximum number of repair attempts allowed. Minimum 1.
	// Zero means "unlimited" on the wire but defaults to 1 for safety when
	// omitted.
	MaxRounds int `json:"max_rounds,omitempty"`
}
