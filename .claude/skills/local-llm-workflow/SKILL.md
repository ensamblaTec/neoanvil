---
name: local-llm-workflow
description: Doctrina operativa del tool `mcp__neoanvil__neo_local_llm` (ADR-013). Task-mode skill — invoke with `/local-llm-workflow` when deciding whether a prompt should go to the local Ollama LLM (Qwen 2.5-Coder 7B by default) or to the DeepSeek plugin. Covers the routing matrix, prompt shape, system prompt usage, model override decisions, hardware-fit caveats, latency expectations, the cost calculus vs DS, and the empirical quality calibration.
disable-model-invocation: true
---

# Local LLM Workflow — `neo_local_llm` operacional

> Reglas para usar `mcp__neoanvil__neo_local_llm` desde Claude Code o
> daemon-mode. El tool es complemento $0/call al plugin DeepSeek y se
> activa via `cfg.AI.LocalModel` (default `qwen2.5-coder:7b` en
> `neo.yaml::ai.local_model`).

Tool: `cmd/neo-mcp/tool_local_llm.go` (15ª MCP tool, ADR-013).
Cliente: `pkg/llm/client.go` (Ollama `/api/generate`).
Decisión arquitectónica: [`docs/adr/ADR-013-local-llm-tool.md`](../../../docs/adr/ADR-013-local-llm-tool.md).
Directives: #53 `[LOCAL-LLM-ROUTING]` y #54 `[CONFIG-FIELD-BACKFILL-RULE]`.

---

## Regla #1 — La decisión de routing es del agent

El tool **no** hace routing automático. La decisión "este prompt va a
local o a DS" vive en el agent prompt y aplica la matriz de abajo. No
existe un classifier server-side porque añadiría latencia + más LLM
calls + sería propenso a errores silenciosos.

| Use `neo_local_llm` para… | Mantén `deepseek_call` para… |
|---|---|
| Boilerplate (test stubs, README skeletons) | Nuevas primitivas crypto / auth / storage |
| Refactor mecánico (rename, format, doc-comment) | SEV ≥ 9 security audits |
| Mechanical fan-out (rename, migrate, doc-update) | Decisiones arquitectónicas que serán ground truth |
| Daemon-mode triage / yes-no audit | Multi-turn deep-review threads |
| Translation, summarisation | Cipher state / lock state / call-graph walks |
| "Is this a real issue?" classification | Anything you can't pen-and-paper-trace later |

**Regla simple:** si el resultado se va a copy-paste sin verificación
detallada, debe ir a DS. Si lo vas a auditar tú igual, va a local.

---

## Regla #2 — Schema operativo

```jsonc
{
  "prompt": "string (required, English preferred)",
  "model": "string (optional, default cfg.AI.LocalModel)",
  "system": "string (optional, prefixed as SYSTEM:\\n... USER:\\n...)",
  "max_tokens": "int (optional, default 4096)",
  "temperature": "float (optional, default 0.2)"
}
```

Retorno (MCP envelope estándar):

```jsonc
{
  "content": [{
    "type": "text",
    "text": "<response>\n\n---\n_model: ... · latency: ...ms · prompt: N chars · response: M chars_"
  }]
}
```

El footer de metadata está **dentro** del text content — no es una
field separada. Útil para debug: latency revela si el modelo está
warm o cold; size revela si el max_tokens cap se quedó corto.

---

## Regla #3 — Latency expectations en este hardware

| Workload | Cold | Warm |
|---|---|---|
| Trivial classification (≤ 32 tokens out) | 13-15 s | **~407 ms** |
| Short audit (~256 tokens out) | ~5-6 s | ~3 s |
| Realistic audit (~500 tokens out, 16 tok/s sustained) | 25-32 s | ~25-30 s |
| Deep review (4096 tokens out) | ~3-5 min | ~3-5 min |

Cold = primera call después de boot u Ollama keep-alive expirado.
Warm = modelo residente en VRAM. Ollama default keep-alive es 5 min;
para daemon-mode subir a `OLLAMA_KEEP_ALIVE=-1` mantiene el modelo
permanentemente cargado.

---

## Regla #4 — Babel pattern aplica

Los prompts a `neo_local_llm` se mandan en **inglés**, igual que con
DeepSeek (la regla está en directiva del plugin DS). El operator y
Claude pueden conversar en español, pero antes de invocar el tool,
traducir el `target_prompt`. Razón: Qwen 2.5-Coder fue trained
primarily en inglés + chino; coding tasks degradan ~15-20% en otros
idiomas.

---

## Regla #5 — Cuándo usar `system` field

| Caso | system field |
|---|---|
| Single-shot triage / Q&A | omitir |
| Audit con guardrails específicos ("be terse", "list only HIGH SEV") | usar |
| Generate boilerplate con style guide | usar |
| Translation with target language | usar |

El sistema concatena como `SYSTEM:\n<system>\n\nUSER:\n<prompt>`. NO
es chat-API multi-turn — es prompt único. Para multi-turn use sesión
explícita pasando el contexto previo en el prompt.

Cap recomendado: ≤ 4 KB de system prompt. Más allá empieza a comer
contexto efectivo del modelo (Qwen 7B context window 32K total).

---

## Regla #6 — Modelo override (cuándo + cuándo no)

Default `qwen2.5-coder:7b` (4.5 GB) cubre el 95% de casos. Override
vía `args["model"]`:

| Modelo | Cuándo override | Cost |
|---|---|---|
| `qwen2.5-coder:32b` | **Solo** operadores con ≥ 64 GB RAM (este PC NO) | 3-5× más lento |
| `qwen2:0.5b` | Trivial classification @ 500-1000 tok/s | Calidad baja |
| `qwen2.5-coder:14b` (si está pulled) | Sweet spot 24 GB GPU + 32 GB RAM | ~2× del 7b |

