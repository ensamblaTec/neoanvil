# ADR-012: DeepSeek Plugin Design — fan-out cache discipline, thread continuity, single-tenant auth

- **Fecha:** 2026-05-10
- **Estado:** Aceptado
- **Pilar:** PILAR XXIV (Épicas 131.A-K + bootstrap 143)
- **Hereda de:** [ADR-005](./ADR-005-plugin-architecture.md) (subprocess MCP pattern)
- **Hermano:** [ADR-006](./ADR-006-jira-auth-flow.md), [ADR-011](./ADR-011-github-plugin-design.md)

## Contexto

`cmd/plugin-deepseek` (1396 LOC pkg + 2871 LOC cmd) es el fan-out engine
hacia DeepSeek API. Cubre 4 actions: `distill_payload`,
`map_reduce_refactor`, `red_team_audit`, `generate_boilerplate`. La
mayor parte de las decisiones de subprocess MCP están en ADR-005. Este
ADR captura las que son **específicas de DeepSeek** y que no se
derivan del patrón subprocess: cache discipline, thread continuity,
single-tenant auth, y la decisión "Babel pattern" (prompts en inglés
incluso si el operator usa español).

## Decisiones específicas de DeepSeek

### 1. Single-tenant (no multi-tenant)

#### Opciones consideradas

| Opción | Pros | Contras | Veredicto |
|---|---|---|---|
| **Single-tenant** (un solo `DEEPSEEK_API_KEY` en config) | Setup mínimo (1 var de entorno); contabilidad clara de tokens/costos contra una sola cuenta | Operator que trabaja contra varios proyectos paga del mismo bucket | ✅ Adoptado |
| Multi-tenant (espejo de jira/github via `credentials.json::deepseek.<tenant>`) | Aislamiento de costos por proyecto/cliente | DeepSeek facturación es por API key, no por consumo. Multi-tenant añade fricción de setup sin valor real para nuestro caso | ❌ Rechazado |

**Decisión:** una sola key. Si el operator necesita aislamiento de
costos, abre cuentas DeepSeek separadas y rota la env var.

### 2. Cache discipline obligatoria — `Files[]` idéntico cross-call = 50× cheaper

DeepSeek V4-flash cobra **$0.0028/M tokens** en cache hit vs **$0.14/M
tokens** en cache miss. Cache se basa en SHA-256 fingerprint del
prefix (Block 1 = system + files concatenados).

**Regla operacional canónica:** cuando hagas N calls relacionados sobre
el mismo set de archivos, mantén `Files[]` IDÉNTICO (mismo orden, misma
lista, mismo contenido). Si cambias un archivo entre calls, invalidas
el prefix y pagas miss en TODOS los siguientes hasta volver al
fingerprint original. Cache window ~1h. Batch tu trabajo: 10 prompts
diferentes sobre los mismos 50 archivos = 1 miss + 9 hits = 50× cheaper
que 10 misses.

#### Por qué este enfoque

| Alternativa | Trade-off |
|---|---|
| **Cache discipline** (decisión) | Operator debe pensar en `Files[]`; reward operacional 50× |
| Cache transparente / auto | Sería conveniente pero requiere predicción de qué archivos formarán el prefix más rentable. No factible client-side |
| No usar cache (1 call = 1 miss) | $$$ a escala. Documentado en directive #54 que sesiones reales burning $40+ con audits paralelos |

### 3. Session modes — ephemeral vs threaded

| Action | Mode | Razón |
|---|---|---|
| `distill_payload` | ephemeral | Compresión one-shot, sin estado |
| `map_reduce_refactor` | ephemeral | Cada archivo es independiente; resultado se reduce localmente |
| `red_team_audit` | **threaded** | Multi-turn audit del mismo código. Continuidad de razonamiento + cache discipline 50× |
| `generate_boilerplate` | threaded background | Devuelve `task_id` inmediato; polling con `action:status` |

#### Auto-invalidation de threads

Cuando un archivo en `thread.FileDeps` cambia (sha256 diff), el thread
expira y la siguiente call retorna error `thread invalidated, start new`.
Evita que el agent siga razonando sobre código stale.

### 4. Babel pattern — prompts en inglés

Los prompts a DeepSeek API se envían en INGLÉS. El operator y Claude
pueden conversar en español, pero antes de invocar `deepseek/call`,
traducir el `target_prompt` a inglés.

