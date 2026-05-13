---
name: jira-workflow
description: Doctrina Scrum/Kanban para issues Jira creados desde el plugin neoanvil. Task-mode skill — invoke with `/jira-workflow` when creating issues (Epic, Story, Bug), transitioning ticket status, attaching documentation artifacts, or operating the project board. Covers naming conventions, transition order, story points scale, and the Epic ↔ Story hierarchy invariants.
disable-model-invocation: true
---

# Jira Workflow — Doctrina Scrum/Kanban para neoanvil ↔ Atlassian

> Reglas operativas para crear/transicionar issues en Jira desde el plugin
> `jira/` de neoanvil. Cargada cuando el usuario habla de Jira, tickets,
> sprints, o cuando Claude detecta llamadas a `mcp__neoanvil__jira_jira`.

---

## Workflow states (project MCPI, your-org.atlassian.net)

```
Backlog
  ↓ id=21 Selected for Development
Selected for Development
  ↓ id=31 In Progress
In Progress
  ↓ id=61 REVIEW
REVIEW
  ↓ id=51 READY TO DEPLOY
READY TO DEPLOY
  ↓ id=41 Done
Done
```

El plugin ya hace el mapping name→ID, no es necesario pasar IDs. Las
transitions están todas accesibles desde cualquier estado (no enforce
server-side de orden); la doctrina de orden se mantiene client-side.

---

## Regla #1 — Epic ↔ Story hierarchy

**Toda Story PERTENECE a un Epic.** El plugin enforca:
- `create_issue` con `issue_type: "Story"` SIEMPRE recibe `parent_key`
- Sin parent_key → registrar como Bug/Task individual, no como Story

### Gating Epic ↔ Story (CRÍTICA)

**Story child NO puede salir de `Backlog`/`Selected for Development` antes de que el Epic padre esté en `In Progress`.** Si el Epic está en Backlog y necesitas avanzar una Story, primero transicionar el Epic con `transition` action.

**Naming convention** del summary — doble-label UPPERCASE (revisión 2026-04-30):
```
[CATEGORÍA][SCOPE] texto descriptivo
```

Ambos labels en MAYÚSCULAS. Texto **solo palabras descriptivas** — sin URLs, sin paths, sin "Task X.Y", sin commit hashes. Esos van en el body o en los comments.

**Categorías permitidas (UPPER):**
- `FEATURE` — funcionalidad nueva visible
- `BUG` — corrección de defectos observables
- `FIX` — alias de BUG (uso indistinto)
- `ARCHITECTURE` — refactors, ADRs, infra
- `CHORE` — mantenimiento, deps, build
- `DOCS` — documentación pura
- `JIRA` — trabajo del propio plugin Jira (meta)

**Catálogo de scopes (UPPER) por proyecto:**

Frontend (strategosia_frontend): `UI` · `PLANIFICADOR` · `APS` · `BALANCEADOR` · `SUPERVISION` · `HMI` · `WMS` · `QUALITY` · `MAINT` · `INVENTORY` · `WORKFORCE` · `MASTER-DATA` · `ADMIN` · `PURCHASING` · `AUTH` · `API`

Backend (strategos): `ENGINE` · `HANDLER` · `SERVICE` · `DTO` · `REPO` · `DB` · `WS` · `CACHE` · `WORKER`

Cross-cutting / infra: `DOCKER` · `CI` · `TEST` · `NEO` · `DOCS` · `SHARED`

**Field `labels[]` también UPPER:**
```python
labels: ["FEATURE", "PLANIFICADOR"]   # mismos strings UPPER del título
```

Mismo `<scope>` técnico que los commits convencionales del repo (`feat(planificador): ...` ↔ `[FEATURE][PLANIFICADOR]`).

---

## Regla #2 — Flujo de transiciones para una STORY

Orden canónico:

```
1. Backlog                  ← creada con create_issue
2. Selected for Development ← cuando se decide tomarla en este ciclo
3. In Progress              ← al empezar el código
4. REVIEW                   ← al abrir PR / pedir revisión
5. READY TO DEPLOY          ← review aprobado, certify green
6. Done                     ← merged a main/feature branch
```

