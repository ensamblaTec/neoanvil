---
name: github-workflow
description: Doctrina operativa del plugin GitHub (mcp__neoanvil__github_github). Task-mode skill — invoke with `/github-workflow` when invoking any GitHub action (list_prs, create_pr, merge_pr, get_checks, cross_ref, etc.), troubleshooting auth, picking PAT scopes, or wiring GitHub ↔ Jira cross-references. Covers action selection, multi-tenant credentials, audit log shape, rate-limit awareness, and the cross_ref → jira/link_issue pattern.
disable-model-invocation: true
---

# GitHub Workflow — plugin operacional

> Reglas para usar `mcp__neoanvil__github_github` desde Claude Code.
> Cargada cuando el usuario menciona GitHub, PRs, issues, o cuando Claude
> detecta llamadas al plugin.

Plugin: `cmd/plugin-github/` (20 actions + `__health__`).
Guía completa: [`docs/plugins/github-integration-guide.md`](../../../docs/plugins/github-integration-guide.md).
Decisión arquitectónica: [`docs/adr/ADR-011-github-plugin-design.md`](../../../docs/adr/ADR-011-github-plugin-design.md).

---

## Regla #1 — Selecciona la action correcta

### PR surface (7)

| Quiero... | Action | Notas |
|---|---|---|
| Listar PRs abiertos | `list_prs` con `state: "open"` | Default state es `open`, puedes omitirlo |
| Ver detalle de un PR | `get_pr` con `number: N` | Devuelve title, body, head, base, mergeable, review state |
| Crear un PR | `create_pr` con `title, body, head, base` | `head` puede ser `branch` o `org:branch` para forks |
| Mergear un PR | `merge_pr` con `merge_method` | `merge_method`: `merge` (default), `squash`, `rebase` |
| Cerrar PR sin mergear | `close_pr` con `number` | NO usa fields, solo cierra |
| Listar comentarios review-side | `pr_comments` con `number` | Comentarios del review pane — diferente a comentarios del issue |
| Crear review | `create_review` con `event: APPROVE\|REQUEST_CHANGES\|COMMENT` | Necesita scope `workflow` en el PAT si el repo tiene required reviews |

### Issue surface (4)

| Quiero... | Action | Notas |
|---|---|---|
| Listar issues | `list_issues` con `state` | Mismo shape que list_prs |
| Ver detalle de un issue | `get_issue` con `number` | Mirror simétrico de get_pr |
| Crear issue | `create_issue` con `title, body, labels` | `labels` es array de strings (deben existir en el repo) |
| PATCH parcial issue | `update_issue` con `fields: {...}` | `fields` es objeto: state, title, body, labels |
| Comentar un issue (no review) | `add_issue_comment` con `body` | Diferente a `pr_comments` (read-only) y `create_review` (review pane) |

### Repo state (4)

| Quiero... | Action | Notas |
|---|---|---|
| Estado CI de un commit | `get_checks` con `ref` | `ref` puede ser SHA, branch, o tag |
| Listar branches | `list_branches` | Sin paginación visible — devuelve todo |
| Diff entre dos refs | `compare` con `base, head` | Devuelve commits + files changed |
| Histórico de commits en branch | `list_commits` con `branch` | Cuando branch es vacío usa default branch |

### Code surface — review remote sin clone (3)

| Quiero... | Action | Notas |
|---|---|---|
| Navegar el árbol de archivos | `list_files` con `path, ref` | path vacío = repo root; ref vacío = default branch |
| Leer contenido de un archivo | `get_file` con `path, ref` | ≤1MB inline (base64-decoded). >1MB error: usa download_url |
| Search code cross-repo | `search_code` con `query` | GitHub q= grammar; rate-limit estricto: 30 req/min authenticated |

### Helpers

| Quiero... | Action | Notas |
|---|---|---|
| Extraer keys Jira de texto | `cross_ref` con `text, jira_pattern` | LOCAL — no llama API |
| Liveness probe | `__health__` | <10ms, mandatory |

---

## Regla #2 — `cross_ref` es PURA (no llama APIs)

`cross_ref` es un parser local. Recibe texto + regex, retorna keys.

```
github_github(action:"cross_ref", text:"<PR body>")
→ ["MCPI-7", "MCPI-12"]
```

Para enlazar al ticket Jira, encadena con plugin-jira:

```
1. github_github(action:"get_pr", owner, repo, number) → body
2. github_github(action:"cross_ref", text:body) → ["MCPI-7"]
3. jira_jira(action:"link_issue", from_key:"MCPI-7", to_key:..., link_type:"Implements")
```

NO esperes que `cross_ref` cree el link automáticamente. Es decisión arquitectónica (ver ADR-011 §3).

---

## Regla #3 — PAT scopes mínimos

| Scope | Necesario para |
|---|---|
| `repo` | Casi todo: list_prs, get_pr, list_issues, create_pr, create_issue, list_files, list_commits, list_branches, compare |
| `read:org` | list_repos en orgs privadas; verificar membership |
| `workflow` | `create_review` en repos con required reviews; modificar GitHub Actions |
| `delete_repo` | (nunca lo necesitamos, no lo agregues) |

Si recibes un 403 con `Resource not accessible by integration`, el PAT no tiene el scope. Edita en GitHub Settings → Developer Settings → Personal access tokens → regenera o añade scope.

---

## Regla #4 — Multi-tenant: usa el active context

