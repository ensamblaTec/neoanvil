# BRIEFING Format Reference

Documents the output format of `neo_radar(intent:"BRIEFING")`. [SRE-118 / Épica 127]

---

## Compact mode (`mode: "compact"`)

```
## SRE BRIEFING (compact)

<stale_prefix><resume_prefix><nexus_debt_prefix>Mode: <mode> | Phase: <phase> | Open: N | Closed: M | Next: Task X.Y | RAM: ZMB | IO: WKB | RAG: P%<suffix>
git: <branch> ↔ origin (<ahead>/<behind>) <clean|N changes>
hooks: post-commit:✓|✗ · style: <name> · skills: N (A auto, T task)
last_epics: N, M, K
```

### Compact line fields

| Field | Source | Notes |
|-------|--------|-------|
| `stale_prefix` | `binaryStaleAlert` | `⚠️ BINARY_STALE:Nm \| ` when bin older than HEAD |
| `resume_prefix` | `resumeWarning` | `⚠️ RESUME \| ` when agent worked without prior BRIEFING |
| `nexus_debt_prefix` | Nexus `/internal/nexus/debt/affecting` | `⚠️ NEXUS-DEBT:N P0:M \| ` when debt events exist |
| `Mode` | `NEO_SERVER_MODE` | pair / fast / daemon |
| `Phase` | `.neo/master_plan.md` active section | First non-empty heading after trimming `#🌟 ` |
| `Open` / `Closed` | master_plan.md `- [ ]` / `- [x]` count | Authoritative (BoltDB is secondary) |
| `Next` | First open task line | e.g. `Task 127.1` |
| `RAM` | `runtime.MemStats.HeapAlloc` | MB |
| `IO` | Atomic recv + sent counters | KB (session total) |
| `RAG` | HNSW indexed vs total workspace files | % |

### Suffix segments (appended when non-zero)

| Segment | Condition |
|---------|-----------|
| `\| rag_warn` | RAG coverage < 80% |
| `bin_age` | Age of neo-mcp binary vs HEAD |
| `\| muts: N files` | Session mutations count > 0 |
| `\| INC-IDX: N/M` | Incident index counters |
| `\| ⚠️ inc: N critical (24h)` | Critical incidents in last 24h |
| `\| Qcache: X%` | Query cache hit rate |
| `\| Tcache: X%` | Text cache hit rate |
| `\| Ecache: X%` | Embedding cache hit rate |
| `\| CPG: X/YMB (Z%)` | CPG heap vs limit |
| `\| GPU: name X/YGB util%` | GPU available |
| `\| 📬 inbox: N` | Unread inbox messages |
| `peers: N active` | Peer workspaces active in last 2min |
| `plugins: N active` | Plugin pool status |

### Session context lines [127.1-127.3]

Three optional lines appended after the compact line in compact and auto-compact modes:

```
git: <branch> ↔ origin (<ahead>/<behind>) <clean|N changes>
hooks: post-commit:✓|✗ · style: <name> · skills: N (A auto, T task)
last_epics: N, M, K
```

| Line | Source | Notes |
|------|--------|-------|
| `git:` | `git status --porcelain -b` (200ms timeout) | Fail-open → omitted |
| `hooks:` | `.git/hooks/post-commit` stat + `.claude/skills/*/SKILL.md` glob | `disable-model-invocation: true` = task-driven |
| `last_epics:` | `.neo/master_plan.md` — last 3 fully-closed `## ÉPICA N` sections | Omitted when none found |

---

## Full mode (`mode: "full"`)

Sections in order:
1. Warnings (stale binary, resume, nexus debt)
2. Plan section (active phase, open tasks)
3. Tool inventory + radar intents
4. Digest age
5. Incident section
6. Infrastructure (Ollama, GPU, RAM, sessions)
7. Project federation health table (when configured)
8. **`### Session context`** — git, tooling, recent epics [127.4]
9. Architectural memory (top-3 semantic matches)
10. Contract drift alerts
11. Knowledge store
12. Stale contracts
13. Session mutations
14. Peer session mutations

---

## Delta mode (`mode: "delta"`)

Returns only fields that changed since the previous BRIEFING call this session.
First call returns the compact line with a `_[first BRIEFING — no delta]_` note.
