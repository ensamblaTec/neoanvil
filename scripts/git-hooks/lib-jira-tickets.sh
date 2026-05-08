#!/usr/bin/env bash
# scripts/git-hooks/lib-jira-tickets.sh — shared helpers for Jira ticket
# extraction + validation, sourced by post-commit and commit-msg hooks.
#
# Resolves Épica 139 — eliminates false positives where the post-commit
# regex matched literal strings inside commit bodies (e.g. "Z0-9" from
# regex examples, "ADR-009" from documentation refs, "MCPI-130" phantom
# from operator typos). Two correctives:
#
#   1. extract_subject_tickets — parses ONLY the commit subject (head -n 1),
#      same precedent as scripts/git-hooks/sync-master-plan.sh:39 (134.A).
#      Body is free-form prose where ticket-shaped strings should NEVER
#      trigger Jira API calls.
#
#   2. validate_jira_ticket — pre-flight check via jira/get_context against
#      Nexus before the caller fires prepare_doc_pack. Returns 0 if the
#      ticket exists, non-zero otherwise. Fail-open on network errors —
#      we never want validation to block a commit.
#
# Functions exit nothing on stderr unless NEO_HOOK_VERBOSE=1 is set.

# extract_subject_tickets <commit_msg>
#   Echoes deduplicated, sorted Jira-shaped tokens found ONLY in the first
#   line of the message. Empty output if none found.
#
# Example:
#   COMMIT_MSG=$'feat(jira): MCPI-52 closes thing\n\nBody mentions ADR-009 — ignored.'
#   extract_subject_tickets "$COMMIT_MSG"
#   → "MCPI-52"  (NOT "ADR-009 MCPI-52")
extract_subject_tickets() {
    local msg="$1"
    local subject
    subject=$(printf '%s' "$msg" | head -n 1)
    printf '%s' "$subject" | grep -oE '[A-Z][A-Z0-9]+-[0-9]+' | sort -u
}

# validate_jira_ticket <ticket_key> <nexus_url> [<workspace_id>]
#   Returns:
#     0 — ticket exists in Jira
#     1 — ticket does not exist (Nexus reachable, plugin returned not-found)
#     2 — Nexus unreachable / plugin offline / network error (fail-open contract:
#         caller must decide whether to proceed; we never wedge a commit on
#         transient infra failure).
#
# Exact same payload semantics as commit-msg hook (134.B.3).
validate_jira_ticket() {
    local ticket="$1"
    local nexus_url="$2"
    local ws_id="${3:-}"

    if [[ -z "$ticket" || -z "$nexus_url" ]]; then
        return 2
    fi

    # Health check first — distinguish "ticket missing" from "Nexus down".
    if ! curl -sf --max-time 2 "$nexus_url/health" >/dev/null 2>&1; then
        return 2
    fi

    local payload
    payload=$(cat <<EOF
{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"jira/jira","arguments":{"action":"get_context","ticket_id":"$ticket"}}}
EOF
    )

    local curl_args=(-s --max-time 5 -X POST "$nexus_url/mcp/message"
        -H "Content-Type: application/json")
    [[ -n "$ws_id" ]] && curl_args+=(-H "X-Neo-Workspace: $ws_id")
    curl_args+=(-d "$payload")

    local response
    response=$(curl "${curl_args[@]}" 2>/dev/null || echo "")

    # Empty response = network glitch. Fail-open.
    if [[ -z "$response" ]]; then
        return 2
    fi

    # Plugin maps 404 → JSON-RPC error with "not found" / "does not exist" / "404".
    if printf '%s' "$response" | grep -qiE '"error".*(not found|does not exist|404)'; then
        return 1
    fi

    return 0
}

# resolve_workspace_id <repo_root>
#   Echoes the workspace ID from ~/.neo/workspaces.json whose path matches
#   <repo_root>. Empty output if no match or python3 unavailable.
#   Reads NEO_WORKSPACE_ID env first as override.
resolve_workspace_id() {
    local repo_root="$1"
    if [[ -n "${NEO_WORKSPACE_ID:-}" ]]; then
        printf '%s' "$NEO_WORKSPACE_ID"
        return 0
    fi
    if ! command -v python3 >/dev/null 2>&1; then
        return 0
    fi
    python3 -c "
import json
try:
    with open('$HOME/.neo/workspaces.json') as f:
        data = json.load(f)
    for w in data.get('workspaces', []):
        if w.get('path', '') == '$repo_root':
            print(w.get('id', ''))
            break
except Exception:
    pass
" 2>/dev/null || true
}
