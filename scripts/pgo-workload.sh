#!/usr/bin/env bash
# pgo-workload.sh — PILAR LXIX / Épica 364.B.
#
# Drives a representative workload against the running neo-mcp so that
# `make capture-profile` captures hot paths that match production usage.
#
# Covers:
#   - BRIEFING (session coordination — gatherBriefingData, fmt-heavy)
#   - SEMANTIC_CODE (HNSW search + embedder + hybrid fusion)
#   - AST_AUDIT batch (pkg/astx + go/parser hot loops)
#   - FILE_EXTRACT (AST node.End() resolution)
#   - COMPILE_AUDIT (symbol map build + buildSymbolMap)
#   - TECH_DEBT_MAP (heatmap aggregation)
#   - PROJECT_DIGEST (CodeRank PageRank + package coupling)
#   - neo_memory store/fetch/list (BoltDB Put/Get + hot cache)
#
# Durations + cadence chosen so a 60s capture sees ~5-10 invocations per
# hot tool — enough for the PGO compiler to tag branches correctly.
#
# Usage:
#   ./scripts/pgo-workload.sh [duration_seconds]
#
# Defaults: 60s duration, port 9142 (neoanvil child). Overridable via env:
#   NEO_MCP_PORT=9213 ./scripts/pgo-workload.sh 90
#
# Designed to run WHILE `make capture-profile` is scraping pprof.

set -euo pipefail

DURATION="${1:-60}"
PORT="${NEO_MCP_PORT:-9142}"
ENDPOINT="http://127.0.0.1:${PORT}/mcp/message"

# Probe endpoint before starting — fail fast if the worker is down.
if ! curl -s -o /dev/null -w "%{http_code}" "${ENDPOINT%/mcp/message}/health" | grep -qE "^(200|404)$"; then
    echo "[PGO-WORKLOAD] neo-mcp not reachable at ${ENDPOINT}" >&2
    exit 1
fi

echo "[PGO-WORKLOAD] driving ${DURATION}s workload against ${ENDPOINT}"

END_TS=$(( $(date +%s) + DURATION ))
ITER=0

# call_tool <tool_name> <args_json>
# Fires a tools/call RPC. Silent failure — workload pressure over error theater.
call_tool() {
    local tool="$1"
    local args="$2"
    curl -s -o /dev/null -X POST "${ENDPOINT}" \
        -H "Content-Type: application/json" \
        --max-time 5 \
        -d "{\"jsonrpc\":\"2.0\",\"id\":${ITER},\"method\":\"tools/call\",\"params\":{\"name\":\"${tool}\",\"arguments\":${args}}}" \
        || true
}

BRIEFING_ARGS='{"intent":"BRIEFING","mode":"compact"}'
SEMANTIC_ARGS='{"intent":"SEMANTIC_CODE","target":"HNSW graph persistence layer","min_results":3}'
TECH_DEBT_ARGS='{"intent":"TECH_DEBT_MAP","limit":10}'
PROJECT_DIGEST_ARGS='{"intent":"PROJECT_DIGEST","min_calls":3}'
AST_AUDIT_ARGS='{"intent":"AST_AUDIT","target":"pkg/rag/"}'
COMPILE_ARGS='{"intent":"COMPILE_AUDIT","target":"pkg/astx"}'
FILE_EXTRACT_ARGS='{"intent":"FILE_EXTRACT","target":"pkg/rag/hnsw.go","query":"Search","context_lines":0}'
MEM_STORE_ARGS='{"action":"store","namespace":"test","key":"pgo-workload","content":"profile-capture marker"}'
MEM_FETCH_ARGS='{"action":"fetch","namespace":"test","key":"pgo-workload"}'
MEM_LIST_ARGS='{"action":"list","namespace":"test"}'

while [ "$(date +%s)" -lt "${END_TS}" ]; do
    ITER=$((ITER + 1))
    case $((ITER % 10)) in
        0) call_tool neo_radar "${PROJECT_DIGEST_ARGS}"  ;;  # heavy
        1) call_tool neo_radar "${BRIEFING_ARGS}"        ;;
        2) call_tool neo_radar "${SEMANTIC_ARGS}"        ;;
        3) call_tool neo_memory "${MEM_STORE_ARGS}"      ;;
        4) call_tool neo_radar "${TECH_DEBT_ARGS}"       ;;
        5) call_tool neo_memory "${MEM_FETCH_ARGS}"      ;;
        6) call_tool neo_radar "${FILE_EXTRACT_ARGS}"    ;;
        7) call_tool neo_radar "${COMPILE_ARGS}"         ;;
        8) call_tool neo_memory "${MEM_LIST_ARGS}"       ;;
        9) call_tool neo_radar "${AST_AUDIT_ARGS}"       ;;  # heavy
    esac
    sleep 0.3
done

echo "[PGO-WORKLOAD] drove ${ITER} tool invocations over ${DURATION}s"
