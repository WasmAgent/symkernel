package policyimport

import (
	"strconv"
	"strings"
)

// celString renders a Go string as a double-quoted CEL string literal with
// the minimal escaping CEL requires. CEL string literals follow the same
// escaping rules as JSON for the characters we care about here.
func celString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// celNumber renders a numeric literal. Integers pass through unchanged; other
// forms (floats) are emitted verbatim since CEL accepts the same decimal
// syntax. The input is the canonical string form from the source AST.
func celNumber(s string) string {
	// Validate it parses as a number so we never emit a non-numeric token.
	if _, err := strconv.ParseInt(s, 10, 64); err == nil {
		return s
	}
	if _, err := strconv.ParseFloat(s, 64); err == nil {
		return s
	}
	// Fall back to a quoted string only if it is not numeric; callers that
	// reach here have already validated numeric-ness, so this is defensive.
	return celString(s)
}
