# Jira Integration Guide — neoanvil

> Guía unificada del ecosistema Jira en neoanvil: plugin, skills, output
> styles, workflow, naming, doc packs. Empieza aquí cuando vayas a
> crear, transicionar o documentar tickets desde Claude.

Última actualización: 2026-04-28 · branch: `feature/neoanvil-v5` ·
heads: PILAR XXIII complete (`2bfa2a6`).

---

## TL;DR

```bash
# 1. Boot
make rebuild-restart           # Nexus arranca + plugin-jira spawnea
make install-git-hooks         # one-time: instala post-commit auto-doc

# 2. Verifica
curl http://127.0.0.1:9000/api/v1/plugins
# → {"enabled": true, "plugins": [{"name":"jira","status":"running"}],
#    "tools":["jira/jira"]}

# 3. ZERO-token automation (después de 'make install-git-hooks'):
git commit -m "feat(jira): MCPI-28 ..."
# → hook detecta MCPI-28, dispara prepare_doc_pack, sube zip
# → "[neo-hook] MCPI-28: documented from e091da7 ✓"

# 4. Manual (cuando no hay commit todavía o quieres control):
mcp__neoanvil__jira_jira(action: "get_context", ticket_id: "MCPI-1")
mcp__neoanvil__jira_jira(action: "transition", ticket_id: "MCPI-1", target_status: "Done", resolution_comment: "...")
mcp__neoanvil__jira_jira(action: "create_issue", issue_type: "Story", summary: "[architecture] ...", parent_key: "MCPI-2", ...)
mcp__neoanvil__jira_jira(action: "link_issue", from_key: "MCPI-3", to_key: "MCPI-5", link_type: "Relates")
mcp__neoanvil__jira_jira(action: "prepare_doc_pack", ticket_id: "MCPI-X", repo_root: "...", commit_hash: "<hash>", auto_attach: true)
```

---

## Componentes

### Plugin (`cmd/plugin-jira/`)

Subprocess MCP plugin spawneado por Nexus. Expone una sola tool
**`jira/jira`** con 5 acciones (macro-tool pattern). Habla JSON-RPC
sobre stdio con Nexus, que rutea las llamadas de Claude.

| Acción | Operación | Audit log? |
|---|---|---|
| `get_context` | Lee ticket: summary, status, description, last 3 comments | no |
| `transition` | Mueve status con resolution_comment + audit | ✅ |
| `create_issue` | Crea Epic/Story/Bug/Task con parent, dates, story points, labels | ✅ |
| `link_issue` | Crea relación entre tickets (Relates/Blocks/Duplicates) | ✅ |
| `attach_artifact` | Zippea folder + sube como attachment | ✅ |
| `prepare_doc_pack` | One-shot: lee files locales + git log + README + smart zip + upload (todo local, cero context cost). Con `commit_hash` auto-deriva files+summary del commit | ✅ |

Binary: `bin/neo-plugin-jira` · Config: `~/.neo/plugins.yaml` ·
Logs: `~/.neo/logs/plugin-jira.log`.

### Auth + active context

Persistencia local (todos `0600`):

```
~/.neo/credentials.json   ← token + email + domain (manejados por `neo login`)
~/.neo/contexts.json      ← active space + board (manejados por `neo space use`)
```

Vault del plugin inyecta automáticamente al spawn:

```
JIRA_TOKEN, JIRA_EMAIL, JIRA_DOMAIN          (de credentials.json)
JIRA_ACTIVE_SPACE, JIRA_ACTIVE_BOARD,
JIRA_ACTIVE_SPACE_NAME, JIRA_ACTIVE_BOARD_NAME  (de contexts.json)
```

CLI:
```bash
neo login --provider jira --token ATL_xxx --email tu@empresa.com \
          --domain empresa.atlassian.net
neo space use --provider jira --id MCPI --name "MCP-IDEAS" \
              --board 2 --board-name "MCPI board"
```

### Audit log

Append-only JSONL hash-chain en `~/.neo/audit-jira.log` (0600). Cada
mutación (transition, create, link, attach) escribe un entry firmado
con SHA256 prev-link. Verificable con `auth.AuditLog.Verify()`. NO
compartido con `~/.neo/audit.log` del core (in-process mutex per-file).