```bash
neo space use --provider github --tenant <org-name>
```

Antes de un call que toca a otra org, cambia el active. Si necesitas mezclar orgs en el mismo flujo (raro), abre Claude para inspeccionar `~/.neo/contexts.json` y rotalo entre calls.

El plugin enruta automáticamente: el `_meta.tenant_id` viene injectado por Nexus, basado en el active context. NO pases `tenant` explícito en los args (no es un campo del action schema).

---

## Regla #5 — Rate limiting: 5000 req/h authenticated

GitHub PAT classic permite 5000 req/h. El plugin emite warning soft a 4000 (80%); NO bloquea. Si excedes, GitHub devuelve 429 + `X-RateLimit-Reset` header. El plugin retorna error genérico con la hora de reset; espera y reintenta.

**Patrones que queman rate budget rápido:**
- `list_files` recursivo en repo grande (peor caso: ~1 req per dir)
- Polling `get_checks` cada <30s
- `compare` entre refs muy distantes (>1000 commits)

**Mitigation:** para listings grandes, usar `gh api graphql` desde Bash + `neo_command(action:"run")` (no consume del rate budget del plugin).

---

## Regla #6 — Audit log shape (hash-chained)

Cada action escribe a `~/.neo/audit-github.log`. Schema:

```json
{"ts":"2026-05-09T22:15:00Z","tenant":"...","tool":"github",
 "action":"list_prs","owner":"x","repo":"y","result":"ok",
 "prev_sha256":"abc..."}
```

`prev_sha256` encadena entries → tampering detectable. Si auditing un incidente:
1. `tail ~/.neo/audit-github.log | jq` para ver últimas 10 actions
2. `wc -l` para volumen total
3. Si sospechas tampering: walk forward desde la primera entry verificando que el `prev_sha256` de cada entry == sha256(entry-anterior). Mismatch = punto de tampering.

NO necesitas escribir esto a mano: `cmd/neo/audit-verify` (TBD) lo automatizará.

---

## Regla #7 — Sin GraphQL, sin webhooks (defer rationale)

| Feature | Estado | Workaround |
|---|---|---|
| GraphQL queries | Sin soporte (defer) | `gh api graphql -f query=...` via `neo_command(action:"run")` |
| Webhooks GitHub → NeoAnvil | Sin soporte (defer; ver ADR-007 + ADR-011 §5) | Polea `get_checks` o `list_prs` |
| GitHub Actions trigger (workflow_dispatch) | Sin soporte | `gh api -X POST /repos/.../actions/workflows/.../dispatches` |

El plugin se mantiene REST v3 puro. Si pides una feature que requiere GraphQL/webhooks, no esperes una action nueva: usa el workaround.

---

## Regla #8 — `__health__` es local-only (PILAR I 152.H)

```
github_github(action:"__health__")
→ {plugin_alive:true, tools_registered:["github/github"],
   uptime_seconds:1234, last_dispatch_unix:..., error_count:0,
   api_key_present:true}
```

NO llama GitHub API — instantáneo (<10ms). Detecta zombie cuando el plugin process vive pero el dispatcher murió. Si lo invocas y se cuelga, el plugin process necesita restart (`make rebuild-restart`).

---

## Cross-checks rápidos

```bash
# ¿plugin running?
curl -s http://127.0.0.1:9000/api/v1/plugins | jq '.plugins[] | select(.name=="github")'

# ¿credenciales configuradas?
cat ~/.neo/credentials.json | jq '.github | keys'

# ¿active tenant?
cat ~/.neo/contexts.json | jq '.github.active_tenant'

# ¿últimas 5 actions?
tail -5 ~/.neo/audit-github.log | jq -c '{ts, tenant, action, result}'
```

---

## Pattern: Jira issue → GitHub PR → cross-ref bidirectional

```
1. jira_jira(action:"transition", ticket_id:"MCPI-7", to_state:"In Progress")
2. # ... operator hace código + commits ...
3. github_github(action:"create_pr", owner, repo, title:"feat: ... [MCPI-7]",
                                       body:"Closes MCPI-7\n\n...", head, base)
4. github_github(action:"cross_ref", text:<PR body>) → ["MCPI-7"]
5. jira_jira(action:"transition", ticket_id:"MCPI-7", to_state:"REVIEW",
             resolution_comment:"PR <owner>/<repo>#<N>: <title>")
6. # ... después del merge:
7. jira_jira(action:"transition", ticket_id:"MCPI-7", to_state:"Done")
```

El audit trail bidireccional queda automático: PR body referencia MCPI-7; Jira comments referencian PR URL.

---

## Referencias rápidas

- Guía operativa completa: [`docs/plugins/github-integration-guide.md`](../../../docs/plugins/github-integration-guide.md)
- ADR de diseño: [`docs/adr/ADR-011-github-plugin-design.md`](../../../docs/adr/ADR-011-github-plugin-design.md)
- ADR padre subprocess MCP: [`docs/adr/ADR-005-plugin-architecture.md`](../../../docs/adr/ADR-005-plugin-architecture.md)
- Directiva canónica: `[GITHUB-PLUGIN-WORKFLOW]` en `.claude/rules/neo-synced-directives.md`
- Pipeline observability (`pkg/notify/otelx/openapi`): [`docs/general/observability.md`](../../../docs/general/observability.md)
- Skill hermano (Jira): [`.claude/skills/jira-workflow/SKILL.md`](../jira-workflow/SKILL.md)
