# NeoAnvil — Docker Architecture & Workflow (Pattern D)

> **Audience:** developers running NeoAnvil in containers while keeping
> source code, IDE editing, and Git operations on the host. Single
> workstation deployments. For multi-host / Kubernetes, see the
> [scalability notes](#scalability) at the bottom.

---

## TL;DR

```
┌──────────────────────────── HOST (your laptop / workstation) ──────────────────────────────┐
│                                                                                            │
│   ~/.neo/credentials.json    ~/.neo/plugins.yaml    /path/to/your/repo/                    │
│         │                         │                       │                                │
│         │ bind RO                 │ bind RO               │ bind RW                        │
│         ▼                         ▼                       ▼                                │
│ ┌─────────────────────────────────────────────────────────────────────────┐               │
│ │                      docker-compose stack                                │               │
│ │                                                                          │               │
│ │  ┌─────────────────────────┐   ┌────────────┐   ┌──────────────────┐   │               │
│ │  │ neoanvil  (Go binary +  │   │  ollama    │   │  ollama-embed    │   │               │
│ │  │  Nexus + spawned MCP)   │◄──┤  :11434    │   │  :11434          │   │               │
│ │  │  EXPOSE 9000, 8087      │   │  GPU       │   │  GPU             │   │               │
│ │  └────────────┬────────────┘   └─────┬──────┘   └────────┬─────────┘   │               │
│ │               │                      │                    │             │               │
│ │   neoanvil-state            ollama-models       ollama-embed-models    │               │
│ │   neoanvil-work             (named volume)      (named volume)         │               │
│ │   (named volumes)                                                       │               │
│ └─────────────────────────────────────────────────────────────────────────┘               │
└────────────────────────────────────────────────────────────────────────────────────────────┘
```

**One-line summary:** code on host, state inside Docker, GPU passthrough
to ollama, single bridge network for service-name DNS.

---

## Why Pattern D and not the others

| Pattern | What it does | Why we don't use it |
|---|---|---|
| A — Ephemeral | All Docker, fresh on every up | Loses your master_plan, RAG index, KS, audit baseline. Useful only for CI smoke tests. |
| B — Full migration | Move everything from host to Docker | One-way commitment; rollback to native is non-trivial. |
| C — Side-by-side isolated | Native + Docker, separate state | Two parallel realities (creds, plans, baselines) to keep in sync. Maintenance tax. |
| **D — Hybrid (what we use)** | Code on host, state in Docker, configs cross-mounted RO | IDE works on real files, BoltDB stays on host filesystem driver (flock-safe), single source of truth for auth. |
| E — Shared BoltDB | Native + Docker on same DB files | **Forbidden.** Concurrent flock between host process and container corrupts BoltDB. |

---

## Persistence — what survives what

| Action | Named volumes | Bind mounts | Container layer |
|---|---|---|---|
| `docker compose down` | ✅ survive | ✅ survive | ❌ rebuilt next up |
| `docker compose down -v` | ❌ deleted | ✅ survive | ❌ rebuilt |
| `docker volume rm <name>` | ❌ deleted | ✅ survive | n/a |
| host reboot | ✅ survive | ✅ survive | ✅ survive (auto-restart per `restart: unless-stopped`) |
| `docker rmi neoanvil:local` | ✅ survive | ✅ survive | ❌ image removed |

**Rule of thumb:** anything in named volumes survives normal lifecycle
operations. Only `down -v` or explicit `volume rm` deletes them.

---

## Memory model — where does each kind of state live?

| Memory layer | Backing store | Location | First-boot behavior |
|---|---|---|---|
| **HNSW vector store** (RAG) | `<repo>/.neo/db/hnsw.db` + `hnsw.bin` | bind-mounted from host (your repo) | rebuilds on first `BLAST_RADIUS` / `SEMANTIC_CODE` call (~5 min cold) |
| **CPG** (Code Property Graph) | `<repo>/.neo/db/cpg.bin` | bind-mounted from host | rebuilds on first `PROJECT_DIGEST` / `GRAPH_WALK` (~30s) |
| **Episodic memex** (lessons, REM sleep) | `<repo>/.neo/db/brain.db` | bind-mounted from host | starts empty; lessons accrue via `neo_memory(action: commit)` |
| **Knowledge Store project tier** (DTOs, contracts) | `<repo>/.neo-project/db/shared.db` | bind-mounted from host | starts empty; populated via `neo_memory(action: store, tier: project)` |
| **Knowledge Store nexus tier** (cross-workspace) | `~/.neo/shared/db/global.db` inside container | named volume `neoanvil-state` | starts empty; rebuild only via explicit re-population |
| **Cache stack** (QueryCache, TextCache, EmbeddingCache) | `<repo>/.neo/db/*.snapshot.json` | bind-mounted from host | persisted on SIGTERM, loaded on next boot |
| **Plugin auth** (Jira/DS tokens) | `~/.neo/credentials.json` (host) → `~/.neo/credentials.json` (volume) | host bind RO → seeded into volume | copied on first boot only; volume's copy is writable |
| **Plugin manifests** | `~/.neo/plugins.yaml` (host) → `~/.neo/plugins.yaml` (volume) | host bind RO → seeded | same as creds |
| **Workspace registry** | `~/.neo/workspaces.json` inside container | named volume | recreated on first boot; operator must `neo space use` again |
| **Master plan + baseline** | `<repo>/.neo/master_plan.md`, `<repo>/.neo/audit-baseline.txt` | bind-mounted from host (your repo) | preserved across container lifecycle; edits visible to host IDE |
| **GPU model files** | `/root/.ollama/` per ollama service | named volumes `ollama-models`, `ollama-embed-models` | first `ollama run <model>` pulls; subsequent boots reuse |

**The bind-mount-from-host trick for `<repo>/.neo/db/*`:** because the
repo dir is bind-mounted, BoltDB lives on the host's native filesystem
driver (ext4/xfs), where flock works correctly. Only one process can
have the BoltDB open at a time — that means **the host's native
neoanvil and the container's neoanvil cannot run against the same
workspace simultaneously.** Stop one before starting the other.

---

## Lifecycle

### First-time setup

```bash
# 1. Build the image (one-time, ~3 min)
make docker-build

# 2. (Optional, side-by-side mode) shift ports if you have a native install
cp .env.example .env
# edit .env: NEXUS_PORT=19000, HUD_PORT=18087, OLLAMA_PORT=11534, OLLAMA_EMBED_PORT=11535

# 3. Bring up the stack
make docker-up

# 4. Pull the LLM and embedding models into the GPU services
docker exec -it neoanvil-ollama       ollama pull llama3.2:3b
docker exec -it neoanvil-ollama-embed ollama pull nomic-embed-text

# 5. Verify
make docker-status
curl http://127.0.0.1:${NEXUS_PORT:-9000}/status
```

### Daily use

Edit your code in your IDE on the host as usual. The container sees
file changes immediately (bind mount). Tools like `neo_radar`,
`neo_sre_certify_mutation`, etc. operate on the same files.

For Jira / DeepSeek auth: edit `~/.neo/credentials.json` on the host;
the container bind-mounts it read-only, then the entrypoint seeds it
into the volume on first boot. After first boot, the volume's copy is
authoritative — to update auth in a running container, either
`docker exec` and edit, or `make docker-down && make docker-up` to
re-seed.

### Stopping

```bash
make docker-down              # stop + remove containers; volumes survive
make docker-down DOWN_FLAGS=-v # stop + DELETE all volumes (destructive!)
```

### Backup

```bash
# Snapshot a volume to a tarball
docker run --rm -v neoanvil_neoanvil-state:/v -v $PWD:/backup alpine \
  tar czf /backup/neoanvil-state-$(date +%F).tar.gz -C /v .

# Restore
docker run --rm -v neoanvil_neoanvil-state:/v -v $PWD:/backup alpine \
  sh -c 'cd /v && tar xzf /backup/neoanvil-state-2026-05-09.tar.gz'
```

---

## Migration paths

### Native → Docker (one-time)

```bash
# Stop native first
pkill neo-nexus
systemctl --user stop ollama  # or whatever your setup uses

# Bring up docker
make docker-build
make docker-up
make docker-seed              # copies master_plan + audit-baseline into container
```

Then, optionally, copy your existing `~/.neo/db/global.db` into the
volume:

```bash
make docker-down
docker run --rm \
  -v ~/.neo/shared/db:/host \
  -v neoanvil_neoanvil-state:/v \
  alpine sh -c 'mkdir -p /v/shared/db && cp -p /host/global.db /v/shared/db/'
make docker-up
```

### Docker → Native (rollback)

```bash
make docker-down

# Pull state out of the volume back to the host
docker run --rm \
  -v neoanvil_neoanvil-state:/v \
  -v ~/.neo:/host \
  alpine sh -c 'cp -ap /v/shared /host/shared'

# Start native
bin/neo-nexus &
```

---

## Configuration injected by compose

Compose injects four env vars that the binary reads at boot to switch
from native defaults to container defaults. None are persisted to disk.

| Env var | Code path | Purpose |
|---|---|---|
| `NEO_BIND_ADDR` | `pkg/nexus/config.go::applyEnvOverrides` | Binds dispatcher to `0.0.0.0` instead of localhost |
| `OLLAMA_HOST` | `pkg/config/config.go::LoadConfig` | Points RAG at `http://ollama:11434` (compose service) |
| `OLLAMA_EMBED_HOST` | same | Points embed at `http://ollama-embed:11434` |
| `NEO_NEXUS_OLLAMA_LIFECYCLE=disabled` | `pkg/nexus/config.go::applyEnvOverrides` | Stops Nexus from spawning its own ollama (compose owns them) |

---

## Scalability notes

This architecture targets **single workstation** with one operator.
Things to revisit if scaling out:

- **Multi-workspace inside one container:** already supported. Nexus
  spawns one neo-mcp per workspace registered in `~/.neo/workspaces.json`.
  Children share the GPU pool serially; `OLLAMA_NUM_PARALLEL=2` (chat)
  and `4` (embed) cap concurrent calls per service.
- **Multi-tenant** (several developers, same host): the current
  Dockerfile creates a single `neo` user. To isolate: spawn a separate
  compose stack per developer with port-shifted `.env`. Volumes are
  namespaced per stack via `COMPOSE_PROJECT_NAME`.
- **Multi-host:** out of scope for Pattern D. The BoltDB-on-bind-mount
  trick assumes a single filesystem. For genuinely distributed
  state, see PILAR XXVI Brain Portable docs (R2 + Tailscale sync).
- **CI builds:** use Pattern A (ephemeral) with `make docker-build` +
  smoke test. Don't `docker-seed` in CI — fresh state per run.
- **Production deployment:** named volumes are local-disk by default.
  For prod, use a volume driver (e.g., `local-persist`, `nfs`, `gluster`)
  declared at the volume level; nothing else changes.

---

## Known limitations / scalability constraints

| Limit | Threshold | Symptom | Mitigation |
|---|---|---|---|
| GPU VRAM budget | RTX 3090 24GB ≈ 7B chat (5GB) + nomic-embed (500MB) + headroom | OOM on 13B+ chat models when both ollama services active | Use chat models ≤ 7B for default; for larger models stop `ollama-embed` or use a second GPU |
| Multi-developer same host | volumes namespaced via `COMPOSE_PROJECT_NAME=${USER}_neoanvil` | first dev's stack works; second dev's `make docker-up` collides on ports | port-shift via `.env` per developer |
| BoltDB concurrent writers | host-native + container both opening `<repo>/.neo/db/*` | second writer hangs on flock (no corruption) | stop one before starting the other |
| `docker compose down -v` | destructive | named volumes deleted including credentials | recoverable: re-up reseeds from `~/.neo/` host bind |
| `docker-seed` race window | <100ms during `docker cp` | BRIEFING reads partial master_plan once | resolved via `.seeding` atomic rename in target |
| GPU passthrough | requires `nvidia-container-toolkit` on host | "could not select device driver nvidia" | install toolkit or comment out `deploy:` blocks (CPU fallback) |

## Troubleshooting

| Symptom | Likely cause | Fix |
|---|---|---|
| `port already in use` on `make docker-up` | native install still running | `pkill neo-nexus; systemctl --user stop ollama` OR shift ports in `.env` |
| `ollama list` healthcheck never passes | nvidia-container-toolkit missing | comment out the `deploy:` GPU block in compose; ollama runs CPU-only |
| `BoltDB: timeout` in container logs | host's native neoanvil has the lock on the bind-mounted DB | stop native first |
| credentials.json edits not seen | first-boot seed is one-shot; named volume copy is now authoritative | `docker exec` to edit, OR `make docker-down -v && make docker-up` to re-seed |
| RAG returns 0 results for ~5 min | HNSW cold rebuild after first up | wait, or pre-seed by copying `<repo>/.neo/db/hnsw.bin` from a previous build |
| Container can't reach host services (e.g., your DB on host) | bridge network can't reach host loopback by default | use `extra_hosts: ["host.docker.internal:host-gateway"]` |
