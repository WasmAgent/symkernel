package criterion

// VerifyCriterionRequest is the JSON body of a POST /v1/verify/criterion call.
// The outer wrapper carries the criterion to verify; Arg is opaque to the
// transport layer and interpreted by the named verifier.
//
//	Wire shape: {"criterion":{"id":"…","verify_method":"cel_expr","arg":{…}}}
type VerifyCriterionRequest struct {
	// Criterion is the constraint to evaluate.
	Criterion ConstraintIR `json:"criterion"`
}

// VerifyCriterionResponse is the JSON body returned by POST /v1/verify/criterion.
// Two concrete cases are supported:
//
//	Pass: {"ok":true,"criterionId":"…"}
//	Fail: {"ok":false,"criterionId":"…","hint":"…"}
type VerifyCriterionResponse struct {
	// OK is true when the criterion was satisfied.
	OK bool `json:"ok"`

	// CriterionID echoes back the ID of the evaluated criterion.
	CriterionID string `json:"criterionId"`

	// Hint is set only on failure (OK == false). It carries the verifier's
	// human-readable explanation of why the criterion was not met.
	Hint string `json:"hint,omitempty"`
}
