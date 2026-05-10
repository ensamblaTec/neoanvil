# GitHub Integration Guide — neoanvil

Plugin `mcp__neoanvil__github_github` expone GitHub REST v3 desde Claude Code. Multi-tenant (varias orgs), audit log hash-chain, cross-ref con plugin-jira para enlazar PR ↔ issue.

> **Hermano operativo:** [`docs/plugins/jira-integration-guide.md`](./jira-integration-guide.md). Mismo patrón subprocess MCP (ADR-005), mismo audit-log shape, mismo flujo `neo login`.

---

## TL;DR

```bash
# 1. Auth (el plugin lee ~/.neo/credentials.json)
neo login --provider github --tenant my-org
# (te pide GitHub Personal Access Token con scopes repo + read:org)

# 2. Activa el contexto (multi-tenant)
neo space use --provider github --tenant my-org

# 3. Verifica que Nexus ve el plugin
curl -s http://127.0.0.1:9000/api/v1/plugins | jq '.plugins[] | select(.name=="github")'
# → {"name":"github","status":"running","tools":["github/github"]}

# 4. Llama una action (ejemplo: listar PRs abiertos del repo)
# Desde Claude Code:
#   github_github(action: "list_prs", owner: "ensamblatec", repo: "neoanvil", state: "open")
```

---

## 20 actions disponibles

### PR surface (7)

| Action | Propósito | Args principales |
|---|---|---|
| `list_prs` | Lista PRs (open/closed/all) | `owner`, `repo`, `state` (default `open`) |
| `get_pr` | Snapshot detallado de un PR | `owner`, `repo`, `number` |
| `create_pr` | Crea PR | `owner`, `repo`, `title`, `body`, `head`, `base` |
| `merge_pr` | Merge PR | `owner`, `repo`, `number`, `merge_method` (`merge`/`squash`/`rebase`) |
| `close_pr` | Cierra PR sin merge | `owner`, `repo`, `number` |
| `pr_comments` | Lista comentarios review-side | `owner`, `repo`, `number` |
| `create_review` | Crea review (APPROVE / REQUEST_CHANGES / COMMENT) | `owner`, `repo`, `number`, `event`, `body` |

### Issue surface (4)

| Action | Propósito | Args principales |
|---|---|---|
| `list_issues` | Lista issues | `owner`, `repo`, `state` |
| `get_issue` | Snapshot de un issue individual | `owner`, `repo`, `number` |
| `create_issue` | Crea issue | `owner`, `repo`, `title`, `body`, `labels` |
| `update_issue` | PATCH parcial sobre issue | `owner`, `repo`, `number`, `fields` (state/title/body/labels) |
| `add_issue_comment` | Comentar un issue (no review-side) | `owner`, `repo`, `number`, `body` |

### Repo state (5)

