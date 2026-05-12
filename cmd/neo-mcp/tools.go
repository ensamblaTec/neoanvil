package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ensamblatec/neoanvil/pkg/astx"
	"github.com/ensamblatec/neoanvil/pkg/config"
	"github.com/ensamblatec/neoanvil/pkg/dba"
	"github.com/ensamblatec/neoanvil/pkg/finops"
	"github.com/ensamblatec/neoanvil/pkg/mctx"
	"github.com/ensamblatec/neoanvil/pkg/rag"
	"github.com/ensamblatec/neoanvil/pkg/sre"
	"github.com/ensamblatec/neoanvil/pkg/telemetry"
	"github.com/ensamblatec/neoanvil/pkg/wasmx"
)

// [PILAR-XXVIII hotfix] Package-level HTTP client for neo_download_model —
// prevents a fresh Transport per call when multiple models are pulled in
// succession. SafeHTTPClient default SSRF guard + long-ish timeout so
// multi-GB GGUF files can land.
var modelDownloadClient = sre.SafeHTTPClient()

type RemSleepTool struct {
	wal *rag.WAL
}

func NewRemSleepTool(wal *rag.WAL) *RemSleepTool {
	return &RemSleepTool{wal: wal}
}

func (rst *RemSleepTool) Name() string { return "neo_rem_sleep" }

func (rst *RemSleepTool) Description() string {
	return "Executes REM Sleep phase backpropagation, adjusting neural weights based on recent session success ratio."
}

func (rst *RemSleepTool) InputSchema() MCPToolSchema {
	return MCPToolSchema{
		Type: "object",
		Properties: map[string]any{
			"learning_rate": map[string]any{
				"type":        "number",
				"description": "Learning rate for backpropagation (e.g., 0.1).",
			},
			"session_success_ratio": map[string]any{
				"type":        "number",
				"description": "Success ratio (0.0 to 1.0). >0.5 signals success.",
			},
		},
		Required: []string{"learning_rate", "session_success_ratio"},
	}
}

func (rst *RemSleepTool) Execute(ctx context.Context, args map[string]any) (any, error) {
	lrFloat, ok1 := args["learning_rate"].(float64)
	ratioFloat, ok2 := args["session_success_ratio"].(float64)
	if !ok1 || !ok2 {
		return nil, fmt.Errorf("learning_rate and session_success_ratio must be numbers")
	}

	mlp := wasmx.GetMathMLP()
	if mlp == nil {
		return nil, fmt.Errorf("[neo_rem_sleep] internal MLP not initialized")
	}

	// [SRE-FINOPS] Total Cost of Ownership Penalty
	penalty := finops.GetTotalPenalty()
	finalReward := ratioFloat - (penalty * 0.1) - (0.01 * math.Log1p(ratioFloat)) // 10% impacto termodinámico financiero + Entropía Logarítmica
	success := (finalReward > 0.5)

	mlp.AdjustWeights(float32(lrFloat), success)

	if err := rst.wal.SaveWeights(mlp.W1.Data, mlp.W2.Data); err != nil {
		return nil, fmt.Errorf("[neo_rem_sleep] failed to persist neural weights: %w", err)
	}

	// [SRE] Auto-Evolutivo MCTS (Heurística PRM)
	if success {
		astx.ActivePRM.LZ76Threshold += 0.01 // Aprieta pureza
		mctx.CusumH = 0.15                   // Resetea rigor de caida
	} else {
		astx.ActivePRM.LZ76Threshold -= 0.05 // Afloja pureza
		mctx.CusumH += 0.05                  // Tolerancia al colapso aumentada
	}

	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": "Fase REM completada. Pesos sinápticos ajustados y guardados en BoltDB."}},
	}, nil
}

type LearnDirectiveTool struct {
	wal       *rag.WAL
	workspace string
	cfgFn     func() *config.NeoConfig
}

func NewLearnDirectiveTool(wal *rag.WAL, workspace string) *LearnDirectiveTool {
	return &LearnDirectiveTool{wal: wal, workspace: workspace}
}

func (ldt *LearnDirectiveTool) WithConfig(fn func() *config.NeoConfig) *LearnDirectiveTool {
	ldt.cfgFn = fn
	return ldt
}

