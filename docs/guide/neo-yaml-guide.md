# Guía de Configuración: neo.yaml (Ouroboros V10.5)

NeoAnvil busca `neo.yaml` con resolución recursiva desde el directorio de trabajo hacia la raíz.
Si el archivo no existe al arrancar, se genera automáticamente desde `neo.yaml.example` (si existe)
o desde los defaults del código. Los campos desconocidos son ignorados silenciosamente.

---

## Secciones

### `server` — Transporte y Puertos

| Campo | Default | Descripción |
|-------|---------|-------------|
| `log_level` | `"info"` | Verbosidad: `debug`, `info`, `warn`, `fatal` |
| `transport` | `"stdio"` | Puente MCP: `"stdio"` (proceso hijo) o `"sse"` (HTTP independiente) |
| `host` | `"127.0.0.1"` | Interfaz de red. Usar `0.0.0.0` solo en LAN controlada |
| `port` | `8080` | Puerto HUD legacy (reemplazado por `dashboard_port`) |
| `sandbox_port` | `8081` | Servidor de ingesta mTLS |
| `sse_port` | `8085` | Transporte SSE MCP (`GET /mcp/sse`, `POST /mcp/message`) |
| `sse_path` | `"/mcp/sse"` | Ruta GET del endpoint SSE |
| `sse_message_path` | `"/mcp/message"` | Ruta POST de mensajes MCP |
| `dashboard_port` | `8087` | **Operator HUD** (SPA embebida, restringido a localhost) |
| `mode` | `"pair"` | `pair` / `fast` / `daemon`. Ver tabla de modos abajo |
| `gossip_peers` | `[]` | IPs Tailscale para Gossip P2P. Vacío = desactivado |
| `gossip_port` | `8086` | Puerto TCP del listener Gossip (debe coincidir en todos los peers) |
| `tailscale` | `false` | Activar túneles WireGuard vía Tailscale |

**Modos de operación:**

| Modo | Certificación | `neo_daemon` | Ideal para |
|------|--------------|--------------|------------|
| `pair` | AST + Bouncer + Tests | PROHIBIDO | Desarrollo diario con Claude |
| `fast` | Solo AST + Index | PROHIBIDO | POC / iteración rápida |
| `daemon` | Completa | Habilitado | Automatización nocturna |

---

### `workspace` — Indexado y Monorepo

| Campo | Default | Descripción |
|-------|---------|-------------|
| `ignore_dirs` | `[node_modules, vendor, .git, dist, .neo]` | Dirs ignorados por el watcher y RAG |
| `allowed_extensions` | `[.go, .ts, .js, .py, .md, .rs, .html, .css, .yaml]` | Extensiones indexadas en HNSW |
| `max_file_size_mb` | `5` | Límite OOM para lectura de archivos grandes |
| `modules` | `{web: "npm run build"}` | Mapa `subdirectorio → comando build` para el Bouncer Agnóstico |

**Ejemplo `modules` para monorepo:**
```yaml
workspace:
  modules:
    web: "npm run build"
    mobile: "expo build"
    firmware: "cargo build --release"
    ml: "python -m pytest tests/"
```

---

### `ai` — Motor de Embedding

| Campo | Default | Descripción |
|-------|---------|-------------|
| `provider` | `"ollama"` | `"ollama"`, `"openai"`, `"anthropic"`, `"wasm"` |
| `base_url` | `"http://localhost:11434"` | Endpoint Ollama (o API externa) |
| `embedding_model` | `"nomic-embed-text"` | Modelo de embeddings (dimensión debe ser 768) |
| `context_window` | `8192` | Ventana de contexto máxima |

> **Nota:** `nomic-embed-text` produce vectores de 768 dimensiones. Si cambias el modelo,
> actualiza también la dimensión en código (`embedder.Dimension()`). Los vectores ya
> almacenados en `hnsw.db` serán incompatibles — borra y re-indexa.

---

### `rag` — Grafo HNSW y Pipeline de Ingesta