| Action | Propósito | Args principales |
|---|---|---|
| `get_checks` | Estado CI de un commit/branch | `owner`, `repo`, `ref` |
| `list_branches` | Lista branches del repo | `owner`, `repo` |
| `compare` | Diff entre dos refs | `owner`, `repo`, `base`, `head` |
| `list_commits` | Histórico de commits en branch | `owner`, `repo`, `branch` (default repo's default branch) |

### Code surface — review remote without clone (3)

| Action | Propósito | Args principales |
|---|---|---|
| `list_files` | Lista contenido de un dir en owner/repo @ ref | `owner`, `repo`, `path` (default root), `ref` |
| `get_file` | Lee contenido de un archivo (≤1MB inline) | `owner`, `repo`, `path`, `ref` |
| `search_code` | GitHub code search (q= grammar) | `query` (e.g. `auth.Load language:go repo:foo/bar`) |

### Helpers (2)

| Action | Propósito | Args principales |
|---|---|---|
| `cross_ref` | Extrae claves Jira de texto libre (PURE — no API call) | `text`, `jira_pattern` (default `[A-Z][A-Z0-9]{1,9}-\d+`) |
| `__health__` | Liveness probe local (mandatory) | — |

> **Cross-ref con plugin-jira:** `cross_ref` parsea el body de un PR (o cualquier texto) buscando `MCPI-N`, `ABC-123`, etc. Útil para auto-link en CI: el hook de pre-merge corre `cross_ref` sobre el PR body, luego el plugin-jira hace `link_issue` con los keys encontrados.

> **Code review remote sin clonar:** combina `list_files` (navega el árbol) + `get_file` (lee contenido) + `search_code` (búsqueda cross-repo). El uso típico para PR review sin tener el repo localmente.

---

## Componentes

### Plugin (`cmd/plugin-github/`)

Subprocess MCP que speak JSON-RPC sobre stdio. Se spawne automáticamente por Nexus al boot (registro en `~/.neo/plugins.yaml`). 1480 LOC + 626 LOC en `pkg/github/` para el HTTP client + retry.

### Auth + multi-tenant context

| Archivo | Rol | Permisos |
|---|---|---|
| `~/.neo/credentials.json` | Token GitHub PAT por tenant. Schema: `{"github": {"<tenant>": {"token": "ghp_..."}}}` | `0600` |
| `~/.neo/contexts.json` | Active tenant por provider. Schema: `{"github": {"active_tenant": "my-org"}}` | `0644` |
| `~/.neo/audit-github.log` | JSONL hash-chain de cada action. Cada línea contiene `prev_sha256` para detectar tampering | `0600` |

**Patrón multi-tenant:** mismo plugin process atiende a múltiples orgs. La action recibe el contexto activo via `_meta.tenant_id` (extraído por Nexus). Si necesitas hablar a una org diferente sin cambiar el active context, usa `neo space use --provider github --tenant <other>` antes del call.

### Audit log

Cada action emite una línea JSON a `~/.neo/audit-github.log`:

```json
{"ts":"2026-05-09T22:15:00Z","tenant":"my-org","tool":"github","action":"list_prs",
 "owner":"ensamblatec","repo":"neoanvil","result":"ok","prev_sha256":"abc..."}
```

Tampering detection: `prev_sha256` encadena entries. `cmd/neo/audit-verify` (TBD) walks el chain.

---

## Setup paso a paso (primer uso)

```bash
# 1. Genera un GitHub PAT (Settings → Developer Settings → Tokens)
#    Scopes mínimos: repo, read:org
#    Para writes: + repo (full)
#    Si vas a usar create_review: + repo + workflow

# 2. Login (ejemplo: dos orgs)
neo login --provider github --tenant ensamblatec
neo login --provider github --tenant personal

# 3. Verifica que se guardaron en credentials.json (NO commitees este archivo)
cat ~/.neo/credentials.json | jq '.github | keys'
# → ["ensamblatec","personal"]

# 4. Activa el primero como default
neo space use --provider github --tenant ensamblatec

# 5. Verifica que el plugin lo ve (requiere Nexus running)
curl -s http://127.0.0.1:9000/api/v1/plugins | jq '.plugins[] | select(.name=="github") | .health'
# → {"plugin_alive":true,"tools_registered":["github/github"],"api_key_present":true,...}
```

---

## Test mock harness

Para CI sin tocar GitHub real:

```bash
# El testmock harness en internal/testmock/github.go expone un servidor
# HTTP test que devuelve respuestas GitHub-shaped. Plugin apunta vía
# GITHUB_BASE_URL env override.

go test -short ./cmd/plugin-github/   # unit tests
go test -tags=integ ./cmd/plugin-github/  # integ_test.go contra mock (333 LOC, gated)
```

`cmd/plugin-github/integ_test.go` (PILAR I 2.3.C) spawne el binario real y lo dirige por stdin/stdout JSON-RPC contra el mock, replicando la shape de `cmd/plugin-jira/integ_test.go` para que el futuro shared infra (drain, audit, multi-tenant) migre con minimal diff.

---

## Limitaciones conocidas

- **Rate limiting:** GitHub PAT tiene 5000 req/h authenticated. El plugin throttle es soft (warning a 80%); no bloquea. Si excedes, el call retorna 429 y la próxima request espera el reset window.
- **GraphQL:** sin soporte. REST v3 only. Si necesitas un GraphQL query (ej: PRs con review-state agregado), hazlo desde Claude con un Bash + `gh api graphql` (autorizado por el operador).
- **Webhooks:** sin soporte. `pkg/notify` puede dispatchar eventos NeoAnvil → Slack/Discord, pero no consume webhooks GitHub. Para CI ↔ NeoAnvil, polea `get_checks`.
- **Search code:** la action `cross_ref` es local (parsea texto que tú le pasas). Para search en repos, usa `gh search code` desde Bash.

---

## Cross-ref Jira ↔ GitHub patrón

Workflow recomendado para enlazar PRs a Jira tickets:

```
1. github_github(action: "get_pr", owner, repo, number) → body
2. github_github(action: "cross_ref", text: <body>) → ["MCPI-7", "MCPI-12"]
3. Por cada key:
   jira_jira(action: "link_issue", from_key: "MCPI-7", to_key: <pr-derived>, link_type: "Implements")
   o
   jira_jira(action: "transition", ticket_id: "MCPI-7", to_state: "REVIEW",
             resolution_comment: "PR <owner>/<repo>#<number>: <title>")
```

Esto deja un audit trail bidireccional: GitHub PR body → Jira issue link, Jira workflow comment → GitHub PR ref.

---

## Referencias

- ADR-005 (subprocess MCP plugin pattern, compartido con jira/deepseek)
- [`docs/plugins/jira-integration-guide.md`](./jira-integration-guide.md) — patrón hermano
- [`docs/general/observability.md`](../general/observability.md) — pipeline notify + otelx + openapi
- [`pkg/github/`](../../pkg/github/) — HTTP client (626 LOC: rate-limit, retry, audit)
- [`cmd/plugin-github/`](../../cmd/plugin-github/) — plugin process (1480 LOC: action dispatch, validation, audit hash-chain)
- Directiva canónica `[GITHUB-PLUGIN-WORKFLOW]` en `.claude/rules/neo-synced-directives.md`