**Hardware-fit guard** (esta PC, RTX 3090 24 GB / 32 GB RAM): solo 7B
y 0.5B son viables. 32B falla con `model requires 44.5 GiB system
memory than is available (41.9)`. Operadores con más RAM deben setear
`cfg.AI.LocalModel` en `neo.yaml` una vez en lugar de override per-call.

---

## Regla #7 — Cost calculus (la palanca real)

100 audits/noche en daemon mode:

| Backend | Costo/mes | Latency | Quality (race-condition sample) |
|---|---|---|---|
| DeepSeek API (chat default) | $3-15 | ~5-15 s | found bug correctly |
| **Local Qwen 7B** | **$0** | ~25-30 s warm | found bug correctly (1-shot) |

A 100 audits/noche × 30 días la diferencia paga la electricidad de la
GPU corriendo 24/7 (~$3-5/mes). Para daemon-mode operations este
cálculo es decisivo.

**Single audits ad-hoc** (operator-driven, no daemon): la diferencia
de costo es despreciable; usar el mejor modelo para la tarea.

---

## Regla #8 — Hallucination triage (empirical, ~5 sessions sample)

Pequeño sample en este hardware vs DeepSeek (~30 sessions baseline):

- **Qwen 7B sobre primitive-recognition tasks**: ~75-80% accuracy.
  Detecta race conditions clásicas, missing locks, off-by-one, error
  swallowing. No detecta clases sutiles (TOCTOU sin tag verification,
  AEAD nonce reuse, cipher state corruption sin trace).
- **Qwen 7B sobre architecture decisions**: bias hacia respuestas
  "mainstream" — buena cuando estás auditando código común, mala
  cuando el código tiene una decisión deliberada non-obvious. Verificar
  con DS antes de rollback.
- **Triage time budget recomendado**: 20% del audit time para validar
  hallazgos (vs 30% recomendado para DS). Razón: Qwen 7B alucina
  menos cuando la respuesta es corta y específica; alucina más en
  multi-turn deep review.

---

## Regla #9 — Smoke-test antes de bulk

Cuando vayas a hacer >5 calls similares (refactor fan-out, audit batch):

1. Una call de prueba con un input representativo
2. Verificar que el response shape es lo que esperás
3. Si OK, escalar al batch

Razón: Qwen 7B puede entregar respuestas inesperadamente largas si el
prompt es ambiguo; mejor verificar en 1 call que descubrir 100 calls
con `max_tokens` cap a la mitad de la respuesta.

---

## Regla #10 — Operator-routing template

Para daemon-mode autónomo o /loop largos, prefijo recomendado en el
agent prompt:

```text
You have two LLM tools:

  · neo_local_llm — local Qwen 7B, $0/call, 25-30s warm, ~75-80%
    accuracy. Use for: refactor sketches, mechanical fan-out, daemon
    triage, yes/no decisions, translation, summarization.

  · deepseek_call — DeepSeek frontier, $0.005-0.05/call, 5-30s,
    frontier accuracy. Use for: SEV >= 9 audits, new crypto/auth/
    storage primitives, architectural decisions, anything that
    becomes ground truth.

Default to local. Escalate to DeepSeek explicitly when criteria match.
Never silently use the more expensive option.
```

Codified en directiva #53 [LOCAL-LLM-ROUTING].

---

## Cross-checks rápidos

```bash
# ¿Tool registrado y reachable?
curl -s http://127.0.0.1:9000/api/v1/plugins | python3 -c '
import sys,json
print([p["name"] for p in json.load(sys.stdin).get("plugins",[])])'

# ¿Default model configurado?
grep "local_model" neo.yaml

# ¿Modelo cargado en Ollama?
curl -s http://127.0.0.1:11434/api/tags | python3 -c '
import sys,json
[print(m["name"]) for m in json.load(sys.stdin).get("models",[])]'

# ¿Latency baseline OK? (warm-cache trivial reply)
time curl -s -X POST http://127.0.0.1:11434/api/generate \
  -H "Content-Type: application/json" \
  -d '{"model":"qwen2.5-coder:7b","prompt":"reply OK","stream":false,"options":{"num_predict":4}}'
```

---

## Self-Audit V2 obligatorio

Al cerrar épica/bloque que use `neo_local_llm`:

**Parte 1 — Coverage:** ¿qué prompts fueron a local vs DS?

| Prompt class | Local | DS | Razón |
|---|---|---|---|
| Audit code change | N | M | new crypto → DS |
| Generate test | N | 0 | mechanical → local |

**Parte 2 — Quality:** ¿hallazgos de local fueron verificables? ¿Algún
hallazgo de local fue false-positive y costó tiempo? Documentar para
calibrar futuras decisiones.

---

## Referencias rápidas

- Tool: [`cmd/neo-mcp/tool_local_llm.go`](../../../cmd/neo-mcp/tool_local_llm.go)
- ADR: [`docs/adr/ADR-013-local-llm-tool.md`](../../../docs/adr/ADR-013-local-llm-tool.md)
- Tests: [`cmd/neo-mcp/tool_local_llm_test.go`](../../../cmd/neo-mcp/tool_local_llm_test.go)
- Directives: [`.claude/rules/neo-synced-directives.md`](../../rules/neo-synced-directives.md) #53, #54
- Skill hermano (DS): [`.claude/skills/deepseek-workflow/SKILL.md`](../deepseek-workflow/SKILL.md)
- Skill hermano (Jira/GH): [`.claude/skills/jira-workflow/SKILL.md`](../jira-workflow/SKILL.md), [`github-workflow/SKILL.md`](../github-workflow/SKILL.md)
