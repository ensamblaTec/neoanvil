#!/usr/bin/env bash
# scripts/git-hooks/sync-master-plan.sh — auto-mark master_plan.md tasks as
# done when their ID appears in a commit message.
#
# Resolves Épica 134.A. The post-commit hook calls this with the latest
# commit message + repo root. Tasks whose ID appears in the message and
# are still "- [ ]" are flipped to "- [x]" in .neo/master_plan.md.
#
# Usage:
#   sync-master-plan.sh <commit_msg> <repo_root>
#
# Recognised task IDs: <NN..NNNN>(.<segment>)+ where each segment is
# alphanumeric. Examples: 134.C.1, 132.A, 131.B.4, 130.4.2.
#
# Logging:
#   stderr: "[neo-hook] master_plan: marked N task(s) as done"
#   nothing logged when N=0 to keep clean commits quiet.
#
# Failure mode: fail-open (always exits 0). Errors logged to stderr.

set -u

COMMIT_MSG="${1:-}"
REPO_ROOT="${2:-}"

if [[ -z "$COMMIT_MSG" || -z "$REPO_ROOT" ]]; then
    exit 0
fi

PLAN="$REPO_ROOT/.neo/master_plan.md"
if [[ ! -f "$PLAN" ]]; then
    exit 0
fi

# Extract task tokens from the SUBJECT LINE only (first line of commit msg).
# Body lines often contain incidental references — regex examples, paths,
# version strings — that should not auto-mark tasks. Subject is the canonical
# statement of "what this commit closes".
SUBJECT=$(printf '%s' "$COMMIT_MSG" | head -n 1)

# Pattern: at least 2 leading digits, then >=1 dotted segment of alphanums.
# Examples matched: 134.C.1, 132.A, 131.B.4, 130.4.2, 128.1
# Examples not matched: MCPI-42 (different prefix), 1.0.0 (only 1 leading digit)
TOKENS=$(printf '%s' "$SUBJECT" | grep -oE '\b[0-9]{2,4}(\.[A-Z0-9]+)+\b' | sort -u || true)

if [[ -z "$TOKENS" ]]; then
    exit 0
fi

MARKED=0
for tok in $TOKENS; do
    # Escape "." for grep/sed; the rest of the chars in our tokens are safe.
    esc="${tok//./\\.}"
    if grep -qE "^- \[ \] \*\*${esc}\*\*" "$PLAN"; then
        # sed -i.bak portable across BSD (macOS) and GNU.
        sed -i.bak -E "s/^- \[ \] (\*\*${esc}\*\*)/- [x] \1/" "$PLAN"
        rm -f "${PLAN}.bak"
        MARKED=$((MARKED + 1))
    fi
done

if [[ $MARKED -gt 0 ]]; then
    echo "[neo-hook] master_plan: marked $MARKED task(s) as done" >&2
fi

exit 0