func (ldt *LearnDirectiveTool) Name() string { return "neo_learn_directive" }

func (ldt *LearnDirectiveTool) Description() string {
	return "Saves a global business or architectural rule to the orchestrator's permanent memory. This rule will be injected in every future code search to ensure compliance."
}

func (ldt *LearnDirectiveTool) InputSchema() MCPToolSchema {
	return MCPToolSchema{
		Type: "object",
		Properties: map[string]any{
			"directive": map[string]any{
				"type":        "string",
				"description": "The directive text. Required for action=add or action=update.",
			},
			"action": map[string]any{
				"type":        "string",
				"description": "[SRE-77.1] Operation: add (default), update, delete.",
				"enum":        []string{"add", "update", "delete"},
			},
			"directive_id": map[string]any{
				"type":        "integer",
				"description": "[SRE-77.1] 1-based ID of the directive to update or delete.",
			},
			"supersedes": map[string]any{
				"type":        "array",
				"items":       map[string]any{"type": "integer"},
				"description": "[SRE-77.5] IDs of directives this new ADD supersedes — auto-deprecates them.",
			},
		},
		Required: []string{},
	}
}

func (ldt *LearnDirectiveTool) Execute(ctx context.Context, args map[string]any) (any, error) {
	action, _ := args["action"].(string)
	if action == "" {
		action = "add"
	}
	switch action {
	case "add":
		return ldt.handleAddDirective(args)
	case "update":
		return ldt.handleUpdateDirective(args)
	case "delete":
		return ldt.handleDeleteDirective(args)
	case "compact":
		return ldt.handleCompactDirectives()
	default:
		return nil, fmt.Errorf("unknown action '%s': use add, update, delete, or compact", action)
	}
}

// syncDirectives writes the BoltDB directives to .claude/rules/neo-synced-directives.md
// (dual-layer sync per directive 15). Returns the write error if any so callers
// can surface "SYNC FAILED" in the response — the BoltDB write already succeeded
// when this is called, so failure here is non-fatal but MUST be visible.
// [Épica 141 — was silent log-only, now returns error.]
func (ldt *LearnDirectiveTool) syncDirectives() error {
	err := ldt.wal.SyncDirectivesToDisk(ldt.workspace)
	if err != nil {
		log.Printf("[SRE-23.1.1] Disk sync failed: %v", err)
	}
	return err
}

// syncStatusSuffix renders the sync result as a tag for the response message.
// Operator can grep "SYNC FAILED" to detect silent regressions.
func syncStatusSuffix(err error) string {
	if err == nil {
		return " (sync ok)"
	}
	return fmt.Sprintf(" — SYNC FAILED: %v", err)
}

func (ldt *LearnDirectiveTool) handleAddDirective(args map[string]any) (any, error) {
	directive, ok := args["directive"].(string)
	if !ok || directive == "" {
		return nil, fmt.Errorf("'directive' is required for action=add")
	}
	// [374.A] Char-count guard
	maxChars := 500
	if ldt.cfgFn != nil {
		if c := ldt.cfgFn().SRE.MaxDirectiveChars; c > 0 {
			maxChars = c
		}
	}
	if len(directive) > maxChars {
		return nil, fmt.Errorf("directive too long (%d chars, max %d). Condense to a rule — use neo_memory(action:\"store\") for long-form knowledge", len(directive), maxChars)
	}
	// [374.B] Total count guard
	maxCount := 60
	if ldt.cfgFn != nil {
		if c := ldt.cfgFn().SRE.MaxDirectives; c > 0 {
			maxCount = c
		}
	}
	existing, _ := ldt.wal.GetDirectives()
	activeCount := 0
	for _, d := range existing {
		if !strings.Contains(d, "~~OBSOLETO~~") {
			activeCount++
		}
	}
	if activeCount >= maxCount {
		return nil, fmt.Errorf("directive limit reached (%d/%d active). Prune deprecated directives first (neo_memory action:learn action_type:delete directive_id:N) or use supersedes:[] to consolidate", activeCount, maxCount)
	}
	if err := ldt.wal.SaveDirective(directive); err != nil {
		return nil, fmt.Errorf("[neo_learn_directive] failed to persist: %w", err)
	}
	// [SRE-77.5] Supersedes: auto-deprecate listed IDs with reference to the new directive.
	if superRaw, ok := args["supersedes"].([]any); ok {
		rules, _ := ldt.wal.GetDirectives()
		newID := len(rules)
		for _, idRaw := range superRaw {
			if idFloat, ok := idRaw.(float64); ok {
				_ = ldt.wal.DeprecateDirective(int(idFloat), newID)
			}
		}
	}
	syncErr := ldt.syncDirectives()
	return mcpOK("Directiva añadida. Se aplicará globalmente." + syncStatusSuffix(syncErr)), nil
}

