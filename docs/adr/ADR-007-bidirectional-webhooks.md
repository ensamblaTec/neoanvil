# ADR-007: Bidirectional Webhooks for Plugins

- **Fecha:** 2026-04-28
- **Estado:** Aceptado (decisión: **diferir**)
- **Pilar:** XXIII (Épica 126.6 / cierra 125.7)
- **Supersedes:** —
- **Superseded by:** —

## Contexto

ADR-005 estableció que los plugins son MCP servers subprocess. La pregunta
abierta de Épica 125.7 fue si añadir un canal **inverso**: que el provider
externo (Jira, GitHub) envíe webhooks a Nexus, y Nexus los empuje al agente
Claude vía SSE proactivamente.

Caso de uso teórico: el agente está investigando ENG-42; otro humano
comenta el ticket; el agente recibe el comentario sin tener que volver
a llamar `jira/get_context`.

## Opciones consideradas

### 1. Receiver completo + delivery SSE proactivo

- **Pros:** UX premium — agente "siempre actualizado".
- **Contras:**
  - **Requiere endpoint HTTPS público.** Atlassian llama desde internet,
    no desde localhost. Devs sin túnel (ngrok/cloudflare-tunnel) no pueden
    usarlo en local.
  - **Verificación de firma HMAC** + secret rotation per-instance.
  - **Filtrado** complejo — Jira manda webhooks por cada cambio de
    cualquier campo de cualquier issue del project. Sin filtros agresivos,
    el agente se ahoga en noise.
  - **Mapeo session→subscription** — qué session(s) reciben qué eventos.
    Para neoanvil con multi-workspace + active space concept, son varios ejes.
  - **Replay/dedup** — webhooks llegan más de una vez bajo failure; agente
    no puede procesar duplicados sin idempotency.
  - **Coste de mantenimiento** alto (~500-800 LOC + tests integración con
    httptest emulando Atlassian).

### 2. Receiver-only (event log persistente, polling al consumir)

- **Pros:** menos complejidad — Nexus recibe + persiste a archivo
  (`~/.neo/events-jira.log` JSONL). Tool nuevo `jira/recent_events` lo lee.
- **Contras:**
  - Aún requiere HTTPS público
  - El polling sigue siendo polling — `get_context` actual ya cubre el
    caso "necesito el estado actual"
  - Solo aporta cuando se quieren **eventos del pasado** (ej. "¿qué cambió
    hoy?") — caso lateral

### 3. **Diferir** (opción adoptada)

No implementar webhooks ahora. Cerrar 125.7 + 126.6 con justificación
documentada. Reabrir cuando exista demanda concreta + uno de estos
triggers:

- 3+ usuarios pidiendo proactive events específicamente
- Existe ya un caso de uso de equipo (no solo single-developer)
- Túnel HTTPS automatizado (Cloudflare Tunnel via `cloudflared`) parte
  del operator flow estándar

## Decisión

**Diferir webhooks bidireccionales.** El polling vía `jira/get_context`
satisface el caso de uso primario (agente quiere estado actual de un
ticket cuando lo pide). El coste de implementación (~700 LOC + setup
HTTPS público + filtros + mapeo session↔subscription) supera el beneficio
para el modo de uso conversacional típico de Claude Code en local.

**125.7 cierra como "out of scope"**, ligado a este ADR.

## Consecuencias

### Positivas

- Mantiene la superficie de ataque pequeña — sin endpoint público
  expuesto.
- No requiere setup de túnel HTTPS para devs locales (ngrok, Cloudflare
  Tunnel, etc.).
- Plugin Jira queda enfocado en lectura/escritura on-demand, contrato simple.
- Cero dependencia operacional: agente funciona idéntico con o sin
  conectividad inbound.

### Negativas

- Sin notificación proactiva — agente debe pedir explícitamente
  `jira/get_context` para ver cambios.
- Para queries tipo "¿qué cambió hoy?" no hay alternativa nativa hoy
  (operador puede mirar la UI Jira, pero el agente no tiene visibilidad).

## Triggers para reabrir

Revisitar este ADR (escribir ADR-007.B u ADR-008) cuando:

1. **Demanda concreta documentada** en master_plan o issues — al menos
   3 referencias independientes a "necesito eventos proactivos".
2. **Modo team/server** se vuelve un caso de uso real (vs. el actual
   single-developer Claude Code local), donde varios humanos comparten
   un workspace y el agente necesita reaccionar a sus acciones.
3. **HTTPS público trivializado** — túnel automatizado en el operator
   flow, ya sea Cloudflare Tunnel zero-config o equivalente.

## Referencias

- ADR-005 — Plugin architecture (subprocess MCP).
- ADR-006 — Jira auth flow (API token vs OAuth 3LO).
- `cmd/plugin-jira/` — implementación actual con tools `get_context` +
  `transition`, suficiente para el caso de uso polling-driven.
- [Atlassian webhooks documentation](https://developer.atlassian.com/cloud/jira/platform/webhooks/) —
  base técnica si se reabre.
