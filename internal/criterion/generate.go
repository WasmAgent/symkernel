//go:build ignore

// generate reads schemas/*.schema.json and writes Go type definitions into
// this package. Run via "go generate ./internal/criterion" (or "go generate ./...").
//
// Generated files:
//
//   - constraintir.go        — ConstraintIR, ConstraintRepair, enum types
//   - constraintviolation.go — ConstraintViolation, EvidenceSpan, DetectedAt
//   - criterion.go           — type Criterion = ConstraintIR
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"go/format"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ── Minimal JSON Schema subset used by constraint-ir / constraint-violation ──

type schema struct {
	Title      string              `json:"title"`
	Required   []string            `json:"required"`
	Properties map[string]property `json:"properties"`
}

type property struct {
	Type       any                 `json:"type"`
	Enum       []string            `json:"enum"`
	Required   []string            `json:"required"`
	Items      *property           `json:"items"`
	Properties map[string]property `json:"properties"`
}

type field struct {
	Name string // Go exported name
	Type string // Go type expression
	JSON string // json tag value (name[,omitempty])
}

// ── Main ───────────────────────────────────────────────────────────────────

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "generate criterion types: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	const schemaDir = "../../schemas"

	irSchema, err := readSchema(filepath.Join(schemaDir, "constraint-ir.schema.json"))
	if err != nil {
		return fmt.Errorf("read constraint-ir schema: %w", err)
	}
	violSchema, err := readSchema(filepath.Join(schemaDir, "constraint-violation.schema.json"))
	if err != nil {
		return fmt.Errorf("read constraint-violation schema: %w", err)
	}

	if err := writeFormatted("constraintir.go", renderConstraintIR(irSchema)); err != nil {
		return err
	}
	if err := writeFormatted("constraintviolation.go", renderConstraintViolation(violSchema)); err != nil {
		return err
	}
	if err := writeFormatted("criterion.go", renderCriterion()); err != nil {
		return err
	}
	return nil
}

// ── Schema I/O ─────────────────────────────────────────────────────────────

func readSchema(path string) (schema, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return schema{}, err
	}
	var s schema
	if err := json.Unmarshal(data, &s); err != nil {
		return schema{}, err
	}
	return s, nil
}

// ── Render: ConstraintIR ──────────────────────────────────────────────────

func renderConstraintIR(s schema) []byte {
	var buf bytes.Buffer
	buf.WriteString("package criterion\n\n")
	buf.WriteString("import \"encoding/json\"\n\n")

	writeStringEnum(&buf, "ConstraintLevel", "ConstraintLevel", s.Properties["level"].Enum)
	writeStringEnum(&buf, "ConstraintCategory", "ConstraintCategory", s.Properties["category"].Enum)
	writeStringEnum(&buf, "RepairStrategy", "RepairStrategy",
		s.Properties["repair"].Properties["strategy"].Enum)
	writeStruct(&buf, "ConstraintRepair",
		s.Properties["repair"].Properties, s.Properties["repair"].Required)
	writeStruct(&buf, "ConstraintIR", s.Properties, s.Required)
	return buf.Bytes()
}

// ── Render: ConstraintViolation ────────────────────────────────────────────

func renderConstraintViolation(s schema) []byte {
	var buf bytes.Buffer
	buf.WriteString("package criterion\n\n")

	writeStringEnum(&buf, "DetectedAt", "DetectedAt", s.Properties["detected_at"].Enum)
	writeStruct(&buf, "EvidenceSpan",
		s.Properties["evidence_span"].Properties, nil)
	writeStruct(&buf, "ConstraintViolation", s.Properties, s.Required)
	return buf.Bytes()
}

// ── Render: Criterion alias ───────────────────────────────────────────

func renderCriterion() []byte {
	return []byte(`package criterion

// Criterion is the wasmagent-js direct verification criterion shape.
type Criterion = ConstraintIR
`)
}

// ── Enum writer ───────────────────────────────────────────────────────────

func writeStringEnum(buf *bytes.Buffer, typeName, constPrefix string, values []string) {
	if len(values) == 0 {
		return
	}
	fmt.Fprintf(buf, "type %s string\n\n", typeName)
	buf.WriteString("const (\n")
	for _, v := range values {
		fmt.Fprintf(buf, "\t%s%s %s = %q\n", constPrefix, exportName(v), typeName, v)
	}
	buf.WriteString(")\n\n")
}

// ── Struct writer ──────────────────────────────────────────────────────────

func writeStruct(buf *bytes.Buffer, name string, properties map[string]property, required []string) {
	reqSet := make(map[string]bool, len(required))
	for _, r := range required {
		reqSet[r] = true
	}

	fields := make([]field, 0, len(properties))
	for jsonName, prop := range properties {
		fields = append(fields, field{
			Name: exportName(jsonName),
			Type: goType(jsonName, prop),
			JSON: jsonTag(jsonName, reqSet[jsonName]),
		})
	}
	sort.Slice(fields, func(i, j int) bool { return fields[i].JSON < fields[j].JSON })

	fmt.Fprintf(buf, "type %s struct {\n", name)
	for _, f := range fields {
		fmt.Fprintf(buf, "\t%s %s `json:\"%s\"`\n", f.Name, f.Type, f.JSON)
	}
	buf.WriteString("}\n\n")
}

// ── Type mapping ────────────────────────────────────────────────────────────

func goType(name string, prop property) string {
	// Enum properties → named string types.
	if len(prop.Enum) > 0 {
		switch name {
		case "level":
			return "ConstraintLevel"
		case "category":
			return "ConstraintCategory"
		case "strategy":
			return "RepairStrategy"
		case "detected_at":
			return "DetectedAt"
		}
	}

	// Hardcoded structural mappings.
	switch name {
	case "arg":
		return "json.RawMessage"
	case "repair":
		return "*ConstraintRepair"
	case "evidence_span":
		return "*EvidenceSpan"
	}

	// Array → []int (matches current schemas: char_range, line_range).
	if prop.Items != nil {
		return "[]int"
	}

	// Primitive types.
	switch schemaType(prop.Type) {
	case "integer":
		return "int"
	case "number":
		return "float64"
	default:
		return "string"
	}
}

func schemaType(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case []any:
		for _, e := range t {
			if s, ok := e.(string); ok && s != "null" {
				return s
			}
		}
	}
	return ""
}

// ── JSON tag ────────────────────────────────────────────────────────────────

func jsonTag(name string, required bool) string {
	if required {
		return name
	}
	return name + ",omitempty"
}

// ── Naming ─────────────────────────────────────────────────────────────────

func exportName(value string) string {
	parts := strings.FieldsFunc(value, func(r rune) bool {
		return r == '_' || r == '-'
	})
	var out strings.Builder
	for _, part := range parts {
		if part == "" {
			continue
		}
		if acr, ok := acronyms[strings.ToLower(part)]; ok {
			out.WriteString(acr)
			continue
		}
		out.WriteString(strings.ToUpper(part[:1]))
		if len(part) > 1 {
			out.WriteString(part[1:])
		}
	}
	return out.String()
}

var acronyms = map[string]string{
	"id":   "ID",
	"json": "JSON",
}

// ── File output ────────────────────────────────────────────────────────────

func writeFormatted(path string, src []byte) error {
	src = append([]byte("// Code generated by go generate; DO NOT EDIT.\n\n"), src...)
	formatted, err := format.Source(src)
	if err != nil {
		return fmt.Errorf("format %s: %w\n%s", path, err, src)
	}
	return os.WriteFile(path, formatted, 0o644)
}
