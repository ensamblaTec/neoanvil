# NeoAnvil Project Federation — Guía Operativa

_Template para proyectos multi-workspace. Copiar a `.claude/rules/` en cada workspace miembro y ajustar la tabla "Workspaces de este proyecto"._

Este documento describe cómo trabajar correctamente en un **Project Federation**: un conjunto de 2+ workspaces NeoAnvil vinculados por un `.neo-project/neo.yaml` común. Aplica a agentes que operen en cualquiera de los workspaces miembro.

## Índice

1. [Arquitectura del proyecto](#1-arquitectura-del-proyecto)
2. [Protocolo de sesión multi-workspace](#2-protocolo-de-sesión-en-contexto-multi-workspace)
3. [Análisis de impacto cross-workspace (BLAST_RADIUS)](#3-análisis-de-impacto-cross-workspace)
4. [CONTRACT_QUERY — validar frontera API](#4-contract_query--validar-frontera-api)
5. [KnowledgeStore compartido](#5-knowledgestore-compartido--neo_memory-con-namespace)
6. [Búsqueda semántica cross-workspace](#6-búsqueda-semántica-cross-workspace)
7. [CPG walk + digest federado](#7-cpg-walk--digest-federado)
8. [Lectura quirúrgica de código](#8-lectura-quirúrgica-de-código)
9. [Certificación y commits multi-repo](#9-certificación-en-contexto-multi-workspace)
10. [Deuda técnica compartida](#10-deuda-técnica-compartida)
11. [Cache observability en federación](#11-cache-observability-en-federación)
12. [Operaciones frecuentes — referencia rápida](#12-operaciones-frecuentes--referencia-rápida)
13. [Diagnóstico y troubleshooting](#13-diagnóstico-y-troubleshooting)

---

## 1. Arquitectura del proyecto

La federación tiene tres capas:

```
<project-root>/
├── .neo-project/
│   ├── neo.yaml          ← config de federación (project_name, member_workspaces, coordinator_workspace)
│   ├── knowledge/        ← Markdown sync dir del tier:"project" (mirror de knowledge.db)
│   ├── db/
│   │   ├── knowledge.db  ← KnowledgeStore project-tier (coordinator owns flock)
│   │   └── shared.db     ← HNSW vectorial compartido (coordinator owns flock)
│   └── SHARED_DEBT.md    ← deuda técnica cross-workspace
├── <workspace-A>/        ← coordinator: opens knowledge.db RW
└── <workspace-B>/        ← non-coord: ks=nil, proxies tier:"project" via Nexus

~/.neo/shared/db/global.db    ← Nexus-global tier (tier:"nexus", Nexus owns)
```

Cada workspace tiene su propio proceso neo-mcp con puerto dinámico asignado por Nexus. Los procesos son independientes — no comparten BoltDB local. La coordinación pasa por tiers:

- **tier:"project"** (`.neo-project/db/knowledge.db`) — coordinator workspace owns flock; non-coords proxy via Nexus MCP routing.
- **tier:"nexus"** (`~/.neo/shared/db/global.db`) — Nexus dispatcher owns; all children proxy via HTTP REST `/api/v1/shared/nexus/*`.
- APIs del orquestador (`target_workspace`, scatter explícito) para operaciones cross-workspace no relacionadas con el knowledge store.
- SHARED_DEBT.md en `.neo-project/` (deuda técnica de frontera).

Declarar `coordinator_workspace:` en `.neo-project/neo.yaml` para determinismo — el dueño del flock es fijo. Sin este campo, legacy first-to-boot wins (nondeterministic).

Consultar IDs y puertos activos:

```bash
curl -s http://127.0.0.1:9000/status | python3 -c \
  "import sys,json; [print(f'{w[\"name\"]} id={w[\"id\"]} port={w[\"port\"]} status={w[\"status\"]}') for w in json.load(sys.stdin)]"
```

---

## 2. Protocolo de sesión en contexto multi-workspace

### 2.1 BRIEFING obligatorio al arrancar

La primera acción de cualquier sesión — sin excepción — es BRIEFING en el workspace donde está el foco actual:

```
neo_radar(intent: "BRIEFING")
```

Esto aplica igual cuando reanudas desde un resumen de contexto comprimido: el resumen es orientativo, el orquestador es autoritativo.

Si necesitas ver el estado de otro workspace, usa `target_workspace`:

```
neo_radar(intent: "BRIEFING", target_workspace: "<workspace-id>")
```

El BRIEFING reporta qué workspace tiene `binary_stale`, cuánta RAM consume cada proceso, lista archivos certificados en la sesión, y muestra el pool de caches. Esta información no está en ningún resumen comprimido previo.

### 2.2 Al cambiar de tarea entre workspaces

Si pasas de trabajar en el backend a trabajar en el frontend (o viceversa), ejecuta un BRIEFING compact en el workspace destino antes de editar:

```
neo_radar(intent: "BRIEFING", mode: "compact", target_workspace: "<id-del-workspace-destino>")
```

Verificar que el workspace destino está `status=running` antes de cualquier tool call.

---

## 3. Análisis de impacto cross-workspace

### 3.1 BLAST_RADIUS con scatter a todos los miembros

Para analizar el impacto de un cambio que cruza la frontera (p.ej., cambio de API entre backend y frontend):

```
neo_radar(intent: "BLAST_RADIUS", target: "ruta/al/archivo.go", target_workspace: "project")
```

`target_workspace: "project"` activa `handleBlastRadiusProjectScatter` — análisis paralelo (sem=4) a todos los workspaces miembro vía Nexus POST. La respuesta incluye `project_health_table` con el status de cada workspace. Solo válido cuando el workspace actual tiene `.neo-project/neo.yaml` arriba en el árbol.

Si el scatter falla (workspace parado o unreachable): la respuesta lo indica por workspace. No bloquea — usar Grep cross-repo como fallback.

### 3.2 BLAST_RADIUS en workspace específico

Si sabes que el cambio solo impacta un workspace concreto:

```
neo_radar(intent: "BLAST_RADIUS", target: "ruta/archivo.go", target_workspace: "<workspace-id>")
```

### 3.3 BLAST_RADIUS solo para callers cross-boundary

Cuando solo te interesa saber qué TS consume un handler Go (sin el CPG walk completo):

```
neo_radar(intent: "BLAST_RADIUS", target: "backend/handler/user.go", force_contract: true)
```

`force_contract: true` (Épica 256.B) salta el walk del CPG y devuelve solo los callers cross-boundary vía OpenAPI + AST routes Go + patrones fetch TypeScript. Mucho más rápido cuando solo quieres el diff de frontera.

---

## 4. CONTRACT_QUERY — validar frontera API

Cuando el backend define un endpoint y el frontend lo consume, usar CONTRACT_QUERY para verificar alineación:

```
neo_radar(intent: "CONTRACT_QUERY", target: "/api/v1/endpoint-path", method: "POST")
```

Retorna:
- **Request schema** (campos Go del handler, tipos, required)
- **Response shape** (si está documentado en OpenAPI o inferible del código)
- Callers TypeScript detectados en el frontend (archivo:línea)

Con `validate_payload` opcional para verificar un JSON concreto contra el schema inferido:

```
neo_radar(intent: "CONTRACT_QUERY", target: "/api/v1/users", method: "POST",
          validate_payload: "{\"email\": \"test@example.com\"}")
```

CONTRACT_QUERY es la herramienta correcta cada vez que frontend y backend van a cambiar un contrato compartido. Ejecutar **antes** de editar cualquiera de los dos lados.

---

## 5. KnowledgeStore compartido — `neo_memory` con namespace

El KnowledgeStore compartido en `.neo-project/knowledge/` permite que todos los workspaces persistan y consulten conocimiento común: contratos, decisiones de diseño, restricciones de negocio, deuda técnica.

### Guardar un contrato o decisión

```
neo_memory(action: "store", namespace: "contracts",
           key: "user-endpoint-v2",
           content: "POST /api/v1/users acepta email+role. role es required desde 2026-04. Frontend usa FormData, no JSON.")
```

### Recuperar

```
neo_memory(action: "fetch", namespace: "contracts", key: "user-endpoint-v2")
```

### Buscar por semántica + texto

```
neo_memory(action: "search", namespace: "contracts", query: "autenticación de usuario")
```

### Listar todas las entradas de un namespace

```
neo_memory(action: "list", namespace: "contracts")
```

### Eliminar (marca como inactivo)

```
neo_memory(action: "drop", namespace: "contracts", key: "user-endpoint-v1")
```

**Namespaces recomendados:**

| Namespace | Contenido | Quién escribe |
|-----------|-----------|---------------|
| `contracts` | Contratos de API, payloads, versiones | Backend al definir endpoint; frontend al documentar uso |
| `decisions` | ADRs — decisiones de arquitectura | Cualquier workspace al tomar una decisión no-obvia |
| `incidents` | Post-mortems y lecciones aprendidas | El workspace donde ocurrió el incidente |
| `debt` | Deuda técnica cross-workspace | Se sincroniza con `SHARED_DEBT.md` |
| `shared` | Configuración cross-workspace general | Ambos workspaces |
| `types` | DTOs, enums canónicos cross-boundary | Backend al cambiar modelos |

El namespace se mapea a un tenant BoltDB distinto — no hay colisiones entre namespaces ni entre workspaces. Flag `hot: true` carga la entrada en RAM al boot para lookup O(1).

---

## 6. Búsqueda semántica cross-workspace

Para buscar un concepto o patrón en TODO el proyecto (no solo en el workspace activo):

```
neo_radar(intent: "SEMANTIC_CODE", target: "patrón de autenticación JWT",
          cross_workspace: true)
```

`cross_workspace: true` (Épica 274.C) hace scatter a todos los workspaces miembro vía Nexus y retorna resultados deduplicados por `file:line`. **NO usar `target_workspace: "project"`** — ese flag solo existe para BLAST_RADIUS.

Para búsqueda en un workspace específico:

```
neo_radar(intent: "SEMANTIC_CODE", target: "hook useAuth",
          target_workspace: "<id-frontend>")
```

**Regla de degradación:** si el resultado es 0 → cambiar inmediatamente a Grep nativo. No reintentar SEMANTIC_CODE con otra frase — el problema es cobertura del índice, no la query. Desde 2026-04-23 el fallback grep tokeniza la query y devuelve hits ranked por número de términos que matchean.

---

## 7. CPG walk + digest federado

### 7.1 GRAPH_WALK — explorar subgrafo de calls

Cuando BLAST_RADIUS identifica un nodo central y quieres ver qué llama hacia abajo:

```
neo_radar(intent: "GRAPH_WALK", target: "NombreDeLaFunción",
          max_depth: 2, edge_kind: "call")
```

`target` debe ser el nombre exacto del símbolo (no `Type.Method`). Edge kinds: `call` (default), `cfg`, `contain`, `all`.

**Limitación documentada:** receiver methods pueden retornar `No reachable nodes` porque el SSA no emite call edges desde métodos con receiver. Si esto ocurre con CPG activo → usar BLAST_RADIUS (inverso: quién llama al símbolo).

### 7.2 PROJECT_DIGEST — resumen ejecutivo

Para calibrar prioridades al inicio de una sesión de refactor:

```
neo_radar(intent: "PROJECT_DIGEST", min_calls: 3)
```

Retorna: hotspots de mutación, top CodeRank, package coupling (filtrable con `filter_package` + `min_calls`), HNSW coverage, nodos y aristas CPG del workspace activo. Para digest del otro workspace: añadir `target_workspace`.

---

## 8. Lectura quirúrgica de código

### 8.1 FILE_EXTRACT para símbolos nombrados (recomendado)

```
neo_radar(intent: "FILE_EXTRACT", target: "backend/internal/handler/user.go",
          query: "CreateUser", context_lines: 0)
```

`context_lines: 0` retorna el cuerpo completo del símbolo usando `ast.Node.End()`. Mucho más eficiente que READ_SLICE para funciones nombradas (~375 tokens vs 42K en archivos grandes).

**Validaciones:** `query` debe ser ≥ 2 caracteres (queries de 1 char matchean letras dentro de identificadores y generan ruido). Para queries de 2-4 caracteres, el matching usa word-boundary automático.

Para múltiples símbolos en una sola llamada:

```
neo_radar(intent: "FILE_EXTRACT", target: "...", symbols: ["CreateUser", "UpdateUser"], context_lines: 0)
```

### 8.2 READ_SLICE para bloques sin nombre

```
neo_radar(intent: "READ_SLICE", target: "frontend/src/hooks/useAuth.ts",
          start_line: 42, limit: 30)
```

Obligatorio para archivos ≥ 100 líneas. **Prohibido `Read` nativo en archivos grandes** — no actualiza métricas IO, no aplica OOM-safe chunking.

### 8.3 Flujo óptimo para exploración de paquete nuevo

1. `COMPILE_AUDIT target: "./ruta/paquete"` → retorna `symbol_map` con líneas exactas de cada símbolo exportado + estado build + lista de archivos stale-cert.
2. `FILE_EXTRACT` con el símbolo de interés y `context_lines: 0`.

Esta secuencia es O(1) en IO frente a READ_SLICE a ciegas.

---

## 9. Certificación en contexto multi-workspace

Regla fundamental: **cada archivo se certifica en el workspace al que pertenece.**

```
# Archivo backend → certificar en workspace backend
neo_sre_certify_mutation(
  mutated_files: ["/ruta/absoluta/backend/internal/handler/user.go"],
  complexity_intent: "FEATURE_ADD"
)

# Archivo frontend → certificar en workspace frontend
neo_sre_certify_mutation(
  mutated_files: ["/ruta/absoluta/frontend/src/components/UserForm.tsx"],
  complexity_intent: "FEATURE_ADD"
)
```

Si la herramienta se invoca desde un workspace que no es el propietario del archivo, el sello se emite correctamente pero el RAG indexa en el workspace receptor — el grafo del workspace propietario no se actualiza. **Siempre certificar en el workspace propietario.**

Batch cross-workspace: si editas archivos en ambos workspaces en la misma sesión, **hacer dos llamadas** — una por workspace. No mezclar paths de workspaces distintos en un mismo batch.

**TTL del sello:**
- Pair mode: 15 min
- Fast / Daemon mode: 5 min
- Override via `sre.certify_ttl_minutes` en `neo.yaml` por workspace

Si el pre-commit hook rechaza por TTL expirado: re-certificar y commitear en la misma secuencia sin pausa.

### 9.1 Dry-run para pre-flight

```
neo_sre_certify_mutation(mutated_files: [...], complexity_intent: "FEATURE_ADD", dry_run: true)
```

Corre AST + build checks sin escribir el sello ni indexar al RAG. Safe para archivos aún en edición.

### 9.2 Commits — una repo, un commit

Cada workspace es un repositorio git independiente. El flujo correcto:

```bash
# 1. Certificar todos los archivos del workspace A (batch único, justo antes del commit)
neo_sre_certify_mutation(mutated_files: [...archivos del workspace A...], ...)

# 2. Commit en repo A
cd /ruta/workspace-A
git add -p
git commit -m "feat(api): descripción del cambio"

# 3. Certificar archivos del workspace B
neo_sre_certify_mutation(mutated_files: [...archivos del workspace B...], ...)

# 4. Commit en repo B
cd /ruta/workspace-B
git add -p
git commit -m "feat(frontend): descripción correlacionada"
```

El pre-commit hook de cada workspace verifica solo los archivos de su propio árbol. No hay hook cross-repo.

---

## 10. Deuda técnica compartida

La deuda técnica cross-workspace se registra en `.neo-project/SHARED_DEBT.md` y en el namespace `debt` del KnowledgeStore compartido (el watcher los mantiene sincronizados).

Cada workspace mantiene además su propia `.neo/technical_debt.md` para deuda local.

**Registrar deuda nueva (desde cualquier workspace):**

```
neo_memory(action: "store", namespace: "debt",
           key: "api-v1-users-sin-versionar",
           content: "El endpoint POST /api/v1/users no tiene header X-API-Version. Un cambio en backend rompe frontend sin aviso. Fix: añadir header + CONTRACT_QUERY como CI gate.",
           tags: ["backend", "frontend", "contract"])
```

**Ver deuda pendiente:**

```
neo_memory(action: "list", namespace: "debt")
```

**Marcar como resuelta:**

```
neo_memory(action: "drop", namespace: "debt", key: "api-v1-users-sin-versionar")
```

Priorización en `SHARED_DEBT.md`: secciones P0 (blocker), P1 (alto), P2 (medio), P3 (bajo).

---

## 11. Cache observability en federación

Cada workspace mantiene sus propios caches (QueryCache, TextCache, EmbeddingCache). No hay cache compartido entre workspaces.

```
# Stats del workspace activo
neo_cache(action: "stats")

# Invalidar todas las entries O(1) tras edit manual sin certify
neo_cache(action: "flush")

# Warmup con misses recientes
neo_cache(action: "warmup", from_recent: true)
```

En un refactor que toca ambos workspaces, flushear caches en ambos tras las certificaciones:

```
neo_cache(action: "flush")                                        # workspace actual
neo_cache(action: "flush", target_workspace: "<otro-workspace>")  # otro workspace
```

---

## 12. Operaciones frecuentes — referencia rápida

| Necesidad | Tool | Notas |
|-----------|------|-------|
| Estado del proyecto completo | `BRIEFING` + uno por cada miembro con `target_workspace` | Prefer `mode: compact` |
| Impacto de cambio en API | `BLAST_RADIUS` + `CONTRACT_QUERY` | BLAST primero, CONTRACT después |
| Solo callers frontend→backend | `BLAST_RADIUS force_contract: true` | Salta CPG walk, más rápido |
| Validar payload frontend→backend | `CONTRACT_QUERY` con `validate_payload` | Ejecutar en workspace backend |
| Guardar decisión de diseño | `neo_memory(action:"store", namespace:"decisions")` | Desde cualquier workspace |
| Buscar lección aprendida | `neo_memory(action:"search", namespace:"incidents")` | Desde cualquier workspace |
| Registrar deuda de frontera | `neo_memory(action:"store", namespace:"debt")` | Se sincroniza con SHARED_DEBT.md |
| Leer función backend desde sesión frontend | `FILE_EXTRACT` con `target_workspace: "<id-backend>"` | `context_lines:0` para función completa |
| Buscar patrón en toda la federación | `SEMANTIC_CODE cross_workspace: true` | Si 0 → Grep cross-repo |
| Subgrafo de calls de un símbolo | `GRAPH_WALK target: "SimboloExacto"` | Receiver methods = limitación SSA |
| Digest del workspace para priorizar | `PROJECT_DIGEST min_calls: 3` | Top CodeRank + coupling |
| Certify cambios de la sesión | Una llamada por workspace con todos sus archivos | Justo antes del commit |
| Stress test de la frontera | `neo_chaos_drill target: "http://127.0.0.1:<port>/health" aggression_level: 3` | Con port del workspace backend |
| Flush caches post-refactor | `neo_cache(action: "flush")` en cada workspace | Tras certify cross-workspace |

---

## 13. Diagnóstico y troubleshooting

```bash
# Estado de todos los workspaces
curl -s http://127.0.0.1:9000/status | python3 -m json.tool

# Health directo a un hijo (bypass Nexus)
curl -s http://127.0.0.1:<port>/health

# Forzar arranque de un workspace parado
curl -s -X POST http://127.0.0.1:9000/api/v1/workspaces/start/<workspace-id>

# Hot-reload de nexus.yaml sin reiniciar
kill -HUP $(pgrep neo-nexus)

# Log de Nexus
tail -F /tmp/neo-nexus.log
```

### 13.1 BoltDB lock por proceso zombie

Síntoma: un workspace no arranca con `hnsw.db: timeout` o `EWOULDBLOCK`.

```bash
lsof +D .neo/db/ | grep -v COMMAND
kill <PID>
curl -X POST http://127.0.0.1:9000/api/v1/workspaces/start/<workspace-id>
```

Root cause: `verifyBoot()` en Nexus no mata el hijo cuando el health-check expira — queda como zombie sosteniendo el lock. Épica de deuda técnica abierta.

### 13.2 Doble Nexus

Síntoma: `pgrep -la neo-nexus` lista 2+ procesos.

```bash
pgrep -la neo-nexus               # listar todos
kill <PID-más-bajo>               # matar el más antiguo primero
make rebuild-restart              # único superviviente
```

### 13.3 Binario stale vs HEAD

Si BRIEFING muestra `⚠️ BINARY_STALE:Nm`: el binario activo es más viejo que el último commit en `cmd/` o `pkg/`.

```bash
make rebuild-restart              # SIGTERM gracioso + verificación post-start
```

Durante el rebuild-restart la sesión MCP del cliente se invalida ("session not found") — reconectar vía `/mcp` en el cliente tras el restart.

### 13.4 MCP session not found

Después de `rebuild-restart`, las sesiones SSE activas quedan huérfanas:

```
Error POSTing to endpoint (HTTP 404): session not found
```

En Claude Code: ejecutar `/mcp` para reconectar. En curl puro: re-negociar el handshake SSE antes del próximo `/mcp/message`.

### 13.5 Embedder reporta down pero Ollama responde

Síntoma: `SEMANTIC_CODE` devuelve `_embed_status: down_` aunque `curl :11435/api/tags` funcione.

Causa frecuente: `ai.embedding_model` en `neo.yaml` apunta a un modelo no cargado en Ollama (p.ej. `nomic-embed-text-16k` sin Modelfile custom). Verificar:

```bash
curl -s -X POST http://127.0.0.1:11435/api/embeddings \
  -H "Content-Type: application/json" \
  -d "{\"model\":\"$(grep embedding_model neo.yaml | head -1 | awk '{print $2}')\",\"prompt\":\"test\"}" | head -c 200
```

Si retorna `"error":"model \"...\" not found"` → cambiar `embedding_model` a uno existente (p.ej. `nomic-embed-text`) o crear el Modelfile custom y `ollama create`. La nueva salida de SEMANTIC_CODE desde 2026-04-23 incluye un campo `_embed_error: ...` con la causa exacta.

---

## Referencias

- Template base (este archivo): `neoanvil/docs/neo-project-federation-guide.md`
- Doctrina de 15 MCP tools (4-tier workspace/project/org/nexus): `neoanvil/.claude/skills/sre-tools/SKILL.md` + full schemas en `neoanvil/docs/general/sre-tools-reference.md`
- Guide org-tier PILAR LXVII (`.neo-org/` federation): `neoanvil/docs/pilar-lxvii-org-tier.md`
- Workflow operativo: `neoanvil/.claude/skills/sre-workflow/SKILL.md`
- Leyes de calidad Go/MCP: `neoanvil/docs/general/code-quality-laws.md`
- Doctrina DB/RAG: `neoanvil/.claude/skills/sre-db/SKILL.md` (paths-scoped auto-load)
- Directivas activas (auto-gen): `neoanvil/.claude/rules/neo-synced-directives.md`
