#!/bin/bash
# SessionStart helper invoked by briefing.sh to compute tool-discipline
# mirror — lifetime cumulative diff of which neo-tools / neo_radar intents
# the agent has actually used vs the canonical inventory of 15 tools / 23
# intents documented in directive 46 [SRE-DOCTRINE-CURRENT].
#
# Output: ≤200 token Markdown block appended to the briefing additionalContext.
#         Cold-start (empty stats) renders "full inventory underused" — also
#         useful, since it surfaces what's available.
#
# Fail-soft: any error (Nexus down, JSON drift, parse failure, timeout) →
# silent exit 0. The parent briefing.sh keeps working without the diff.
#
# Opt-out: NEO_BRIEFING_DIFF_DISABLE=1.
#
# Design note: this is the B1 deliverable of feature/adaptive-runtime —
# see docs/general/adaptive-runtime-charter.md. It deliberately does NOT
# classify the user's task (that's B2's job at UserPromptSubmit). At
# SessionStart we only know the workspace state, not the intent — so the
# diff is generic. The thesis being tested: even a generic "you used
# X/Y tools" mirror at session boot moves the agent's tool-diversity score.
#
# Spec: 2026-05-15 adaptive-runtime initiative.

set -uo pipefail

[ "${NEO_BRIEFING_DIFF_DISABLE:-0}" = "1" ] && exit 0

NEXUS_URL="${NEO_NEXUS_URL:-http://127.0.0.1:9000}"

# This script lives in neoanvil; the literal default reflects that. Cross-
# workspace invocations (e.g. from strategos hooks) set NEO_WORKSPACE_ID
# via env. Dead `case "$PWD"` block (both branches returned the same
# value) removed 2026-05-15.
WORKSPACE_ID="${NEO_WORKSPACE_ID:-neoanvil-35694}"

# Probe Nexus (1s) — parent already did this, but defensive when invoked
# standalone for tests.
curl -fsS --max-time 1 -o /dev/null "${NEXUS_URL}/health" 2>/dev/null || exit 0

# Fetch neo_tool_stats (2s cap). Returns JSON-RPC envelope wrapping the
# stats payload (which is itself JSON serialised as text).
response=$(curl -fsS --max-time 2 \
  -H "Content-Type: application/json" \
  -H "X-Neo-Workspace: ${WORKSPACE_ID}" \
  -X POST "${NEXUS_URL}/mcp/message" \
  -d '{"jsonrpc":"2.0","id":42,"method":"tools/call","params":{"name":"neo_tool_stats","arguments":{"sort_by":"calls"}}}' \
  2>/dev/null) || exit 0

[ -z "$response" ] && exit 0

# Parse + diff via python3. Stays inline so the helper is one-file
# auditable. Token budget enforced by output line count + truncation
# at end.
#
# Response passed via env var (not stdin) because `python3 - <<'PYEOF'`
# already consumes stdin from the heredoc; piping into it shadows the
# script with the response and breaks parsing silently.
NEO_TS_RESPONSE="$response" python3 - <<'PYEOF' 2>/dev/null
import json, os, sys

raw = os.environ.get("NEO_TS_RESPONSE", "")
if not raw:
    sys.exit(0)

try:
    env = json.loads(raw)
    payload_text = env.get("result", {}).get("content", [{}])[0].get("text", "{}")
    payload = json.loads(payload_text)
except Exception:
    sys.exit(0)

