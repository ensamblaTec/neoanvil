# NeoAnvil — Docker Quick Start

5-minute setup. For the architectural deep dive, see
[`docker-architecture.md`](./docker-architecture.md).

---

## Prerequisites

- Docker 24+ with `docker compose` v2 plugin
- `nvidia-container-toolkit` for GPU passthrough (skip the `deploy:`
  blocks in `docker-compose.yaml` if absent — Ollama runs CPU-only)
- Linux host (POSIX). macOS/Windows untested.

## 30-second setup

```bash
git clone <repo> && cd neoanvil

# 1. Build (~3 min cold, <1 min cached). UID/GID auto-detected.
make docker-build

# 2. Bring up
make docker-up

# 3. Pull the LLM models on first up (one-time)
docker exec -it neoanvil-ollama       ollama pull llama3.2:3b
docker exec -it neoanvil-ollama-embed ollama pull nomic-embed-text

# 4. Verify
make docker-status
curl http://127.0.0.1:9000/status
```

The container auto-registers your repo (the directory you ran
`docker compose up` from) as workspace `<basename>-<8hex>`. Point your
MCP client at `http://127.0.0.1:9000/mcp/sse` and you're online.

---

## Side-by-side with a native install

Defaults clash with native ports `9000 / 8087 / 11434 / 11435`. To run
both in parallel:

```bash
cp .env.example .env
# Edit .env:
NEXUS_PORT=19000
HUD_PORT=18087
OLLAMA_PORT=11534
OLLAMA_EMBED_PORT=11535
```

Then `make docker-up` uses the shifted ports while native keeps the
canonical ones.

---

## Gotchas

| Symptom | Cause | Fix |
|---|---|---|
| `port already in use` | native install running on the same ports | shift via `.env` (see above) OR stop native |
| Cannot reach `:9000` | host firewall / Docker bridge issue | `curl 127.0.0.1:9000` (loopback works regardless of bridge) |
| GPU not detected | `nvidia-container-toolkit` not installed | install it OR comment out `deploy:` blocks (CPU fallback) |
| BoltDB hangs | host-native + container both opening the same `<repo>/.neo/db/*` | stop one before starting the other |
| First boot slow | HNSW cold-rebuilds from `<repo>/.neo/db/hnsw.db` | wait ~5 min, subsequent boots use the fast-boot snapshot |
| `make docker-up` fails on bind mount | `~/.neo/credentials.json` doesn't exist on host | create it: `neo login` (native) OR touch + edit manually |
| Auth tokens stale after rotating on host | seeded copy in volume drifts | re-seed: edit + `make docker-down -v && make docker-up` (destructive — wipes ALL volumes) OR `docker exec` to edit in place |

---

## Persistence model (one-line summary)

| What | Where | Survives `down`? | Survives `down -v`? |
|---|---|---|---|
| Source code | bind from host repo | ✅ (lives on host) | ✅ (lives on host) |
| Workspace state (`<repo>/.neo/db/*`) | bind from host repo | ✅ | ✅ |
| Nexus configs / credentials volume | named volume `neoanvil-state` | ✅ | ❌ deleted |
| Ollama models | named volumes per service | ✅ | ❌ deleted |
| Container itself | runtime layer | ❌ rebuilt | ❌ rebuilt |

**Rule of thumb:** anything in a *named* volume can be wiped by
`down -v`. Anything bind-mounted from the host is unaffected.

For full backup/restore, migration paths, scalability notes, and the
8-threat security audit, see [`docker-architecture.md`](./docker-architecture.md).

---

## Useful commands

```bash
make docker-build          # rebuild image
make docker-up             # start stack (detached)
make docker-down           # stop + remove containers; volumes survive
make docker-down DOWN_FLAGS=-v   # ALSO wipe volumes (destructive)
make docker-logs           # tail Nexus + Ollama logs
make docker-status         # health + volume sizes
make docker-seed           # one-time copy of master_plan / audit-baseline / technical_debt into running container
docker exec -it neoanvil sh                     # shell into the orchestrator
docker exec -it neoanvil-ollama ollama list     # list pulled chat models
docker exec -it neoanvil-ollama-embed ollama list  # list embed models
```
