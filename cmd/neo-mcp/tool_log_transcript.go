package main

// tool_log_transcript.go — Claude Code .jsonl transcript analyzer for neo_log_analyzer. [130.2]

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
)

// transcriptEntry is one line of a Claude Code .jsonl transcript. [130.2.2]
type transcriptEntry struct {
	Type    string          `json:"type"`
	Message json.RawMessage `json:"message,omitempty"`
}

type transcriptMessage struct {
	Role    string            `json:"role"`
	Content []json.RawMessage `json:"content"`
	Usage   *transcriptUsage  `json:"usage,omitempty"`
}

type transcriptUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

type transcriptContentBlock struct {
	Type    string          `json:"type"`
	Name    string          `json:"name,omitempty"`
	Input   json.RawMessage `json:"input,omitempty"`
	IsError bool            `json:"is_error,omitempty"`
	Text    string          `json:"text,omitempty"`
}

type toolCallStat struct {
	tool   string
	calls  int
	errors int
}

type retryLoop struct {
	tool  string
	count int
}

type transcriptReport struct {
	totalTurns    int
	totalTokensIn int
	totalTokensOut int
	toolCalls     map[string]*toolCallStat
	filesEdited   map[string]int // file → revision count
	retryLoops    []retryLoop
	thinking      int
	textBlocks    int
}

// parseTranscript parses a Claude Code .jsonl file into a report. [130.2.2]
func parseTranscript(path string) (*transcriptReport, error) {
	f, err := os.Open(path) //nolint:gosec // G304-CLI-CONSENT: path from operator arg
	if err != nil {
		return nil, fmt.Errorf("open transcript %s: %w", path, err)
	}
	defer f.Close()

	rpt := &transcriptReport{
		toolCalls:   make(map[string]*toolCallStat),
		filesEdited: make(map[string]int),
	}

	const windowSize = 3
	recentToolsByTurn := make([][]string, 0, windowSize)

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 4*1024*1024), 4*1024*1024) // 4 MB per line
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		msg, ok := parseAssistantEntry(line)
		if !ok {
			continue
		}
		rpt.totalTurns++
		if msg.Usage != nil {
			rpt.totalTokensIn += msg.Usage.InputTokens
			rpt.totalTokensOut += msg.Usage.OutputTokens
		}
		turnTools := processContentBlocks(msg.Content, rpt)
		if len(turnTools) > 0 {
			recentToolsByTurn = advanceRetryWindow(recentToolsByTurn, turnTools, windowSize, rpt)
		}
	}
	return rpt, scanner.Err()
}

// parseAssistantEntry decodes one JSONL line and returns the message if it is an assistant turn.
func parseAssistantEntry(line string) (transcriptMessage, bool) {
	var entry transcriptEntry
	if err := json.Unmarshal([]byte(line), &entry); err != nil || entry.Type != "assistant" {
		return transcriptMessage{}, false
	}
	var msg transcriptMessage
	if err := json.Unmarshal(entry.Message, &msg); err != nil || msg.Role != "assistant" {
		return transcriptMessage{}, false
	}
	return msg, true
}

// processContentBlocks iterates over one assistant turn's content and updates rpt.
// Returns the list of tool names called in this turn (for retry-loop tracking).
func processContentBlocks(blocks []json.RawMessage, rpt *transcriptReport) []string {
	var turnTools []string
	for _, rawBlock := range blocks {
		var block transcriptContentBlock
		if err := json.Unmarshal(rawBlock, &block); err != nil {
			continue
		}
		switch block.Type {
		case "thinking":
			rpt.thinking++
		case "text":
			rpt.textBlocks++
		case "tool_use":
			if block.Name == "" {
				continue
			}
			recordToolUse(block, rpt)
			turnTools = append(turnTools, block.Name)
		case "tool_result":
			if block.IsError && len(turnTools) > 0 {
				if stat, ok := rpt.toolCalls[turnTools[len(turnTools)-1]]; ok {
					stat.errors++
				}
			}
		}
	}
	return turnTools
}

