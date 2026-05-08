package main

import (
	"context"
	"fmt"

	"github.com/ensamblatec/neoanvil/pkg/astx"
)

type KillCommandTool struct{}

func (t *KillCommandTool) Name() string { return "neo_kill_command" }

func (t *KillCommandTool) Description() string {
	return "Destruye un proceso zombi (background command) lanzado previamente, a través de su PID."
}

func (t *KillCommandTool) InputSchema() MCPToolSchema {
	return MCPToolSchema{
		Type: "object",
		Properties: map[string]any{
			"pid": map[string]any{
				"type":        "number",
				"description": "ID del proceso SRE a fulminar",
			},
		},
		Required: []string{"pid"},
	}
}

func (t *KillCommandTool) Execute(ctx context.Context, args map[string]any) (any, error) {
	pidFloat, ok := args["pid"].(float64)
	if !ok {
		return nil, fmt.Errorf("invalid args: pid must be a number")
	}

	pid := int(pidFloat)
	err := astx.KillProcess(pid)
	if err != nil {
		return nil, fmt.Errorf("Fallo al matar el proceso: %v", err)
	}

	return fmt.Sprintf("✅ Proceso zombi con PID %d exterminado con éxito.", pid), nil
}
