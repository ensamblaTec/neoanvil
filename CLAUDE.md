# NeoAnvil — PARQUEADO (2026-07-21)

> **Estado: PARKED.** Este repo es un codebase/experimento en Go. Su antigua doctrina MCP
> (BRIEFING, `neo_radar`, `neo_sre_certify_mutation`, los 15 tools, modos pair/fast/daemon)
> **ya NO gobierna las sesiones** y no debe seguirse.

## Por qué está parqueado

El servidor MCP de neoanvil **nunca estuvo registrado en Claude Code** (`~/.claude.json::mcpServers`
= `playwright, context7, vault` — sin `neoanvil`; tampoco hay `.mcp.json` en el repo). La doctrina
antigua ordenaba usar 15 tools MCP que **no existen en la sesión** → era imposible de cumplir → cada
sesión caía a tools nativas e ignoraba el resto, erosionando la autoridad del `CLAUDE.md` completo.

Estado en esta máquina (2026-07-21): `~/.neo/` no existe (nunca booteado), Nexus/neo-mcp caídos, y el
build nativo pure-Go **falla** — `CGO_ENABLED=0 go build ./cmd/neo-mcp` → `go-tree-sitter/iter.go:
undefined: Node` (dependencia bit-rotted; la afirmación "Native build = Pure Go" del doc está obsoleta).

## Cómo trabajar en este repo mientras está parqueado

- Trátalo como un proyecto **Go normal**: tools nativas (Read, Grep, Edit) + `go build` / `go test`.
- Para búsqueda semántica usa el **MCP `vault`** (`mcp__vault__*`) y el CLI **`kb`** — requieren
  **Ollama** arriba (`systemctl --user is-active ollama`; modelo `nomic-embed-text`).
- Convenciones globales (git flow, commits **sin** co-autoría de IA, etc.): en `~/.claude/CLAUDE.md`.
- Las reglas en `.claude/rules/*.md` describen la doctrina antigua: **inactivas** mientras el MCP no
  esté conectado. No las sigas como obligatorias.

## Comandos de build (por si retomas el codebase)

| Qué | Cómo |
|-----|------|
| Build MCP | `make build-mcp` — ⚠️ el build nativo hoy rompe en tree-sitter; requiere CGO/tags |
| Build CLI | `go build -o bin/neo ./cmd/neo` |
| Tests | `go test ./...` |
| Audit | `make audit` (staticcheck + ineffassign + modernize) |

## Cómo revivir neoanvil (si algún día se decide)

1. Arreglar el build (CGO/tags para los parsers tree-sitter — `go-tree-sitter/iter.go`).
2. Bootstrap `~/.neo/` + `make rebuild-restart` (levanta Nexus + neo-mcp + Ollama).
3. Crear `.mcp.json` → `http://127.0.0.1:9000/workspaces/<id>/mcp/sse` y togglear `/mcp` en Claude Code.
4. Reintroducir la doctrina gradualmente desde el archivo histórico (abajo), verificando que las tools
   aparezcan como `mcp__neoanvil__*` en la sesión antes de declararla activa.

## Doctrina histórica completa (archivada)

Todo el `CLAUDE.md` anterior (28 KB: 15 tools / 60+ ops, 23 intents, tiers, federación, plugins, ADRs)
está en **[`docs/neoanvil-doctrine-archive.md`](./docs/neoanvil-doctrine-archive.md)** —
**referencia histórica, NO seguir como doctrina activa** hasta que el MCP esté registrado y booteado.