// recordToolUse updates the tool call stats and file-edit tracking for a tool_use block.
func recordToolUse(block transcriptContentBlock, rpt *transcriptReport) {
	stat := rpt.toolCalls[block.Name]
	if stat == nil {
		stat = &toolCallStat{tool: block.Name}
		rpt.toolCalls[block.Name] = stat
	}
	stat.calls++
	if block.Name == "Edit" || block.Name == "Write" || block.Name == "MultiEdit" {
		var input map[string]any
		if jsonErr := json.Unmarshal(block.Input, &input); jsonErr == nil {
			if fp, ok := input["file_path"].(string); ok && fp != "" {
				rpt.filesEdited[fp]++
			}
		}
	}
}

// advanceRetryWindow appends turnTools to the sliding window and detects retry loops.
func advanceRetryWindow(window [][]string, turnTools []string, windowSize int, rpt *transcriptReport) [][]string {
	window = append(window, turnTools)
	if len(window) > windowSize {
		window = window[1:]
	}
	if len(window) == windowSize {
		counts := make(map[string]int)
		for _, turn := range window {
			seen := make(map[string]bool)
			for _, t := range turn {
				if !seen[t] {
					counts[t]++
					seen[t] = true
				}
			}
		}
		for tool, cnt := range counts {
			if cnt >= windowSize {
				rpt.retryLoops = append(rpt.retryLoops, retryLoop{tool: tool, count: cnt})
			}
		}
	}
	return window
}

// buildTranscriptReport formats the transcript analysis as Markdown. [130.2.3]
func buildTranscriptReport(rpt *transcriptReport) string {
	var sb strings.Builder
	sb.WriteString("## Tool Usage\n\n")

	type toolEntry struct {
		name      string
		calls     int
		errors    int
		errorRate float64
	}
	var entries []toolEntry
	for _, s := range rpt.toolCalls {
		er := 0.0
		if s.calls > 0 {
			er = float64(s.errors) / float64(s.calls)
		}
		entries = append(entries, toolEntry{s.tool, s.calls, s.errors, er})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].calls > entries[j].calls })

	if len(entries) == 0 {
		sb.WriteString("_No tool calls found._\n")
	} else {
		sb.WriteString("| tool | calls | errors | error_rate |\n")
		sb.WriteString("|------|-------|--------|------------|\n")
		for _, e := range entries {
			fmt.Fprintf(&sb, "| `%s` | %d | %d | %.0f%% |\n", e.name, e.calls, e.errors, e.errorRate*100)
		}
	}

	fmt.Fprintf(&sb, "\n**Turns:** %d | **Tokens in:** %d | **Tokens out:** %d | **Thinking blocks:** %d | **Text blocks:** %d\n",
		rpt.totalTurns, rpt.totalTokensIn, rpt.totalTokensOut, rpt.thinking, rpt.textBlocks)

	sb.WriteString("\n## Edit Patterns\n\n")
	if len(rpt.filesEdited) == 0 {
		sb.WriteString("_No file edits detected._\n")
	} else {
		type fileEntry struct {
			path     string
			revisits int
		}
		var files []fileEntry
		for p, n := range rpt.filesEdited {
			files = append(files, fileEntry{p, n})
		}
		sort.Slice(files, func(i, j int) bool { return files[i].revisits > files[j].revisits })
		sb.WriteString("| file | revisits |\n")
		sb.WriteString("|------|----------|\n")
		for _, f := range files {
			fmt.Fprintf(&sb, "| `%s` | %d |\n", f.path, f.revisits)
		}
	}

	sb.WriteString("\n## Retry Loops\n\n")
	seen := make(map[string]bool)
	written := false
	for _, loop := range rpt.retryLoops {
		if seen[loop.tool] {
			continue
		}
		seen[loop.tool] = true
		fmt.Fprintf(&sb, "- `%s` called %d× in a %d-turn window — possible retry loop\n", loop.tool, loop.count, 3)
		written = true
	}
	if !written {
		sb.WriteString("_No retry loops detected._\n")
	}

	sb.WriteString("\n_Analysis is local-only — transcripts are not sent to external services._\n")
	return sb.String()
}