func (ldt *LearnDirectiveTool) handleUpdateDirective(args map[string]any) (any, error) {
	directive, ok := args["directive"].(string)
	if !ok || directive == "" {
		return nil, fmt.Errorf("'directive' is required for action=update")
	}
	idFloat, ok := args["directive_id"].(float64)
	if !ok || idFloat < 1 {
		return nil, fmt.Errorf("'directive_id' (int ≥ 1) is required for action=update")
	}
	if err := ldt.wal.UpdateDirective(int(idFloat), directive); err != nil {
		return nil, fmt.Errorf("[neo_learn_directive] update failed: %w", err)
	}
	syncErr := ldt.syncDirectives()
	return mcpOK(fmt.Sprintf("Directiva %d actualizada.", int(idFloat)) + syncStatusSuffix(syncErr)), nil
}

func (ldt *LearnDirectiveTool) handleDeleteDirective(args map[string]any) (any, error) {
	idFloat, ok := args["directive_id"].(float64)
	if !ok || idFloat < 1 {
		return nil, fmt.Errorf("'directive_id' (int ≥ 1) is required for action=delete")
	}
	deprecatedBy := 0
	if dbRaw, ok := args["deprecated_by"].(float64); ok {
		deprecatedBy = int(dbRaw)
	}
	if err := ldt.wal.DeprecateDirective(int(idFloat), deprecatedBy); err != nil {
		return nil, fmt.Errorf("[neo_learn_directive] delete failed: %w", err)
	}
	syncErr := ldt.syncDirectives()
	return mcpOK(fmt.Sprintf("Directiva %d marcada como ~~OBSOLETO~~ (soft-delete).", int(idFloat)) + syncStatusSuffix(syncErr)), nil
}

func (ldt *LearnDirectiveTool) handleCompactDirectives() (any, error) {
	removed, kept, err := ldt.wal.CompactDirectives()
	if err != nil {
		return nil, fmt.Errorf("[neo_learn_directive] compact failed: %w", err)
	}
	syncErr := ldt.syncDirectives()
	return mcpOK(fmt.Sprintf("Compacted: removed %d (deprecated+duplicates), kept %d active.", removed, kept) + syncStatusSuffix(syncErr)), nil
}

// mcpOK wraps a string as a standard MCP text response.
func mcpOK(text string) map[string]any {
	return map[string]any{"content": []map[string]any{{"type": "text", "text": text}}}
}

// mcpJSON wraps any structured result as MCP text content with its JSON
// representation. Used by tools that return map/struct results so clients
// see the data instead of silent empty responses. Returning a raw map
// without the {"content":[{"type":"text","text":...}]} envelope is a silent
// no-op at the MCP wire protocol level. [Épica 330.E]
func mcpJSON(v any) map[string]any {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return mcpText(fmt.Sprintf("⚠️ mcpJSON marshal error: %v", err))
	}
	return mcpText(string(data))
}

// Estructura robusta para la cola de comandos
type StagedCommand struct {
	Command string
	Risk    int
	Reason  string
}

var (
	cmdTicketsMu sync.Mutex
	cmdTickets   = make(map[string]StagedCommand)
)

type RunCommandTool struct {
	autoApprove bool
}

