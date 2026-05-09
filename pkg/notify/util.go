package notify

import (
	"fmt"
	"sort"
)

// truncate cuts s to maxLen runes (rough byte clamp; ASCII-safe). The
// `…` ellipsis communicates the cut to the human eye in chat.
func truncate(s string, maxLen int) string {
	if maxLen <= 0 || len(s) <= maxLen {
		return s
	}
	return s[:maxLen-1] + "…"
}

// stringify renders the field value the way an operator wants to see
// it in chat — strings as-is, numbers/bools via fmt, complex things
// via JSON-ish %v. Avoids deep-marshalling large structs into noise.
func stringify(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case nil:
		return "<nil>"
	default:
		return fmt.Sprintf("%v", x)
	}
}

// sortedKeys returns map keys in lexical order so the rendered
// payload is deterministic — easier for tests + operator scanning.
func sortedKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