# Canonical inventory from directive 46 [SRE-DOCTRINE-CURRENT].
# Updates here should trip the directive too — keep in sync.
canonical_tools = {
    # 7 Macro
    "neo_radar", "neo_sre_certify_mutation", "neo_daemon", "neo_chaos_drill",
    "neo_cache", "neo_command", "neo_memory",
    # 7 Specialist (neo_forge_tool deprecated per directive 46 — removed
    # 2026-05-15 so it stops appearing as "underused" forever).
    "neo_compress_context", "neo_apply_migration", "neo_download_model",
    "neo_log_analyzer", "neo_tool_stats", "neo_debt", "neo_local_llm",
}
canonical_intents = {
    "BLAST_RADIUS", "SEMANTIC_CODE", "DB_SCHEMA", "TECH_DEBT_MAP",
    "READ_MASTER_PLAN", "SEMANTIC_AST", "READ_SLICE", "BRIEFING",
    "AST_AUDIT", "HUD_STATE", "FRONTEND_ERRORS", "WIRING_AUDIT",
    "COMPILE_AUDIT", "GRAPH_WALK", "PROJECT_DIGEST", "INCIDENT_SEARCH",
    "PATTERN_AUDIT", "CONTRACT_QUERY", "FILE_EXTRACT", "CONTRACT_GAP",
    "INBOX", "PLUGIN_STATUS", "CLAUDE_FOLDER_AUDIT",
}

observed_tools_raw = set()
observed_intents = set()
for t in payload.get("tools", []):
    name = t.get("name", "")
    calls = t.get("lifetime_count", 0) or t.get("window_count", 0)
    if calls <= 0:
        continue
    if name.startswith("neo_radar/"):
        observed_intents.add(name.split("/", 1)[1])
    elif name:
        observed_tools_raw.add(name)

# Filter observed against canonical to avoid double-counting non-canonical
# entries (pkg/inference internal metric, deprecated forge_tool, etc.).
observed_tools = observed_tools_raw & canonical_tools

# Cold-start: agent's first session. Render inventory + priority list
# instead of "diff" (a diff against nothing is misleading).
is_coldstart = len(observed_tools) == 0 and len(observed_intents) == 0

tool_cov_pct = (len(observed_tools) * 100) // max(len(canonical_tools), 1)
intent_cov_pct = (len(observed_intents) * 100) // max(len(canonical_intents), 1)

# Priority list — these are the high-leverage tools that tend to be
# underused per the 2026-05-15 self-audit. Static for B1; B2's classifier
# will replace this with task-aware ranking.
priority_underused = [
    "neo_radar/COMPILE_AUDIT",      # before editing unknown pkg
    "neo_radar/READ_SLICE",         # for files >=100 lines
    "neo_compress_context",         # after 3+ edits
    "neo_radar/CLAUDE_FOLDER_AUDIT",# detect doctrine drift
    "neo_radar/PATTERN_AUDIT",      # detect recurring anti-patterns
    "neo_chaos_drill",              # post-edit HTTP surface
    "neo_log_analyzer",             # semantic log correlation
    "neo_radar/INCIDENT_SEARCH",    # recovery context
]

def status(name):
    if "/" in name:
        return "✓" if name.split("/", 1)[1] in observed_intents else "✗"
    return "✓" if name in observed_tools else "✗"

# Trim to top 5 underused (those marked ✗ in priority list).
top_underused = [n for n in priority_underused if status(n) == "✗"][:5]

print("")
print("---")
print("")
print("## Tool Discipline Mirror (lifetime cumulative)")
if is_coldstart:
    print("")
    print("**Cold-start session.** No tool usage history yet. Available surface:")
    print(f"- {len(canonical_tools)} MCP tools, {len(canonical_intents)} `neo_radar` intents")
    print("- Priority recommendations for first task:")
    for name in priority_underused[:5]:
        print(f"  - `{name}`")
else:
    print("")
    print(f"- **Tools used:** {len(observed_tools)}/{len(canonical_tools)} ({tool_cov_pct}%) · "
          f"**Intents used:** {len(observed_intents)}/{len(canonical_intents)} ({intent_cov_pct}%)")
    if top_underused:
        print("- **High-leverage tools never invoked** (top 5 from priority list):")
        for name in top_underused:
            print(f"  - `{name}`")
        print(f"- Per directive 58 [TOOL-DISCIPLINE-CHECKLIST]: prefer these over bash/Read native when applicable.")
    else:
        print(f"- Priority list fully covered. Continue measuring diversity per session.")
print("")
PYEOF
