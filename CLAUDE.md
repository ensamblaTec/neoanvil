# NeoAnvil — MCP Orchestrator (Go)

Motor SRE de orquestación MCP. **Ouroboros V10.6** · 15 tools MCP · 3 plugins (Jira, DeepSeek, GitHub) · Local LLM via Ollama. Pure-Go build (native) / CGO en Docker stage 3 (tree-sitter).

> Snapshot completo pre-refactor: [`docs/general/claude-md-archive-2026-05-13.md`](./docs/general/claude-md-archive-2026-05-13.md). Contrato base reutilizable: [`CLAUDE-global.md`](./CLAUDE-global.md) + [`docs/general/neo-global.md`](./docs/general/neo-global.md).

## Build / Audit

| Comando | Qué hace |
|---|---|
| `make build-mcp` | Compila `bin/neo-mcp` |
| `make build-tui` | Compila TUI (go.work submódulo) |
| `make audit` | staticcheck + ineffassign + modernize + coverage |
| `make audit-ci` | Fail-on-new vs `.neo/audit-baseline.txt` |
| `make rebuild-restart` | SIGTERM graceful + health-verified restart |
| `go test -short ./pkg/...` | Test suite rápido |

## Invariantes Ouroboros

1. **BRIEFING** (`neo_radar intent=BRIEFING`) al inicio de sesión, al reanudar, y al cambiar tarea. Sin excepciones — el resumen comprimido NO reemplaza la sincronización.
2. **BLAST_RADIUS** antes de cualquier edit. Si retorna `not_indexed` → fallback Grep + continuar con `confidence:low`.
3. **`neo_sre_certify_mutation`** después de cada edit en `.go/.ts/.tsx/.js/.css`. Batch único, atomic rollback default, TTL 15min (pair) / 5min (fast).

## Modos

| Mode | `NEO_SERVER_MODE` | Cert | `neo_daemon` | TTL |
|---|---|---|---|---|
| **pair** | `pair` | AST + Bouncer + tests | PROHIBIDO | 15min |
| **fast** | `fast` | AST + index only | PROHIBIDO | 5min |
| **daemon** | `daemon` | Full (suspendido si RAPL>60W) | Habilitado | 5min |

## Doctrina + reglas activas

- Doctrina operativa (laws): [`.claude/skills/sre-doctrine/SKILL.md`](./.claude/skills/sre-doctrine/SKILL.md) (auto-load) · Flujo paso a paso (procedural): [`.claude/skills/sre-workflow/SKILL.md`](./.claude/skills/sre-workflow/SKILL.md) (auto-load)
- Tools MCP (15 tools, 60+ ops): [`.claude/skills/sre-tools/SKILL.md`](./.claude/skills/sre-tools/SKILL.md) (task) · Schemas completos: [`docs/general/sre-tools-reference.md`](./docs/general/sre-tools-reference.md)
- Leyes de código Go/MCP: skill `/sre-quality` (paths-scoped auto-load en `pkg/**/*.go`, `cmd/**/*.go`) · Archive: [`docs/general/code-quality-laws.md`](./docs/general/code-quality-laws.md)
- Doctrina Database: skill `/sre-db` (paths-scoped auto-load en `pkg/dba/`, `pkg/rag/`, `migrations/`)
- Directivas vivas (auto-sync BoltDB↔disk): [`.claude/rules/neo-synced-directives.md`](./.claude/rules/neo-synced-directives.md)
- Skills task-mode disponibles: `/jira-workflow`, `/jira-create-pilar`, `/jira-doc-from-commit`, `/deepseek-workflow`, `/github-workflow`, `/local-llm-workflow`, `/neo-doc-pack`, `/sre-federation`, `/sre-troubleshooting`, `/brain-doctor`, `/daemon-flow`, `/daemon-trust`. Ver [`.claude/skills/`](./.claude/skills/).

## Federación

- **Workspace registry:** `~/.neo/workspaces.json`. Migración: `workspaces.json` + `.neo/db/{brain,hnsw,cpg}.{db,bin}` + `~/.neo/credentials.json`.
- **Tier ownership** (workspace → project → org → nexus): [`docs/general/tier-ownership.md`](./docs/general/tier-ownership.md). bbolt no soporta mixed RW+RO — cada tier tiene leader único.
- **Nexus dispatcher** (multi-workspace MCP proxy): config `~/.neo/nexus.yaml`. Routing por `target_workspace > URL path > X-Neo-Workspace header > active workspace`. HUD: `http://127.0.0.1:8087/`.

## Convenciones

- Commits: `feat|fix|docs|refactor|test|chore(<scope>): <descripción>`.
- Config extension: yaml tag → `defaultNeoConfig()` → backfill en `LoadConfig()` → `neo.yaml.example` → docs. Backfill OBLIGATORIO ([CONFIG-FIELD-BACKFILL-RULE]).
- Hot-reload safe: `inference.*`, `governance.*`, `sentinel.*`, `rag.{query,embedding}_cache_capacity`, `cpg.max_heap_mb`, `cognitive.strictness`. Unsafe (puertos, DB paths, provider) → `make rebuild-restart`.
- Master plan: `master_plan.md` autoritativo (BRIEFING cuenta `- [ ]` vs `- [x]`). Cerrado: `master_done.md`.
