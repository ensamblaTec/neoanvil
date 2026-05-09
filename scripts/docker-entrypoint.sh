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

# ---- 2. Seed nexus.yaml on first boot ------------------------------------
# Docker propagates image content into a fresh named volume on first
# mount, so the seed file from the Dockerfile (/home/neo/.neo-seed/)
# becomes available here. We only copy if the destination is missing
# — never overwrite an operator's edits.
#
# Defense against TOCTOU symlink-redirect attack: if the destination
# path exists OR is a dangling symlink, refuse to copy. cp dereferences
# symlinks by default, so a malicious symlink at the destination could
# cause cp (running as root) to overwrite an arbitrary host-mounted
# file. The dual `[ ! -e ]` + `[ ! -L ]` guard rejects both real files
# and symlinks (broken or not). [DS-AUDIT 1.1.B Finding 2, SEV 5]
if [ -f "$NEO_HOME/.neo-seed/nexus.yaml" ] \
        && [ ! -e "$NEO_HOME/.neo/nexus.yaml" ] \
        && [ ! -L "$NEO_HOME/.neo/nexus.yaml" ]; then
    cp "$NEO_HOME/.neo-seed/nexus.yaml" "$NEO_HOME/.neo/nexus.yaml"
    chown -h "$NEO_USER:$NEO_USER" "$NEO_HOME/.neo/nexus.yaml"
    echo "[entrypoint] seeded $NEO_HOME/.neo/nexus.yaml from bundled template"
elif [ -L "$NEO_HOME/.neo/nexus.yaml" ]; then
    echo "[entrypoint] WARNING: $NEO_HOME/.neo/nexus.yaml is a symlink — refusing to overwrite (possible attack)"
fi

# ---- 3. Drop privileges and exec neo-nexus -------------------------------
# `exec` replaces this shell with neo-nexus so PID 1 is the actual app
# and signals propagate correctly. `su-exec` does NOT fork — same PID.
exec su-exec "$NEO_USER:$NEO_USER" /usr/local/bin/neo-nexus "$@"
