# ADR-013: Local LLM tool — `neo_local_llm` complement to plugin-deepseek

- **Fecha:** 2026-05-10
- **Estado:** Aceptado (initial implementation shipped)
- **Hereda de:** [ADR-005](./ADR-005-plugin-architecture.md) (subprocess MCP pattern), [ADR-012](./ADR-012-deepseek-plugin-design.md) (DeepSeek plugin)

## Contexto

`plugin-deepseek` cubre el caso "frontier quality": $0.04-0.10/sesión y
~25% de hallucination en SEV ≥ 9 (per directive #54
`[SRE-NEW-CODE-AUDIT-MANDATORY]`). Funciona, pero tiene tres limitaciones
operacionales:

1. **Costo no-cero a escala**: una sesión de daemon-mode auditando 50
   archivos cuesta $0.50-2.50; una semana de daemon nocturno = $20+.
2. **Disponibilidad dependiente de la red**: cualquier operación
   offline (vuelo, tren, blackout regional) deja al daemon sin LLM.
3. **Latencia inherente**: ~5-15s por audit incluso para tareas
   triviales (boilerplate, refactor mecánico) donde DS-pro es overkill.

El operador ya tiene una RTX 3090 con 24 GB VRAM corriendo y la auditoría
post-Option-B mostró que la GPU está al 9-28% durante operación normal.
Hay capacidad espare para correr un LLM local capaz.

## Decisión

Añadir `neo_local_llm` como **15ª tool MCP** (`cmd/neo-mcp/tool_local_llm.go`).
Reusa `pkg/llm.Client` (ya existe desde SRE-95.A.1) que habla a Ollama
`/api/generate`. NO reemplaza al plugin-deepseek — lo complementa.

**Default model:** `qwen2.5-coder:7b` (4.5 GB).

## Decisiones específicas

### 1. Modelo default — 7B en vez de 32B

#### Opciones consideradas

| Modelo | Tamaño | RAM real medida (Ollama) | Quality vs DS-pro | Speed |
|--------|--------|--------------------------|-------------------|-------|
| `qwen2.5-coder:32b` | 18.9 GB | **44.5 GB** (sistema) | ~85% | ~30s/audit |
| `qwen2.5-coder:7b` (✅ default) | 4.5 GB | ~7 GB | ~75% | 5-30s/audit |
| `qwen2:0.5b` | 336 MB | <1 GB | ~30% | <1s |

Medido empíricamente en este hardware (RTX 3090 24 GB VRAM, 32 GB RAM):

```
qwen2.5-coder:32b → Ollama error "model requires more system memory
                    (44.5 GiB) than is available (41.9 GiB)"
qwen2.5-coder:7b  → 25-32s por audit completo (16 tok/s sostenido,
                    487-512 tokens output, encontró el bug real
                    en el test prompt)
qwen2:0.5b        → 0.16s por respuesta (500-1000 tok/s)
```

**Decisión:** 7B como default. 32B no funciona en hardware 32 GB; la 0.5B
es útil pero no para audits sustanciales. Operators con ≥64 GB pueden
override per-call vía `args["model"]`.

#### Por qué este enfoque vs hosted vLLM/llama.cpp

| Alternativa | Pros | Contras |
|---|---|---|
| **Ollama existing instance** (✅ adoptado) | Ya está corriendo en :11434 (Option B la usa para embeddings); cero infra nueva; quick path to value | Modelo lockeado al runtime de Ollama (sin tensor parallelism multi-GPU) |
| vLLM sidecar | Mejor batching, paged attention, mucho mejor throughput a >10 reqs concurrentes | Otra dependencia; configurar GPU; el caso de uso real es 1-2 reqs concurrentes |
| llama.cpp server | Más eficiente memory, soporta más cuantizaciones | Sin HTTP API standard; integración custom |

Se rechazaron vLLM/llama.cpp en este punto porque (a) Ollama ya está,
(b) el caso de uso es low-concurrent (1-2 reqs/s), (c) podemos migrar
sin cambiar la interface MCP cuando la carga lo justifique.

### 2. Routing — local vs DeepSeek

El tool **NO incluye routing** automático. La decisión de qué prompt va
a `neo_local_llm` vs `deepseek_call` vive en el agent prompt (Claude
Code, daemon iterativo, etc.). Razones:

- Cada caller tiene contexto del task que ningún router server-side
  puede aproximar bien (e.g. "este audit ya falló en local → escala a DS",
  "este task es nuevo y crítico → DS first").
- Add server-side routing means we'd need a model classifier (more LLM
  calls, more latency), and getting it wrong = silent quality drops.

#### Heurística recomendada (para incluir en SKILL.md siguiente sesión)

