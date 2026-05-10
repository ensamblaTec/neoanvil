# ADR-011: GitHub Plugin Design — PAT auth, audit hash-chain, cross-ref Jira

- **Fecha:** 2026-05-09
- **Estado:** Aceptado
- **Pilar:** PILAR I (Area 2 GitHub plugin, sesión 2026-05-09)
- **Supersedes:** —
- **Superseded by:** —
- **Hereda de:** [ADR-005](./ADR-005-plugin-architecture.md) (subprocess MCP pattern), [ADR-006](./ADR-006-jira-auth-flow.md) (token storage pattern)

## Contexto

Area 2 entrega `cmd/plugin-github` con **20 actions** REST v3 (Area 2.2.A-E + 2.2.E delivered 2026-05-10). Las decisiones arquitectónicas heredadas (subprocess MCP, multi-tenant credentials.json) están en ADR-005/006. Este documento captura las decisiones **específicas de GitHub** que NO se derivan de los anteriores: elección de auth flow, audit-log shape, cross-ref pattern con Jira, code-review-without-clone surface.

## Decisiones específicas de GitHub

### 1. Auth: GitHub Personal Access Token (Classic), no GitHub App

#### Opciones consideradas

| Opción | Pros | Contras | Veredicto |
|---|---|---|---|
| **PAT classic** | Setup en 30s (Settings → Developer Settings); scopes bien documentados (`repo`, `read:org`, `workflow`); compatible con multi-tenant via `credentials.json::github.<tenant>` ya existente | Caducidad manual (90d default, configurable); scope a usuario, no a app | ✅ Adoptado |
| **GitHub App + Installation Token** | Scope a installation (más limpio para orgs grandes); tokens auto-rotated a 1h; rate limit 15K/h vs 5K/h del PAT | Setup ~10min (registrar app, instalar en orgs, manejar JWT signing); UX desproporcionado para uso individual | ❌ Rechazado (defer hasta que aparezca un caso de uso de marketplace) |
| **Fine-grained PAT** | Scopes per-repo, no per-org; mejor blast radius | Disponible solo en algunas orgs (depende de policy); Atlassian-style "review your old tokens" UX que confunde | ❌ Rechazado por inestabilidad de la feature |

**Decisión:** PAT classic. Multi-tenant ya resuelto por el `credentials.json` schema heredado de plugin-jira.

#### Storage shape

```json
{
  "github": {
    "ensamblatec": { "token": "ghp_...", "scopes": "repo,read:org" },
    "personal":    { "token": "ghp_...", "scopes": "repo,read:org,workflow" }
  }
}
```

Rationale: el campo `scopes` es informativo (NO se valida server-side antes de cada call — el scope check vive en GitHub al recibir la request). Pero permite al operator hacer `audit-verify` consistency check si rota un PAT con scope inferior y olvida actualizar.

### 2. Audit log: hash-chain encadenado, JSONL, file lock por write

`~/.neo/audit-github.log` (perms `0600`). Cada línea:

```json
{"ts":"2026-05-09T22:15:00Z","tenant":"my-org","tool":"github",
 "action":"list_prs","owner":"x","repo":"y","result":"ok",
 "prev_sha256":"abc..."}
```

#### Por qué hash-chain en vez de plain log

| Threat | Hash-chain | Plain log |
|---|---|---|
| Operador olvida un call destructivo (merge_pr accidental) | Verifiable post-hoc | Verifiable post-hoc |
| Atacante con write access tampea el log para borrar evidencia | **Detectable**: `prev_sha256` rompe la cadena | Indetectable |
| Race entre 2 plugin processes (multi-tenant concurrent) | flock por write; última gana, audit íntegro | Posible interleaving / lost writes |

`cmd/neo/audit-verify` (TBD) walks la cadena. Si falla, alerta operator: tampering o crash mid-write (recuperable detectando el último valid prev_sha256 + truncating).

#### Por qué NO firmamos cada entry

Considerado: añadir HMAC con key derivada del PAT. Rechazado: la key estaría en `credentials.json` (mismo perm 0600 que el log), entonces atacante con read del log también lee la key. Sin asimetría no hay defensa real.

### 3. Cross-ref Jira ↔ GitHub: action local, no integración server-side

Action `cross_ref` es **PURA**: recibe texto + regex, retorna keys. NO llama Jira API. NO llama GitHub API. Es un parser local.

#### Por qué pure function

| Diseño | Pros | Contras | Veredicto |
|---|---|---|---|
| `cross_ref` puro (decisión) | Idempotente, testeable sin red, useful como helper para scripts no-MCP | Operator orquesta el siguiente paso (`jira/link_issue`) explícitamente | ✅ |
| `cross_ref` autoejecuta `jira/link_issue` | Menos llamadas para el operator | Acopla plugin-github a plugin-jira; double-call si el operator quería el list para hacer cross-check primero | ❌ |
| Webhook receiver: GitHub webhook → NeoAnvil → Jira link | "Real-time" cross-ref sin operator action | Requiere endpoint público o tunnel; aumenta blast radius del NeoAnvil dispatcher; webhook secret rotation overhead | ❌ Defer (alineado con [ADR-007](./ADR-007-bidirectional-webhooks.md) que también deferred bidirectional webhooks) |

### 4. Sin GraphQL

REST v3 cubre 100% de las 20 actions actuales. GraphQL vale la pena cuando necesitas agregaciones imposibles en REST (PR + reviews + checks + workflow runs en una llamada). Defer hasta que el operator pida explícitamente.

Workaround documentado: `Bash + gh api graphql` autorizado por operator vía `neo_command(action:"run")`.

### 5. Sin webhook consume

Mismo argumento que ADR-007 para Jira: requiere endpoint público o tunnel + secret rotation. NeoAnvil opera local-first; la decisión consistent con jira-side.

Para CI ↔ NeoAnvil, polea `get_checks(ref)`. Polling cost = bajo (PAT 5K/h alcanza para >1 poll/sec sustained).

## Consecuencias

- ✅ Setup operator <2min (PAT genera + `neo login` + verifica plugin).
- ✅ Multi-tenant out-of-the-box (heredado de jira pattern).
- ✅ Audit trail forensically-verifiable.
- ⚠️ PAT rotation manual: el operator es responsable de rotar antes de expiry. `__health__` puede surface days-until-expiry en una iteración futura (out of scope ahora; defer).
- ⚠️ Sin integration con GitHub Actions runs/jobs API más allá de `get_checks`. Si los workflows necesitan trigger, defer a `gh api workflow_dispatch`.

## Implementación

- `cmd/plugin-github/main.go` — map-dispatch (20 actions + `__health__`)
- `pkg/github/client.go` — REST client + 20 endpoint wrappers + base64 file decoder
- `cmd/plugin-github/config.go` — credentials.json reader, audit log writer (hash-chain)
- `pkg/github/client.go` — REST v3 HTTP client (rate-limit, retry, audit hook)
- `cmd/plugin-github/integ_test.go` — 333 LOC, subprocess JSON-RPC vs testmock harness

## Referencias

- [ADR-005](./ADR-005-plugin-architecture.md) — subprocess MCP pattern (parent ADR)
- [ADR-006](./ADR-006-jira-auth-flow.md) — credentials.json + multi-tenant pattern (parent)
- [ADR-007](./ADR-007-bidirectional-webhooks.md) — webhook deferral rationale (referenced)
- [`docs/plugins/github-integration-guide.md`](../plugins/github-integration-guide.md) — operator-facing guide
- Directiva `[GITHUB-PLUGIN-WORKFLOW]` en `.claude/rules/neo-synced-directives.md`
