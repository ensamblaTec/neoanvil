// Command plugin-echo is a trivial reference MCP plugin used by integration
// tests in pkg/nexus and as a template for plugin authors.
//
// Wire format: newline-delimited JSON-RPC over stdio (MCP stdio transport).
// Tools exposed:
//   - echo    : echoes back the "text" argument as MCP text content
//   - version : returns the plugin version string
//
// See [docs/pilar-xxiii-plugin-architecture.md] for the architecture and
// the plugin authoring contract.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
)

const (
	protocolVersion = "2024-11-05"
	pluginVersion   = "0.1.0"
)

func main() {
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 0, 1<<16), 1<<20)
	enc := json.NewEncoder(os.Stdout)

	for scanner.Scan() {
		var req map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &req); err != nil {
			fmt.Fprintln(os.Stderr, "plugin-echo: bad json:", err)
			continue
		}
		resp := handle(req)
		if resp == nil {
			continue // notification — no response expected
		}
		if err := enc.Encode(resp); err != nil {
			fmt.Fprintln(os.Stderr, "plugin-echo: encode:", err)
			return
		}
	}
}

func handle(req map[string]any) map[string]any {
	method, _ := req["method"].(string)
	id := req["id"]
	switch method {
	case "initialize":
		return handleInitialize(id)
	case "notifications/initialized":
		return nil
	case "tools/list":
		return handleToolsList(id)
	case "tools/call":
		return handleToolsCall(id, req)
	}
	return rpcErr(id, -32601, "method not found: "+method)
}

func handleInitialize(id any) map[string]any {
	return ok(id, map[string]any{
		"protocolVersion": protocolVersion,
		"capabilities":    map[string]any{"tools": map[string]any{}},
		"serverInfo":      map[string]any{"name": "plugin-echo", "version": pluginVersion},
	})
}

func handleToolsList(id any) map[string]any {
	return ok(id, map[string]any{
		"tools": []map[string]any{
			{
				"name":        "echo",
				"description": "Echoes the input text back as MCP text content.",
				"inputSchema": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"text": map[string]any{"type": "string"},
					},
					"required": []string{"text"},
				},
			},
			{
				"name":        "version",
				"description": "Returns the plugin version string.",
				"inputSchema": map[string]any{
					"type":       "object",
					"properties": map[string]any{},
				},
			},
		},
	})
}

func handleToolsCall(id any, req map[string]any) map[string]any {
	params, _ := req["params"].(map[string]any)
	name, _ := params["name"].(string)
	args, _ := params["arguments"].(map[string]any)

	switch name {
	case "echo":
		text, _ := args["text"].(string)
		return ok(id, textContent(text))
	case "version":
		return ok(id, textContent("plugin-echo v"+pluginVersion))
	}
	return rpcErr(id, -32602, "unknown tool: "+name)
}

func textContent(s string) map[string]any {
	return map[string]any{
		"content": []map[string]any{
			{"type": "text", "text": s},
		},
	}
}

func ok(id any, result map[string]any) map[string]any {
	return map[string]any{"jsonrpc": "2.0", "id": id, "result": result}
}

func rpcErr(id any, code int, msg string) map[string]any {
	return map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"error":   map[string]any{"code": code, "message": msg},
	}
}
