package astx

import (
	"regexp"
)

func ExtractExportedCalls(src []byte) []string {

	re := regexp.MustCompile(`\b([A-Z][a-zA-Z0-9_]+)\b`)
	matches := re.FindAllSubmatch(src, -1)

	seen := make(map[string]bool)
	var targets []string

	for _, m := range matches {
		if len(m) > 1 {
			symbol := string(m[1])
			if !seen[symbol] {
				seen[symbol] = true
				targets = append(targets, symbol)
			}
		}
	}
	return targets
}
