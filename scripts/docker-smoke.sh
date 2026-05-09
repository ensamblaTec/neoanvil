#!/usr/bin/env bash
# scripts/docker-smoke.sh — end-to-end smoke test of the Docker stack
# (Area 1.3.A). Builds + ups + asserts + tears down. Designed to fail
# loudly on the first regression so CI / `make test-integration` get
# a precise error.
#
# Idempotent: tears down any existing stack at start (no double-up).
# Cleans up on exit (trap).
#
# Skips the GPU assertion when `nvidia-container-toolkit` is absent
# so the script doubles as a CPU-only CI gate.

set -euo pipefail

PORT=${NEXUS_PORT:-9000}
HUD_PORT=${HUD_PORT:-8087}
COMPOSE=${COMPOSE:-docker compose}
TIMEOUT_SECONDS=${SMOKE_TIMEOUT:-180}

err() { printf "\033[31m[smoke] FAIL\033[0m %s\n" "$*" >&2; exit 1; }
ok()  { printf "\033[32m[smoke] OK\033[0m   %s\n" "$*"; }
log() { printf "\033[36m[smoke]\033[0m      %s\n" "$*"; }

cleanup() {
    log "tearing down..."
    $COMPOSE down --timeout 5 >/dev/null 2>&1 || true
}
trap cleanup EXIT

# ─── 1. Pre-flight ────────────────────────────────────────────────────
log "preflight: docker compose available?"
$COMPOSE version >/dev/null 2>&1 || err "docker compose v2 plugin missing"

log "preflight: image present?"
if ! docker image inspect neoanvil:local >/dev/null 2>&1; then
    log "  building (this may take a few minutes)..."
    make docker-build >/dev/null 2>&1 || err "make docker-build failed"
fi

log "preflight: leftover containers from previous run?"
$COMPOSE down --timeout 5 >/dev/null 2>&1 || true

# ─── 2. Bring up ──────────────────────────────────────────────────────
log "docker compose up -d"
$COMPOSE up -d >/dev/null 2>&1 || err "docker compose up failed"

# Wait for neoanvil to become healthy. The container has its own
# healthcheck (wget /status) with start_period=30s, so we poll
# `docker compose ps` for the (healthy) marker rather than racing
# the HTTP endpoint.
log "waiting for neoanvil container to be healthy..."
deadline=$(( $(date +%s) + TIMEOUT_SECONDS ))
while true; do
    state=$($COMPOSE ps --format json neoanvil 2>/dev/null | python3 -c '
import sys, json
try:
    out = sys.stdin.read().strip()
    if not out:
        print("missing"); sys.exit(0)
    # Older compose returns one obj per line; newer returns array
    obj = json.loads("[" + ",".join(l for l in out.splitlines() if l.strip()) + "]")
    if not obj: print("missing")
    else: print(obj[0].get("Health", obj[0].get("State", "unknown")))
except Exception as e:
    print("err:" + str(e))
' 2>/dev/null || echo "missing")
    case "$state" in
        healthy) break ;;
        missing|err:*) err "container not present: $state" ;;
        *) ;;
    esac
    [ "$(date +%s)" -ge "$deadline" ] && err "timeout after ${TIMEOUT_SECONDS}s waiting for healthy (last state=$state)"
    sleep 2
done
ok "neoanvil container healthy"

# ─── 3. Endpoint assertions ───────────────────────────────────────────
log "GET /status → expect at least 1 workspace"
status_body=$(curl -s --max-time 5 "http://127.0.0.1:${PORT}/status") || err "/status unreachable"
ws_count=$(echo "$status_body" | python3 -c 'import sys,json; print(len(json.load(sys.stdin)))')
[ "$ws_count" -lt 1 ] && err "/status returned 0 workspaces (auto-register failed?)"
ok "/status: $ws_count workspace(s) registered"