| Campo | Default | Descripción |
|-------|---------|-------------|
| `db_path` | `".neo/db/hnsw.db"` | Ruta del grafo vectorial BoltDB |
| `chunk_size` | `3000` | Tamaño de fragmento de texto (bytes) |
| `overlap` | `500` | Solapamiento entre fragmentos |
| `batch_size` | `100` | Límite de inserción por batch en HNSW |
| `ingestion_workers` | `4` | Goroutines para lectura + chunking (I/O bound) |
| `ollama_concurrency` | `3` | Slots simultáneos de Embed. **No subir de 4** — Ollama tiene cola limitada |
| `max_nodes_per_workspace` | `50000` | **[SRE-35]** Límite de vectores por workspace antes de alerta |
| `workspace_capacity_warn_pct` | `0.80` | **[SRE-35]** Fracción del límite que dispara `EventMemoryCapacity` |
| `drift_threshold` | `0.45` | **[SRE-35]** Media móvil de distancia coseno que activa `EventCognitiveDrift` |
| `gc_pressure_threshold` | `5.0` | **[SRE-36]** NumGC/archivo que activa `EventGCPressure` durante ingesta |
| `arena_miss_rate_threshold` | `0.20` | **[SRE-36]** Miss-rate del pool de arenas que activa `EventArenaThresh` |

**Ajuste de `ollama_concurrency`:**
- El runner de embeddings de Ollama es independiente del de generación.
- Con `OLLAMA_NUM_PARALLEL` no configurado, Ollama permite ~2-4 embeds simultáneos.
- HTTP 500 = queue overflow. Si ves errores, reduce a 2.
- Si ves `EventArenaThresh` frecuente, el pool `f32Pool` necesita más capacidad.

---

### `inference` — Gateway de Inferencia de 4 Niveles

```
LOCAL → OLLAMA → HYBRID → CLOUD
```

| Campo | Default | Descripción |
|-------|---------|-------------|
| `cloud_token_budget_daily` | `50000` | **Hard-limit** diario de tokens para capa CLOUD. 0 = desactivado |
| `cloud_model` | `"claude-sonnet-4-6"` | Modelo externo (requiere `ANTHROPIC_API_KEY`) |
| `ollama_model` | `"qwen2:0.5b"` | Modelo local para capas OLLAMA/HYBRID |
| `ollama_base_url` | `""` | URL Ollama para inferencia. Vacío = usa `ai.base_url` |
| `confidence_threshold` | `0.70` | Confianza mínima. Si score < threshold → escalar al nivel siguiente |

> **Seguridad:** El presupuesto CLOUD se resetea a medianoche UTC. El reset es atómico
> (CAS loop sobre `atomic.Int32`). Si el proceso se reinicia, el contador vuelve a 0 —
> esto es by design: el límite protege contra bursts, no contra reinicios maliciosos.

---

### `sre` — Resiliencia y Circuit Breaker

| Campo | Default | Descripción |
|-------|---------|-------------|
| `strict_mode` | `true` | Bloquear fallos degradados |
| `cusum_target` | `0.70` | Entropía ideal del sistema (CUSUM) |
| `cusum_threshold` | `0.15` | Umbral de colapso termodinámico |
| `ring_buffer_size` | `150` | Buffer circular de telemetría |
| `shadow_compiler` | `true` | Validación npm/tsc en background |
| `auto_vacuum_interval` | `"5m"` | Frecuencia de defragmentación WAL (Go duration) |
| `gameday_baseline_rps` | `5000` | RPS base para Chaos Drill |
| `trusted_local_ports` | `[11434, 8085, 8080, 8081, 6060]` | Puertos localhost que bypasan el escudo SSRF |
| `safe_commands` | ver ejemplo | **[SRE-34]** Prefijos auto-aprobables en modo UNSUPERVISED |
| `unsupervised_max_cycles` | `10` | **[SRE-34]** Ciclos máximos sin supervisión antes de revertir |

> **Circuit Breaker del Embedder:** threshold=5, resetTimeout=30s. Si ves cascadas de
> HTTP 500 de Ollama, verifica que `ollama_concurrency ≤ 4`. El breaker se trip cuando
> acumula 5 fallos (concurrencia+2) — diseñado para tolerar bursts sin trip prematuro.

---

### `cognitive` — MCTS y Bouncer

| Campo | Default | Descripción |
|-------|---------|-------------|
| `strictness` | `0.75` | Severidad del Bouncer termodinámico (0.0–1.0) |
| `arena_size` | `100000` | Nodos máximos en RAM para el árbol MCTS |
| `xai_enabled` | `true` | Explicaciones detalladas en rechazos del Bouncer |
| `auto_approve` | `false` | Omitir validación humana en `neo_run_command` |

---

### `pki` — Certificados mTLS

| Campo | Default | Descripción |
|-------|---------|-------------|
| `ca_cert_path` | `".neo/pki/ca.crt"` | CA raíz |
| `server_cert_path` | `".neo/pki/server.crt"` | Certificado del servidor |
| `server_key_path` | `".neo/pki/server.key"` | Clave privada del servidor |