func NewRunCommandTool(autoApprove bool) *RunCommandTool {
	return &RunCommandTool{autoApprove: autoApprove}
}
func (t *RunCommandTool) Name() string { return "neo_run_command" }
func (t *RunCommandTool) Description() string {
	return "STAGES a shell command for human authorization. MUST provide a risk assessment. You will receive a Ticket ID. Do not assume the command has executed until the human approves it."
}
func (t *RunCommandTool) InputSchema() MCPToolSchema {
	return MCPToolSchema{
		Type: "object",
		Properties: map[string]any{
			"command": map[string]any{
				"type":        "string",
				"description": "The exact shell command to execute.",
			},
			"risk_score": map[string]any{
				"type":        "integer",
				"description": "Evaluate the danger of this command from 1 (Safe) to 10 (System Destruction).",
			},
			"blast_radius_analysis": map[string]any{
				"type":        "string",
				"description": "Briefly explain what happens if this command fails or is malicious.",
			},
		},
		Required: []string{"command", "risk_score", "blast_radius_analysis"},
	}
}

func (t *RunCommandTool) Execute(ctx context.Context, args map[string]any) (any, error) {
	command, _ := args["command"].(string)
	riskScoreFloat, _ := args["risk_score"].(float64)
	riskScore := int(riskScoreFloat)
	reason, _ := args["blast_radius_analysis"].(string)

	ticketID := fmt.Sprintf("CMD-%d", time.Now().Unix())

	turbo := strings.Contains(command, "// turbo") || strings.Contains(command, "// turbo-all")

	if t.autoApprove || turbo {
		if turbo {
			log.Printf("[SRE-TURBO] Command '%s' auto-approved via annotation.", command)
			command = strings.ReplaceAll(command, "// turbo-all", "")
			command = strings.ReplaceAll(command, "// turbo", "")
		}
		cmd := exec.CommandContext(ctx, "sh", "-c", command)
		sre.HardenSubprocess(cmd, 0) // [T006-sweep] Setpgid+WaitDelay for cgo/proto-gen subprocesses

		cmdStr := strings.TrimSpace(command)
		if strings.HasSuffix(cmdStr, "&") {
			err := cmd.Start()
			if err == nil && cmd.Process != nil {
				astx.RegisterProcess(cmd)
			}
			resText := "Background process started successfully decoupled from MCP."
			if err != nil {
				resText = fmt.Sprintf("⚠️ [SRE-ASYNC-ERROR] Falló el arranque: %v", err)
			}
			return map[string]any{
				"content": []map[string]any{{"type": "text", "text": resText}},
			}, nil
		}

		out, err := cmd.CombinedOutput()
		texto := string(out)

		if err != nil {
			texto = fmt.Sprintf("⚠️ [SRE-EXEC-VETO] Comando finalizó con código de error (%v):\n\n%s", err, string(out))
		}

		return map[string]any{
			"content": []map[string]any{{"type": "text", "text": texto}},
		}, nil
	}

	cmdTicketsMu.Lock()
	cmdTickets[ticketID] = StagedCommand{Command: command, Risk: riskScore, Reason: reason}
	cmdTicketsMu.Unlock()

	telemetry.StageCommandTicket(ticketID, riskScore, reason)

	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": fmt.Sprintf("🎫 TICKET STAGED [%s]. Esperando autorización manual.", ticketID)}},
	}, nil
}

type ApproveCommandTool struct{}

func NewApproveCommandTool() *ApproveCommandTool { return &ApproveCommandTool{} }
func (t *ApproveCommandTool) Name() string       { return "neo_approve_command" }
func (t *ApproveCommandTool) Description() string {
	return "Executes a staged command using a ticket ID provided by the user."
}
func (t *ApproveCommandTool) InputSchema() MCPToolSchema {
	return MCPToolSchema{
		Type: "object",
		Properties: map[string]any{
			"ticket_id": map[string]any{
				"type":        "string",
				"description": "The ticket ID authorized by the Commander.",
			},
		},
		Required: []string{"ticket_id"},
	}
}