### Code-snap renderer (`pkg/jira/codesnap.go`)

Convierte snippets a PNGs estilo "code-snap":

```
source.go (raw)
  → chroma syntax highlight (github-dark theme, line numbers)
  → HTML "card" con header (3 dots + filename + lang badge) + gradient bg
  → headless Chrome via chromedp → screenshot del .card element
  → PNG (~65 KB típico)
```

Detecta Chrome en paths comunes (override con `$CHROMEDP_CHROME` o
`$CHROME_BIN`). Sin Chrome: cae a HTML-only (operador puede abrir en
browser).

### Pack zipper (`pkg/jira/attachments.go`)

```
~/.neo/jira-docs/<TICKET_KEY>/         ← carpeta operador
├── README.md
├── code/<descriptor>.go
└── images/<descriptor>.{html,png}
       ↓ jira_jira(action:"attach_artifact", ticket_id:"<KEY>")
<TICKET_KEY>.zip                        ← zip (root folder = TICKET_KEY/)
       ↓
POST /rest/api/3/issue/<KEY>/attachments
       ↓
Audit log entry
```

---

## Skills (`.claude/skills/`)

3 skills migradas/creadas en esta sesión:

### 1. `jira-workflow/SKILL.md` — auto-load

Doctrina general del workflow. Se activa automáticamente cuando Claude
detecta operación Jira. Cubre:

- Naming `[<label>] <texto>` con label ∈ {architecture, feature, bug, jira, docs, chore}
- Workflow Story: `Backlog → Selected for Development → In Progress → REVIEW → READY TO DEPLOY → Done`
- Workflow Epic: `Backlog → In Progress (cuando primera child In Progress) → Done (cuando TODAS Done)`
- Story points Asana scale {1, 2, 3, 5, 8, 13}
- Reporter: NO en create screen de la instancia, autoasignado
- Naming convention para archivos del doc-pack
- Anti-patrones

### 2. `jira-create-pilar/SKILL.md` — `/jira-create-pilar <PILAR>`

User-invocable. Mirror de un PILAR de `master_plan.md` a Jira:

```
/jira-create-pilar PILAR XXIII
```

Walk-through:
1. Leer `.neo/master_plan.md`, extraer el bloque
2. Crear Epic con `[architecture]` label, story_points 13, dates
3. Por cada épica del bloque: crear Story con `parent_key`, story
   points por complejidad, labels coherentes
4. Si épica está `[x]` (closed): walk through workflow Story completo
   con resolution_comments contextuales (commit hash, tests, etc.)
5. Si todas las stories Done: mover Epic a Done

### 3. `neo-doc-pack/SKILL.md` — `/neo-doc-pack <KEY> [files...]`

User-invocable. Construye + atacha el documentación pack:

```
/neo-doc-pack MCPI-3 pkg/plugin/manifest.go
```

Walk-through:
1. `mkdir -p ~/.neo/jira-docs/<KEY>/{code,images,design}`
2. Renombra cada archivo con descriptor snake_case (jira-workflow rule)
3. Genera README.md con resumen + commits + tests
4. Render code-snap PNG via codesnap (HTML + PNG)
5. `jira_jira(attach_artifact)` zippea + sube
6. Verifica con `get_context`

---

## Output style (`.claude/output-styles/neo-sre.md`)

Refuerza la disciplina Ouroboros V7.2 sin reescribir el system prompt
base de Claude Code. Activación:

```json
// settings.json o settings.local.json
{
  "outputStyle": "neo-sre"
}
```

Cubre:
- BRIEFING obligatorio al inicio
- BLAST_RADIUS antes de Edit
- READ_SLICE en archivos ≥100 líneas
- certify tras cada Edit
- AST_AUDIT pre-Read en código complejo
- Self-Audit al cierre de Épica
- Reglas Jira específicas
- Fail-loud en warnings
- Prohibiciones (Agent Explore, Read en archivos grandes, etc.)

---

## Schema y custom fields

Confirmado en la instancia `your-org.atlassian.net`:

