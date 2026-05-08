// pkg/llm/classifier.go — Intent classifier for natural language → MCP tool calls. [SRE-95.A.2]
//
// Converts free-text user requests into structured JSON-RPC calls that can be
// dispatched to the NeoAnvil MCP server.
package llm

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Intent is a classified tool invocation derived from natural language. [SRE-95.A.2]
type Intent struct {
	Method     string          `json:"method"`       // e.g. "tools/call"
	ToolName   string          `json:"tool_name"`    // e.g. "neo_radar"
	Params     json.RawMessage `json:"params"`       // full JSON-RPC params
	Confidence float64         `json:"confidence"`
	RawLLM     string          `json:"-"` // raw LLM response for debug
}

// sanitizeUserInput escapes characters that could break prompt structure. [SRE-96.A.3]
func sanitizeUserInput(s string) string {
	// Strip characters that could inject prompt instructions.
	r := strings.NewReplacer(
		`"`, `\"`,
		"```", "'''",
	)
	return r.Replace(s)
}

// ClassifyIntent sends the user's natural language to the LLM and parses the
// result into a structured MCP tool call. [SRE-95.A.2]
func ClassifyIntent(userText string, client *Client) (Intent, error) {
	userText = sanitizeUserInput(userText)
	prompt := fmt.Sprintf(`You are a NeoAnvil MCP command translator. Convert the user's request into a JSON-RPC tool call.

Available tools:
- neo_radar: intents BRIEFING, BLAST_RADIUS, SEMANTIC_CODE, TECH_DEBT_MAP, READ_MASTER_PLAN, AST_AUDIT, HUD_STATE, COMPILE_AUDIT, READ_SLICE
- neo_daemon: actions PullTasks, PushTasks, Vacuum_Memory, SetStage, FLUSH_PMEM, QUARANTINE_IP
- neo_sre_certify_mutation: mutated_files (array of paths), complexity_intent (FEATURE_ADD, BUG_FIX, O(1)_OPTIMIZATION)
- neo_chaos_drill: target (URL), aggression_level (1-10), inject_faults (bool)
- neo_memory_commit: topic, scope, content

User request: "%s"

Respond with JSON only:
{"tool_name": "...", "arguments": {...}, "confidence": 0.0-1.0}`, userText)

	response, err := client.Generate(prompt, &GenerateOpts{
		Format:      "json",
		Temperature: 0.1,
		MaxTokens:   512,
	})
	if err != nil {
		return Intent{}, fmt.Errorf("classify: %w", err)
	}

	var parsed struct {
		ToolName   string          `json:"tool_name"`
		Arguments  json.RawMessage `json:"arguments"`
		Confidence float64         `json:"confidence"`
	}
	if err := json.Unmarshal([]byte(response), &parsed); err != nil {
		return Intent{}, fmt.Errorf("parse intent: %w (raw: %s)", err, response)
	}

	// Build the full JSON-RPC params.
	params := map[string]any{
		"name":      parsed.ToolName,
		"arguments": json.RawMessage(parsed.Arguments),
	}
	paramsJSON, _ := json.Marshal(params)

	return Intent{
		Method:     "tools/call",
		ToolName:   parsed.ToolName,
		Params:     paramsJSON,
		Confidence: parsed.Confidence,
		RawLLM:     response,
	}, nil
}

// ValidateIntent checks that the classified intent is valid and safe to execute. [SRE-95.A.3]
func ValidateIntent(intent Intent, serverMode string) error {
	validTools := map[string]bool{
		"neo_radar":                true,
		"neo_daemon":               true,
		"neo_sre_certify_mutation": true,
		"neo_chaos_drill":          true,
		"neo_memory_commit":        true,
		"neo_learn_directive":      true,
		"neo_compress_context":     true,
		"neo_run_command":          true,
		"neo_kill_command":         true,
		"neo_approve_command":      true,
		"neo_rem_sleep":            true,
		"neo_load_snapshot":        true,
		"neo_apply_migration":      true,
		"neo_forge_tool":           true,
		"neo_download_model":       true,
	}

	if !validTools[intent.ToolName] {
		return fmt.Errorf("unknown tool: %s", intent.ToolName)
	}

	// Mode restrictions.
	if intent.ToolName == "neo_daemon" {
		mode := strings.ToLower(serverMode)
		if mode == "pair" || mode == "fast" {
			return fmt.Errorf("neo_daemon is prohibited in %s mode", mode)
		}
	}

	// Confidence threshold.
	if intent.Confidence < 0.3 {
		return fmt.Errorf("confidence too low (%.2f) — rephrase your request", intent.Confidence)
	}

	return nil
}

// ResolveAlias checks if the user input matches a configured alias and expands it.
// Returns (expanded, true) if matched, or ("", false) if no alias matches. [SRE-95.B.3]
func ResolveAlias(input string, aliases map[string]string) (string, bool) {
	parts := strings.Fields(input)
	if len(parts) == 0 {
		return "", false
	}

	template, ok := aliases[parts[0]]
	if !ok {
		return "", false
	}

	// Replace $1, $2, etc. with positional args.
	for i := 1; i < len(parts); i++ {
		placeholder := fmt.Sprintf("$%d", i)
		template = strings.ReplaceAll(template, placeholder, parts[i])
	}

	// [SRE-96.C.3] Check for unreplaced placeholders — missing positional args.
	if strings.Contains(template, "$") {
		return "", false
	}

	return template, true
}

// BuildJSONRPC constructs a JSON-RPC 2.0 request envelope from an Intent.
func BuildJSONRPC(intent Intent, id int) []byte {
	envelope := map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  intent.Method,
		"params":  json.RawMessage(intent.Params),
	}
	data, _ := json.Marshal(envelope)
	return data
}
