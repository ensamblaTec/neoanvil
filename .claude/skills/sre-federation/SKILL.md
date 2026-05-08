---
name: sre-federation
description: Federation tier ownership (workspace/project/org/nexus), PILAR XXXI project federation walk-up, PILAR XXXII CPG fast-boot, PILAR XXXIII auth+multi-tenant BoltDB, PILAR LXVI nexus debt registry, PILAR LXVII org tier. Use when working with .neo-project/, .neo-org/, knowledge store, debt registry, or cross-workspace coordination.
paths:
  - "pkg/federation/**"
  - "pkg/nexus/**"
  - "pkg/cpg/**"
  - "pkg/auth/**"
  - "pkg/knowledge/**"
---

# SRE Federation — Tier Ownership + PILARs XXXI-LXVII

> 4-tier knowledge store, project/org coordination, debt registry,
> cross-workspace patterns. Migrado de
> `.claude/rules/neo-synced-directives.md` (split temático
> 2026-04-28).

---

## 4-tier ownership (354.Z + LXVII)

| Tier | Backing | Owner | Cómo escriben los no-dueños |
|------|---------|-------|------------------------------|
| `workspace` | `<ws>/.neo/db/knowledge.db` o alias de project | Local child o coord en federation | Proxy al coord |
| `project` | `.neo-project/db/knowledge.db` | **coordinator_workspace** declarado en `.neo-project/neo.yaml` | Proxy via Nexus MCP routing |
| `org` [LXVII] | `.neo-org/db/org.db` | **coordinator_project** declarado en `.neo-org/neo.yaml` | Non-coord projects reciben `ErrOrgStoreReadOnly` (HTTP proxy pendiente) |
| `nexus` | `~/.neo/shared/db/global.db` | **Nexus dispatcher** (singleton) | Proxy HTTP a `/api/v1/shared/nexus/*` |

bbolt NO soporta mixed RW+RO — cada tier necesita un leader único.
BRIEFING compact muestra `tier:project=leader|proxy:X|legacy`.

**Namespaces reservados org-tier:** `directives` (autosincs a
`.claude/rules/org-*.md` via 355.B), `memory`, `debt`, `context`.

---

## PILAR XXXI — Project Federation

`.neo-project/neo.yaml` en root del proyecto con campos:
- `project_name` (string)
- `member_workspaces []string` (paths absolutos)
- `dominant_lang` (go|python|rust|typescript)
- `ignore_dirs_add []string` (sumado a workspace ignore_dirs)
- `coordinator_workspace` (declara dueño tier:project)

`LoadConfig` descubre via walk-up; aplica `MergeConfigs` 3-tier:
workspace > project > global. `pkg/workspace/registry.Add()`
detecta `.neo-project/neo.yaml` y asigna `Type="project"`.

**BLAST_RADIUS scatter:**
```
neo_radar(intent:"BLAST_RADIUS", target:"file.go", target_workspace:"project")
```
→ `handleBlastRadiusProjectScatter` — análisis paralelo (sem=4) a
todos los `member_workspaces` vía Nexus POST. Respuesta incluye
`project_health_table` (running|error|not_running por workspace).

**API Go:**
- `config.WriteProjectConfig(rootDir, *ProjectConfig)`
- `config.LoadProjectConfig(wsDir)`
- `config.MergeConfigs(workspace, project)`

**CRÍTICO (NEXUS-WORKSPACE-TYPE-GUARD):** En `~/.neo/workspaces.json`,
los workspaces MIEMBRO de un project federation NUNCA deben tener
`"type": "project"`. registry.Add() solo asigna ese tipo cuando
`.neo-project/neo.yaml` existe DIRECTAMENTE dentro del workspace
path — NO por walk-up. Si un miembro tiene `type: "project"` por
error: (1) `filteredWorkspaces()` lo excluye del boot, (2) la API
`/start` devuelve 400 "project-federation roots cannot be started",
(3) OAuth `.well-known/oauth-protected-resource` devuelve workspace
incorrecto. **Fix:** eliminar el campo `type` del entry +
`kill -HUP $(pgrep neo-nexus)`.

**Walk-up safety:** NUNCA usar `filepath.Join(workspace, ".neo-project")`
directo — siempre `findNeoProjectDir(startDir)` que hace walk-up
hasta 5 niveles. Workspaces son subdirectorios del proyecto; `.neo-project/`
vive en el padre.

---

## PILAR XXXII — CPG Fast-Boot

neo-mcp serializa el Code Property Graph a `.neo/db/cpg.bin` (Gob +
schema version). Al arrancar:

```
cpg.LoadCPG(persistPath) →
  cpg.IsCPGStale(workspace, header) →
    si NO stale: Manager.LoadSnapshot(g) → bootedFast=true
    si stale o schema mismatch: cold build vía SSA
```

BRIEFING muestra `boot=fast|cold` en línea CPG.

**neo.yaml:**
- `cpg.persist_path` default `.neo/db/cpg.bin`
- `cpg.persist_interval_minutes` default 15 (0=desactivado)