```
Local (neo_local_llm)              | DeepSeek (deepseek_call)
-----------------------------------|---------------------------------
Boilerplate generation             | New crypto/auth/storage primitives
Refactor sketches                  | SEV ≥ 9 security audits
Mechanical fan-out (rename, etc)   | Architectural decisions
Daemon-mode triage                 | Hallucination-triage iterations
"Is this a real issue?" yes/no     | Multi-turn deep-review threads
Translation, summarization         | Anything that becomes ground truth
```

### 3. Quality calibration — ¿el 7B es suficiente?

Sample empírico de la implementación (audit prompt en el ADR de prueba):

> Code: SessionStore con Get sin lock, mu solo en Put.
> Qwen 7B: "Potential Race Condition in Get Method (Line 10): The Get method
> does not lock the mu mutex before accessing s.sessions[id]..."

**Encontró el bug real en el primer attempt.** Una decisión basada en una
sola muestra es débil, pero suficiente para shipping con caveat:

- 7B se usa para hallazgos preliminares + tareas mecánicas
- Un follow-up audit con DeepSeek es necesario para shipping de cambios sensibles
- La doctrina existente de directive #54 (`[SRE-NEW-CODE-AUDIT-MANDATORY]`)
  sigue válida: nuevas primitivas sensibles requieren DS-pro audit antes de
  marcar "done"

### 4. Cost model

| Workload | DeepSeek API | Local 7B | Notes |
|----------|-------------:|---------:|-------|
| 1 audit (~500 token output) | $0.001-0.005 | $0 (electricidad ~$0.0001) | Local 2× más lento pero gratis |
| 10 audits/día (week) | $0.07-0.35 | $0 | Diferencia despreciable |
| Daemon mode 100 audits/noche (mes) | $3-15 | $0 | **Win operacional real** |
| Audit batch 10 archivos paralelos | $0.10 + spike de cache | $0 + serial 5min | Use DS para batch grande |

## Implementación

```go
// cmd/neo-mcp/tool_local_llm.go (~120 LOC)
type LocalLLMTool struct {
    defaultModel string  // qwen2.5-coder:7b
    baseURL      string  // cfg.AI.BaseURL
}

func (t *LocalLLMTool) Execute(ctx, args) (any, error) {
    // 1. Validate prompt (non-empty)
    // 2. Resolve model: args["model"] OR defaultModel
    // 3. Optional system prompt prefix: "SYSTEM:\n... USER:\n..."
    // 4. Call llm.Client.Generate via SafeOperatorHTTPClient
    // 5. Return {response, model, latency_ms, prompt_chars, response_chars}
}
```

Registered in `cmd/neo-mcp/main.go` after the `neo_debt` registration:

```go
mustRegister(NewLocalLLMTool(cfg.AI.BaseURL, ""))
```

## Consecuencias

- ✅ Daemon mode puede correr offline / sin costo
- ✅ Trivial / mecánico / refactor work no quema budget DS
- ✅ Setup operator: cero (Ollama ya corre)
- ✅ 6 unit tests con httptest mock
- ⚠️ Quality default es 7B — operadores con más RAM deberían usar 32B vía override
- ⚠️ Routing es responsabilidad del caller — sin guía de skill, el agente
  tiende a usar lo que ya conoce (DS). Follow-up: añadir
  `.claude/skills/local-llm-workflow/SKILL.md` con routing rules.
- ⚠️ No hay sidecar dedicado — si Ollama crashea bajo carga embed+chat
  combinada, ambos fallan. Mitigation: el embed instance dedicado
  (:11435) para embeddings y :11434 para chat ya separa estos workloads.
  Si combinamos en :11434, el operator debe redimensionar
  `ollama_concurrency`.

## Validación

- `go test -race -short ./cmd/neo-mcp/ -run TestLocalLLMTool` — 6/6 pass
- `make audit-ci` — 0 NEW findings vs baseline
- Live test contra Qwen 7B real: 25-32s/audit, 16 tok/s, hallazgo correcto

## Follow-ups (siguiente sesión)

1. `cfg.AI.LocalModel` config field para evitar pasar el override por
   tool call.
2. Skill `/local-llm-workflow` con routing heuristics + ejemplos de prompts.
3. Live bench harness (similar a embed_bench_live_test.go) para
   capturar regresiones de latencia/quality cuando se actualice Ollama
   o el modelo.
4. `neo_local_llm` como `--turbo` flag en daemon mode: cuando habilitado,
   todas las decisiones triage van local primero, escalación manual a DS.

## Referencias

- [ADR-005](./ADR-005-plugin-architecture.md) — subprocess MCP pattern
- [ADR-012](./ADR-012-deepseek-plugin-design.md) — DeepSeek plugin design
- `pkg/llm/client.go` — pre-existing Ollama client (SRE-95.A.1)
- `cmd/neo-mcp/tool_local_llm.go` — tool implementation
- `cmd/neo-mcp/tool_local_llm_test.go` — unit tests (httptest mock)
- `[DEEPSEEK-MODEL-DEFAULT]`, `[SRE-NEW-CODE-AUDIT-MANDATORY]` — DS doctrine context