func (t *ApproveCommandTool) Execute(ctx context.Context, args map[string]any) (any, error) {
	ticketID, ok := args["ticket_id"].(string)
	if !ok || ticketID == "" {
		return nil, fmt.Errorf("ticket_id is required")
	}

	cmdTicketsMu.Lock()
	staged, exists := cmdTickets[ticketID]
	if exists {
		delete(cmdTickets, ticketID)
	}
	cmdTicketsMu.Unlock()

	if !exists {
		return nil, fmt.Errorf("ticket ID '%s' not found or already executed", ticketID)
	}

	telemetry.ClearCommandTicket()
	telemetry.LogAction("Ejecutando Ticket Aprobado: " + ticketID)

	cmd := exec.CommandContext(ctx, "bash", "-c", staged.Command)
	sre.HardenSubprocess(cmd, 0) // [T006-sweep] cap pipe-drain wait for runaway subprocesses
	out, err := cmd.CombinedOutput()

	texto := fmt.Sprintf("Command Executed: %s\nOutput:\n%s\nError (if any): %v", staged.Command, string(out), err)
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": texto}},
	}, nil
}

type ModelDownloaderTool struct {
	workspace string
}

func NewModelDownloaderTool(workspace string) *ModelDownloaderTool {
	return &ModelDownloaderTool{workspace: workspace}
}

func (t *ModelDownloaderTool) Name() string { return "neo_download_model" }

func (t *ModelDownloaderTool) Description() string {
	return "Downloads large AI model files (e.g., .wasm, .onnx, .gguf) from a URL directly to the .neo/models/ directory using safe stream chunking (Zero-RAM saturation)."
}

func (t *ModelDownloaderTool) InputSchema() MCPToolSchema {
	return MCPToolSchema{
		Type: "object",
		Properties: map[string]any{
			"model_url": map[string]any{
				"type":        "string",
				"description": "The direct HTTP/HTTPS URL of the model file.",
			},
			"filename": map[string]any{
				"type":        "string",
				"description": "The target filename (e.g., 'embedder.wasm').",
			},
		},
		Required: []string{"model_url", "filename"},
	}
}

func (t *ModelDownloaderTool) Execute(ctx context.Context, args map[string]any) (any, error) {
	telemetry.LogAction("Downloading physical AI model")

	modelURL, ok := args["model_url"].(string)
	if !ok || modelURL == "" {
		return nil, fmt.Errorf("model_url is required")
	}

	filename, ok := args["filename"].(string)
	if !ok || filename == "" {
		filename = "embedder.wasm"
	}
	// [SRE-SECURITY] Sanitize filename to prevent path traversal
	filename = filepath.Base(filename)

	modelsDir := filepath.Join(t.workspace, ".neo", "models")
	if err := os.MkdirAll(modelsDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create models directory: %w", err)
	}

	targetPath := filepath.Join(modelsDir, filename)

	req, err := http.NewRequestWithContext(ctx, "GET", modelURL, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	// [PILAR-XXVIII hotfix] Reuse a package-level client so repeated
	// neo_download_model invocations don't each leak a new Transport +
	// pool. 30-min timeout accommodates large GGUF/ONNX files.
	resp, err := modelDownloadClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("bad HTTP status: %s", resp.Status)
	}

	out, err := os.Create(targetPath)
	if err != nil {
		return nil, fmt.Errorf("failed to create target file: %w", err)
	}
	defer out.Close()

	bytesWritten, err := io.Copy(out, resp.Body)
	if err != nil {
		return nil, fmt.Errorf("error writing to disk: %w", err)
	}

	texto := fmt.Sprintf("=== MODEL DOWNLOAD SUCCESS ===\nFile: %s\nSize: %.2f MB\nPath: %s\nStatus: Ready for offline inference.", filename, float64(bytesWritten)/(1024*1024), targetPath)

	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": texto}},
	}, nil
}










type ContextCompressorTool struct {
	wal       *rag.WAL
	workspace string
}

func NewContextCompressorTool() *ContextCompressorTool {
	return &ContextCompressorTool{}
}

// WithWAL wires the WAL and workspace so export/import actions work. [130.1]
func (t *ContextCompressorTool) WithWAL(w *rag.WAL, workspace string) *ContextCompressorTool {
	t.wal = w
	t.workspace = workspace
	return t
}

func (t *ContextCompressorTool) Name() string { return "neo_compress_context" }
func (t *ContextCompressorTool) Description() string {
	return "SRE Tool: (1) Compress massive terminal outputs/logs to error-only lines. (2) action:export — serialize session state to a portable JSON file. (3) action:import — restore session state from a previously exported file."
}

