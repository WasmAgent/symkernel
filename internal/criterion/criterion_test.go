package criterion

import (
	"encoding/json"
	"testing"
)

func TestConstraintIRRoundTrip(t *testing.T) {
	original := ConstraintIR{
		ID:           "no-hardcoded-secrets",
		Description:  "Artifact must not contain hardcoded API keys or secrets",
		VerifyMethod: "cel_expr",
		Arg: map[string]any{
			"expression": "!artifact.matches('\\b(sk-|key-)[A-Za-z0-9]{20,}\\b')",
		},
		Path:     "/src/config.go",
		Level:    LevelHard,
		Priority: 90,
		Category: CategorySecurity,
		Repair: &RepairDirective{
			Strategy:     RepairPatch,
			TargetRegion: "config-block",
			MaxRounds:    3,
		},
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var decoded ConstraintIR
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if decoded.ID != original.ID {
		t.Errorf("ID mismatch: got %q, want %q", decoded.ID, original.ID)
	}
	if decoded.VerifyMethod != original.VerifyMethod {
		t.Errorf("VerifyMethod mismatch: got %q, want %q", decoded.VerifyMethod, original.VerifyMethod)
	}
	if decoded.Level != original.Level {
		t.Errorf("Level mismatch: got %q, want %q", decoded.Level, original.Level)
	}
	if decoded.Category != original.Category {
		t.Errorf("Category mismatch: got %q, want %q", decoded.Category, original.Category)
	}
	if decoded.Priority != original.Priority {
		t.Errorf("Priority mismatch: got %v, want %v", decoded.Priority, original.Priority)
	}
	if decoded.Repair == nil {
		t.Fatal("Repair should not be nil after round-trip")
	}
	if decoded.Repair.Strategy != original.Repair.Strategy {
		t.Errorf("Repair.Strategy mismatch: got %q, want %q", decoded.Repair.Strategy, original.Repair.Strategy)
	}
	if decoded.Repair.MaxRounds != original.Repair.MaxRounds {
		t.Errorf("Repair.MaxRounds mismatch: got %d, want %d", decoded.Repair.MaxRounds, original.Repair.MaxRounds)
	}
}

func TestConstraintIRMinimal(t *testing.T) {
	// Only required fields.
	input := `{
		"id": "min",
		"description": "d",
		"verify_method": "cel_expr",
		"level": "soft",
		"priority": 50,
		"category": "format"
	}`

	var c ConstraintIR
	if err := json.Unmarshal([]byte(input), &c); err != nil {
		t.Fatalf("Unmarshal minimal: %v", err)
	}
	if c.ID != "min" {
		t.Errorf("ID = %q, want %q", c.ID, "min")
	}
	if c.Arg != nil {
		t.Errorf("Arg should be nil for minimal input, got %v", c.Arg)
	}
	if c.Path != "" {
		t.Errorf("Path should be empty for minimal input, got %q", c.Path)
	}
	if c.Repair != nil {
		t.Errorf("Repair should be nil for minimal input")
	}
}

func TestConstraintViolationRoundTrip(t *testing.T) {
	original := ConstraintViolation{
		ConstraintID: "no-hardcoded-secrets",
		Level:        LevelHard,
		Category:     CategorySecurity,
		Hint:         "Found sk-abc123… API key at line 42",
		DetectedAt:   DetectedPostDecode,
		EvidenceSpan: &EvidenceSpan{
			RegionID:    "config-block",
			JSONPointer: "/config/apiKey",
			CharRange:   []int{1042, 1068},
			LineRange:   []int{42, 43},
		},
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var decoded ConstraintViolation
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if decoded.ConstraintID != original.ConstraintID {
		t.Errorf("ConstraintID mismatch")
	}
	if decoded.Level != original.Level {
		t.Errorf("Level mismatch")
	}
	if decoded.DetectedAt != original.DetectedAt {
		t.Errorf("DetectedAt mismatch")
	}
	if decoded.EvidenceSpan == nil {
		t.Fatal("EvidenceSpan should not be nil")
	}
	if len(decoded.EvidenceSpan.CharRange) != 2 || decoded.EvidenceSpan.CharRange[0] != 1042 {
		t.Errorf("CharRange mismatch: %v", decoded.EvidenceSpan.CharRange)
	}
}

func TestConstraintViolationMinimal(t *testing.T) {
	input := `{
		"constraint_id": "c1",
		"level": "hard",
		"category": "format",
		"hint": "bad indent",
		"detected_at": "post_tool_call"
	}`

	var v ConstraintViolation
	if err := json.Unmarshal([]byte(input), &v); err != nil {
		t.Fatalf("Unmarshal minimal violation: %v", err)
	}
	if v.EvidenceSpan != nil {
		t.Errorf("EvidenceSpan should be nil for minimal input")
	}
}

func TestVerifyCriterionRequestRoundTrip(t *testing.T) {
	original := VerifyCriterionRequest{
		Criterion: ConstraintIR{
			ID:           "lang-go",
			Description:  "Source files must be valid Go",
			VerifyMethod: "cel_expr",
			Arg: map[string]any{
				"expression": "artifact.language == 'go'",
			},
			Level:    LevelHard,
			Priority: 80,
			Category: CategoryFormat,
		},
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var decoded VerifyCriterionRequest
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if decoded.Criterion.ID != original.Criterion.ID {
		t.Errorf("Criterion.ID mismatch")
	}
}

func TestVerifyCriterionResponsePass(t *testing.T) {
	input := `{"ok":true,"criterionId":"lang-go"}`
	var r VerifyCriterionResponse
	if err := json.Unmarshal([]byte(input), &r); err != nil {
		t.Fatalf("Unmarshal pass: %v", err)
	}
	if !r.OK {
		t.Error("OK should be true")
	}
	if r.CriterionID != "lang-go" {
		t.Errorf("CriterionID = %q", r.CriterionID)
	}
	if r.Hint != "" {
		t.Errorf("Hint should be empty on pass, got %q", r.Hint)
	}
}

func TestVerifyCriterionResponseFail(t *testing.T) {
	input := `{"ok":false,"criterionId":"lang-go","hint":"file is not valid Go"}`
	var r VerifyCriterionResponse
	if err := json.Unmarshal([]byte(input), &r); err != nil {
		t.Fatalf("Unmarshal fail: %v", err)
	}
	if r.OK {
		t.Error("OK should be false")
	}
	if r.Hint != "file is not valid Go" {
		t.Errorf("Hint = %q", r.Hint)
	}
}

func TestEnumStringValues(t *testing.T) {
	tests := []struct {
		name string
		got  string
		want string
	}{
		{"LevelHard", string(LevelHard), "hard"},
		{"LevelSoft", string(LevelSoft), "soft"},
		{"CategoryFormat", string(CategoryFormat), "format"},
		{"CategoryContent", string(CategoryContent), "content"},
		{"CategoryStyle", string(CategoryStyle), "style"},
		{"CategoryTool", string(CategoryTool), "tool"},
		{"CategoryState", string(CategoryState), "state"},
		{"CategorySecurity", string(CategorySecurity), "security"},
		{"CategorySemantic", string(CategorySemantic), "semantic"},
		{"RepairPatch", string(RepairPatch), "patch"},
		{"RepairInsertSection", string(RepairInsertSection), "insert_section"},
		{"RepairRegenerateRegion", string(RepairRegenerateRegion), "regenerate_region"},
		{"RepairFull", string(RepairFull), "full"},
		{"DetectedPreDecode", string(DetectedPreDecode), "pre_decode"},
		{"DetectedPostDecode", string(DetectedPostDecode), "post_decode"},
		{"DetectedPostToolCall", string(DetectedPostToolCall), "post_tool_call"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.got != tt.want {
				t.Errorf("got %q, want %q", tt.got, tt.want)
			}
		})
	}
}
