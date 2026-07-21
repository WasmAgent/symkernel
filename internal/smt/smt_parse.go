package smt

import (
	"strconv"
	"strings"
)

func parseModel(model string) map[string]any {
	out := make(map[string]any)
	for _, block := range defineFunBlocks(model) {
		fields := strings.Fields(block)
		if len(fields) < 5 || fields[0] != "define-fun" {
			continue
		}

		value := strings.TrimSpace(strings.Join(fields[4:], " "))
		value = strings.TrimSuffix(value, ")")
		out[fields[1]] = parseValue(value)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func defineFunBlocks(model string) []string {
	var blocks []string
	lines := strings.Split(model, "\n")
	for i := 0; i < len(lines); i++ {
		line := strings.TrimSpace(lines[i])
		if !strings.HasPrefix(line, "(define-fun ") {
			continue
		}

		block := line
		depth := parenDepth(block)
		for depth > 0 && i+1 < len(lines) {
			i++
			next := strings.TrimSpace(lines[i])
			block += " " + next
			depth += parenDepth(next)
		}
		block = strings.TrimSpace(block)
		block = strings.TrimPrefix(block, "(")
		block = strings.TrimSuffix(block, ")")
		blocks = append(blocks, strings.TrimSpace(block))
	}
	return blocks
}

func parenDepth(s string) int {
	depth := 0
	for _, r := range s {
		switch r {
		case '(':
			depth++
		case ')':
			depth--
		}
	}
	return depth
}

func parseValue(value string) any {
	switch value {
	case "true":
		return true
	case "false":
		return false
	}
	if i, err := strconv.ParseInt(value, 10, 64); err == nil {
		return i
	}
	if u, err := strconv.ParseUint(value, 10, 64); err == nil {
		return u
	}
	return value
}