| Field | ID | Note |
|---|---|---|
| Story Points (UI) | `customfield_10038` | el que aparece en columnas del board |
| Story point estimate (modern Jira default) | `customfield_10016` | el que escribe Jira por default |
| Start date | `customfield_10015` | |
| Due date | `duedate` | standard |
| Reporter | `reporter` | NO está en Create screen — pasarlo da 400 |
| Assignee | `assignee` | OK; resolver email → accountId via `/user/search` |

Plugin escribe SP a **ambos** custom fields (10016 + 10038) por
default — la instancia tiene los dos y el screen scheme depende de
admin.

### Workflow transitions (project MCPI)

| ID | name | to status |
|----|------|-----------|
| 11 | Backlog | Backlog |
| 21 | Selected for Development | Selected for Development |
| 31 | In Progress | In Progress |
| 41 | Done | Done |
| 51 | READY TO DEPLOY | READY TO DEPLOY |
| 61 | REVIEW | REVIEW |

El plugin hace el mapping name→ID automático (case-insensitive +
fuzzy). El workflow es no-lineal server-side (todas las transitions
disponibles desde cualquier estado), pero la doctrina mantiene orden
client-side.

---

## Convenciones de naming

### Issues

```
[<label>] <texto descriptivo corto>
```

Donde `<label>` es coherente con el scope del commit del repo.

```
[architecture] Subprocess MCP Plugin Architecture           ← Epic
[architecture] Épica 123.4 — Cliente MCP stdio + agregador  ← Story
[bug] Plugin pool no spawnea con manifest empty             ← Bug
[docs] Plugin author guide                                  ← Docs
```

### Documentation pack files

Snake_case descriptivo, 2-4 palabras, lowercase, sin acentos:

```
~/.neo/jira-docs/MCPI-3/
├── README.md                                         ← siempre así
├── code/
│   └── manifest_permissions_check.go                 ← qué hace, no de dónde viene
└── images/
    ├── manifest_permissions_check.html               ← mismo basename
    └── manifest_permissions_check.png                ← mismo basename
```

Anti-patrón: `pkg_plugin_manifest.go.snippet` (revela path original
del repo, ruido para el lector). Pro-patrón:
`manifest_permissions_check.go` (concepto).

### Zip output

```
<TICKET_KEY>.zip   (NO <TICKET_KEY>-artifacts.zip)
```

Root folder dentro del zip:

```
unzip MCPI-3.zip
→ MCPI-3/                 ← se crea automáticamente
   ├── README.md
   ├── code/...
   └── images/...
```

Nunca dump al cwd.

---

## Story points scale (Asana / Fibonacci)

| SP | Significado | Ejemplo neoanvil |
|----|-------------|----------------|
| 1  | Trivial — minutos, una sola línea | feature flag default change, .yaml.example update |
| 2  | Pequeño — un archivo, < 100 LOC | schema field add, helper extraction |
| 3  | Estándar — un paquete, < 300 LOC | manifest loader, single tool action |
| 5  | Mediano — cross-package, ~500 LOC | PluginPool con lifecycle, REST client |
| 8  | Grande — multi-paquete + integración | full pipeline, federation work |
| 13 | Épico — solo Epics deberían tener este valor | PILAR completo |

Stories con > 8 SP son Epics disfrazados. Partir.

---

## Operator runbook

### Crear Epic + Stories desde un PILAR

```bash
# Reconectar MCP si hace falta — luego:
/jira-create-pilar PILAR XXIII

# O manual:
mcp__neoanvil__jira_jira(action: "create_issue", issue_type: "Epic", ...)
mcp__neoanvil__jira_jira(action: "create_issue", issue_type: "Story", parent_key: "<EPIC>", ...)
```

### Documentar y atachar — desde un commit (recomendado)

**Más eficiente** (1 tool call, ~50 tokens):

```
mcp__neoanvil__jira_jira(
  action: "prepare_doc_pack",
  ticket_id: "MCPI-28",
  repo_root: "/path/to/neoanvil",
  commit_hash: "b19e7bc",
  auto_attach: true
)
```

Plugin descubre files via `git show --name-status`, summary del
commit message body, aplica default `exclude_paths` (drops
master_plan.md, technical_debt.md, go.sum, etc.), copia con
descriptors snake_case, escribe README conciso, smart zip + upload.

