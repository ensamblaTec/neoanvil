#!/bin/sh
# NeoAnvil container entrypoint (Area 1.1.B / fix 2-3).
#
# Runs as root just long enough to:
#   1. Reconcile ownership of the named volumes (Docker mounts them
#      as root:root by default; the container process runs as `neo`).
#   2. Seed ~/.neo/nexus.yaml from the bundled template if the volume
#      is empty (first boot in a fresh deployment).
#
# Then drops privileges via `su-exec neo:neo` and execs neo-nexus.
# `su-exec` is a tiny static binary (~10KB) installed via apk; it is
# the alpine-native equivalent of gosu. Unlike `su` or `sudo`, it
# replaces PID 1 cleanly so signal forwarding (SIGTERM, SIGINT) works.

set -eu

NEO_HOME="/home/neo"
NEO_USER="neo"

# ---- 1. Volume ownership reconciliation ----------------------------------
# Idempotent: chown is a no-op if files are already neo-owned.
# Only chown the directories we own — never recurse into a bind-mounted
# host repo at /home/neo/work which may contain files owned by the host
# operator's UID.
#
# `-h` (--no-dereference) is CRITICAL. Without it, a malicious symlink
# left in the named volume by a previously compromised container (e.g.,
# `link → /etc/shadow`) would cause `chown -R` to follow the symlink at
# the leaf and rewrite ownership on the host's sensitive files. With
# `-h`, the symlink itself is chowned, leaving the target untouched.
# [DS-AUDIT 1.1.B Finding 1, SEV 8]
if [ -d "$NEO_HOME/.neo" ]; then
    chown -hR "$NEO_USER:$NEO_USER" "$NEO_HOME/.neo"
fi

# /home/neo/work: only chown the mountpoint itself (so neo can `cd` and
# `ls`), not the contents. If the operator bind-mounted their repo from
# the host with UID != neo's UID, recursing would brick their ownership.
# `-h` for symlink-safety per Finding 3.
if [ -d "$NEO_HOME/work" ]; then
    chown -h "$NEO_USER:$NEO_USER" "$NEO_HOME/work" || true
fi

# ---- 2. Seed configs on first boot ---------------------------------------
# Three sources, three destinations, identical first-boot semantics:
#
#   IMAGE-bundled (Dockerfile COPY):
#     /home/neo/.neo-seed/nexus.yaml  →  $NEO_HOME/.neo/nexus.yaml
#
#   HOST-bind RO (compose volumes, Pattern D — Area 1.4):
#     /home/neo/.neo-host/credentials.json  →  $NEO_HOME/.neo/credentials.json
#     /home/neo/.neo-host/plugins.yaml       →  $NEO_HOME/.neo/plugins.yaml
#
# All three are gated on `[ ! -e ] && [ ! -L ]` of the destination so:
#   (a) operator edits inside the volume never get overwritten on
#       subsequent boots
#   (b) a malicious symlink at the destination cannot redirect the cp
#       to an arbitrary host file (cp dereferences by default; the
#       guard refuses to copy through symlinks too).
# [DS-AUDIT 1.1.B Finding 2, SEV 5]
seed_if_absent() {
    src="$1"
    dst="$2"
    label="$3"
    if [ ! -f "$src" ]; then
        return 0  # source absent (e.g., host config not present) — silent skip
    fi
    # Treat an empty source as absent. `make docker-up` `touch`es empty
    # placeholders for credentials.json + plugins.yaml when the host
    # operator hasn't run `neo login` yet — those empty files satisfy
    # docker-compose's bind-mount-must-exist rule but should NOT be
    # propagated into the named volume (they would shadow real configs
    # later AND make Nexus fail to parse on first boot).
    if [ ! -s "$src" ]; then
        return 0  # source empty — same UX as absent
    fi
    # Refuse to seed when the SOURCE is a symlink — host operator (or
    # attacker with write access to ~/.neo) could redirect through a
    # symlink to e.g. ~/.ssh/id_rsa, leaking host secrets into the
    # container's writable volume where neo-mcp could exfiltrate them
    # through plugin calls. [Manual-audit Finding #2, SEV 7]
    if [ -L "$src" ]; then
        echo "[entrypoint] WARNING: $src is a symlink — refusing to seed (potential secret-leak vector)"
        return 0
    fi
    if [ -L "$dst" ]; then
        echo "[entrypoint] WARNING: $dst is a symlink — refusing to overwrite ($label)"
        return 0
    fi
    # Re-seed when host source is newer than the seeded copy. Without
    # this, credentials rotated on the host stay stale in the named
    # volume — operator's `docker compose down && up` cycle does NOT
    # re-trigger seeding because the volume persists. mtime-based
    # check is non-destructive: only re-seeds when host has a strictly
    # newer file. [DS-AUDIT 1.4 Finding 2, SEV 7]
    if [ -e "$dst" ] && [ "$src" -ot "$dst" ]; then
        return 0  # already populated and not older than dest; preserve
    fi
    # Atomic write via temp + rename. A kill mid-`cp` would otherwise
    # leave a partial dst that the next-boot guard (`[ -e dst ]`) skips,
    # leaving Nexus to load a corrupt config. Using a sibling tempfile
    # keeps the rename(2) on the same filesystem (POSIX-atomic).
    # [DS-AUDIT 1.4 Finding 3, SEV 5]
    tmp="${dst}.seeding"
    cp "$src" "$tmp"
    chown -h "$NEO_USER:$NEO_USER" "$tmp"
    chmod 600 "$tmp"
    mv "$tmp" "$dst"
    echo "[entrypoint] seeded $dst from $label"
}

