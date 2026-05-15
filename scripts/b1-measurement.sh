#!/bin/bash
# B1 measurement helper — captures the tool-discipline mirror state
# at a point in time to a CSV log so consecutive sessions can be
# diffed and the diversity-score trajectory plotted.
#
# Usage:
#   scripts/b1-measurement.sh snapshot   → record current state to log
#   scripts/b1-measurement.sh report     → render trajectory summary
#   scripts/b1-measurement.sh clear      → wipe log (start fresh A/B)
#
# Log format: .neo/b1-measurements.csv
#   timestamp,tools_used,tools_total,intents_used,intents_total,treatment,workspace_boot,notes
#   2026-05-15T16:00:00Z,7,15,9,23,baseline,neoanvil,startup
#   2026-05-15T17:00:00Z,8,15,10,23,treatment,strategos,auto-post
#
# `treatment` column is environment-driven (NEO_BRIEFING_DIFF_DISABLE):
#   - "1" or unset on B1-disabled run        → baseline
#   - "0" or unset on B1-enabled run         → treatment
# This way the same script captures both arms of the A/B test.
#
# `workspace_boot` column tracks WHICH workspace the session opened in,
# even though `neo_tool_stats` is Nexus-global (same counter across all
# workspaces this Nexus serves). The tag lets us correlate task focus
# with mirror exposure when the agent moves between workspaces.
#
# Spec: docs/general/adaptive-runtime-charter.md (B1 ship criterion).
# Cross-workspace finding: docs/general/adaptive-runtime-charter.md#openq3

set -uo pipefail

CMD="${1:-snapshot}"
LOG_FILE="${NEO_B1_LOG:-.neo/b1-measurements.csv}"
NEXUS_URL="${NEO_NEXUS_URL:-http://127.0.0.1:9000}"
WORKSPACE_ID="${NEO_WORKSPACE_ID:-neoanvil-35694}"

ensure_log() {
  mkdir -p "$(dirname "$LOG_FILE")"
  if [ ! -f "$LOG_FILE" ]; then
    echo "timestamp,tools_used,tools_total,intents_used,intents_total,treatment,workspace_boot,notes" > "$LOG_FILE"
  fi
}

snapshot() {
  ensure_log
  # Probe Nexus
  if ! curl -fsS --max-time 2 -o /dev/null "${NEXUS_URL}/health" 2>/dev/null; then
    echo "[B1-MEASURE] Nexus unreachable at ${NEXUS_URL} — snapshot skipped" >&2
    return 1
  fi

  local response
  response=$(curl -fsS --max-time 5 \
    -H "Content-Type: application/json" \
    -H "X-Neo-Workspace: ${WORKSPACE_ID}" \
    -X POST "${NEXUS_URL}/mcp/message" \
    -d '{"jsonrpc":"2.0","id":99,"method":"tools/call","params":{"name":"neo_tool_stats","arguments":{"sort_by":"calls"}}}' \
    2>/dev/null) || {
      echo "[B1-MEASURE] neo_tool_stats call failed" >&2
      return 1
    }

  local treatment="treatment"
  if [ "${NEO_BRIEFING_DIFF_DISABLE:-0}" = "1" ]; then
    treatment="baseline"
  fi

  # Reuse the canonical-intersection logic from briefing-behavior-diff.sh.
  # Returns CSV row: tools_used,tools_total,intents_used,intents_total
  local counts
  counts=$(NEO_TS_RESPONSE="$response" python3 - <<'PYEOF'
import json, os, sys

try:
    env = json.loads(os.environ.get("NEO_TS_RESPONSE", "{}"))
    payload = json.loads(env.get("result", {}).get("content", [{}])[0].get("text", "{}"))
except Exception:
    sys.exit(1)

canonical_tools = {
    # 14 active tools — kept in lockstep with briefing-behavior-diff.sh.
    # neo_forge_tool removed 2026-05-15 (deprecated per directive 46).
    "neo_radar", "neo_sre_certify_mutation", "neo_daemon", "neo_chaos_drill",
    "neo_cache", "neo_command", "neo_memory",
    "neo_compress_context", "neo_apply_migration", "neo_download_model",
    "neo_log_analyzer", "neo_tool_stats", "neo_debt", "neo_local_llm",
}
canonical_intents = {
    "BLAST_RADIUS","SEMANTIC_CODE","DB_SCHEMA","TECH_DEBT_MAP","READ_MASTER_PLAN",
    "SEMANTIC_AST","READ_SLICE","BRIEFING","AST_AUDIT","HUD_STATE","FRONTEND_ERRORS",
    "WIRING_AUDIT","COMPILE_AUDIT","GRAPH_WALK","PROJECT_DIGEST","INCIDENT_SEARCH",
    "PATTERN_AUDIT","CONTRACT_QUERY","FILE_EXTRACT","CONTRACT_GAP","INBOX",
    "PLUGIN_STATUS","CLAUDE_FOLDER_AUDIT",
}

observed_tools_raw = set()
observed_intents = set()
for t in payload.get("tools", []):
    name = t.get("name", "")
    calls = t.get("lifetime_count", 0) or t.get("window_count", 0)
    if calls <= 0: continue
    if name.startswith("neo_radar/"):
        observed_intents.add(name.split("/", 1)[1])
    elif name:
        observed_tools_raw.add(name)
observed_tools = observed_tools_raw & canonical_tools

print(f"{len(observed_tools)},{len(canonical_tools)},{len(observed_intents)},{len(canonical_intents)}")
PYEOF
) || {
    echo "[B1-MEASURE] parse failure" >&2
    return 1
  }

  local ts
  ts=$(date -u +"%Y-%m-%dT%H:%M:%SZ")
  local note="${NEO_B1_NOTE:-}"
  # workspace_boot tag: derive from NEO_WORKSPACE_BOOT (set by per-workspace
  # b1-snapshot.sh) or fall back to detecting from $WORKSPACE_ID prefix.
  local ws_boot="${NEO_WORKSPACE_BOOT:-}"
  if [ -z "$ws_boot" ]; then
    case "$WORKSPACE_ID" in
      neoanvil*)            ws_boot="neoanvil" ;;
      strategos-*)          ws_boot="strategos" ;;
      strategosia-*)        ws_boot="strategosia_frontend" ;;
      *)                    ws_boot="unknown" ;;
    esac
  fi
  echo "${ts},${counts},${treatment},${ws_boot},${note}" >> "$LOG_FILE"
  echo "[B1-MEASURE] ${treatment}@${ws_boot} ${ts}: ${counts}  → ${LOG_FILE}"
}