> Los certificados se generan automáticamente al arrancar si no existen.
> **NUNCA commitear `.neo/pki/`** — las claves privadas son específicas de cada máquina.

---

### `optimization` / `storage` — Avanzado

```yaml
optimization:
  pgo_enabled: false          # Profile-Guided Optimization (Go PGO)
  zero_allocation_slabs: true # Flag informativo: sync.Pool en hot-paths

storage:
  engine: "bbolt"             # Motor de persistencia (solo bbolt soportado)
  brain_file: "brain.db"      # Archivo BoltDB principal
  require_hardware_encryption: false
  flush_interval_ms: 50       # Frecuencia de flush a SSD (ms)
```

### `sentinel` — Gobernanza y Active Dreaming (Épicas 40-41)

```yaml
sentinel:
  heap_threshold_mb: 500           # Max heap (MB) antes de denegar acciones intensivas
  goroutine_explosion_limit: 10000 # Max goroutines antes de activar guard
  cold_start_grace_sec: 30         # Segundos sin auto-approve post-boot
  audit_log_max_size: 1000         # Entradas en audit log de políticas
  dream_cycle_count: 3             # Sueños adversariales por ciclo REM
  immunity_confidence_init: 0.6    # Confianza inicial de inmunidades aprendidas
  immunity_activation_min: 0.5     # Confianza mínima para activar inmunidad
```

### `kinetic` — Bio-feedback de Hardware (Épica 44)

```yaml
kinetic:
  spectral_bins: 8                 # Bins DFT para análisis espectral
  anomaly_threshold_sigma: 2.0     # Sigma para detección de anomalías
  gc_pause_threshold_us: 10000     # Pausa GC máxima (µs)
  heap_critical_sigma: 5.0         # Sigma para acción crítica
  heap_warning_sigma: 3.0          # Sigma para warning
```

### `coldstore` — OLAP Cold Storage (Épica 38)

```yaml
coldstore:
  max_open_conns: 3                # Conexiones SQLite abiertas
  max_idle_conns: 2                # Conexiones idle
  default_query_limit: 50          # Límite por defecto en queries analíticas
```

### `hypergraph` — Relaciones Multidimensionales (Épica 42)

```yaml
hypergraph:
  max_impact_depth: 5              # Profundidad BFS efecto mariposa
  risk_decay_factor: 0.7           # Decaimiento de riesgo por hop
  min_risk_threshold: 0.01         # Umbral mínimo de riesgo propagado
```

### `causal` — Conciencia Causal (Épica 39)

```yaml
causal:
  max_causal_depth: 10             # Profundidad máxima cadena causal
  max_recent_errors: 5             # Errores recientes en contexto
```

### `cpg` — Code Property Graph (PILARs XX + XXXII)

Controla el builder SSA del grafo de propiedades de código y los algoritmos sobre él (CodeRank, Spreading Activation). Desde PILAR XXXII incluye **persistencia gob** para fast-boot — el CPG se serializa a disco y se restaura al arrancar si no está obsoleto, evitando el rebuild SSA completo (ahorra 5-30s según tamaño del workspace).

| Campo | Tipo | Default | Hot-reload | Descripción |
|-------|------|---------|------------|-------------|
| `page_rank_iters` | int | `50` | ✗ | Iteraciones PageRank para CodeRank. |
| `page_rank_damping` | float64 | `0.85` | ✗ | Factor de amortiguación PageRank. |
| `activation_alpha` | float64 | `0.5` | ✗ | Decaimiento por hop en Spreading Activation. |
| `max_heap_mb` | int | `512` | ✅ | OOM guard — si el heap supera este valor el grafo no se sirve (se conserva en memoria). Subir aquí restaura serving sin rebuild. |
| `persist_path` | string | `.neo/db/cpg.bin` | ✗ | Path del snapshot gob para fast-boot. Relativo al workspace root. |
| `persist_interval_minutes` | int | `15` | ✗ | Frecuencia de auto-snapshot. `0` = desactivado. También se guarda en SIGTERM. |

```yaml
cpg:
  page_rank_iters: 50
  page_rank_damping: 0.85
  activation_alpha: 0.5
  max_heap_mb: 512
  persist_path: .neo/db/cpg.bin
  persist_interval_minutes: 15
```

**Fast-boot (PILAR XXXII):** Al arrancar, neo-mcp intenta `LoadCPG(persist_path)`. Condiciones para usar el snapshot:
1. El archivo existe y `cpgSchemaVersion` coincide.
2. Ningún `.go` del workspace tiene mtime posterior a `BuildAtUnix` del header.