**Razón:** DeepSeek fue entrenado primarily en inglés + chino;
benchmarks de coding/refactor tasks son notablemente mejores en
inglés. Esta regla está marcada en el schema del tool ("Babel
Pattern") y debe respetarse incluso para tareas locales (la
traducción la hace Claude antes del call, no el plugin).

### 5. Thinking mode — model-specific constraints

`deepseek-reasoner` (V4-flash thinking) tiene restricciones HTTP 400
si pasas: `temperature`, `top_p`, `presence_penalty`,
`frequency_penalty`, `logprobs`, `top_logprobs`, `tools` (function
calling), o FIM. En multi-turn threaded conversations, NO incluir
prior `reasoning_content` en messages — solo el `content` visible
(incluirlo dispara 400). El campo `max_tokens` limita TODA la salida
incluyendo CoT — un visible response de 500 tokens puede llevar 2000
thinking tokens, así que budget `max_tokens` accordingly (recomendado:
4096 para audits cortos, 16K-32K para deep reviews).

### 6. Cache cold cost ceiling — ~$0.28/M output × 500K cap por sesión

`max_tokens_per_session=500000` en `neo.yaml::deepseek` es un
hard-cap operacional. Asume V4-flash pricing ($0.28/M output
tokens) → ceiling de **~$0.14/sesión** si todo es output.
En la práctica el cache hit rate empuja el costo real a
**~$0.04-0.10/sesión** (medido en sesiones reales de PILAR XXVII).

## Consecuencias

- ✅ Setup operator <1min (una env var)
- ✅ Cache hit rate empíricamente 30-70% para audits batch
- ✅ Threaded mode permite red-team multi-round sobre el mismo código
- ⚠️ Single-tenant means: rotación de billing buckets requiere
  re-deploy con nueva env var
- ⚠️ Cache window 1h — calls separados >1h pagan miss again
- ⚠️ DeepSeek API instability: ~25% de los audits >60s EOFan en
  v4-flash high. Documented en directive #54. Regression tests usan
  pen-and-paper como compensating control cuando DS API falla.

## Implementación

- `pkg/deepseek/client.go` — REST v1 wrapper, throttle, BoltDB threads
- `cmd/plugin-deepseek/tool_distill.go` — distill_payload action
- `cmd/plugin-deepseek/tool_map_reduce.go` — fan-out refactor (refactored
  to CC≤7 helpers in 5138d0f)
- `cmd/plugin-deepseek/tool_red_team_audit.go` — adversarial review
- `cmd/plugin-deepseek/tool_boilerplate.go` — background generation
- `pkg/deepseek/cache/builder.go` — `StructuralCacheBuilder` para
  deduplicación de Block 1
- `pkg/deepseek/session/router.go` — thread continuity routing
- BoltDB threads en `~/.neo/db/deepseek.db` con buckets `threads`,
  `billing`, `checkpoints`

## Referencias

- [ADR-005](./ADR-005-plugin-architecture.md) — subprocess MCP pattern
- [ADR-011](./ADR-011-github-plugin-design.md) — hermano: GitHub plugin
- `docs/plugins/deepseek-api-reference.md` — operator guide
- Directive `[DEEPSEEK-MODEL-DEFAULT]`, `[DEEPSEEK-CACHE-DISCIPLINE]`,
  `[DEEPSEEK-REASONER-CONSTRAINTS]`, `[DEEPSEEK-BABEL-PATTERN]`,
  `[DEEPSEEK-SESSION-MODE]`, `[DEEPSEEK-CONTEXT-1M]`,
  `[DEEPSEEK-CONFIG-CURRENT]`, `[DEEPSEEK-PRECISION-EMPIRICAL]`,
  `[DEEPSEEK-PROTOCOL-MANDATORY]`, `[DEEPSEEK-PAIR-FEEDBACK-LOOP]`,
  `[DEEPSEEK-ROUND-PATTERNS]`, `[SRE-NEW-CODE-AUDIT-MANDATORY]`,
  `[DEEPSEEK-OPERATIONAL-EFFICIENCY]` en
  `docs/general/directives-archive-deepseek.md`
- Hallucination patterns catalog: `feedback_deepseek_hallucination_patterns.md`
