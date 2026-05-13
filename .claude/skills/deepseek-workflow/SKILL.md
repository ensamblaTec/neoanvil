---
name: deepseek-workflow
description: Doctrina operativa del plugin DeepSeek (mcp__neoanvil__deepseek_call). Task-mode skill — invoke with `/deepseek-workflow` when invoking distill_payload, map_reduce_refactor, red_team_audit, or generate_boilerplate. Covers action selection, cache discipline (50× cheaper repeat calls), thread continuity, model selection (v4-flash vs v4-pro), reasoning_effort knobs, the Babel pattern (English prompts), and the empirical hallucination triage rules.
---

# DeepSeek Workflow — plugin operacional

> Reglas para usar `mcp__neoanvil__deepseek_call` desde Claude Code.
> Cargada cuando el usuario menciona DeepSeek, audits adversariales,
> refactor fan-out, distillation, o boilerplate generation.

Plugin: `cmd/plugin-deepseek/` (4 actions + `__health__`).
Guía completa: [`docs/plugins/deepseek-api-reference.md`](../../../docs/plugins/deepseek-api-reference.md).
Decisión arquitectónica: [`docs/adr/ADR-012-deepseek-plugin-design.md`](../../../docs/adr/ADR-012-deepseek-plugin-design.md).
Directives archive (read-only): [`docs/general/directives-archive-deepseek.md`](../../../docs/general/directives-archive-deepseek.md) — 14 entries, ya no auto-cargadas como rules; contenido vive en este SKILL.md.

---

## Regla #1 — Selecciona la action correcta

| Quiero... | Action | Mode | Cost typical |
|---|---|---|---|
| Resumir audit report 5KB → 300 tokens | `distill_payload` | ephemeral | ~$0.001 |
| Refactor mecánico fan-out sobre N archivos | `map_reduce_refactor` | ephemeral | ~$0.005-$0.02 |
| Adversarial review multi-turn sobre código | `red_team_audit` | **threaded** | ~$0.005-$0.015 |
| Generate tests/docs en background | `generate_boilerplate` | threaded background | ~$0.003-$0.008 |

---

## Regla #2 — Cache discipline 50×

**Cache hit en V4-flash es 50× más barato** que el miss
($0.0028/M vs $0.14/M input tokens). Cache se basa en SHA-256
fingerprint del prefix (Block 1 = system + files concatenados).

**Cuando hagas N calls relacionados:**
- Mantén `Files[]` IDÉNTICO (mismo orden, misma lista, mismo contenido)
- Si cambias un archivo → invalidates prefix → pagas miss en TODOS los siguientes
- Cache window ~1h
- 10 prompts × mismos 50 archivos = 1 miss + 9 hits = 50× cheaper

**Mal:**
```
red_team_audit({files: [a.go, b.go]})         // miss
red_team_audit({files: [a.go, c.go]})         // miss (Files cambió)
red_team_audit({files: [a.go, b.go, c.go]})   // miss (orden cambió)
```

**Bien:**
```
red_team_audit({files: [a.go, b.go, c.go]})   // miss inicial
red_team_audit({files: [a.go, b.go, c.go]})   // hit (50×)
red_team_audit({files: [a.go, b.go, c.go]})   // hit
```

---

## Regla #3 — Modelo default + cuándo usar pro

| Modelo | Cuándo | Cost ratio |
|---|---|---|
| `deepseek-chat` / `deepseek-v4-flash` | **80% del trabajo**: distill, map_reduce, generate_boilerplate, red_team rondas iniciales | 1× (baseline) |
| `deepseek-v4-pro` | Audits de seguridad sobre código nuevo con primitivas sensibles (crypto, auth, lock, storage, network) | 3-5× |

**Cuándo NO usar pro:**
- Refactor mecánico (`map_reduce_refactor`) → pro mismos resultados, 5× más caro
- Distillation → pro overkill
- Decisiones que ya validaste pen-and-paper

**Cuándo SÍ usar pro:**
- Nuevo paquete con primitivas sensibles (per directive #54
  `[SRE-NEW-CODE-AUDIT-MANDATORY]`)
