// cmd/neo/ask.go — Voice of the Leviathan: natural language CLI. [SRE-95.B]
//
// `neo ask "..."` — translates natural language into MCP tool calls via a local
// LLM (Ollama), validates the intent, and dispatches to the Nexus dispatcher.
//
// `neo chat` — interactive REPL with conversation history.
//
// `neo ask --status` — prints LLM backend diagnostics.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/ensamblatec/neoanvil/pkg/sre"
)

// askConfig holds LLM configuration loaded from neo.yaml or defaults.
type askConfig struct {
	OllamaURL   string
	Model       string
	MaxTokens   int
	Temperature float64
	NexusURL    string
	Aliases     map[string]string
}

func defaultAskConfig() askConfig {
	return askConfig{
		OllamaURL:   envOr("NEO_OLLAMA_URL", "http://localhost:11434"),
		Model:       envOr("NEO_LLM_MODEL", "llama3.2:3b"),
		MaxTokens:   2048,
		Temperature: 0.3,
		NexusURL:    envOr("NEO_NEXUS_URL", "http://127.0.0.1:9000"),
		Aliases: map[string]string{
			"status": `{"tool_name":"neo_radar","arguments":{"intent":"BRIEFING","mode":"compact"},"confidence":1.0}`,
			"chaos":  `{"tool_name":"neo_chaos_drill","arguments":{"target":"$1","aggression_level":5},"confidence":1.0}`,
		},
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// askCmd implements `neo ask "<text>"`. [SRE-95.B.1]
func askCmd() *cobra.Command {
	var dryRun bool
	var workspace string
	var showStatus bool

	cmd := &cobra.Command{
		Use:   "ask [text]",
		Short: "Translate natural language to MCP tool calls via local LLM",
		Long:  "Sends your request to a local LLM (Ollama) which classifies it into a NeoAnvil MCP tool call and executes it.",
		Args:  cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg := defaultAskConfig()

			if showStatus {
				return printLLMStatus(cfg)
			}

			if len(args) == 0 {
				return fmt.Errorf("usage: neo ask \"your request here\"")
			}

			userText := strings.Join(args, " ")

			// Check aliases first (fast path, no LLM). [SRE-95.B.3]
			if resolved, ok := resolveAlias(userText, cfg.Aliases); ok {
				return dispatchIntent(resolved, cfg.NexusURL, workspace, dryRun)
			}

			// Classify via LLM. [SRE-95.A.2]
			intentJSON, err := classifyViaOllama(userText, cfg)
			if err != nil {
				// Fallback: aliases-only mode. [SRE-95.C.2]
				fmt.Fprintf(os.Stderr, "⚠ LLM unavailable (%v) — only aliases work in offline mode\n", err)
				return fmt.Errorf("try: neo ask status")
			}

			return dispatchIntent(intentJSON, cfg.NexusURL, workspace, dryRun)
		},
	}

	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Show the JSON-RPC call without executing")
	cmd.Flags().StringVar(&workspace, "workspace", "", "Target workspace ID for routing")
	cmd.Flags().BoolVar(&showStatus, "status", false, "Show LLM backend diagnostics")
	return cmd
}

// chatCmd implements `neo chat` — interactive REPL. [SRE-95.B.2]
func chatCmd() *cobra.Command {
	var workspace string

	return &cobra.Command{
		Use:   "chat",
		Short: "Interactive REPL — talk to Neo in natural language",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg := defaultAskConfig()
			fmt.Println("NeoAnvil Voice Interface (Ctrl+C to exit)")
			fmt.Println("Type your requests in natural language.")
			fmt.Println()

			scanner := bufio.NewScanner(os.Stdin)
			for {
				fmt.Print("neo> ")
				if !scanner.Scan() {
					break
				}
				line := strings.TrimSpace(scanner.Text())
				if line == "" {
					continue
				}
				if line == "exit" || line == "quit" {
					break
				}

				// Alias check.
				if resolved, ok := resolveAlias(line, cfg.Aliases); ok {
					if err := dispatchIntent(resolved, cfg.NexusURL, workspace, false); err != nil {
						fmt.Fprintf(os.Stderr, "error: %v\n", err)
					}
					continue
				}

				// LLM classification.
				intentJSON, err := classifyViaOllama(line, cfg)
				if err != nil {
					fmt.Fprintf(os.Stderr, "⚠ LLM error: %v\n", err)
					continue
				}

				if err := dispatchIntent(intentJSON, cfg.NexusURL, workspace, false); err != nil {
					fmt.Fprintf(os.Stderr, "error: %v\n", err)
				}
			}
			return nil
		},
	}
}