# [Bug-2/3 regression] Wait until at least one workspace is `running`
# (not just registered). The previous smoke skipped this check and
# the 1.4.E pass falsely greenlit a build whose child neo-mcp was
# stuck in boot timeout.
log "wait for first workspace to reach status=running (max 60s)"
deadline=$(( $(date +%s) + 60 ))
while true; do
    running=$(curl -s --max-time 3 "http://127.0.0.1:${PORT}/status" | python3 -c '
import sys, json
try:
    d = json.load(sys.stdin)
    print(sum(1 for w in d if w.get("status") == "running"))
except Exception:
    print(0)
' 2>/dev/null || echo 0)
    [ "$running" -ge 1 ] && break
    [ "$(date +%s)" -ge "$deadline" ] && err "no workspace reached running state in 60s (child neo-mcp boot regression?)"
    sleep 2
done
ok "first workspace is running"

# [Phase F regression] /openapi.json is wired in the child's sseMux,
# so it only responds once the child is up. Both the spec response
# and the Swagger UI are smoke-tested here.
log "GET /openapi.json → expect valid OpenAPI 3.0 spec"
openapi_status=$(curl -s -o /tmp/openapi.json -w '%{http_code}' --max-time 5 "http://127.0.0.1:${PORT}/openapi.json")
[ "$openapi_status" = "200" ] || err "/openapi.json returned HTTP $openapi_status (expected 200)"
python3 -c "
import json, sys
d = json.load(open('/tmp/openapi.json'))
assert d.get('openapi','').startswith('3.'), 'openapi version missing'
assert 'paths' in d, 'paths missing'
print(f'  spec ok: openapi={d[\"openapi\"]}, paths={len(d[\"paths\"])}, x-mcp-tools={len(d.get(\"x-mcp-tools\", []))}')
" || err "/openapi.json body is malformed"
ok "/openapi.json valid"

log "GET /docs → expect Swagger UI HTML"
docs_status=$(curl -s -o /tmp/docs.html -w '%{http_code}' --max-time 5 "http://127.0.0.1:${PORT}/docs")
[ "$docs_status" = "200" ] || err "/docs returned HTTP $docs_status (expected 200 — Swagger UI)"
grep -qi "swagger" /tmp/docs.html || err "/docs body missing 'swagger' marker"
ok "/docs Swagger UI served"

log "GET HUD /  → expect HTML"
hud_body=$(curl -s --max-time 5 "http://127.0.0.1:${HUD_PORT}/") || err "HUD unreachable"
echo "$hud_body" | grep -qi '<!doctype' || err "HUD did not return HTML"
ok "HUD returns HTML"

# ─── 4. Bind-mount sanity ─────────────────────────────────────────────
log "bind mount: container sees host repo?"
docker exec neoanvil ls /home/neo/work/repo/CLAUDE.md >/dev/null 2>&1 \
    || err "bind mount missing — /home/neo/work/repo/CLAUDE.md not visible"
ok "bind mount reachable"

# ─── 5. Seeded configs ────────────────────────────────────────────────
log "seeded configs: nexus.yaml, plugins.yaml present?"
docker exec neoanvil test -f /home/neo/.neo/nexus.yaml \
    || err "nexus.yaml not seeded into volume"
ok "nexus.yaml seeded"

# credentials.json is only seeded if host has one — non-fatal if absent
if docker exec neoanvil test -f /home/neo/.neo/credentials.json 2>/dev/null; then
    perms=$(docker exec neoanvil stat -c '%a' /home/neo/.neo/credentials.json 2>/dev/null)
    [ "$perms" = "600" ] || err "credentials.json perms=$perms (expected 600)"
    ok "credentials.json seeded with 0600"
else
    log "  credentials.json absent on host — seeding skipped (expected if no \`neo login\`)"
fi

# ─── 6. GPU passthrough (optional) ────────────────────────────────────
if docker exec neoanvil-ollama nvidia-smi --query-gpu=name --format=csv,noheader 2>/dev/null | head -1 | grep -q .; then
    gpu=$(docker exec neoanvil-ollama nvidia-smi --query-gpu=name --format=csv,noheader 2>/dev/null | head -1)
    ok "GPU passthrough working: $gpu"
else
    log "  GPU passthrough skipped (no nvidia-container-toolkit)"
fi

# ─── 7. Ollama responsiveness ─────────────────────────────────────────
log "ollama: GET /api/tags → expect JSON"
ollama_port=${OLLAMA_PORT:-11434}
curl -s --max-time 5 "http://127.0.0.1:${ollama_port}/api/tags" | python3 -m json.tool >/dev/null \
    || err "ollama /api/tags returned non-JSON"
ok "ollama responsive on :${ollama_port}"

# ─── 8. Tear-down preserves volumes ───────────────────────────────────
log "down + verify volumes survive"
$COMPOSE down --timeout 5 >/dev/null 2>&1
project=${COMPOSE_PROJECT_NAME:-$(whoami)_neoanvil}
docker volume inspect "${project}_neoanvil-state" >/dev/null 2>&1 \
    || err "volume ${project}_neoanvil-state was unexpectedly deleted"
ok "named volumes survive 'down' (would delete only on 'down -v')"

# Cleanup trap will re-down to be safe.
printf "\033[32m[smoke] all 8 checks passed\033[0m\n"