- Decisiones arquitectónicas que el equipo va a tomar como ground truth
- Audits que necesitan walkear cipher state / lock state / call graphs

---

## Regla #4 — `reasoning_effort: high` vs `max`

Default `high` cubre 90% de los casos. `max` produce 3-5× más
reasoning tokens; reservar **solo para crypto, distributed-lock, o
concurrency audits** donde el modelo necesita pensar largo. Cualquier
otro caso usar `high` para no quemar budget.

---

## Regla #5 — Babel pattern: prompts en inglés

Los prompts a DeepSeek API se envían en INGLÉS. El operator y Claude
pueden conversar en español, pero antes de invocar `deepseek/call`,
traducir el `target_prompt` a inglés. **Razón:** benchmarks de
coding tasks notablemente mejores en inglés. Esta regla aplica
incluso para análisis de código local.

---

## Regla #6 — Reasoner model constraints

`deepseek-reasoner` y v4-flash thinking tienen restricciones HTTP
400 si pasas:
- `temperature`, `top_p`, `presence_penalty`, `frequency_penalty`
- `logprobs`, `top_logprobs`
- `tools` (function calling)
- FIM completion

En multi-turn threaded conversations, **NO incluir prior
`reasoning_content`** en messages — solo el `content` visible.
Incluirlo dispara 400.

`max_tokens` limita TODA la salida incluyendo CoT — un visible
response de 500 tokens puede llevar 2000 thinking tokens, así que
budget accordingly:
- 4096 para audits cortos
- 16K-32K para deep reviews

---

## Regla #7 — Threaded mode + thread_id continuity

`red_team_audit` retorna `thread_id`; persiste el ID y pásalo
explícito en la post-fix QA call:

```
red_team_audit({files: [...]})  → thread_id: "ds_thread_xxx"
# fix the finding
red_team_audit({thread_id: "ds_thread_xxx",
                target_prompt: "audit my fix; closed SEV without new regressions?"})
```

Segundo call cuesta 30% menos en tokens (DS ya tiene contexto del
archivo + findings previos) y produce diff-aware analysis.

**Auto-invalidation:** si un archivo en `thread.FileDeps` cambia
(sha256 diff), thread expira y siguiente call retorna error
`thread invalidated, start new`.

---

## Regla #8 — Triage rules (empirical, ~30 sessions)

DS hallucinates SEV 10s **~25% of the time**. Pattern:
"compose-2-true-premises into false-conclusion".

**Triage protocol:**
1. **DS finding SEV ≥ 9** → walk through pen-and-paper trace BEFORE
   applying fix. Especially if AEAD tag verification, TOCTOU windows,
   or distributed-lock state are involved.
2. **DS contradicts itself across rounds** → discard. Round 2 saying
   "incorrect" then "is correct" is a known pattern.
3. **DS hallucinates code that doesn't exist** ("cursor pattern" when
   you use direct `b.Get()`) → discard.
4. **DS recommends micro-opt on code <100 LOC** → discard.

**Accept when:**
- Cites specific line + function
- Explains attack vector concretely
- Suggests incremental fix without invasiveness

**Triage time budget:** 30% of total audit time.
Hallucination patterns catalog en
`~/.claude/projects/.../memory/feedback_deepseek_hallucination_patterns.md`.

---

## Regla #9 — Smoke-test antes de bulk

Cuando vas a correr `map_reduce_refactor` >5 archivos: el plugin
hace un smoke test en el primer archivo automáticamente (375.C).
Si falla:

```
⚠️ SMOKE_TEST_ABORT: first file foo.go failed smoke test
(err=..., response_len=0). Aborting batch of 12 files to avoid wasting tokens.
```

Esto es deliberado — evita gastar $$ en N archivos cuando las
instructions están rotas. Si ves SMOKE_TEST_ABORT, revisa el
`refactor_instructions` antes de re-correr.

---

## Regla #10 — Operational efficiency (~50% más valor por dólar)

Per directive `[DEEPSEEK-OPERATIONAL-EFFICIENCY]`:

1. **Cross-call cache discipline** — mantén `Files[]` superset
   estable cross-call (ver Regla #2)
2. **Thread continuity per-fix** — guarda `thread_id`, pásalo en
   post-fix QA (ver Regla #7)
3. **Smoke-test antes de calibrar bulk** — UNA call para verificar
   `model` field + `reasoning_tokens` field del response object
   antes de gastar $0.08 en una calibración 36-cell
4. **Usa las 4 actions, no solo `red_team_audit`:**
   - `distill_payload` para resumir audit report 5KB → 300 tokens
   - `map_reduce_refactor` para fan-out fixes mecánicos
   - `generate_boilerplate` para tests desde finding+fix
5. **Iterar con DS para finding triage cuando es ambiguo** — después
   de un audit que retorna 7 findings de SEV mixto, follow-up call con
   `thread_id` + prompt "Rank these 7 findings by your own certainty.
   Which 3 are most likely REAL based on your reasoning trace?"

---

## Regla #11 — Cuándo NO usar DeepSeek (route a `neo_local_llm`)

**ADR-013** entregó `neo_local_llm` como complemento $0/call sobre Qwen
2.5-Coder 7B local. Para preservar el budget DS, mover los siguientes
casos al local:

| Caso | Mejor lugar | Razón |
|------|-------------|-------|
| Refactor mecánico (rename, format, doc-comment) | `neo_local_llm` | DS pricing es overkill; 7B local da resultado equivalente |
| Boilerplate generation (test stubs, README skeletons) | `neo_local_llm` | Calidad suficiente, $0 |
| Daemon-mode "is this finding real?" yes/no | `neo_local_llm` | Decision binaria, no requiere razonamiento |
| Translation, summarization | `neo_local_llm` | DS reasoner overkill |
| Distill report 5KB → 300 tokens | DS `distill_payload` | Cache discipline 50× sigue siendo win |
| **SEV ≥ 9 security audits** | DS `red_team_audit` (pro/high) | Local 7B no detecta clases sutiles |
| **New crypto/auth/storage primitives** | DS pro+max | Frontier quality requerida |
| Architectural decisions ground-truth | DS reasoner | Aceptamos costo por correctness |

Numéricamente: 100 audits/noche en daemon mode = $3-15 con DS, $0 con
local. El operator routing rule vive en agent prompt — no es servidor-side
auto-routing.

---

## Cross-checks rápidos

```bash
# ¿plugin running?
curl -s http://127.0.0.1:9000/api/v1/plugins | jq '.plugins[] | select(.name=="deepseek")'

# ¿credenciales configuradas?
grep DEEPSEEK_API_KEY ~/.neo/.env || echo "no key in .env"

# ¿últimos thread IDs activos?
# (no hay CLI directo aún — se ven en BoltDB ~/.neo/db/deepseek.db)
```

---

## Self-Audit V2 obligatorio (per directive #54)

Al finalizar cada bloque significativo:

**Parte 1 — DS Coverage:**
| Archivo | DS Auditado | Razón si no |
|---|---|---|
| pkg/X/Y.go | ✅ / ❌ | offline / trivial / doc |

Si hay ❌ SIN razón válida → episodio incompleto. Documentar en
`technical_debt.md` con tag `[ds-audit-pending]`.

**Parte 2 — Tool efficiency:** tabla 1-10 de tools usadas + propuesta
mutación para la peor.

---

## Referencias rápidas

- Guía API completa: [`docs/plugins/deepseek-api-reference.md`](../../../docs/plugins/deepseek-api-reference.md)
- ADR: [`docs/adr/ADR-012-deepseek-plugin-design.md`](../../../docs/adr/ADR-012-deepseek-plugin-design.md)
- Directives archive (read-only): [`docs/general/directives-archive-deepseek.md`](../../../docs/general/directives-archive-deepseek.md)
- Hallucination catalog: `~/.claude/projects/.../memory/feedback_deepseek_hallucination_patterns.md`
- Skill hermano (jira): [`.claude/skills/jira-workflow/SKILL.md`](../jira-workflow/SKILL.md)
- Skill hermano (github): [`.claude/skills/github-workflow/SKILL.md`](../github-workflow/SKILL.md)