**Cada transición requiere comment** vía `resolution_comment`:
- "Selected" → por qué entra al ciclo, dependencias resueltas
- "In Progress" → archivos que se tocan, plan de tests
- "REVIEW" → tests pasaron, count de assertions, cobertura, edge cases
- "READY TO DEPLOY" → hash del commit certificado, neo_sre_certify status
- "Done" → commit hash mergeado, link al PR si aplica

Sin comments la traza es opaca.

---

## Regla #3 — Flujo de transiciones para un EPIC

Distinto que Story:

```
1. Backlog                  ← creado con create_issue
2. In Progress              ← cuando la PRIMERA child story entra a "In Progress"
3. Done                     ← cuando TODAS las child stories están Done
```

**Atajos cortos a propósito:** Epic salta Selected/REVIEW/READY TO
DEPLOY — esos son estados de unidades individuales, no del agrupador.

**Restricción dura para mover Epic a Done:**
- Verificar via `jira_jira(action:get_context)` cada child story
- Si CUALQUIERA está en estado != Done → bloquear el transition
- El plugin no enforca esto hoy; el agente debe hacer el check manualmente

---

## Regla #4 — Story points

Escala Asana (Fibonacci): `1, 2, 3, 5, 8, 13`.

| SP | Significado | Ejemplo concreto |
|----|-------------|------------------|
| 1  | Trivial · minutos · 1 línea / 1 archivo | flag default · config string · cleanup unused · `.yaml.example` update |
| 2  | Pequeño · <100 LOC · sin nuevos contracts | helper extraction · schema field add |
| 3  | Estándar · 1 componente / 1 endpoint completo | service + hook + view · CRUD · single tool action |
| 5  | Mediano · cross-component · backend wire incluido | feature multi-archivo + API · PluginPool con lifecycle |
| 8  | Grande · multi-archivo + tests E2E + docs | feature pilar completo · full pipeline · federation work |
| 13 | **Solo Epic** · nunca Story | PILAR completo |

**Bug + story_points → 400 en esta instancia.** El `customfield_10038`
no está en la Create screen del Bug. Workaround: usar `issue_type:"Story"`
para todo trabajo cuantificable; reservar `Bug` solo para reportes
externos sin SP.

