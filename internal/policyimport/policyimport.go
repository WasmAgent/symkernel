// Package policyimport translates policies written in mainstream policy
// languages (OPA Rego and AWS Cedar) into Google CEL expression strings that
// symkernel's existing CEL substrate (internal/cel) can evaluate and that the
// constraint/verify path can carry into an SMT-backed proof.
//
// The design goal is honesty about coverage, not maximal coverage. symkernel's
// value proposition is *provable* policy: a translation that silently changes
// a rule's meaning is worse than no translation at all. Every translator here
// is therefore FAIL-CLOSED — any construct it does not explicitly understand
// produces an *UnsupportedError, never a best-effort guess. Callers get either
// a CEL string that provably mirrors the source policy's decision, or a precise
// error naming the construct that could not be translated.
//
// The emitted CEL targets the variable surface that internal/cel.Evaluate
// exposes: a flat map[string]any of input variables. Rego policies read from
// the well-known `input` document; Cedar policies read from `principal`,
// `action`, `resource`, and `context`. Both are surfaced to CEL as top-level
// identifiers of the same name.
package policyimport

import "fmt"

// UnsupportedError is returned when a source policy uses a construct that the
// translator deliberately does not handle. It is the mechanism by which the
// translators stay fail-closed: an unsupported construct is reported, never
// approximated. The Construct field is a short, stable identifier for the
// unsupported feature (useful for tests and metrics); Detail carries a
// human-readable explanation with source context.
type UnsupportedError struct {
	// Lang is the source policy language ("rego" or "cedar").
	Lang string
	// Construct is a short stable slug for the unsupported feature,
	// e.g. "rego.builtin:count", "cedar.node:NodeTypeExtensionCall".
	Construct string
	// Detail explains what was rejected and, where possible, why.
	Detail string
}

func (e *UnsupportedError) Error() string {
	return fmt.Sprintf("%s: unsupported construct %q: %s", e.Lang, e.Construct, e.Detail)
}

// unsupported is a small constructor to keep the translators terse.
func unsupported(lang, construct, format string, args ...any) *UnsupportedError {
	return &UnsupportedError{
		Lang:      lang,
		Construct: construct,
		Detail:    fmt.Sprintf(format, args...),
	}
}

// Result is the outcome of translating a single source policy into CEL.
type Result struct {
	// CEL is the translated expression. It evaluates to a bool: true means
	// the policy's decision is "allow"/"permit" for the given input.
	CEL string
	// Effect records the source policy's decision direction so callers can
	// distinguish an allow-rule from a deny/forbid-rule when composing.
	Effect Effect
	// SourceLang is "rego" or "cedar".
	SourceLang string
}

// Effect is the decision direction of a translated policy.
type Effect string

const (
	// EffectPermit means the CEL expression evaluating true grants access.
	EffectPermit Effect = "permit"
	// EffectForbid means the CEL expression evaluating true denies access.
	EffectForbid Effect = "forbid"
)
