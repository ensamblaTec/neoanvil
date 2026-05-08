package main

// obs_wire.go — helpers that bridge neo-mcp runtime signals into the
// persistent observability Store. [PILAR-XXVII/243.I]
//
// Keeps the main dispatcher free of heuristics like "how many bytes did
// this tool call carry" and the SSE subscription loop out of the boot
// function.

import (
	"encoding/json"
	"log"
	"runtime"
	"sync/atomic"

	"github.com/ensamblatec/neoanvil/pkg/config"
	"github.com/ensamblatec/neoanvil/pkg/observability"
	"github.com/ensamblatec/neoanvil/pkg/pubsub"
)

// tokensPerChar is the canonical byte-to-token estimate — neo-mcp can
// only measure bytes exchanged over JSON-RPC, not the full context
// window of the external agent. See the plan: ~4 chars ≈ 1 token.
const tokensPerChar = 4

// mcpClientAgent carries the initialize handshake identity (e.g.
// "claude-code@2.0.1") across requests. Atomic Value so concurrent
// tools/call can read without contention. [PILAR-XXVII/243.E]
var mcpClientAgent atomic.Value // stores string

func setMCPClientAgent(agent string) { mcpClientAgent.Store(agent) }

func currentMCPClientAgent() string {
	if v := mcpClientAgent.Load(); v != nil {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return "unknown"
}

// sessionAgentID carries the full session identity set on MCP initialize.
// Format: "<workspace-id>:<boot-unix>:<client-name@version>" [336.A]
var sessionAgentID atomic.Value // stores string

func setSessionAgentID(id string) { sessionAgentID.Store(id) }

func currentSessionAgentID() string {
	if v := sessionAgentID.Load(); v != nil {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

// sessionBudgetViolated tracks whether the hard token ceiling was already
// logged this session to suppress repeated log lines after crossing the limit.
var sessionBudgetViolated atomic.Bool

// recordMCPTokens persists the per-call token accounting for MCP traffic.
// inBytes/outBytes are cheap proxies — the plan treats them as upper
// bounds and documents the limitation. [PILAR-XXVII/243.E]
// When action is non-empty, the key is "tool/action" (e.g. "neo_radar/BRIEFING")
// for per-intent granularity in neo_tool_stats token_spend. [313.A]
// After recording, checks session output token ceiling and emits log
// warnings at warn/hard thresholds. [312.A]
func recordMCPTokens(cfg *config.NeoConfig, tool, action string, inBytes, outBytes int) {
	if observability.GlobalStore == nil || (inBytes == 0 && outBytes == 0) {
		return
	}
	toolKey := tool
	if action != "" {
		toolKey = tool + "/" + action
	}
	inToks := inBytes / tokensPerChar
	outToks := outBytes / tokensPerChar
	agent := currentMCPClientAgent()
	// MCP never tells us the real underlying LLM (Claude Code can run
	// Opus/Sonnet/Haiku). Config resolves the agent prefix to a pricing
	// model via inference.agent_model_map — defaults cover claude-code,
	// gemini-cli, mcp-inspector. Operators override for custom clients.
	// [PILAR-XXVII/245.Q]
	model := agent
	cost := 0.0
	if cfg != nil {
		model = cfg.Inference.ResolveAgentModel(agent)
		cost = cfg.Inference.UsageCost(model, inToks, outToks)
	}
	observability.GlobalStore.RecordTokens(observability.TokenEntry{
		Source:       observability.SourceMCPTraffic,
		Agent:        agent,
		Tool:         toolKey,
		Model:        model,
		InputTokens:  inToks,
		OutputTokens: outToks,
		Calls:        1,
		CostUSD:      cost,
	})

	// [312.A] Token budget ceiling — check session output tokens after each call.
	// bytesSent is the canonical session output counter (atomic, zero-copy).
	// Heuristic: bytesSent / tokensPerChar ≈ output tokens (same as ingestion side).
	if cfg == nil {
		return
	}
	liveCfg := LiveConfig(cfg)
	if liveCfg == nil {
		return
	}
	_, sent := GetIOStats()
	sessionOut := sent / tokensPerChar
	hardLimit := int64(liveCfg.SRE.TokenBudgetSessionHard)
	warnLimit := int64(liveCfg.SRE.TokenBudgetSessionWarn)
	if hardLimit > 0 && sessionOut > hardLimit {
		if !sessionBudgetViolated.Swap(true) {
			log.Printf("[TOKEN-BUDGET] ⚠️ SESSION_BUDGET_EXCEEDED: %d output tokens (hard limit: %d) — run neo_compress_context immediately", sessionOut, hardLimit)
		}
	} else if warnLimit > 0 && sessionOut > warnLimit {
		log.Printf("[TOKEN-BUDGET] session output tokens: %d (warn threshold: %d) — consider neo_compress_context", sessionOut, warnLimit)
	}
}

// estimateJSONBytes returns the serialised size of v. Used as a cheap
// proxy for input-token accounting: the plan fixes the heuristic at
// ~4 chars/token downstream. Best-effort — returns 0 when v is unmarshalable.
func estimateJSONBytes(v any) int {
	if v == nil {
		return 0
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return 0
	}
	return len(raw)
}

// estimateResultBytes mirrors estimateJSONBytes but accepts the tool's
// return value directly. Separate name for readability at the call site.
func estimateResultBytes(v any) int { return estimateJSONBytes(v) }

// subscribeEventsToStore wires a pubsub subscriber that persists every
// SSE event into the observability events_ring. One goroutine per bus.
// The bus's subscribe buffer is sized inside pkg/pubsub so we only read
// here; no backpressure logic needed.
func subscribeEventsToStore(bus *pubsub.Bus) {
	if bus == nil || observability.GlobalStore == nil {
		return
	}
	ch, unsub := bus.Subscribe()
	go func() {
		defer unsub()
		defer func() {
			if r := recover(); r != nil {
				log.Printf("[SRE-WARN] obs events subscriber panic: %v", r)
			}
		}()
		for ev := range ch {
			payload, _ := ev.Payload.(map[string]any)
			observability.GlobalStore.RecordEvent(string(ev.Type), severityFromEvent(ev.Type), payload)
		}
	}()
}

// severityFromEvent classifies event types for the UI banner tier.
// Aligned with the HUD handlers documented in neo-synced-directives.md:
// thermal_rollback / oom_guard / policy_veto are the crimson/orange/red
// banners; the rest default to "info".
func severityFromEvent(t pubsub.EventType) string {
	switch t {
	case pubsub.EventThermalRollback, pubsub.EventOOMGuard:
		return "critical"
	case pubsub.EventPolicyVeto:
		return "warning"
	default:
		return "info"
	}
}

// captureFullMemStats wraps observability.CaptureRuntimeMemStats and
// fills the fields that the package can't see on its own (CPG heap,
// cache hit rates). Callers provide the read-side accessors so we don't
// pull the whole orchestrator graph into pkg/observability.
func captureFullMemStats(
	cpgHeapMB, cpgHeapLimitMB int,
	queryHit, textHit, embHit float64,
) observability.MemStatsSnapshot {
	snap := observability.CaptureRuntimeMemStats()
	snap.CPGHeapMB = cpgHeapMB
	snap.CPGHeapLimitMB = cpgHeapLimitMB
	snap.QueryCacheHit = queryHit
	snap.TextCacheHit = textHit
	snap.EmbCacheHit = embHit
	// Fill the goroutine count again here to be explicit — it's cheap.
	snap.Goroutines = runtime.NumGoroutine()
	return snap
}
