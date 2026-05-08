# Tier Ownership (354.Z + PILAR LXVII)

_Canonical reference for multi-workspace Knowledge Store ownership under
Neo-Nexus. 4-tier finalized 2026-04-24 (PILAR LXVII added the org tier)._

---

## The four tiers

| Tier | Backing file | Scope | Owner |
|------|--------------|-------|-------|
| `workspace` | `<workspace>/.neo/db/knowledge.db` (standalone) or project's knowledge.db (federation) | Single workspace (legacy: same as project) | Local child |
| `project`   | `<project_root>/.neo-project/db/knowledge.db` | All member workspaces of a project | **Coordinator workspace** (declared in `.neo-project/neo.yaml`) |
| `org` [LXVII]| `<org_root>/.neo-org/db/org.db` | All projects of an organisation | **Coordinator project** (declared in `.neo-org/neo.yaml`) |
| `nexus`     | `~/.neo/shared/db/global.db` | All workspaces managed by this Nexus installation | **Nexus dispatcher (singleton)** |

See `docs/pilar-lxvii-org-tier.md` for the complete org-tier operational guide.

## Why "leader-only"

bbolt enforces exclusive process-level access to its database files. The
OS `Options.ReadOnly:true` mode still requests a shared flock (`LOCK_SH`),
which **blocks indefinitely** when another process holds `LOCK_EX` on the
same file. There is no mixed RW+RO mode. Consequence: at any moment, only
ONE process can own a given `.db` file. Every tier therefore needs a
deterministic "leader" who holds the flock, and clear routing for everyone
else.

## tier:"nexus" — Nexus-as-god

The Nexus dispatcher process (one per installation, always alive as long
as any child is alive) opens `~/.neo/shared/db/global.db` at boot and
exposes REST endpoints on `:9000`:

```
POST /api/v1/shared/nexus/store    {namespace, key, content, tags, hot}
POST /api/v1/shared/nexus/fetch    {namespace, key}
POST /api/v1/shared/nexus/list     {namespace ('*' = all), tag}
POST /api/v1/shared/nexus/drop     {namespace, key}
POST /api/v1/shared/nexus/search   {namespace, query, k}
```

neo-mcp children never open the global store directly. Their
`tool_memory.go::proxyNexusOp` POSTs to the dispatcher when `tier:"nexus"`.
Boot order is irrelevant; any child can read/write cross-project
knowledge.

**Reserved namespaces** (seeded with `.gitkeep` on first Nexus boot):
`improvements`, `lessons`, `operator`, `upgrades`, `patterns`.

**Use cases:**
- Cross-project lessons (e.g. "this pattern appeared in strategos AND
  proyecto-mes").
- Operator preferences / shortcuts.
- Nexus-level improvement notes ("replace bbolt with X").
- Meta-patterns worth reusing across managed projects.

## tier:"project" — Coordinator-as-leader

Projects declare a coordinator via `coordinator_workspace:` in
`.neo-project/neo.yaml`:

```yaml
project_name: strategos-project
member_workspaces:
  - /home/user/projects/backend
  - /home/user/projects/backendia_frontend
coordinator_workspace: strategos   # backend owns shared.db flock
```

At boot:
- Coordinator child opens `.neo-project/db/knowledge.db` normally.
- Non-coordinator children boot with `ks=nil` and remember the
  coordinator's workspace ID.
- `tier:"project"` ops on non-coordinators go through
  `tool_memory.go::proxyToCoordinator`, which POSTs a `tools/call` to
  `/workspaces/<coord_id>/mcp/message` with `tier:"project"` forced on
  the coord side (prevents re-proxy loop).

**If `coordinator_workspace` is empty:** legacy first-to-boot wins
(nondeterministic — avoid in production). BRIEFING tags this as
`tier:project=legacy` so agents notice.

## BRIEFING indicators

Compact mode surfaces the role in one segment:

```
| tier:project=leader          ← this workspace owns shared.db
| tier:project=proxy:strategos ← proxies tier:"project" to strategos
| tier:project=legacy          ← no coordinator; first-to-boot wins
(segment omitted)              ← no .neo-project/neo.yaml; standalone
```

`tier:"nexus"` never shows a badge — it's always available via Nexus and
degrades only if the dispatcher is unreachable (which would already be
apparent from the loss of MCP routing).

## Agent recipes

**I need a cross-project lesson:**
```
neo_memory(action:"store", tier:"nexus", namespace:"patterns", key:"...", content:"...")
```

**I need a project-shared contract type (DTO/API):**
```
neo_memory(action:"store", tier:"project", namespace:"contracts", key:"...", content:"...")
# On non-coord workspaces, this proxies via Nexus MCP routing automatically.
```

**I need a workspace-local note:**
Use `neo_memory(action:"commit", ...)` (episodic memex) — not the tiered store.

## Diagnostic

```bash
# Verify tier:"nexus" is up
curl -s :9000/api/v1/shared/nexus/list -d '{"namespace":"*"}' | head -5

# Check coordinator identification per child
grep "coordinator\|NEO-BOOT.*knowledge" ~/.neo/logs/nexus-*.log | tail -10

# BRIEFING shows tier role inline
curl -s :9142/mcp/message -d '{... neo_radar BRIEFING compact ...}'
```

## Related commits

- `b001476` fix(shared): 354.Z multi-child leader-only semantics
- `83b47a1` feat(shared): Nexus-as-god + coordinator-as-project-leader
- `afd493a` feat(briefing): surface tier:"project" ownership role
- `393c396` docs: session report 354.Z-redesign

See `.neo/session-reports/SESSION-2026-04-23-nexus-god-redesign.md`.
