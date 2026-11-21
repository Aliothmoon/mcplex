package output

import (
	"fmt"
	"sort"
	"strings"
)

// Shape summarizes a value's top-level structure so the model can slice an auto-spilled result precisely
// next turn, e.g. `{files: array(120) of {path, line}, summary: string(3.2KB), stats: object(5)}`.
func Shape(v any) string {
	switch x := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, 0, min(len(keys), 15)+1)
		for i, k := range keys {
			if i == 15 {
				parts = append(parts, fmt.Sprintf("+%d keys", len(keys)-15))
				break
			}
			parts = append(parts, k+": "+brief(x[k], true))
		}
		return "{" + strings.Join(parts, ", ") + "}"
	case []any:
		return brief(x, true)
	case string:
		return fmt.Sprintf("string(%s, %d lines)", HumanBytes(len(x)), strings.Count(x, "\n")+1)
	}
	return brief(v, false)
}

func brief(v any, withElem bool) string {
	switch x := v.(type) {
	case nil:
		return "null"
	case string:
		return "string(" + HumanBytes(len(x)) + ")"
	case bool:
		return "bool"
	case int, int32, int64, float32, float64, uint, uint32, uint64:
		return "number"
	case map[string]any:
		return fmt.Sprintf("object(%d)", len(x))
	case []any:
		s := fmt.Sprintf("array(%d)", len(x))
		if withElem && len(x) > 0 {
			if m, ok := x[0].(map[string]any); ok {
				keys := make([]string, 0, len(m))
				for k := range m {
					keys = append(keys, k)
				}
				sort.Strings(keys)
				if len(keys) > 8 {
					keys = append(keys[:8], "…")
				}
				s += " of {" + strings.Join(keys, ", ") + "}"
			} else {
				s += " of " + brief(x[0], false)
			}
		}
		return s
	}
	return fmt.Sprintf("%T", v)
}

// HumanBytes renders a byte count as B/KB/MB.
func HumanBytes(n int) string {
	switch {
	case n < 1024:
		return fmt.Sprintf("%dB", n)
	case n < 1024*1024:
		return fmt.Sprintf("%.1fKB", float64(n)/1024)
	}
	return fmt.Sprintf("%.1fMB", float64(n)/(1024*1024))
}