# Image-bundled defaults (always available — copied from Dockerfile)
seed_if_absent "$NEO_HOME/.neo-seed/nexus.yaml"      "$NEO_HOME/.neo/nexus.yaml"      "image template"

# Host-bind seeds (Pattern D — only present if compose mounted them)
seed_if_absent "$NEO_HOME/.neo-host/credentials.json" "$NEO_HOME/.neo/credentials.json" "host bind (read-only)"
seed_if_absent "$NEO_HOME/.neo-host/plugins.yaml"     "$NEO_HOME/.neo/plugins.yaml"     "host bind (read-only)"

# ---- 2b. Auto-register the bind-mounted repo as a workspace --------------
# Without this, a fresh `make docker-up` brings up Nexus with an empty
# registry — no dispatch targets, MCP clients see "workspace not found".
# We register /home/neo/work/repo (the operator's bind-mounted source)
# only on first boot (when the registry is missing); subsequent boots
# preserve any operator-modified registry. [Final-flow audit Gap D]
WORKSPACES_JSON="$NEO_HOME/.neo/workspaces.json"
REPO_DIR="$NEO_HOME/work/repo"
if [ -d "$REPO_DIR" ] && [ ! -e "$WORKSPACES_JSON" ] && [ ! -L "$WORKSPACES_JSON" ]; then
    repo_basename=$(basename "$REPO_DIR")
    # 4-byte random suffix to match the convention `<name>-<8hex>` from
    # the native `neo space use` CLI. /dev/urandom + od is portable
    # across BusyBox and full coreutils.
    repo_suffix=$(head -c 4 /dev/urandom | od -An -tx1 | tr -d ' \n')
    cat > "$WORKSPACES_JSON" <<JSON_EOF
{
  "workspaces": [
    {
      "id": "${repo_basename}-${repo_suffix}",
      "path": "${REPO_DIR}",
      "name": "${repo_basename}",
      "dominant_lang": "",
      "health": "unknown",
      "added_at": "$(date -u +%Y-%m-%dT%H:%M:%SZ)",
      "transport": "sse"
    }
  ],
  "active_id": "${repo_basename}-${repo_suffix}"
}
JSON_EOF
    chown -h "$NEO_USER:$NEO_USER" "$WORKSPACES_JSON"
    chmod 644 "$WORKSPACES_JSON"
    echo "[entrypoint] auto-registered $REPO_DIR as workspace ${repo_basename}-${repo_suffix}"
fi

# ---- 3. Drop privileges and exec neo-nexus -------------------------------
# `exec` replaces this shell with neo-nexus so PID 1 is the actual app
# and signals propagate correctly. `su-exec` does NOT fork — same PID.
exec su-exec "$NEO_USER:$NEO_USER" /usr/local/bin/neo-nexus "$@"