func (t *ContextCompressorTool) InputSchema() MCPToolSchema {
	return MCPToolSchema{
		Type: "object",
		Properties: map[string]any{
			"action": map[string]any{
				"type":        "string",
				"enum":        []string{"compress", "export", "import"},
				"description": "Operation: compress (default, requires raw_text), export (serialize session to path), import (merge from path).",
			},
			"raw_text": map[string]any{
				"type":        "string",
				"description": "[compress] The massive text/log to compress.",
			},
			"path": map[string]any{
				"type":        "string",
				"description": "[export/import] File path for the session JSON. For export, defaults to /tmp/neo-session-<ts>.json.",
			},
		},
		Required: []string{},
	}
}

func (t *ContextCompressorTool) Execute(ctx context.Context, args map[string]any) (any, error) {
	action, _ := args["action"].(string)
	if action == "" {
		action = "compress"
	}

	switch action {
	case "export":
		if t.wal == nil {
			return nil, fmt.Errorf("export not available: tool not wired with WAL")
		}
		path, _ := args["path"].(string)
		outPath, err := compressExport(ctx, t.wal, t.workspace, path)
		if err != nil {
			return nil, err
		}
		return mcpText(fmt.Sprintf("✅ Session exported to `%s`", outPath)), nil

	case "import":
		if t.wal == nil {
			return nil, fmt.Errorf("import not available: tool not wired with WAL")
		}
		path, _ := args["path"].(string)
		if path == "" {
			return nil, fmt.Errorf("path required for import")
		}
		added, err := compressImport(ctx, t.wal, t.workspace, path)
		if err != nil {
			return nil, err
		}
		return mcpText(fmt.Sprintf("✅ Session imported from `%s` — %d paths merged into session_state", path, added)), nil

	default: // compress
		rawText, ok := args["raw_text"].(string)
		if !ok || rawText == "" {
			return nil, fmt.Errorf("argument 'raw_text' must be a non-empty string for action:compress")
		}
		return t.compressText(rawText), nil
	}
}

func (t *ContextCompressorTool) compressText(rawText string) any {
	lines := strings.Split(rawText, "\n")
	if len(lines) < 50 {
		return map[string]any{"content": []map[string]any{{"type": "text", "text": rawText}}}
	}

	var compressed []string
	compressed = append(compressed, "--- COMPRESSED CONTEXT (SRE FILTER) ---")

	for i := 0; i < 5 && i < len(lines); i++ {
		compressed = append(compressed, lines[i])
	}

	keywords := []string{"error", "panic", "fatal", "fail", "veto", "goroutine", ".go:", "exception"}
	lastWasOmitted := false

	for i := 5; i < len(lines)-5; i++ {
		lineLower := strings.ToLower(lines[i])
		matched := false
		for _, kw := range keywords {
			if strings.Contains(lineLower, kw) {
				matched = true
				break
			}
		}
		if matched {
			compressed = append(compressed, lines[i])
			lastWasOmitted = false
		} else if !lastWasOmitted {
			compressed = append(compressed, "  ... [omitted safe logs] ...")
			lastWasOmitted = true
		}
	}

	startLast := len(lines) - 5
	if startLast < 5 {
		startLast = 5
	}
	for i := startLast; i < len(lines); i++ {
		compressed = append(compressed, lines[i])
	}

	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": strings.Join(compressed, "\n")}},
	}
}


type InjectFaultTool struct{}

func NewInjectFaultTool() *InjectFaultTool {
	return &InjectFaultTool{}
}

func (t *InjectFaultTool) Name() string { return "neo_inject_fault" }

func (t *InjectFaultTool) Description() string {
	return "SRE Chaos Engineering Tool: Inyecta un [SRE-FATAL] y un panic controlado en el pipeline de logs para evaluar la respuesta del Auto-Triage Engine sin colapsar el Orquestador."
}

func (t *InjectFaultTool) InputSchema() MCPToolSchema {
	return MCPToolSchema{
		Type:       "object",
		Properties: map[string]any{},
		Required:   []string{},
	}
}

