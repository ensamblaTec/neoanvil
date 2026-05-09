package notify

import (
	"encoding/json"
)

// formatDiscord builds a single embed — colour-coded by severity,
// with each field rendered as a name/value pair. Discord allows up
// to 25 fields per embed; we cap at that to avoid 400.
func formatDiscord(e Event) ([]byte, error) {
	if e.Kind == "" || e.Title == "" {
		return nil, errInvalidEvent
	}
	const maxFields = 25
	fields := make([]map[string]any, 0, len(e.Fields))
	for _, k := range sortedKeys(e.Fields) {
		if len(fields) >= maxFields {
			break
		}
		fields = append(fields, map[string]any{
			"name":   k,
			"value":  truncate(stringify(e.Fields[k]), 1000),
			"inline": true,
		})
	}
	embed := map[string]any{
		"title":  truncate(e.Title, 256),
		"color":  severityColor(e.Severity),
		"fields": fields,
	}
	if e.Body != "" {
		embed["description"] = truncate(e.Body, 2000)
	}
	return json.Marshal(map[string]any{
		"embeds": []map[string]any{embed},
	})
}

// severityColor maps severity 0..10 to an RGB integer Discord
// accepts. Picks roughly: green (info), yellow (notice), orange
// (warn), red (critical).
func severityColor(sev int) int {
	switch {
	case sev >= 8:
		return 0xCC0000 // red
	case sev >= 5:
		return 0xE67E22 // orange
	case sev >= 3:
		return 0xF1C40F // yellow
	default:
		return 0x2ECC71 // green
	}
}