O via skill operator-friendly:

```
/jira-doc-from-commit MCPI-28 b19e7bc
```

### Documentar manualmente — cuando hay screenshots o assets

```bash
mkdir -p ~/.neo/jira-docs/MCPI-7/{code,images}
cp pkg/jira/codesnap.go ~/.neo/jira-docs/MCPI-7/code/codesnap_renderer.go
# Drop frontend screenshots manualmente en images/
$EDITOR ~/.neo/jira-docs/MCPI-7/README.md
```

```
mcp__neoanvil__jira_jira(action: "attach_artifact", ticket_id: "MCPI-7")
```

Smart zip filter: `images/<base>.png` se omite si
`code/<base>.<src-ext>` ya existe (auto-render redundante). Solo
images standalone (frontend screenshots, diagrams) sobreviven.

### Verificación post-action

```bash
# Plugin status
curl -s http://127.0.0.1:9000/api/v1/plugins | python3 -m json.tool

# O desde Claude:
neo_radar(intent: "PLUGIN_STATUS")

# Audit log integrity
python3 -c '
import json, hashlib
prev = "GENESIS"
with open("/home/$USER/.neo/audit-jira.log") as f:
    for line in f:
        e = json.loads(line)
        assert e["prev_hash"] == prev, f"chain broken at seq={e[\"seq\"]}"
        prev = e["hash"]
print("audit chain OK")
'
```

---

## Niveles de automation (token cost)

Cinco niveles disponibles, listados de **mayor → menor** consumo de
tokens del agente:

| Nivel | Mecanismo | Token cost | Cuándo usar |
|---|---|---|---|
| **0** | Tool call con `files: [...]` explícitos | ~500/ticket | Cuando los files no corresponden a un solo commit |
| **1** | Tool call con `commit_hash` (auto-derive files+summary) | ~50/ticket | Cuando el ticket cierra UN commit específico |
| **1.5** | Skill `/jira-doc-from-commit <KEY> <HASH>` (operador escribe slash) | ~5-10/ticket | Operativo recurrente desde Claude |
| **2** | `auto_sync_jira(commit_range)` masifica multi-tickets en un call | pending | Backfill histórico (no implementado) |
| **3** | **Git post-commit hook — cero invocación de Claude** | **0 tokens** | **Default operativo recomendado** |

### Nivel 3 — git post-commit hook (recomendado)

Instalación una sola vez:

```bash
make install-git-hooks
```

Eso symlink-ea `scripts/git-hooks/post-commit` → `.git/hooks/post-commit`.
A partir de ahí, cada `git commit -m "feat(jira): MCPI-28 ..."`:

1. Hook extrae todos los tokens `[A-Z]+-[0-9]+` del commit message
2. Verifica Nexus reachable + plugin alive (degrada silencio si no)
3. Para cada ticket: `curl POST /mcp/message → prepare_doc_pack(commit_hash:HEAD)`
4. Plugin local hace todo: read files via git show, README desde
   commit message, smart zip, upload to Atlassian
5. Output a stderr: `[neo-hook] MCPI-28: documented from e091da7 ✓`

**Properties:**
- NUNCA bloquea el commit. Failures van a stderr; commit procede.
- Idempotente: re-commit con `--amend` re-attach con el nuevo hash.
- Override env vars:
  - `NEO_HOOK_DISABLE=1` — bypass total (rebase loops)
  - `NEO_HOOK_QUIET=1` — suprime mensajes de éxito
  - `NEO_NEXUS_URL=...` — redirigir a otro dispatcher
  - `NEO_REPO_ROOT=...` — override del repo root
- **0 tokens** del agente. Cero invocación.

### Nivel 1.5 — skill `/jira-doc-from-commit`

User-invocable, requiere Claude session activa. Útil cuando el commit
ya existe y el operador quiere documentar manualmente:

```
/jira-doc-from-commit MCPI-28 e091da7
```

Skill body en `.claude/skills/jira-doc-from-commit/SKILL.md` parsea
los args, valida formato, llama 1 tool al plugin, reporta 3-5 líneas.

## Hardening aplicado

