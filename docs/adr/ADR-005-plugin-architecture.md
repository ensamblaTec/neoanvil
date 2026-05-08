# ADR-005: Sistema de Extensibilidad y Arquitectura de Plugins

- **Fecha:** 2026-04-28
- **Estado:** Aceptado
- **Pilar:** XXIII
- **Supersedes:** —
- **Superseded by:** —

## Contexto

Nexus requiere integrarse con herramientas de terceros (Jira, APIs externas,
DeepSeek) para orquestar flujos de trabajo corporativos. Introducir esta
lógica directamente en el núcleo (`cmd/neo-mcp`) infla el binario, acopla
dominios ajenos al SRE y pone en riesgo la estabilidad del demonio principal.
Se propuso inicialmente un modelo de plugins nativos en Go (in-process), el
cual fue auditado y rechazado por fallas críticas de seguridad y aislamiento.

## Opciones consideradas

### 1. Go-native In-Process Plugins (`plugin` package)

- **Pros:** baja latencia, fácil de distribuir en un binario.
- **Contras:** **rechazado**.
  - Mismo espacio de memoria (heap). Un panic en el plugin de Jira tumba
    todo el demonio de Neo.
  - Cero sandboxing.
  - Rompe la frontera de confianza — el plugin puede leer llaves en memoria,
    BoltDB, embeddings, sesión activa.

### 2. WASM Forge (`neo_forge_tool`)

- **Pros:** sandboxing basado en capacidades, compilación en caliente.
- **Contras:** limitaciones actuales en networking complejo y manejo de
  dependencias pesadas (ej. SDKs de OAuth). Se mantiene exclusivamente
  para scripts y tools efímeras/locales.

### 3. Subprocess MCP (estándar adoptado)

- **Pros:**
  - Aislamiento real a nivel de sistema operativo.
  - Agnóstico al lenguaje (futuros plugins en Python, TS, Rust).
  - Reutiliza la infraestructura que Neo ya tiene: dispatcher, watchdog,
    health-checks, OAuth proxy.
- **Contras:** ligero overhead de comunicación (JSON-RPC sobre stdio/IPC),
  irrelevante para llamadas a APIs REST externas.

## Decisión

Adoptamos **Subprocess MCP + Manifest Declarativo** para todas las
integraciones pesadas y externas.

- Las herramientas core de SRE (Radar, Certify, AST, Memory, Cache, Command,
  Daemon, Chaos, Compress, Migration, Forge, Download-Model, Log-Analyzer,
  Tool-Stats, Debt) **se quedan en el núcleo** `cmd/neo-mcp`. No se extraen.
- Los plugins se declaran en `~/.neo/plugins.yaml`.
- El demonio Nexus levanta los plugins como procesos hijos y se comunica con
  ellos usando el estándar MCP estricto.
- El manejo de secretos se delega a `pkg/auth/keystore.go` extendido con
  `99designs/keyring`. Los secretos se inyectan como variables de entorno
  al levantar el subproceso del plugin (mismo patrón que `extra_env` ya usa
  para `OLLAMA_EMBED_HOST`).
- WASM Forge se mantiene en paralelo para tools triviales/locales.

## Consecuencias

### Positivas

- **Seguridad sólida:** si Atlassian cambia su API y el plugin de Jira entra
  en un bucle infinito de fallos, el subproceso muere, el watchdog de Nexus
  lo detecta, y el resto del sistema (AST, CodeRank, Radar) sigue operando
  al 100%.
- **Namespacing:** Nexus actúa como API gateway, prefijando automáticamente
  las tools de los subprocesos (ej. `jira/get_context`) para evitar la
  colisión de nombres y la inundación del contexto de Claude.
- **Reutilización:** cero protocolo nuevo; el dispatcher, OAuth proxy,
  watchdog y health-check ya escritos absorben los plugins sin código nuevo.
- **Hot-reload natural:** restart del subproceso sin tocar neo-mcp.
- **Tier ownership:** plugin manifest declara `tier: workspace|project|nexus`
  alineándose con el modelo 4-tier ya operacional.

### Negativas

- Cada plugin es un binario adicional para construir, distribuir y firmar.
- Latencia JSON-RPC ~1-3 ms por call (negligible vs latencia de red Atlassian).
- El manifest agrega una nueva fuente de configuración a mantener.

## Open Questions (follow-ups)

Estos puntos no bloquean la decisión central pero requieren ADRs o
secciones explícitas antes de cerrar el PILAR XXIII.

1. **Auth flow Jira (ADR-006 candidato):** OAuth 2.0 (3LO) vs Personal Access
   Token. Atlassian forzó expiry obligatorio en PATs en 2024. La decisión
   afecta el UX de `neo login` y el flujo de refresh.
2. **Webhooks bidireccionales:** la propuesta original prometía orquestación
   bidireccional. Este ADR cubre solo outbound (Claude → Jira). Definir si
   webhooks (Jira → Nexus → sesión Claude) entran en PILAR XXIII o en un
   pilar posterior.
3. **Plugin manifest versioning:** `plugins.yaml` debe declarar
   `manifest_version: 1` desde el día uno. Establecer la política de
   migración cuando el schema evolucione.
4. **Audit log inmutable:** mutaciones via plugins (transition tickets,
   modificar issues) requieren append-only log. Decidir si reusar
   `session_state` BoltDB o archivo separado `~/.neo/audit.log` con
   hash-chain.
5. **Lifecycle estricto:** MCP cubre handshake/init y health-check (Nexus
   ya hace polling `/health`). Confirmar que el spec exige también shutdown
   gracioso ante SIGTERM (mismo patrón que children — Épica 229.1).

## Referencias

- PILAR XXXIII (Auth Foundation + Multi-Tenant BoltDB) — base de
  `pkg/auth/keystore.go`.
- Épica 229.1/2/3 — SIGTERM gracioso, OAuth proxy, strip-prefix en Nexus.
- `docs/tier-ownership.md` — modelo 4-tier que el plugin manifest debe
  respetar.
- HashiCorp `go-plugin` — alternativa subprocess+gRPC considerada como
  prior art.
- Anthropic MCP gateway pattern — multi-server aggregation.