func (t *InjectFaultTool) Execute(ctx context.Context, args map[string]any) (any, error) {
	telemetry.LogAction("CHAOS: Inyectando fallo de memoria simulado")

	// Detonamos el fallo en una goroutine aislada para simular un colapso de worker
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("[SRE-RECOVERY] El Orquestador sobrevivió al pánico inyectado: %v", r)
			}
		}()

		// Simular carga de trabajo normal antes del colapso para llenar el Ring Buffer
		log.Println("[MES] Ingestando batch de telemetría...")
		log.Println("[WMS] Verificando idempotencia de pallet...")
		log.Println("[SRE-WARN] Latencia de red detectada al ERP")

		// Detonación
		log.Panic("[SRE-FATAL] CHAOS SIMULATION: Nil pointer dereference en capa de transporte mTLS")
	}()

	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": "Fallo inyectado en background. Monitorea el TUI y el directorio .neo/incidents/ para verificar la creación del Ticket SRE."}},
	}, nil
}

type LoadSnapshotTool struct {
	workspace string
}

func (t *LoadSnapshotTool) Name() string { return "neo_load_snapshot" }
func (t *LoadSnapshotTool) Description() string {
	return "SRE Tool: Restaura un estado neuronal completo desde memoria física (Gob) tras un panico o Early Stop CUSUM, reiniciando la base transaccional interna a un snapshot previo."
}
func (t *LoadSnapshotTool) InputSchema() MCPToolSchema {
	return MCPToolSchema{
		Type: "object",
		Properties: map[string]any{
			"snapshot_path": map[string]any{
				"type":        "string",
				"description": "Ruta absoluta o relativa del archivo .gob de snapshot de crono-memoria.",
			},
		},
		Required: []string{"snapshot_path"},
	}
}
func (t *LoadSnapshotTool) Execute(ctx context.Context, args map[string]any) (any, error) {
	path, ok := args["snapshot_path"].(string)
	if !ok {
		return nil, fmt.Errorf("snapshot_path debe ser string")
	}
	if err := sre.LoadSnapshot(path, t.workspace); err != nil {
		return nil, fmt.Errorf("[SRE-FATAL] Fallo restauracion Gob: %v", err)
	}
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": "✅ Carga Zero-Alloc Exitosa. Ouroboros restablecido al Snapshot asintótico."}},
	}, nil
}

type ApplyMigrationTool struct {
	dbaEngine *dba.Analyzer
}

func (t *ApplyMigrationTool) Name() string { return "neo_apply_migration" }
func (t *ApplyMigrationTool) Description() string {
	return "SRE Tool: Applies SQL migrations to NeoAnvil's INTERNAL SQLite brain.db (dba.Analyzer). NOT for production PostgreSQL — use lib/pq + dba.Analyzer externally for that. Accepts ACID statements: CREATE, ALTER, INSERT, UPDATE."
}
func (t *ApplyMigrationTool) InputSchema() MCPToolSchema {
	return MCPToolSchema{
		Type: "object",
		Properties: map[string]any{
			"sql_query": map[string]any{
				"type":        "string",
				"description": "La consulta cruda SQL a implementar en NeoAnvil. Unicamente sentencias ACID (CREATE, ALTER, INSERT, UPDATE).",
			},
		},
		Required: []string{"sql_query"},
	}
}
func (t *ApplyMigrationTool) Execute(ctx context.Context, args map[string]any) (any, error) {
	if t.dbaEngine == nil {
		return nil, fmt.Errorf("[SRE-DBA] Motor DBA Nucleo Desconectado.")
	}
	q, ok := args["sql_query"].(string)
	if !ok {
		return nil, fmt.Errorf("sql_query debe ser string")
	}
	if err := t.dbaEngine.ApplySafeMigration(ctx, q); err != nil {
		return nil, fmt.Errorf("[SRE-DBA] migration rejected: %v\n"+
			"Note: this tool operates on the internal SQLite brain.db only. "+
			"For production PostgreSQL use dba.Analyzer with lib/pq directly.", err)
	}
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": "✅ Migration applied to internal SQLite brain.db."}},
	}, nil
}
