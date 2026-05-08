# ADR-009: Trust scoring del daemon iterativo (PILAR XXVII)

- **Fecha:** 2026-04-30
- **Estado:** Aceptado
- **Pilar:** XXVII (Daemon iterativo MCP-driven)
- **Supersedes:** —
- **Superseded by:** —

## Contexto

El daemon V2 (PILAR XXV) podía pull-loop tareas y ejecutarlas, pero
sin memoria de calidad por (pattern, scope). Cada tarea era un evento
aislado: el operador aprobaba o rechazaba a ojo y la próxima ejecución
de la misma pattern no se beneficiaba de ese juicio.

Para escalar a un loop semi-autónomo donde el daemon sugiera
`auto-approve` cuando confía en la combinación (pattern, scope), hace
falta un sistema de scoring que:

1. **Acumule evidencia** por bucket (pattern, scope) sin penalizar
   modelos por bugs operacionales (un MkdirAll faltante no es culpa
   del LLM).
2. **Cuantifique incertidumbre** — un pattern con 5 successes no
   merece la misma confianza que uno con 500.
3. **Decaiga** evidencia antigua para que un pattern que dejó de
   ejercitarse no acumule trust indefinidamente.
4. **Sea barato** computacionalmente — el daemon corre en hot-path,
   no podemos darle un modelo bayesiano de 5MB de pesos.
5. **Sea legible** para que el operador entienda por qué el daemon
   tomó la decisión que tomó.

## Decisión

**Beta(α, β) por bucket, con uniform prior (1, 1), lazy decay 0.99/h
sobre la evidencia acumulada (no sobre el prior), tiers L0..L3 según
LowerBound 95% conf, y un gate explícito de min 50 ejecuciones antes
de promote.**

Concretamente:

```
TrustScore {
  Pattern         string         // ej: "refactor"
  Scope           string         // ej: ".go:pkg/state"
  Alpha           float64        // successes + 1 (prior)
  Beta            float64        // failures + 1 (prior)
  LastUpdate      time.Time      // para lazy decay on read
  TotalExecutions int            // crudo, sin prior
  CurrentTier     Tier           // cache denormalizada de TierFor(...)
  ConsecutiveFailures int        // demote-on-3-streak
  ManualWarmup    bool           // operator override audit trail
}

EffectiveAlphaBeta(now) := factor=0.99^hoursSince
                          (1 + (α-1)*factor, 1 + (β-1)*factor)
PointEstimate(now)      := α_eff / (α_eff + β_eff)
LowerBound(now)         := mean − 1.96 * sqrt(α·β / ((α+β)²·(α+β+1)))
                          clamped to [0, 1]

TierFor(score, now):
  if TotalExecutions < 50:        return L0
  if LB ≥ 0.95:                   return L3
  if LB ≥ 0.85:                   return L2
  if LB ≥ 0.65:                   return L1
                                  return L0
```

Categorías de outcome con pesos diferenciados:

```
RecordOutcome(category):
  Success           → α += 1, streak reset
  Infra             → no-op (model not at fault)
  SubOptimal        → β += 0.5, streak++
  Quality           → β += 1,   streak++
  OperatorOverride  → β += 5,   streak++   (operator reject = strong)

  if streak >= 3:    CurrentTier = L0      (policy override math)
  else:              CurrentTier = TierFor(score, now)
```

## Opciones consideradas

### 1. Beta(α, β) — elegida

- **Pros:**
  - Conjugate prior natural para Bernoulli (cada tarea pasa-o-no-pasa
    audit, perfectamente Bernoulli).
  - Lower bound bayesiano (normal-approximation) penaliza
    automáticamente low-evidence — un pattern con 5/5 successes
    aún tiene LB bajo por la varianza, mantiene tier conservador
    sin lógica adicional.
  - Update O(1) por outcome, decay O(1) on read. Cabe en BoltDB
    como struct simple.
  - Operador entiende α/β intuitivamente: "successes vs failures,
    más prior".

- **Contras:**
  - Aproximación normal pierde precisión en α/β extremos (ver
    deuda TRUST-MATH-001). Mitigado por el gate de 50 execs.
  - No captura correlaciones entre patterns (pattern A correlaciona
    con pattern B). Para nuestro caso no aplica — buckets
    independientes por diseño.

### 2. Thompson Sampling

- **Pros:** óptimo en bandit problems con exploración. Maneja la
  incertidumbre nativamente (sample del posterior, no point estimate).
- **Contras:**
  - Decisión estocástica — el operador vería distintos suggested_action
    para inputs idénticos. Difícil de auditar y reproducir.
  - Beneficio principal (exploration vs exploitation) no aplica:
    el daemon NO está explorando un universo de posibles acciones,
    está pasando o rechazando una mutación específica. Cada pattern
    es una rama discreta, no un brazo de bandit.
  - Implementación significativamente más cara — sample del posterior
    Beta requiere generación de números aleatorios y branch tree
    sobre acciones. Beta(α, β) cierra en una fórmula.

### 3. Logistic regression con features

- **Pros:** captura correlaciones (file_size, complexity, time_of_day...).
- **Contras:**
  - Requiere training data labelled antes de funcionar — chicken-egg
    para un sistema que arranca sin data.
  - Pesos del modelo viven en disco — ahora hay que serializarlos,
    versionarlos, migrarlos. Beta(α, β) son 2 floats.
  - Caja negra para el operador. Difícil explicar "por qué este
    pattern subió a L2" cuando el modelo da una salida ofuscada.

### 4. Simple counter (success_rate = α / (α + β))

- **Pros:** trivial.
- **Contras:**
  - No cuantifica incertidumbre. 5/5 success da rate=1.0 → auto-aprueba
    en pattern fresca. **Inseguro.** Beta(α, β) con LB resuelve esto
    naturalmente.

