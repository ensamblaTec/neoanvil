#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
BIN="$ROOT/bin"

echo "╔══════════════════════════════════════════════════════╗"
echo "║  NEO-GO SRE BUILD — Ouroboros V6.3 (PILAR XII)      ║"
echo "╚══════════════════════════════════════════════════════╝"

# ── Kill stale processes ──────────────────────────────────
echo "[1/5] Cleaning stale processes..."
pkill -f "bin/neo-mcp" 2>/dev/null || true
pkill -f "bin/neo-nexus" 2>/dev/null || true
# neo-hud removed in Épica 85 — HUD now served by neo-nexus
pkill -f "bin/neo-sandbox" 2>/dev/null || true
sleep 1

# ── Clean bin/ ────────────────────────────────────────────
echo "[2/5] Cleaning bin/..."
rm -rf "$BIN"
mkdir -p "$BIN"

# ── Build all binaries ────────────────────────────────────
echo "[3/5] Compiling Go binaries..."
cd "$ROOT"

LDFLAGS="-s -w"

echo "  -> neo-mcp (MCP orchestrator)"
go build -ldflags="$LDFLAGS" -o "$BIN/neo-mcp" ./cmd/neo-mcp

echo "  -> neo-nexus (multi-workspace dispatcher)"
go build -ldflags="$LDFLAGS" -o "$BIN/neo-nexus" ./cmd/neo-nexus

echo "  -> neo (CLI)"
go build -ldflags="$LDFLAGS" -o "$BIN/neo" ./cmd/neo

# neo-hud eliminated in Épica 85 — HUD is now embedded in neo-nexus

echo "  -> neo-sandbox (WASM sandbox)"
go build -ldflags="$LDFLAGS" -o "$BIN/neo-sandbox" ./cmd/sandbox

echo "  -> neo-tui (terminal UI)"
go build -ldflags="$LDFLAGS" -o "$BIN/neo-tui" ./cmd/neo-tui

echo "  -> neo-stress (load tester)"
go build -ldflags="$LDFLAGS" -o "$BIN/neo-stress" ./cmd/stress

# ── Generate start scripts ────────────────────────────────
echo "[4/5] Generating start scripts..."

cat > "$BIN/start-mcp.sh" << 'SCRIPT'
#!/bin/bash
# Start neo-mcp for a single workspace (default: current directory)
WORKSPACE="${1:-$(pwd)}"
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
cd "$WORKSPACE"
exec "$SCRIPT_DIR/neo-mcp" "$WORKSPACE" 2>> /tmp/neo-mcp-error.log
SCRIPT
chmod +x "$BIN/start-mcp.sh"

cat > "$BIN/start-nexus.sh" << 'SCRIPT'
#!/bin/bash
# Start neo-nexus multi-workspace dispatcher
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PORT="${1:-9000}"
exec "$SCRIPT_DIR/neo-nexus" --port "$PORT" --bin "$SCRIPT_DIR/neo-mcp" 2>&1 | tee /tmp/neo-nexus.log
SCRIPT
chmod +x "$BIN/start-nexus.sh"

# ── Summary ───────────────────────────────────────────────
echo "[5/5] Build complete!"
echo ""
echo "  Binaries:"
ls -lh "$BIN"/ | grep -v total | awk '{printf "    %-20s %s\n", $NF, $5}'
echo ""
echo "  Usage:"
echo "    Single workspace:  ./bin/start-mcp.sh /path/to/project"
echo "    Multi workspace:   ./bin/start-nexus.sh 9000"
echo "    CLI:               ./bin/neo status"