**Guardado:** goroutina periódica + SIGTERM handler llama
`SaveSnapshot()`. **Forzar cold build:** `rm .neo/db/cpg.bin`.

---

## PILAR XXXIII — Auth Foundation + Multi-Tenant BoltDB

`~/.neo/credentials.json` (0600) almacena tokens y TenantIDs por
provider. `pkg/auth/keystore.go`: `Load(path)` / `Save(creds, path)` /
`AddEntry(provider, token, tenantID)` / `GetByProvider(provider)`.

Boot main.go: si entry "default" tiene TenantID → inyecta en
`cfg.Auth.TenantID` + `state.SetActiveTenant(id)`. `cfg.Auth` es
`yaml:"-"` — nunca se persiste en neo.yaml.

**Multi-Tenant BoltDB:** cuando TenantID activo,
`memexBucketName()` retorna `"memex_buffer:TenantID"` (lazy
CreateBucketIfNotExists). Sin TenantID → bucket legacy.

**Migración V7.1+:** copiar `~/.neo/credentials.json` +
`workspaces.json` + `db/*.db` + `cpg.bin` a la nueva máquina.

---

## PILAR LXVI — Nexus Debt Registry

Cuando BRIEFING compact prefija `⚠️ NEXUS-DEBT:N P0:M |`, ejecutar:

```
neo_debt(action:"affecting_me")
```

Tras resolver issue (típicamente `lsof+kill+restart`):

```
neo_debt(action:"resolve", scope:"nexus", id, resolution)
```

Requiere `X-Nexus-Token` en env `NEO_NEXUS_TOKEN` si
`api.auth_token` está configurado en nexus.yaml.

**4-tier debt:**
- `workspace` → `.neo/technical_debt.md` local
- `project` → `.neo-project/SHARED_DEBT.md`
- `nexus` → `~/.neo/nexus_debt.md` via HTTP al dispatcher
- `org` → `.neo-org/DEBT.md` (LXVII)

Record/resolve MVP solo para workspace/nexus; project/org pendientes.
Dedup por `sha256(title+sorted_affected_workspaces)` dentro de window
configurable (default 15min).

---

## PILAR LXVII — Org Tier

4-tier federation operacional. Tier "org" habilita coordinación
cross-project:

- `neo_memory(scope:"org")` persiste en `.neo-org/db/org.db`
  (coordinator_project posee el flock)
- `neo_memory(learn, scope:"org")` escribe a `.neo-org/DIRECTIVES.md`
  con ID monotónico + supersedes auto-deprecate
- `neo_debt(scope:"org")` lee/escribe `.neo-org/DEBT.md` con
  `AffectedProjects []string` y priority P0-P3
- 355.B auto-sync mirrorea `.neo-org/knowledge/directives/*.md` a
  cada workspace miembro como `.claude/rules/org-*.md` al boot

Non-coordinator projects reciben `ErrOrgStoreReadOnly`. HTTP proxy
pendiente.

**Escalation rule:**
- Deuda afecta >1 workspace → `scope:"project"`
- Deuda afecta >1 project → `scope:"org"`
- Issues detectados automáticamente por Nexus → `scope:"nexus"`

---

## Federation files por scope (355.B auto-sync)

- **Workspace:** `.claude/rules/*.md` (local, incluye
  `org-*.md` auto-mirrorado)
- **Project:** `.neo-project/{neo.yaml, SHARED_DEBT.md,
  CONTRACT_PROPOSALS.md, PROJECT_TASKS.md, knowledge/}`
- **Org:** `.neo-org/{neo.yaml, DEBT.md, DIRECTIVES.md,
  knowledge/directives/}`

---

## Hot-reload safe (PILAR XI)

Campos safe que se recargan automáticamente al editar `neo.yaml`:
- `inference.*`, `governance.*`, `sentinel.*`
- `cognitive.strictness`
- `sre.safe_commands`, `sre.unsupervised_max_cycles`,
  `sre.kinetic_*`, `sre.consensus_*`, `sre.digital_twin_testing`
- `rag.query_cache_capacity`, `rag.embedding_cache_capacity` →
  `Resize()` inmediato
- `cpg.max_heap_mb` → re-evaluado en cada `Graph()` call

**Unsafe (requieren `make rebuild-restart`):**
- `server.*` (puertos)
- `ai.provider`
- DB paths
- certs
- `rag.vector_quant` (estructura HNSW cambia)

---

## See also

- `skills/sre-doctrine/SKILL.md` — flujo Ouroboros
- `skills/sre-tools/SKILL.md` — neo_debt + neo_memory tools
- `skills/sre-quality/SKILL.md` — leyes Nexus children + safe HTTP
- `docs/tier-ownership.md` — detalle full del modelo bbolt RW/RO
- `docs/pilar-xxx-xxxiii-guide.md` — guías PILAR XXXI-XXXIII
- `docs/pilar-lxvii-org-tier.md` — guía LXVII completa
