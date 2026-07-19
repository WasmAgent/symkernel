package criterion

// ConstraintViolation is the Go mirror of constraint-violation.schema.json — a
// failed constraint plus its location in the artifact. It is consumed by the
// RepairPlanner to choose a fix strategy.
type ConstraintViolation struct {
	// ConstraintID references the constraint that was violated. Required;
	// minLength 1.
	ConstraintID string `json:"constraint_id"`

	// Level is the enforcement strictness (hard or soft). Required.
	Level Level `json:"level"`

	// Category classifies the semantic domain of the violated constraint.
	// Required.
	Category Category `json:"category"`

	// Hint is the original verifier output, kept verbatim for round-trip
	// debugging. Required.
	Hint string `json:"hint"`

	// EvidenceSpan is an optional locator into the artifact that pinpoints
	// where the violation was found. At least one locator field must be set
	// when EvidenceSpan is non-nil.
	EvidenceSpan *EvidenceSpan `json:"evidence_span,omitempty"`

	// DetectedAt identifies the verification phase that caught the violation.
	// Required.
	DetectedAt DetectedAt `json:"detected_at"`
}

// EvidenceSpan locates a violation within the artifact. At least one field MUST
// be set. It maps to the "evidence_span" object in
// constraint-violation.schema.json.
type EvidenceSpan struct {
	// RegionID is a logical region identifier (e.g. a section or block name).
	RegionID string `json:"region_id,omitempty"`

	// JSONPointer is an RFC 6901 JSON Pointer into a structured artifact.
	JSONPointer string `json:"json_pointer,omitempty"`

	// CharRange is a half-open byte offset pair [start, end) locating the
	// violation in a text artifact. Exactly two integers.
	CharRange []int `json:"char_range,omitempty"`

	// LineRange is a half-open line-number pair [start, end) locating the
	// violation in a text artifact. Exactly two integers.
	LineRange []int `json:"line_range,omitempty"`
}