- File permissions check en `~/.neo/plugins.yaml` (rechazo si no 0600)
- Pdeathsig (Linux) + Setpgid (cross-platform) para evitar zombies
  cuando Nexus muere
- Stderr tee con prefix routing (visible en log de Nexus + per-plugin file)
- SIGHUP hot-reload del plugin pool (no requiere restart Nexus)
- terminateProcessGroup para matar grandchildren al stop
- 5s grace SIGTERM → SIGKILL escalation

---

## Diferencias con la solución típica

Patrón común para integrar Jira en agentes IA:

```
agent → http.Client → atlassian REST API
```

neoanvil usa una pipeline más larga, intencionalmente:

```
agent → MCP tool call → Nexus dispatcher → plugin subprocess
       → pkg/jira REST client → Atlassian API
```

Razones:

1. **Aislamiento de credenciales:** plugin lee env vars al spawn,
   nunca toca otras workspaces. Si plugin compromete, daño limitado
   al subproceso.
2. **Audit log per-plugin:** cada mutación es trazable y verificable
   offline.
3. **Hot-reload sin downtime:** cambias `~/.neo/plugins.yaml` + SIGHUP
   y Nexus reconcilia sin tirar Claude.
4. **Multi-instance ready:** mañana añades plugin GitHub sin tocar
   plugin Jira; namespace prefix evita colisiones.
5. **Reportable:** `neo_radar(intent: "PLUGIN_STATUS")` da estado
   live al agente.

---

## Lo que NO hay (todavía)

- ❌ Webhooks bidireccionales (Atlassian → Nexus → Claude proactive
  events). Diferido por ADR-007 — requiere HTTPS público.
- ❌ Auto-render de PNGs en `attach_artifact` (next: integrar
  codesnap al action). Hoy es manual.
- ❌ Frontend screenshots con Playwright. Pendiente, mencionado para
  proyecto separado.
- ❌ OAuth 2.0 (3LO) flow. ADR-006 difirió a future.
- ❌ Commands tradicionales (`.claude/commands/`) — usamos skills
  (forma moderna 2026).

---

## Referencias técnicas

- `pkg/jira/client.go` — REST client (~700 LOC) con throttle, retry, ADF render, CreateIssue + LinkIssue + AttachFile + LookupAccountByEmail
- `pkg/jira/attachments.go` — ZIP + multipart upload + smart filter (skip code-snaps redundantes + empty subdirs)
- `pkg/jira/codesnap.go` — chroma + chromedp PNG renderer
- `pkg/jira/docpack.go` — local-only docpack builder (PrepareDocPack + commit_hash auto-derive + exclude_paths defaults)
- `cmd/plugin-jira/main.go` — MCP server (handshake + 6 actions)
- `scripts/git-hooks/post-commit` — auto-doc hook (level-3 zero-token)
- `pkg/auth/keystore.go` — credentials.json
- `pkg/auth/context.go` — contexts.json (active space/board)
- `pkg/auth/audit.go` — JSONL hash-chain audit log
- `pkg/auth/vault.go` — env var injection al spawn
- `cmd/neo-nexus/plugin_routing.go` — interceptor MCP tools/list +
  tools/call routing
- `cmd/neo-nexus/plugin_boot.go` — pluginRuntime + reload pipeline
- `.claude/skills/jira-workflow/SKILL.md` — workflow doctrine
- `.claude/skills/jira-create-pilar/SKILL.md` — PILAR → Jira mirror
- `.claude/skills/neo-doc-pack/SKILL.md` — pack builder
- `.claude/output-styles/neo-sre.md` — Ouroboros tone
- `docs/adr/ADR-005-plugin-architecture.md` — subprocess MCP decision
- `docs/adr/ADR-006-jira-auth-flow.md` — API token vs OAuth choice
- `docs/adr/ADR-007-bidirectional-webhooks.md` — webhook deferral
- `docs/claude-folder-inventory.md` — completo `.claude/` reference

---

## ADRs relevantes

| ADR | Decisión |
|---|---|
| 005 | Subprocess MCP > Go-native plugins (security + isolation) |
| 006 | API Token + vault preparado para OAuth 3LO future |
| 007 | Webhooks bidireccionales: deferred (polling cubre uso primario) |