Si cualquier condición falla → cold build (comportamiento anterior). BRIEFING muestra `cpg_boot: fast | N nodes` o `cpg_boot: cold`.

**Staleness:** Walk recursivo de `.go` excluyendo `vendor/`, `node_modules/` y directorios con prefijo `.`.

---

### `workspace` — Configuración del espacio de trabajo

| Campo | Tipo | Default | Descripción |
|-------|------|---------|-------------|
| `dominant_lang` | string | `""` | Lenguaje dominante del workspace: `go`, `python`, `typescript`, `rust`. Auto-detectado si está vacío. Sobreescribe `ProjectConfig.dominant_lang` si se especifica aquí. |
| `ignore_dirs` | []string | ver abajo | Directorios excluidos del indexado RAG y BLAST_RADIUS walk. |
| `allowed_extensions` | []string | `.go .ts .js .py .md .rs .html .css .yaml` | Extensiones indexadas. |
| `max_file_size_mb` | int | `5` | Archivos más grandes se omiten del RAG. |

```yaml
workspace:
  dominant_lang: ""          # auto-detectado: go|python|typescript|rust
  max_file_size_mb: 5
  ignore_dirs:
    - node_modules
    - vendor
    - .git
    - dist
    - .neo
```

---

### Project Federation — `.neo-project/neo.yaml` (PILAR XXXI)

La federación de proyectos permite agrupar múltiples workspaces bajo un proyecto común. Se configura con un archivo **`.neo-project/neo.yaml`** en el directorio raíz del proyecto (no en el workspace individual).

```
mi-plataforma/
├── .neo-project/
│   └── neo.yaml          ← archivo de proyecto
├── services/
│   ├── api/
│   │   └── neo.yaml      ← workspace A
│   └── frontend/
│       └── neo.yaml      ← workspace B
```

**Crear con CLI:**
```bash
cd mi-plataforma
neo init --project --name acme-platform
# Detecta automáticamente los neo.yaml a profundidad ≤ 3
```

**Contenido de `.neo-project/neo.yaml`:**
```yaml
project_name: acme-platform
member_workspaces:
  - ./services/api
  - ./services/frontend
dominant_lang: go          # sobreescribe workspace.dominant_lang si no está seteado
ignore_dirs_add:
  - migrations             # se añade al ignore_dirs de cada workspace
```

| Campo | Tipo | Descripción |
|-------|------|-------------|
| `project_name` | string | Nombre del proyecto. Aparece en BRIEFING y BLAST_RADIUS scatter. |
| `member_workspaces` | []string | Paths relativos a los workspaces miembro. |
| `dominant_lang` | string | Lenguaje dominante del proyecto. |
| `ignore_dirs_add` | []string | Dirs adicionales a ignorar (se añaden al workspace base). |

**Comportamiento en BRIEFING:**
- Modo full: tabla con estado de cada workspace miembro (running/stopped, RAM, RAG coverage).
- Modo compact: `| Project: acme-platform (2 ws, 1 running)`.

**BLAST_RADIUS federado:**
```json
neo_radar(intent: "BLAST_RADIUS", target: "pkg/auth/keystore.go", target_workspace: "project")
```
Lanza análisis paralelo (sem=4) en todos los workspaces miembro y devuelve tabla consolidada.

---

### `databases` — Bases de datos externas (DB_SCHEMA / neo_radar)

Permite conectar `neo_radar` con bases de datos externas para el intent `DB_SCHEMA`.
Guard read-only: solo `SELECT`. Rechaza `DROP`, `DELETE`, `UPDATE`, `INSERT`, `TRUNCATE`, `ALTER`, `CREATE`, `REPLACE`.

```yaml
databases:
  - name: "server_prod"        # Alias en neo_radar(intent:DB_SCHEMA, db_alias:"server_prod")
    driver: "postgres"         # "postgres" (lib/pq), "pgx" (pgx/v5), "sqlite3"
    dsn: "${SERVER_PROD_DSN}"  # Referencia a variable de entorno — ver sección Secretos
    max_open_conns: 1          # 0 = default (2). SQLite: forzar 1 para evitar SQLITE_BUSY
```

> **IMPORTANTE:** compilar el binario con el driver como blank import en `cmd/neo-mcp/main.go`:
> ```go
> import _ "github.com/lib/pq"                  // PostgreSQL via lib/pq
> // o: import _ "github.com/jackc/pgx/v5/stdlib" // PostgreSQL via pgx
> ```
> Sin este import, `DB_SCHEMA` falla con `unknown driver "postgres"`.

---

## Secretos y variables de entorno en neo.yaml