// classifyViaOllama sends the user text to Ollama for intent classification.
func classifyViaOllama(userText string, cfg askConfig) (string, error) {
	prompt := fmt.Sprintf(`You are a NeoAnvil MCP command translator. Convert the user's request into a JSON tool call.

Available tools:
- neo_radar: intents BRIEFING, BLAST_RADIUS, SEMANTIC_CODE, TECH_DEBT_MAP, READ_MASTER_PLAN, AST_AUDIT, HUD_STATE, COMPILE_AUDIT, READ_SLICE
- neo_daemon: actions PullTasks, PushTasks, Vacuum_Memory, SetStage, FLUSH_PMEM, QUARANTINE_IP
- neo_sre_certify_mutation: mutated_files (paths), complexity_intent (FEATURE_ADD, BUG_FIX, O(1)_OPTIMIZATION)
- neo_chaos_drill: target (URL), aggression_level (1-10), inject_faults (bool)
- neo_memory_commit: topic, scope, content

User: "%s"

Respond with JSON only: {"tool_name": "...", "arguments": {...}, "confidence": 0.0-1.0}`, userText)

	body, _ := json.Marshal(map[string]any{
		"model":  cfg.Model,
		"prompt": prompt,
		"stream": false,
		"format": "json",
		"options": map[string]any{
			"temperature": cfg.Temperature,
			"num_predict": cfg.MaxTokens,
		},
	})

	// [SRE-110.E] Ollama URL is user-configured (NEO_OLLAMA_URL / neo.yaml) —
	// SafeHTTPClient applies SSRF guard for potentially-external endpoints.
	client := sre.SafeHTTPClient()
	client.Timeout = 60 * time.Second
	resp, err := client.Post(cfg.OllamaURL+"/api/generate", "application/json", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("ollama: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("ollama status %d", resp.StatusCode)
	}

	var result struct {
		Response string `json:"response"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("decode: %w", err)
	}

	return result.Response, nil
}

// dispatchIntent builds a JSON-RPC envelope and sends it to Nexus /mcp/message.
func dispatchIntent(intentJSON, nexusURL, workspace string, dryRun bool) error {
	// Parse the intent.
	var intent struct {
		ToolName   string          `json:"tool_name"`
		Arguments  json.RawMessage `json:"arguments"`
		Confidence float64         `json:"confidence"`
	}
	if err := json.Unmarshal([]byte(intentJSON), &intent); err != nil {
		return fmt.Errorf("invalid intent JSON: %w", err)
	}

	if intent.Confidence > 0 && intent.Confidence < 0.3 {
		fmt.Fprintf(os.Stderr, "⚠ Low confidence (%.0f%%) — rephrase if results are wrong\n", intent.Confidence*100)
	}

	// Inject workspace routing if specified.
	if workspace != "" {
		var args map[string]any
		_ = json.Unmarshal(intent.Arguments, &args)
		if args == nil {
			args = make(map[string]any)
		}
		args["target_workspace"] = workspace
		intent.Arguments, _ = json.Marshal(args)
	}

	// Build JSON-RPC 2.0 envelope.
	envelope := map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      intent.ToolName,
			"arguments": json.RawMessage(intent.Arguments),
		},
	}
	rpcBody, _ := json.MarshalIndent(envelope, "", "  ")

	if dryRun {
		fmt.Println(string(rpcBody))
		return nil
	}

	// Send to Nexus. [SRE-110.E] Nexus URL is loopback by default but operator-
	// configurable; SafeHTTPClient applies SSRF guard with 5-minute timeout for
	// long-running tool calls.
	client := sre.SafeHTTPClient()
	client.Timeout = 5 * time.Minute
	resp, err := client.Post(nexusURL+"/mcp/message", "application/json", bytes.NewReader(rpcBody))
	if err != nil {
		return fmt.Errorf("nexus: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)

	// Pretty-print the response.
	var rpcResp struct {
		Result any `json:"result"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(respBody, &rpcResp); err != nil {
		// Raw output fallback.
		fmt.Println(string(respBody))
		return nil
	}

	if rpcResp.Error != nil {
		return fmt.Errorf("tool error: %s", rpcResp.Error.Message)
	}

	// Format the result.
	switch v := rpcResp.Result.(type) {
	case string:
		fmt.Println(v)
	default:
		pretty, _ := json.MarshalIndent(v, "", "  ")
		fmt.Println(string(pretty))
	}

	return nil
}

// resolveAlias checks if the input matches a configured alias. [SRE-95.B.3]
func resolveAlias(input string, aliases map[string]string) (string, bool) {
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

	return template, true
}

// printLLMStatus prints diagnostics about the LLM backend. [SRE-95.C.3]
func printLLMStatus(cfg askConfig) error {
	fmt.Printf("LLM Backend Diagnostics\n")
	fmt.Printf("━━━━━━━━━━━━━━━━━━━━━━\n")
	fmt.Printf("Ollama URL:  %s\n", cfg.OllamaURL)
	fmt.Printf("Model:       %s\n", cfg.Model)
	fmt.Printf("Nexus URL:   %s\n", cfg.NexusURL)

	// Check Ollama health. [SRE-110.E] SafeHTTPClient with short timeout for diag.
	client := sre.SafeHTTPClient()
	client.Timeout = 5 * time.Second
	resp, err := client.Get(cfg.OllamaURL + "/api/tags")
	if err != nil {
		fmt.Printf("Status:      ❌ offline (%v)\n", err)
		fmt.Printf("Mode:        aliases-only\n")
		return nil
	}
	defer resp.Body.Close()

	var tags struct {
		Models []struct {
			Name string `json:"name"`
			Size int64  `json:"size"`
		} `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tags); err != nil {
		fmt.Printf("Status:      ❌ error parsing response\n")
		return nil
	}

	modelFound := false
	for _, m := range tags.Models {
		if m.Name == cfg.Model {
			modelFound = true
			fmt.Printf("Status:      ✅ online\n")
			fmt.Printf("Model size:  %.1f GB\n", float64(m.Size)/(1024*1024*1024))
			break
		}
	}

	if !modelFound {
		fmt.Printf("Status:      ⚠ online but model %q not loaded\n", cfg.Model)
		fmt.Printf("Available:   ")
		for i, mod := range tags.Models {
			if i > 0 {
				fmt.Print(", ")
			}
			fmt.Print(mod.Name)
		}
		fmt.Println()
		fmt.Printf("Hint:        Run 'ollama pull %s' to download\n", cfg.Model)
	}

	return nil
}