## Justificación del decay 0.99/h

- **Half-life ≈ 69h** (`0.99^69 ≈ 0.5`) sobre evidencia acumulada.
- Patterns que se ejercitan diariamente mantienen su trust intacto.
- Patterns idle 1 mes → factor `0.99^720 ≈ 0.0007` → la evidencia se
  evapora a casi-prior. Esto es deseable: si no has visto el pattern
  en un mes, el modelo o tu codebase pueden haber cambiado lo
  suficiente como para que el viejo trust no aplique.
- **Prior NO decae.** Un pattern infinitamente idle vuelve a (1, 1)
  pero nunca por debajo. Esto preserva la base bayesiana correcta y
  evita estados degenerados donde α<1 o β<1 producen LB inválidos.

## Justificación del gate de 50 ejecuciones

- Bayesian variance ya penaliza low-evidence (ver `TestLowerBound_PenalizesLowEvidence`).
- Pero la variance es continua — un pattern con 8 successes 0 fails
  podría dar LB ≈ 0.65 y entrar a L1 técnicamente.
- **Política operacional:** auto-approval requiere "el sistema te ha
  visto trabajar al menos 50 veces". Es un floor explícito que evita
  que un pattern lucky entre temprano a auto-approval.
- 50 viene de un cálculo simple: con success_rate=0.85, después de
  50 cycles la varianza posterior es lo suficientemente baja para que
  el LB se asiente cerca del point estimate. Antes de eso, demasiada
  variance.

## Justificación de las 5 categorías de outcome

Diseño descubierto durante 138.A.1 hands-on (ver memoria
`feedback_self_audit`):

- **Success** — el modelo entregó código que pasó audit. α += 1.
- **Infra** — el modelo entregó código pero hubo un fallo
  operacional (HTTP error, plugin crash, MkdirAll missing, network).
  El modelo no es responsable. **No-op** — ni α ni β cambian, y la
  streak de fallos consecutivos NO se incrementa (un Infra entre dos
  Quality fails NO debe resetear el counter).
- **SubOptimal** — output correcto pero ruidoso/redundante
  (ej: distill_payload over-chunking en archivo <200 LOC produciendo
  5× la misma respuesta). β += 0.5 — penalty reducido porque el
  output sigue siendo usable.
- **Quality** — output incorrecto o hallucinated. β += 1 — penalty
  estándar.
- **OperatorOverride** — el operador rechazó explícitamente con
  reason_kind=quality. β += 5 — señal fuerte de "no quiero ver este
  patrón otra vez sin revisión cuidadosa". El factor 5 viene de:
  un OperatorOverride debería borrar la trust accumulada por
  ~5 successes, equivalente a "vuelve a empezar".

Demote-on-3 (consecutive non-Infra non-Success outcomes → L0
inmediato) es policy override sobre statistics. Aunque la matemática
diga que LB sigue alto, 3 fails seguidos es señal de drift —
mejor exigir review hasta que el operador estabilice el pattern.

## Consecuencias

### Positivas

- Daemon puede operar semi-autónomo sin requerir presencia del
  operador para cada tarea (cuando los trust scores se asienten).
- Auditoría completa: cada decisión `auto-approve` es trazable a
  un (pattern, scope, α, β, tier, LB) verificable.
- Bucket BoltDB ~200 bytes por entrada — escala a >100K patterns sin
  presión de memoria.
- Robusto a falsos positivos del modelo (Infra no contamina) y a
  decisión human-in-the-loop (OperatorOverride pesa 5×).

### Negativas / deudas registradas

- **Atomicidad de approve/reject:** TrustRecord + UpdateDaemonResult
  son 4 BoltDB transactions secuenciales. Race window teórica entre
  goroutines simultáneas (VULN-001 + APPROVE-RACE-WINDOW). Mitigación
  pendiente para cuando daemon corra con concurrent workers.
- **Aproximación normal:** `LowerBound` tiene precisión limitada en
  α/β extremos (TRUST-MATH-001). Acceptable porque el gate de 50
  execs nos mantiene en el rango donde la aproximación es buena.
- **No hay trust_history append-only:** la skill `/daemon-trust` no
  puede mostrar tier transitions cronológicos sin escanear
  daemon_results sorted by completed_at, que es caro. Sub-feature
  pendiente.
- **138.E pendiente:** Pair-mode no genera datos de trust hasta que
  138.E (Pair feedback loop) implemente el infer-from-edits hook.
  Hasta entonces, los trust scores son sintéticos y no representan
  comportamiento real. Bloquea la activación segura de daemon mode
  con tráfico real.

## Validación

Tests E2E en `cmd/neo-mcp/daemon_e2e_test.go` validan:

- Trust ascent monotónico con repeated Success (α[i] = α[i-1] + 1).
- 50/50 success/quality mantiene tier en L0 incluso con abundante
  evidencia (60 cycles).
- Decay correcto: pattern idle 1 año vuelve a casi-prior (LB ≈ 0.5).
- Demote-on-3 fuerza L0 desde L2 en exactamente 3 cycles.
- Recovery: Success post-demote resetea streak; tier recomputa
  según LB normal.
- Concurrency: 100 goroutines updating la misma (pattern, scope) sin
  lost updates.

## Referencias

- [PILAR XXVII master_plan section](../../.neo/master_plan.md)
- [Operator runbook](../pilar-xxvii-daemon-mcp.md)
- `pkg/state/daemon_trust.go` — implementación
- `pkg/state/daemon_trust_test.go` — 42 tests
- Beta distribution variance: $\sigma^2 = \alpha\beta / ((\alpha+\beta)^2 \cdot (\alpha+\beta+1))$
- Normal approximation lower bound: $\mu - 1.96\sigma$
