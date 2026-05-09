package main

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/ensamblatec/neoanvil/pkg/telemetry"
)

type MCPToolSchema struct {
	Type       string         `json:"type"`
	Properties map[string]any `json:"properties"`
	Required   []string       `json:"required,omitempty"`
}

type Tool interface {
	Name() string
	Description() string
	InputSchema() MCPToolSchema
	Execute(ctx context.Context, args map[string]any) (any, error)
}

type ToolRegistry struct {
	tools   map[string]Tool
	buckets sync.Map
}

func NewToolRegistry() *ToolRegistry {
	return &ToolRegistry{
		tools: make(map[string]Tool),
	}
}

func (toolRegistry *ToolRegistry) Register(tool Tool) error {
	if _, exists := toolRegistry.tools[tool.Name()]; exists {
		return fmt.Errorf("[SRE-FATAL] MCP Tool collision detected. Tool name must be unique: %s", tool.Name())
	}
	toolRegistry.tools[tool.Name()] = tool
	return nil
}

func (toolRegistry *ToolRegistry) List() []map[string]any {
	var list []map[string]any
	for _, tool := range toolRegistry.tools {
		schema := tool.InputSchema()
		// [Area 4.2.B] Clone Properties before mutating. If a Tool's
		// InputSchema returns the same map across calls (e.g., a
		// cached schema), naïve mutation would persist `target_workspace`
		// keys across renders + pollute the OpenAPI spec output.
		props := make(map[string]any, len(schema.Properties)+1)
		for k, v := range schema.Properties {
			props[k] = v
		}
		// [SRE-85.B.1] Inject target_workspace on the CLONE so the
		// underlying Tool's schema stays pristine.
		props["target_workspace"] = map[string]any{
			"type":        "string",
			"description": "Optional. Workspace ID or name for Nexus routing. If omitted, routes to active workspace.",
		}
		schema.Properties = props
		list = append(list, map[string]any{
			"name":        tool.Name(),
			"description": tool.Description(),
			"inputSchema": schema,
		})
	}
	return list
}

func (toolRegistry *ToolRegistry) Call(ctx context.Context, name string, args map[string]any) (any, error) {
	tool, exists := toolRegistry.tools[name]
	if !exists {
		return nil, fmt.Errorf("tool not found or deprecated: %s", name)
	}

	// [SRE-13.3.2] Expanded capacity for macro-tools (100 tokens vs legacy 20)
	initialTokens := 100.0
	initialRate := 2.0
	bucketObj, _ := toolRegistry.buckets.LoadOrStore(name, &TokenBucket{
		tokens:     initialTokens,
		lastRefill: time.Now(),
		rate:       initialRate,
		capacity:   initialTokens,
	})

	tb := bucketObj.(*TokenBucket)
	if !tb.Allow() {
		telemetry.IncrementRateLimitBlock()
		return nil, fmt.Errorf("[SRE-RATE-LIMIT] La herramienta '%s' está saturada (Token Bucket vacío). Espera unos segundos para que se recargue antes de volver a usarla.", name)
	}

	result, err := tool.Execute(ctx, args)
	if err == nil {
		result = truncateToolResult(result)
	}
	return result, err
}

// truncateToolResult applies semantic truncation to MCP text responses >25000 chars.
// [SRE-13.5.2] Preserves head, tail, and critical lines (panic/error/fatal/.go:).
func truncateToolResult(result any) any {
	const maxChars = 25000
	resMap, ok := result.(map[string]any)
	if !ok {
		return result
	}
	contentRaw, exists := resMap["content"]
	if !exists {
		return result
	}
	content, ok := contentRaw.([]map[string]any)
	if !ok || len(content) == 0 {
		return result
	}
	text, ok := content[0]["text"].(string)
	if !ok || len(text) <= maxChars {
		return result
	}
	lines := strings.Split(text, "\n")
	head, tail := lines, lines
	if len(lines) > 5 {
		head = lines[:5]
		tail = lines[len(lines)-5:]
	}
	var critical []string
	if len(lines) > 10 {
		for _, ln := range lines[5 : len(lines)-5] {
			lower := strings.ToLower(ln)
			if strings.Contains(lower, "panic") || strings.Contains(lower, "error") ||
				strings.Contains(lower, "fatal") || strings.Contains(ln, ".go:") {
				critical = append(critical, ln)
			}
		}
	}
	var sb strings.Builder
	sb.WriteString(strings.Join(head, "\n"))
	if len(critical) > 0 {
		sb.WriteString("\n... [omitted safe logs] ...\n")
		sb.WriteString(strings.Join(critical, "\n"))
	}
	sb.WriteString("\n... [omitted safe logs] ...\n")
	sb.WriteString(strings.Join(tail, "\n"))
	content[0]["text"] = sb.String()
	return result
}

type TokenBucket struct {
	mu         sync.Mutex
	tokens     float64
	lastRefill time.Time
	rate       float64
	capacity   float64
}

func (tb *TokenBucket) Allow() bool {
	tb.mu.Lock()
	defer tb.mu.Unlock()

	now := time.Now()
	elapsed := now.Sub(tb.lastRefill).Seconds()

	tb.tokens += elapsed * tb.rate
	if tb.tokens > tb.capacity {
		tb.tokens = tb.capacity
	}
	tb.lastRefill = now

	if tb.tokens >= 1.0 {
		tb.tokens -= 1.0
		return true
	}
	return false
}
