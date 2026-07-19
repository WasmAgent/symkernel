package criterion

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

//go:generate go run generate.go

type VerifyRequest struct {
	Criterion Criterion `json:"criterion"`
}

type VerifyResponse struct {
	OK          bool   `json:"ok"`
	CriterionID string `json:"criterionId"`
	Hint        string `json:"hint,omitempty"`
}

func Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var req VerifyRequest
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, VerifyResponse{Hint: "invalid criterion request: " + err.Error()})
			return
		}
		var extra any
		if err := dec.Decode(&extra); err != io.EOF {
			writeJSON(w, http.StatusBadRequest, VerifyResponse{CriterionID: req.Criterion.ID, Hint: "invalid criterion request: trailing JSON"})
			return
		}

		if err := Validate(req.Criterion); err != nil {
			writeJSON(w, http.StatusOK, VerifyResponse{
				OK:          false,
				CriterionID: req.Criterion.ID,
				Hint:        err.Error(),
			})
			return
		}

		writeJSON(w, http.StatusOK, VerifyResponse{OK: true, CriterionID: req.Criterion.ID})
	})
}

func Validate(c Criterion) error {
	if strings.TrimSpace(c.ID) == "" {
		return errors.New("criterion.id is required")
	}
	if strings.TrimSpace(c.Description) == "" {
		return errors.New("criterion.description is required")
	}
	if strings.TrimSpace(c.VerifyMethod) == "" {
		return errors.New("criterion.verify_method is required")
	}
	if !validLevel(c.Level) {
		return fmt.Errorf("criterion.level must be one of: %s", strings.Join(enumValues(ConstraintLevelHard, ConstraintLevelSoft), ", "))
	}
	if !validCategory(c.Category) {
		return fmt.Errorf("criterion.category must be one of: %s", strings.Join(enumValues(
			ConstraintCategoryFormat,
			ConstraintCategoryContent,
			ConstraintCategoryStyle,
			ConstraintCategoryTool,
			ConstraintCategoryState,
			ConstraintCategorySecurity,
			ConstraintCategorySemantic,
		), ", "))
	}
	if c.Repair != nil {
		if !validRepairStrategy(c.Repair.Strategy) {
			return fmt.Errorf("criterion.repair.strategy must be one of: %s", strings.Join(enumValues(
				RepairStrategyPatch,
				RepairStrategyInsertSection,
				RepairStrategyRegenerateRegion,
				RepairStrategyFull,
			), ", "))
		}
		if c.Repair.MaxRounds < 0 {
			return errors.New("criterion.repair.max_rounds must be at least 1 when set")
		}
	}
	return nil
}

func validLevel(level ConstraintLevel) bool {
	return level == ConstraintLevelHard || level == ConstraintLevelSoft
}

func validCategory(category ConstraintCategory) bool {
	switch category {
	case ConstraintCategoryFormat,
		ConstraintCategoryContent,
		ConstraintCategoryStyle,
		ConstraintCategoryTool,
		ConstraintCategoryState,
		ConstraintCategorySecurity,
		ConstraintCategorySemantic:
		return true
	default:
		return false
	}
}

func validRepairStrategy(strategy RepairStrategy) bool {
	switch strategy {
	case RepairStrategyPatch,
		RepairStrategyInsertSection,
		RepairStrategyRegenerateRegion,
		RepairStrategyFull:
		return true
	default:
		return false
	}
}

func enumValues[T ~string](values ...T) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		out = append(out, string(value))
	}
	return out
}

func writeJSON(w http.ResponseWriter, status int, response VerifyResponse) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(response)
}
