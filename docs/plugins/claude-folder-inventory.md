# Claude Code `.claude/` Folder — Inventory & Usage

> Inventario validado contra docs oficiales (Apr 2026) de
> [code.claude.com/docs/en/skills](https://code.claude.com/docs/en/skills),
> [claudedirectory.org/how-to/claude-folder](https://www.claudedirectory.org/how-to/claude-folder),
> y [code.claude.com/docs/en/statusline](https://code.claude.com/docs/en/statusline).

Este doc captura las 9 ubicaciones que `.claude/` reconoce, su propósito
ortogonal, y dónde el repo neoanvil las usa hoy. Sirve como guía para
cuando hace falta ampliar la configuración sin reinventar la estructura.

---

## Estado actual del repo

```
.claude/
├── CLAUDE.md               (no presente — usamos /CLAUDE.md root)
├── settings.json           (no presente)
├── settings.local.json     ✓ (18 KB · permisos por-usuario, gitignored)
├── agents/                 ✓ neo-jira-curator.md (creado, requiere restart Claude)
├── commands/               (no presente — usar skills/)
├── hooks/                  (no presente — Claude lifecycle hooks; git hooks viven en scripts/git-hooks/)
├── skills/                 ✓ 13 skills:
│   ├── brain-doctor/       /brain-doctor Brain Portable health diagnosis
│   ├── daemon-flow/        /daemon-flow iterative daemon UI (PILAR XXVII)
│   ├── daemon-trust/       /daemon-trust trust score dashboard
│   ├── jira-create-pilar/  /jira-create-pilar mass create
│   ├── jira-doc-from-commit/ /jira-doc-from-commit zero-token doc
│   ├── jira-id/            /jira-id master_plan → MCPI resolver
│   ├── jira-workflow/      auto-load doctrina
│   ├── neo-doc-pack/       /neo-doc-pack manual builder
│   ├── sre-doctrine/       Ouroboros workflow (auto-load)
│   ├── sre-federation/     tier ownership + federation (auto-load)
│   ├── sre-quality/        15 leyes path-scoped (auto-load)
│   ├── sre-tools/          15 tools reference (auto-load)
│   └── sre-troubleshooting/ recovery patterns (auto-load)
├── plugins/                (no presente)
├── projects/               (auto-generado por Claude Code, no editar)
├── output-styles/          ✓ neo-sre.md (Ouroboros tone)
├── rules/                  ✓ 10 archivos legacy (synced-directives 92KB queda como referencia archivada)
└── worktrees/              ✓ (git worktrees scratch)

scripts/git-hooks/
└── post-commit             ✓ auto-doc Jira tickets from commit msg (zero-token)
```

---

## Subsistemas explicados

### 1. `CLAUDE.md` (root + nested)

**Memoria persistente** cargada en cada sesión. Markdown plano sin
frontmatter. Convenciones del proyecto, stack, comandos, contexto que
no cambia.

**neoanvil uso:** `/CLAUDE.md` en root + `/CLAUDE-global.md` para template
universal. Hay un cap implícito (~200 líneas útiles antes de truncate)
— por eso el resto vive en `rules/` o `skills/`.

### 2. `settings.json` + `settings.local.json`

JSON. **Project settings** (committed) vs **local override** (per-user,
gitignored). Define:
- `permissions` — qué tools puede usar Claude sin preguntar
- `hooks` — scripts ejecutados en eventos lifecycle
- `mcpServers` — servidores MCP custom
- `env` — env vars
- `disableSkillShellExecution` — flag de seguridad

**neoanvil uso:** solo `settings.local.json` (preferencias por máquina).
`settings.json` no committed — candidate para shipear permission
allowlist del repo.

### 3. `agents/` — Subagentes

Markdown con YAML frontmatter. Cada archivo define un agente
especialista invocable por Claude vía `Agent` tool con
`subagent_type=<filename>`.

```yaml
---
name: my-specialist
description: When to delegate to this agent
tools: Bash, Read, Grep
model: sonnet
---
You are an X expert. Your job is...
```

**neoanvil uso:** ninguno hoy. Candidato: agente `neo-jira-curator`
que genere historias desde master_plan automáticamente.

### 4. `commands/` (LEGACY — merge con skills)

Markdown. `commands/deploy.md` → `/deploy`. **Anthropic merged
commands en skills 2026** — ambos crean el mismo `/slash-command`.
Recomendación oficial: usar `skills/` para nuevos.

**neoanvil uso:** ninguno.

### 5. `skills/` — Workflows reusables ⭐ (RECOMENDADO 2026)

Carpeta-por-skill con `SKILL.md` requerido + opcionales (templates,
scripts, ejemplos). Frontmatter rico:

```yaml
---
name: skill-name              # default: directory name
description: When to use this # crítico para auto-invocation
disable-model-invocation: false  # true = solo manual /name
user-invocable: true          # false = solo Claude
allowed-tools: Bash Read Grep
model: opus
context: fork                 # ejecuta en subagente
agent: Explore
paths: ["**/*.go"]            # auto-load por glob
---
```

**Cap blando:** SKILL.md < 500 líneas (ref oficial). Detalle largo →
archivos hermanos `reference.md`, `examples.md`, `scripts/`.

**Live reload:** Claude Code watches `~/.claude/skills/`,
`.claude/skills/`, y `.claude/skills/` de directorios `--add-dir`.

**neoanvil uso:** `skills/jira-workflow/SKILL.md` (~210 líneas) —
doctrina Scrum/Kanban del plugin Jira. Migrado de `rules/` hoy.

### 6. `hooks/` — Scripts en eventos lifecycle

Scripts ejecutables (.sh, .py, etc.) wired vía `settings.json`. 12
eventos: `UserPromptSubmit`, `PreToolUse`, `PostToolUse`, `Stop`,
`Notification`, etc. `async: true` desde Jan 2026 para non-blocking.

**Casos de uso:** auto-format en write, bloquear `rm -rf`, notificar
a Slack al stop, validar permisos antes de tool call.

**neoanvil uso:** ninguno hoy. Candidato: hook `PostToolUse` para
neo_sre_certify_mutation que dispare un audit_log de qué se mutó.

### 7. `plugins/` — Manifiestos de plugins instalados

Manifiestos de extensiones distribuidas via marketplace. Bundle de
skills/agents/commands/hooks. Editado por `claude plugins add`,
no manualmente.

**neoanvil uso:** ninguno. Si quisiéramos shipear neoanvil-toolkit como
plugin reusable, este sería el canal.

### 8. `projects/` — Histórico de sesiones

Auto-generado por Claude Code per-machine. Contiene transcripts +
state. **No editar manualmente.**

**neoanvil uso:** existe pero auto-managed — nuestro `memory/`
persistente vive ahí (`~/.claude/projects/.../memory/`).

### 9. `output-styles/` — Modificadores de respuesta

Markdown con YAML frontmatter. Cambia el system prompt para
reformatear las respuestas de Claude (ej. `concise`, `pedagogical`,
`bullet-only`).

**neoanvil uso:** ninguno. Candidato: `output-styles/sre.md` que enforce
el patrón "preceder cada tool call con 1 oración" + "summary final
≤ 2 líneas".

### 10. `rules/` — Constraints scoped por path

Markdown. Convención community/empresa, NO formato oficial Anthropic.
Cada archivo es contexto que se añade cuando los paths matcheen.

**Cap operativo (no oficial):** algunas implementaciones cortan a
~40k chars per-file. Recomendación: archivos < 30k chars; split por
dominio si excede.

**neoanvil uso (post ctx-bloat refactor 2026-05-13, fase 4):** 4 archivos, total ~56 KB en `.claude/rules/`.
- `neo-synced-directives.md` — 17 KB (51 entries, BoltDB-synced)
- `neo-sre-doctrine.md` — 22 KB (candidate split a skill futuro)
- `neo-workflow.md` — 14 KB (candidate migrate a skill futuro)
- `neo-db.md` — 3 KB

Archivados a `docs/general/` (mayo 2026, content vive en skills):
- `directives-archive-deepseek.md` ← skill `deepseek-workflow`
- `directives-archive-jira.md` ← skill `jira-workflow`
- `directives-archive-pilar.md` ← skill `sre-federation`
- `code-quality-laws.md` ← skill `sre-quality`
- `gosec-audit-policy.md` ← skill `sre-quality`
- `deadcode-policy.md` ← skill `sre-quality`
- `claude-md-archive-2026-05-13.md` ← snapshot pre-trim de CLAUDE.md (232L)

Deleted: `neo-synced-directives-history.md` (git preserva linaje).

### 11. Statusline (no carpeta — settings.json + ~/.claude/)

Custom script que renderiza la línea inferior de Claude Code (token
count, git branch, cost, etc.). Generado por `/statusline` con
natural language. Vive en `~/.claude/` (script) + `settings.json`.

**neoanvil uso:** ninguno.

---

## Git hooks (separados de Claude lifecycle hooks)

`scripts/git-hooks/` contiene scripts wired al git workflow del repo
(no Claude lifecycle). Instalación via `make install-git-hooks`
symlink-ea cada uno a `.git/hooks/`. Idempotente.

Hooks instalados hoy:

| Hook | Trigger | Función |
|---|---|---|
| `post-commit` | tras `git commit` exitoso | extrae `[A-Z]+-[0-9]+` del commit msg, dispara `prepare_doc_pack` para cada ticket via Nexus REST. Zero-token automation level 3. |

Override flags: `NEO_HOOK_DISABLE`, `NEO_HOOK_QUIET`, `NEO_NEXUS_URL`,
`NEO_REPO_ROOT`. NUNCA bloquea el commit (degrada silencio si Nexus
unreachable).

**Distinción:** Claude lifecycle hooks (PreToolUse, PostToolUse, Stop,
Notification, etc.) van en `.claude/settings.json` con `hooks` key
apuntando a scripts. Esos NO son git hooks. Para neoanvil usamos los
git hooks porque son el ciclo natural del developer; Claude
lifecycle hooks futuros podrían añadirse para auto-cert post-edit.

## Reglas de migración para neoanvil

### `rules/` → `skills/` (cuando aplique)

Migrar de `rules/` a `skills/` los archivos que:
- Cubren un workflow procedimental (no solo facts)
- Excederán 30k chars en el futuro
- Quieren ser invocables por nombre (`/skill`)
- Necesitan auto-load por path glob

**Quedan en `rules/`:**
- Constraints de código que aplican siempre (ej. zero-allocation rules)
- Listados de incidentes históricos (read-only)
- Convenciones referencia que NO son procedimientos

### Candidatos inmediatos

| Archivo actual | Acción | Razón |
|---|---|---|
| `rules/neo-synced-directives.md` (17 KB, post-compact) | KEEP (BoltDB sync target, hardcoded en `pkg/rag/wal.go`) | Auto-managed |
| `rules/neo-workflow.md` (14 KB) | Migrar a `skills/sre-workflow/SKILL.md` | Procedimental |
| `rules/neo-sre-doctrine.md` (22 KB) | Migrar a `skills/sre-doctrine/SKILL.md` (parcial hoy) | Procedimental |
| `rules/jira-workflow.md` | ✅ migrado a `skills/jira-workflow/` | hecho 2026-04-28 |
| `rules/neo-code-quality.md` (10 KB) | Mover a `docs/general/` (step E) | Constraints — referenciable via skill |
| `rules/neo-gosec-audit.md` (5 KB) | Mover a `docs/general/` (step E) | Policy doc |
| `rules/neo-deadcode-triage.md` (3 KB) | Mover a `docs/general/` (step E) | Policy doc |
| ✅ `rules/neo-synced-directives-deepseek.md` | Archivado a `docs/general/directives-archive-deepseek.md` 2026-05-13 | Content vive en `skills/deepseek-workflow/` |
| ✅ `rules/neo-synced-directives-jira.md` | Archivado a `docs/general/directives-archive-jira.md` 2026-05-13 | Content vive en `skills/jira-workflow/` |
| ✅ `rules/neo-synced-directives-pilar.md` | Archivado a `docs/general/directives-archive-pilar.md` 2026-05-13 | Content vive en `skills/sre-federation/` |
| ✅ `rules/neo-synced-directives-history.md` | Deleted 2026-05-13 (lineage en git) | Era 33KB→1.7KB con 2 duplicados |

### Carpetas nuevas que neoanvil debería abrir

- **`agents/`** — agentes especializados (`neo-jira-curator`, `neo-pilar-planner`)
- **`hooks/`** — `PostToolUse` para auto-audit + `Stop` para REM sleep
- **`output-styles/sre.md`** — enforce Ley 11 (Ouroboros)

---

## Priority order de overrides

Cuando un mismo nombre existe en varios niveles, **enterprise > personal > project**:

```
managed (org admin) > ~/.claude/skills/X > .claude/skills/X
```

Plugin skills usan namespace `<plugin>:<skill>` → no colisionan con
otros niveles.

---

## Cómo usar este doc

1. **Antes de añadir un .md a `.claude/`**: verifica si encaja mejor
   en `skills/` (procedimental) o `rules/` (constraint).
2. **Cuando un archivo de `rules/` se vuelve grande**: mira la tabla
   de candidatos arriba.
3. **Cuando hace falta automatización lifecycle** (auto-cert, lint
   pre-commit, notificación post-stop): es `hooks/`, no skill.
4. **Cuando quieras delegar trabajo complejo aislado** (research,
   audit batch): es un `agents/<name>.md`.

## Sources

- [Extend Claude with skills (official)](https://code.claude.com/docs/en/skills)
- [.claude Folder structure 2026 (Claude Directory)](https://www.claudedirectory.org/how-to/claude-folder)
- [Statusline customization (official)](https://code.claude.com/docs/en/statusline)
- [Claude Code Customization Guide (alexop.dev)](https://alexop.dev/posts/claude-code-customization-guide-claudemd-skills-subagents/)
- [Anatomy of .claude folder (codewithmukesh)](https://codewithmukesh.com/blog/anatomy-of-the-claude-folder/)