Plugin escribe a BOTH custom fields (`customfield_10016` "Story point
estimate" + `customfield_10038` "Story Points") porque la instancia
tiene los dos y el screen scheme puede mostrar cualquiera. No hay que
pensarlo — es default.

---

## Regla #5 — Reporter

NO está en el create screen de la instancia your-org.atlassian.net
(Jira next-gen + project setting). Pasarlo en `create_issue` retorna
400. Se setea automáticamente al usuario del API token.

**Acción:** no usar `reporter_email` en este project. Si otra instancia
permite reporter, el plugin lo soporta.

---

## Regla #6 — Assignee

`assignee_email` resuelve a `accountId` via `/user/search`. Default
recomendado: `user@example.com`. Si el email no
existe en la instancia, `create_issue` falla con `lookup assignee
<email>: not found` — strict a propósito.

---

## Regla #7 — Documentación adjunta (artifacts)

Stories ≥3 SP que cierran un cambio sustancial DEBEN atacharle un
paquete de docs.

**Estructura local** (no trackeada en git):

```
~/.neo/jira-docs/<TICKET_KEY>/
  ├─ README.md              ← summary del cambio + commit hash
  ├─ code/                  ← snippets clave (raw .go o .ts)
  ├─ images/                ← code-snap PNGs (auto-generables) + UI screenshots
  ├─ design/                ← diagramas, ADR snippets
  └─ <TICKET_KEY>.zip       ← bundle creado por attach_artifact (root folder = TICKET_KEY/)
```

**Tool:** `jira_jira(action:"attach_artifact", ticket_id:"MCPI-X")` —
zippea + sube. Default `folder_path` = `~/.neo/jira-docs/<ticket_id>`.
El zip preserva la carpeta `<TICKET_KEY>/` como root para que al
expandir se cree ese folder automáticamente, no se ensucie el cwd.

### Naming convention para los archivos del pack

Los archivos en `code/` se nombran con descriptor snake_case derivado
del path original (auto-generado por `prepare_doc_pack`):

| Path original | Descriptor en code/ |
|---|---|
| `pkg/jira/client.go` | `jira_client.go` |
| `pkg/auth/keystore.go` | `auth_keystore.go` |
| `cmd/plugin-jira/main.go` | `plugin_jira_main.go` |

Reglas: lowercase, underscores, last-2-segments del path, strip de
prefijos comunes (pkg/cmd/internal). Coherencia con extensión
original (`.go` se preserva para que chroma highlight funcione).

### Smart zip filter

`prepare_doc_pack` aplica filtros automáticos al zippear:

1. **Empty subdirs (design/, images/) skipped** — no contaminan el zip
2. **`images/<base>.png`/`html` skipped if `code/<base>.<src-ext>` exists** —
   son code-snaps redundantes (mismo contenido que la fuente, syntax
   highlighted). Solo se incluyen images/ con basename SIN twin en code/
   (frontend screenshots, diagrams hand-made)

### Auto exclusion defaults

`prepare_doc_pack` con `commit_hash` aplica defaults a `exclude_paths`
si el operador NO lo pasa explícitamente. Default drop:

```
.neo/master_plan.md       (auto-managed)
.neo/master_done.md       (auto-managed)
.neo/technical_debt.md    (auto-managed)
.neo/.env                 (secrets, gitignored)
.neo/db/                  (BoltDB binary)
go.sum                    (lock file)
.gitignore                (config artifact)
```

Override pasando lista explícita (incluyendo `[]` para opt-out).

**Auto-generación de code-snaps** (next: pkg/jira/codesnap):
chroma syntax highlight → HTML → headless Chrome screenshot → PNG en
`images/`. Pendiente de wire al action.

---

## Regla #8 — Master plan IDs vs Jira ticket IDs

Dos espacios de IDs coexisten y NO son intercambiables:

| Espacio | Forma | Vive en |
|---------|-------|---------|
| **Master plan ID** | `134.A.1`, `132.D`, `130.4.2` | `.neo/master_plan.md` checkboxes |
| **Jira ticket ID** | `MCPI-52`, `MCPI-130` | Atlassian (project MCPI) |

Un Epic en Jira (`MCPI-52`) suele agrupar varias subtareas del master plan
(`130.1`, `130.2`, `130.3`). El número que sigue a `MCPI-` NO es el número
de la épica del master plan.

**Convención canónica para commits:**

```
feat(scope): MCPI-N <master_plan_id> — descripción corta

[EPIC-FINAL MCPI-N]    ← solo en el commit que cierra la épica entera
```

- En el subject del commit: `MCPI-N` y el `<master_plan_id>` (ej. `132.D`)
  pueden coexistir. El hook `sync-master-plan.sh` detecta el master plan ID
  para auto-marcar `[x]`. El hook Jira detecta `MCPI-N` para `prepare_doc_pack`.
- `[EPIC-FINAL MCPI-N]` SIEMPRE referencia el ticket Jira real, NUNCA un
  ID del master plan. Si dudas qué `MCPI-N` corresponde a tu épica del
  master plan, ejecutar `neo jira-id <master_plan_id>` (CLI subcommand) o
  llamar `pkg/jira.ResolveMasterPlanID` directamente.
- En el cuerpo del commit puedes mencionar IDs adicionales — el hook de
  sync solo parsea el subject (evita falsos positivos por regex examples,
  paths, version strings).

**Anti-patrón documentado:** commit `30220ae` referenció `[EPIC-FINAL MCPI-130]`
porque el agente confundió el ID del master plan (Épica 130) con el ticket
Jira (que era MCPI-52). El hook Jira intentó adjuntar evidencia y falló con
`MCPI-130: not found`.

---

## Anti-patrones

- ❌ Crear Story sin `parent_key` (orphan, no aparece en board del Epic)
- ❌ Crear Epic o Story sin `start_date` o `due_date` (rompe roadmap + Gantt)
- ❌ Crear Story sin doble-label `[CATEGORÍA][SCOPE]` UPPER en el summary (board filters + scope filters se rompen)
- ❌ Título con URLs / paths / `Task X.Y` / commit hashes (usar solo palabras descriptivas)
- ❌ Labels en lowercase (todo UPPERCASE en title + labels[])
- ❌ Marcar `[x]` al crear el ticket aunque el código ya esté escrito (viola checkbox hygiene · arrancar siempre `[ ]`)
- ❌ Avanzar Story child sin que el Epic padre esté en `In Progress` (gating violation)
- ❌ Mover Epic a Done con stories child no-Done (estado inconsistente)
- ❌ Transición sin `resolution_comment` (traza inútil)
- ❌ Skipear estados (ej. Backlog → Done directo): esconde si hubo testing
- ❌ Story con > 8 SP (es Epic disfrazado, partir en sub-stories)
- ❌ Reusar el mismo summary entre stories (ambigüedad en el board)
- ❌ Doc-pack generado al final del lifecycle en lugar de en `In Progress` (perdés evidencia intermedia)
- ❌ README del doc-pack con menciones de "Claude" / "AI" / paths absolutos crudos
- ❌ Pasar Epic a `Done` con cualquier Story child fuera de `Done`

---

## Automation niveles

Para evitar gastos de tokens del agente, las operaciones recurrentes
tienen niveles de automatización:

```
nivel 0 — operator types files explicit       → ~500 tokens/ticket
nivel 1 — pasa solo commit_hash, plugin parses → ~50 tokens/ticket
nivel 1.5 — /jira-doc-from-commit slash skill  → ~5-10 tokens/ticket
nivel 3 — git post-commit hook (zero invocation) → 0 tokens
```

**Recomendado por default: nivel 3.** Setup one-time:

```bash
make install-git-hooks
```

A partir de ahí cada `git commit -m "fix(X): MCPI-N — ..."` dispara
auto-attach del pack. El hook detecta tokens `[A-Z]+-[0-9]+` en el
mensaje, llama `prepare_doc_pack(commit_hash:HEAD, auto_attach:true)`
para cada uno. Cero invocación del agente.

**Override flags del hook:**
- `NEO_HOOK_DISABLE=1` bypass (rebase loops)
- `NEO_HOOK_QUIET=1` suprime success
- `NEO_NEXUS_URL` redirigir
- `NEO_REPO_ROOT` override repo path

## Operación habitual desde Claude

```text
# 1. Crear Epic al inicio del PILAR
jira_jira(action:create_issue, issue_type:"Epic",
          summary:"[architecture] X",
          description:"<plantilla epic>",
          labels:["architecture"], story_points:13,
          start_date:"YYYY-MM-DD", due_date:"YYYY-MM-DD",
          assignee_email:"...")

# 2. Crear cada Story con el parent
jira_jira(action:create_issue, issue_type:"Story",
          summary:"[architecture] Épica N.M — ...",
          description:"<plantilla story>",
          parent_key:"<EPIC_KEY>",
          labels:["architecture"], story_points:N, ...)

# 3. Iterar el flujo Story
jira_jira(action:transition, ticket_id:"<STORY_KEY>",
          target_status:"Selected for Development",
          resolution_comment:"<por qué>")
jira_jira(action:transition, ticket_id:"<STORY_KEY>",
          target_status:"In Progress",
          resolution_comment:"<files+plan>")
jira_jira(action:transition, ticket_id:"<STORY_KEY>",
          target_status:"REVIEW",
          resolution_comment:"<tests+coverage>")
jira_jira(action:transition, ticket_id:"<STORY_KEY>",
          target_status:"READY TO DEPLOY",
          resolution_comment:"<commit certified>")
jira_jira(action:transition, ticket_id:"<STORY_KEY>",
          target_status:"Done",
          resolution_comment:"<merge hash>")

# 4. Cuando se inicia trabajo activo en cualquier child:
jira_jira(action:transition, ticket_id:"<EPIC_KEY>",
          target_status:"In Progress",
          resolution_comment:"<por qué>")

# 5. Cuando TODAS las child stories están Done:
jira_jira(action:transition, ticket_id:"<EPIC_KEY>",
          target_status:"Done",
          resolution_comment:"<all children verified done>")

# 6. Atachar docs cuando aplique
jira_jira(action:attach_artifact, ticket_id:"<KEY>")
# (asume ~/.neo/jira-docs/<KEY>/ existe con README + code + images)
```

---

## Templates

Plantillas de `epic.md` y `history.md` viven en
`.neo/plantilla-jira/` del repo. Copia, rellena, pasa el contenido
como `description` al `create_issue`.

Estas tienen el header con metadata obligatoria (start_date, due_date, parent_key, labels UPPER), catálogo de scopes, story points scale, y secciones del body alineadas con la Definition of Done.

---

## Regla #9 — Campos obligatorios en `create_issue` (revisión 2026-04-30)

| Campo | Epic | Story |
|---|---|---|
| `summary` con `[CATEGORÍA][SCOPE] texto` UPPER | obligatorio | obligatorio |
| `labels[]` con CATEGORÍA + SCOPE UPPER | obligatorio | obligatorio |
| `parent_key` (Epic key) | n/a | **OBLIGATORIO** |
| `story_points` | siempre 13 | 1/2/3/5/8 (nunca 13) |
| `start_date` (YYYY-MM-DD) | **OBLIGATORIO** | **OBLIGATORIO** |
| `due_date` (YYYY-MM-DD) | **OBLIGATORIO** | **OBLIGATORIO** |
| `assignee_email` | recomendado | recomendado |
| `description` | body desde `epic.md` | body desde `history.md` con checkboxes `[ ]` (no `[x]`) |

---

## Regla #10 — Checkbox hygiene en el body de la Story

Los `[ ]` de **Criterios de aceptación** y **Definition of Done** **arrancan VACÍOS al `create_issue`**, INCLUSO si el código ya está hecho (ticket retrospectivo). Se flippean a `[x]` solo cuando el item está efectivamente verificado durante la lifecycle progression:

| Estado | ACs | DoD |
|---|---|---|
| Backlog | todos `[ ]` | todos `[ ]` |
| Selected for Development | todos `[ ]` | todos `[ ]` |
| In Progress | flippear `[x]` los criterios completándose | flippear `[x]` los del DoD que ya estén |
| REVIEW | criterios casi todos `[x]` | tests + lint + cert ya `[x]` |
| READY TO DEPLOY | todos `[x]` | todos `[x]` salvo "merge" |
| Done | todos `[x]` | todos `[x]` |

### Limitación actual del plugin (gap conocido)

El plugin **no expone `update_issue`/`set_description`** — el body de la descripción queda **frozen al momento del create**. La progression de checkboxes se documenta en los `resolution_comment` de cada `transition` (bloque "Checkbox progress"), NO en el body.

Workaround inmediato:
- Cada `resolution_comment` incluye `Checkbox progress` con estado actual `[x]/[ ]` de ACs y DoD.
- Manual UI edit en Jira es el único recambio del body inmutable.

Workaround programático (cuando aterrice `update_issue`):
- Backfill batch los tickets con `[ ]` stale en body de tickets cerrados.

Debt registrado: `~/develop/own/neoanvil/.neo/technical_debt.md` P2 — feature request `update_issue` action.

---

## Regla #11 — Doc-pack timing y README style

### Timing

**Generar al transitionar a `In Progress`** — NO al crear (sin código consolidado), NO al final (perdés evidencia intermedia).

Stories ≥ 3 SP que cierran un cambio sustancial DEBEN llevar doc-pack. SP 1-2 opcional pero recomendable para BUGs con audit trail relevante.

### Invocación

```python
prepare_doc_pack(
  ticket_id: "MCPI-N",
  commit_hash: "<hash>",
  repo_root: "<absolute path>",
  auto_attach: true,
  summary: "<README markdown completo · override del auto-generado>"
)
```

### README style guide

El README debe ser **explicativo y NO técnico**:

- **Sin mención de "Claude code", "Claude AI"** o cualquier herramienta del proceso de desarrollo.
- Sin paths absolutos en el body, sin commit hashes inline.
- Centrado en QUÉ cambió y POR QUÉ importa para el usuario final.
- Estructura recomendada:
  - `## Qué cambió en este sistema`
  - `## Problema previo` o `## Por qué importa`
  - `## Qué se corrigió` o `## Qué se agregó`
  - `## Qué cambia para el usuario` (Antes/Ahora simétrico)
  - `## Verificación de que funciona`
  - `## Archivos involucrados en el cambio` (mención alto-nivel)
- Para BUGs agregar `## Cómo se logró` (alto-nivel, sin código).

---

## Regla #13 — `update_issue` para backfill y sync retroactivo [Épica 140]

El plugin Jira expone `update_issue` con semántica PATCH parcial sobre tickets
ya creados. Cualquier subset de `description`, `summary`, `labels`,
`assignee_email`, `start_date`, `due_date` puede mutarse sin tocar los demás
campos. Casos canónicos:

**1. Backfill de fechas omitidas en create.**
Cuando una Story se crea sin `start_date` o `due_date` (omisión común si la
plantilla del agente no marcaba esos campos como obligatorios), no es necesario
recrear el ticket. Patch:

```python
jira_jira(
  action: "update_issue",
  ticket_id: "MCPI-57",
  start_date: "2026-04-29",
  due_date:   "2026-04-30"
)
```

**2. Fix de checkbox hygiene violations.**
Si una Story se creó con `[x]` ya marcado en Criterios de Aceptación (común
si el agente reflejaba el estado del código en lugar del estado del ticket
en Backlog), el operador puede resetear:

```python
jira_jira(
  action: "update_issue",
  ticket_id: "MCPI-57",
  description: "<full body con [ ] vacíos en ACs y DoD>"
)
```

**3. Sync retroactivo del body al cerrar Done.**
La doctrina obliga a `[ ]` al crear y `[x]` al cerrar. Pero el body capturado
al inicio nunca refleja el progreso — solo los `resolution_comment` de cada
transición lo registran. Para que un humano que abre el ticket cerrado vea
los checkboxes verificados sin scrollear los comments, sincronizar al
transicionar Done:

```python
jira_jira(
  action: "update_issue",
  ticket_id: "MCPI-59",
  description: "<body original con todos los [x] marcados + resumen final>"
)
```

**Semántica de `labels`:**
- arg ausente → no toca labels existentes (preserve)
- arg presente, slice no vacío → reemplaza el array completo
- arg presente, array vacío `[]` → limpia todos los labels

**Semántica de strings:** empty string (o ausente) = skip ese campo. NO se
puede usar `update_issue` para limpiar description/summary a vacío — Jira
requiere otro payload (`update.set: null`) que esta action NO cubre. Caso
de uso raro, usar Jira UI o curl directo.

**Audit trail:** cada llamada genera un evento `jira_update_issue` con el
subset de campos mutados en `~/.neo/audit-jira.log` (hash-chained).

---

## Regla #14 — `neo space use` propaga sin restart [Épica 142]

Antes de Épica 142 (Phase A), el plugin Jira capturaba `JIRA_ACTIVE_SPACE`
y `JIRA_ACTIVE_BOARD` UNA SOLA VEZ al spawn. Cambiar el active space con
`neo space use --provider jira --id NEW_PROJECT` no surtía efecto hasta
`kill -HUP $(pgrep neo-nexus)` que respawneaba todos los plugins.

Desde 2026-04-30, el plugin lee `~/.neo/contexts.json` **per-call** con
cache TTL 5s. El nuevo active space propaga al plugin en máximo 5
segundos sin tocar Nexus.

**Operador:**
```bash
neo space use --provider jira --id ENG --name "Engineering" --board 12 --board-name "Sprint"
# El siguiente jira_jira(action:get_context, ...) ya usa ENG/12.
```

**Cuándo SÍ se necesita kill -HUP:** solo si configuras setups
multi-workspace donde workspace A debe usar Jira project X y workspace B
proyecto Y simultáneamente (Phase B, no implementado todavía). Para el
99% de los casos (single-active-space), no toques Nexus.

**Fallback:** si `~/.neo/contexts.json` falla o no existe, el plugin
sigue usando el env-var `JIRA_ACTIVE_SPACE` capturado al spawn. Failsafe
para fresh deployments antes del primer `neo space use`.

---

## Regla #12 — Hooks parsean SOLO commit subject (no body) [Épica 139]

Los git hooks del repo (`scripts/git-hooks/post-commit`, `commit-msg`) extraen
identificadores Jira (`[A-Z][A-Z0-9]+-[0-9]+`) **únicamente del subject** del
commit message — la primera línea, vía `head -n 1`. **El body es free-form
prose**: ahí pueden aparecer tickets como ejemplos, referencias, o explicación,
sin disparar `prepare_doc_pack` ni warnings.

Razón de la regla: el regex de tickets matches literales como `Z0-9`,
`ADR-009`, o `MCPI-130` cuando esos strings aparecen en regex examples,
referencias a ADRs, o body explicativo. Cada match disparaba
`prepare_doc_pack` que fallaba con "ticket not found", ensuciando el output
y consumiendo ops contra Jira API.

**Implementación canónica** (Épica 139): `scripts/git-hooks/lib-jira-tickets.sh`
expone `extract_subject_tickets(msg)` + `validate_jira_ticket(key, nexus, ws)`.
Tanto `post-commit` como `commit-msg` la sourcean y validan cada ticket vía
`jira/get_context` antes de fire — phantom tickets son skip silently con
`[neo-hook] skip <KEY>: not in Jira`.

**Convención para el operador:** si quieres que un commit dispare doc-pack
para varios tickets, ponlos TODOS en el subject:

```
feat(jira): MCPI-52 + MCPI-53 paired fix — body puede mencionar ADR-009 sin disparar
```

Si solo quieres referenciar un ticket en el cuerpo (no documentar), nada
extra que hacer — el body es ignorado por el hook.

Mismo patrón ya aplicado en `sync-master-plan.sh` (Épica 134.A) para el
auto-marcado de master_plan tasks. **Single source of truth:** body es prosa,
subject es contrato.

---

## Regla #15 — Cross-project: 1 Epic + Stories per scope (federation)

Cuando un cambio cruza la frontera backend↔frontend (API contract change,
DTO compartido, auth flow), **NO crear un solo ticket multi-scope**. Patrón
canónico:

1. **Crear UN solo Epic** en MCPI con `[FEATURE][SHARED]` o `[ARCHITECTURE][SHARED]`.
2. **Crear Stories separadas** por scope real:
   - `[FEATURE][HANDLER] ...` → backend deliverable
   - `[FEATURE][PLANIFICADOR] ...` → frontend deliverable
3. Cada Story tiene `parent_key` al MISMO Epic.
4. Lifecycle independiente por Story · doc-pack con `repo_root` correspondiente.
5. Registrar deuda compartida en `.neo-project/SHARED_DEBT.md`
   (no solo en Jira) — el board de Jira es estado, SHARED_DEBT.md es contrato persistente.

Razón: federation planifier (`strategos` + `strategosia_frontend`) tiene
work-streams paralelos. UN ticket multi-scope se vuelve invisible desde el
otro lado del repo. Stories per scope mantienen el work-stream visible
desde ambos workspaces sin colisión.

---

## Regla #16 — Tickets retrospectivos (work-already-done)

Cuando se crea una Story para registrar trabajo que YA ocurrió en commits
previos (refactor cerrado en mayo, doctrina cambió en junio, ahora se
crea ticket retrospectivo):

| Campo | Valor para retrospectivo |
|---|---|
| `start_date` | Fecha del **primer commit relevante** |
| `due_date` | Fecha del **último commit / merge** |
| Lifecycle | Recorrer **todas las transiciones igual** · cada `resolution_comment` documenta lo que pasó en su momento |
| Checkboxes en body | **AÚN así arrancan en `[ ]`** · se flippean a `[x]` durante las transiciones via `resolution_comment` o `update_issue` |
| Doc-pack | Generar en el transition a `In Progress` con el `commit_hash` ya conocido |

El audit trail de checkbox progression vive en los comments + `update_issue`
backfill, no en el body inmutable. Esto preserva la traza de "cómo se
verificó cada criterio" aunque el código ya estuviera escrito antes.

---

## Regla #17 — Multi-workspace tool naming

El plugin Jira es **nexus-tier singleton** spawneado una vez por Nexus.
Distinto cliente MCP en cada workspace, mismo plugin físico:

| Workspace | Tool name accesible desde Claude |
|-----------|------------------------------------|
| `neoanvil` | `mcp__neoanvil__jira_jira` |
| `strategos` (backend) | `mcp__neo-strategos__jira_jira` |
| `strategosia_frontend` | `mcp__neo-strategosia__jira_jira` |

**Mismo proyecto MCPI · misma instancia Atlassian · mismo audit log.** El
prefijo `mcp__<server>__` viene del cliente MCP (Claude Code) según qué
workspace está activo. La doctrina de naming, lifecycle y campos es
idéntica en todos los workspaces.

Reinicio del plugin sin tirar Nexus completo:

```bash
kill -HUP $(pgrep neo-nexus)   # respawn de plugins, dispatcher mantiene state
```

`~/.neo/audit-jira.log` (hash-chained) recibe events desde todos los
workspaces simultáneamente — único archivo de auditoría compartido.

---

## Single source of truth

**Esta SKILL.md es la doctrina canónica.** Las copias workspace-local
en `.claude/rules/jira-plugin.md` apuntan a este archivo + listan
particularidades del workspace (tool name, repo_root, scopes típicos).

Plantillas operativas en `.neo/plantilla-jira/{epic.md, history.md}`.
