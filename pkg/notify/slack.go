package notify

import (
	"encoding/json"
)

// formatSlack renders a minimal Block Kit payload — a header section
// with the title plus a context section listing the fields. Severity
// drives the accessory icon emoji so on-call eyes catch the worst
// stuff first.
func formatSlack(e Event) ([]byte, error) {
	if e.Kind == "" || e.Title == "" {
		return nil, errInvalidEvent
	}
	icon := severityEmoji(e.Severity)
	header := icon + " " + e.Title

	blocks := []map[string]any{
		{
			"type": "header",
			"text": map[string]any{
				"type": "plain_text",
				"text": truncate(header, 150),
			},
		},
	}
	if e.Body != "" {
		blocks = append(blocks, map[string]any{
			"type": "section",
			"text": map[string]any{
				"type": "mrkdwn",
				"text": truncate(e.Body, 3000),
			},
		})
	}
	if len(e.Fields) > 0 {
		blocks = append(blocks, map[string]any{
			"type":     "context",
			"elements": fieldsToContextElements(e.Fields),
		})
	}
	return json.Marshal(map[string]any{
		"blocks": blocks,
	})
}

func fieldsToContextElements(fields map[string]any) []map[string]any {
	out := make([]map[string]any, 0, len(fields))
	// Keep keys deterministic for readability + tests.
	keys := sortedKeys(fields)
	for _, k := range keys {
		out = append(out, map[string]any{
			"type": "mrkdwn",
			"text": "*" + k + ":* `" + truncate(stringify(fields[k]), 80) + "`",
		})
	}
	return out
}

// severityEmoji maps an event severity 0..10 to a one-char visual cue.
// Slack requires plain_text with no embedded markdown for headers,
// so we rely on the unicode icon to carry the urgency signal.
func severityEmoji(sev int) string {
	switch {
	case sev >= 8:
		return "🔴"
	case sev >= 5:
		return "🟠"
	case sev >= 3:
		return "🟡"
	default:
		return "ℹ️"
	}
}