`neo.yaml` va commiteado — **nunca pongas credenciales directamente en él**.
Usa referencias `${VAR_NAME}` en cualquier campo de texto: el loader las expande al arrancar.

### Flujo

1. Crea `.neo/.env` (gitignoreado) con los valores reales:
   ```
   # .neo/.env — nunca commitear. Ver .neo/.env.example como plantilla.
   SERVER_PROD_DSN=postgres://user:pass@host:5432/db?sslmode=disable
   OTRA_API_KEY=sk-...
   ```
2. Referencia `${VAR}` en `neo.yaml`:
   ```yaml
   databases:
     - name: "server_prod"
       driver: "postgres"
       dsn: "${SERVER_PROD_DSN}"
   ```
3. El loader (`pkg/config/config.go`) carga `.neo/.env` antes de parsear YAML y expande con `os.ExpandEnv`.

### Prioridad de resolución

```
Shell env  >  .neo/.env  >  valor literal en neo.yaml
```

Las variables del shell nunca son sobreescritas por `.neo/.env`.
En CI/CD: define los secretos como env vars del runner y no necesitas `.neo/.env`.

### Plantilla `.neo/.env.example`

Commiteada al repo sin valores reales. Al incorporar un nuevo workspace:
```bash
cp .neo/.env.example .neo/.env
# editar .neo/.env con los valores reales
```

---

## Archivos auto-generados al arrancar

```
~/.neo/workspaces.json       ← Registry global de workspaces (auto-creado)
<proyecto>/.neo/             ← Directorio raíz del orquestador
<proyecto>/.neo/db/          ← BoltDB: hnsw.db, brain.db, planner.db, certified_state.lock
<proyecto>/.neo/logs/        ← mcp.log
<proyecto>/.neo/models/      ← Modelos WASM locales
<proyecto>/.neo/pki/         ← Certificados mTLS (ca.crt, server.crt, server.key)
<proyecto>/.neo/master_plan.md        ← EDITABLE: define tus épicas aquí
<proyecto>/.neo/technical_debt.md     ← Auto-gestionado por Kanban (Épica 30)
```

**¿Qué commitear?**

| Archivo | ¿Commit? | Razón |
|---------|----------|-------|
| `neo.yaml` | ✅ (sin secretos) | Configuración del proyecto — usa `${VAR}` para credenciales |
| `neo.yaml.example` | ✅ | Plantilla para nuevos devs |
| `.neo/.env.example` | ✅ | Plantilla de secretos sin valores reales |
| `.neo/.env` | ❌ | **Secretos reales — nunca commitear** |
| `.neo/master_plan.md` | ✅ | Plan de trabajo versionado |
| `.neo/technical_debt.md` | ✅ | Historial de épicas |
| `.neo/incidents/*.md` | ✅ | Postmortems |
| `.neo/db/` | ❌ | Estado runtime (BoltDB) |
| `.neo/pki/` | ❌ | Claves privadas |
| `.neo/snapshots/` | ❌ | Binarios de crash |
| `.neo/logs/` | ❌ | Logs de runtime |

---

## Variables de entorno

| Variable | Uso |
|----------|-----|
| `NEO_RAPL_OVERRIDE_WATTS` | Simular carga térmica en tests (override RAPL) |
| `SRE_PHOENIX_ARMED=true` | Activar Phoenix Protocol (operación destructiva) |
| `ANTHROPIC_API_KEY` | Requerida para capa CLOUD del gateway de inferencia |
| `TS_AUTHKEY` | Autenticación Tailscale para Gossip P2P |
| `OLLAMA_NUM_PARALLEL` | Paralelismo interno de Ollama (afecta `ollama_concurrency` óptimo) |

---

## Migración entre máquinas

Al mover a una nueva máquina (ej. macOS → Linux):

1. `git clone` — recupera código, reglas y `neo.yaml`
2. Copiar manualmente:
   - `~/.neo/workspaces.json` → registry de workspaces
   - `.neo/db/brain.db` → memex episódico
   - `.neo/db/hnsw.db` → grafo vectorial (sin esto, re-indexa desde cero)
   - `~/.claude/projects/.../memory/` → memorias de Claude Code
   - `.neo/.env` → secretos del workspace (o crearlo desde `.neo/.env.example`)
3. Instalar dependencias del sistema:
   ```bash
   # Go (versión en go.mod)
   # Ollama + modelo
   ollama pull nomic-embed-text
   # Compilar binarios
   go build -o bin/neo-mcp ./cmd/neo-mcp
   go build -o bin/neo ./cmd/neo
   ```
4. Los PKI se regeneran solos al primer arranque.