report() {
  if [ ! -f "$LOG_FILE" ]; then
    echo "[B1-MEASURE] no log yet — run \`snapshot\` first"
    return 1
  fi
  echo "## B1 Measurement Trajectory"
  echo ""
  echo "Log file: \`${LOG_FILE}\`"
  echo ""
  # Aggregate by treatment arm
  python3 - "$LOG_FILE" <<'PYEOF'
import csv, sys
from collections import defaultdict

log = sys.argv[1]
buckets = defaultdict(list)
ws_counter = defaultdict(int)
with open(log) as f:
    rdr = csv.DictReader(f)
    for row in rdr:
        try:
            tools_pct = (int(row["tools_used"]) * 100) // int(row["tools_total"])
            intents_pct = (int(row["intents_used"]) * 100) // int(row["intents_total"])
            wsb = (row.get("workspace_boot") or "unknown").strip() or "unknown"
            buckets[row["treatment"]].append((row["timestamp"], int(row["tools_used"]), int(row["intents_used"]), tools_pct, intents_pct, wsb, row.get("notes", "")))
            ws_counter[wsb] += 1
        except Exception:
            continue

if not buckets:
    print("No measurements logged yet.")
    sys.exit(0)

for arm in ("baseline", "treatment"):
    rows = buckets.get(arm, [])
    if not rows:
        print(f"### {arm}: 0 snapshots")
        print("")
        continue
    avg_tools = sum(r[1] for r in rows) / len(rows)
    avg_intents = sum(r[2] for r in rows) / len(rows)
    print(f"### {arm}: {len(rows)} snapshots · avg tools {avg_tools:.1f}/15 · avg intents {avg_intents:.1f}/23")
    print("")
    print("| timestamp | tools | intents | t% | i% | ws_boot | notes |")
    print("|---|---|---|---|---|---|---|")
    for ts, t, i, tp, ip, wsb, n in rows[-10:]:  # last 10
        print(f"| {ts} | {t} | {i} | {tp}% | {ip}% | {wsb} | {n} |")
    print("")

if ws_counter:
    print("### Workspace boot coverage")
    print("")
    for wsb, n in sorted(ws_counter.items(), key=lambda kv: -kv[1]):
        print(f"- `{wsb}`: {n} snapshots")
    print("")

# Cross-arm comparison
if buckets.get("baseline") and buckets.get("treatment"):
    b_intents = sum(r[2] for r in buckets["baseline"]) / len(buckets["baseline"])
    t_intents = sum(r[2] for r in buckets["treatment"]) / len(buckets["treatment"])
    delta = t_intents - b_intents
    sign = "↑" if delta > 0 else ("↓" if delta < 0 else "→")
    print(f"### Comparativa: intents promedio baseline={b_intents:.1f} → treatment={t_intents:.1f} ({sign}{abs(delta):.1f})")
    print("")
    if delta >= 2.0:
        print("**Ship criterion (≥+2 intents promedio sostenido): cumplido** ✅")
    elif delta > 0:
        print(f"Progreso visible pero no llega aún al ship criterion (+{delta:.1f}/≥2.0 needed).")
    else:
        print("Sin mejora — B1 no está moviendo la métrica. Considerar discard o pivot.")
PYEOF
}

clear_log() {
  rm -f "$LOG_FILE"
  echo "[B1-MEASURE] log cleared: ${LOG_FILE}"
}

case "$CMD" in
  snapshot) snapshot ;;
  report)   report ;;
  clear)    clear_log ;;
  *)        echo "usage: $0 {snapshot|report|clear}" >&2; exit 1 ;;
esac
