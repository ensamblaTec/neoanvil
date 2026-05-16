# Adaptive Runtime — Branch Charter

**Branch:** `feature/adaptive-runtime`
**Parent:** `develop` (`4649f16` at branch creation)
**Created:** 2026-05-15
**Audience:** future operator + future Claude sessions reading this charter to
understand why this branch exists and how features arrive here.

---

## Why this branch exists

The 2026-05-15 long-form session produced 18 commits closing Phase 2 +
fixing 5 real bugs. During self-audit the agent (Claude Opus 4.7)
acknowledged using only ~40 % of the available MCP tool surface
(8 / 23 `neo_radar` intents, 6 / 15 tools), defaulting to `bash` and
native `Read` for many operations the neo-tools cover better. Hooks
auto-fire BRIEFING + BLAST_RADIUS + certify reminders, so the Ouroboros
cycle FEELS followed while ~60 % of the arsenal — `COMPILE_AUDIT`,
`READ_SLICE`, `neo_compress_context`, `chaos_drill`,
`CLAUDE_FOLDER_AUDIT`, `PATTERN_AUDIT`, `INCIDENT_SEARCH`,
`CONTRACT_QUERY`/`CONTRACT_GAP`, `GRAPH_WALK` — stays in the box.

Two complementary fixes were already shipped to `develop` in the same
session (commit `dfcf983`):

1. PreToolUse:Read warning hook (`pre-read-large-file.sh`) — passive,
   triggers only when the Read happens.
2. `[TOOL-DISCIPLINE-CHECKLIST]` directive 58 — synced to
   `.claude/rules/neo-synced-directives.md`, loaded at SessionStart but
   competes with 57 other directives.

Both are **pull**: the agent has to invoke / read them. The branch
**adaptive-runtime** experiments with the inverse — **push** the
relevant tool context into the agent's view at the moment it matters,
based on what the workspace already knows. The conjecture: a Claude
that *sees* its own behavior diff at SessionStart and *receives* tool
hints at UserPromptSubmit will reach for more of the arsenal than one
that has to remember.

The branch is the experimental playground for that conjecture. Features
land here, get measured for N sessions, and only graduate to `develop`
when their primary metric moves.

## Hard constraints (Opus 4.7)

These shape what's worth building. Architectural decisions reference
them:

- **No background async between turns.** Anything the MCP learns has to
  materialise as injected context the next time the agent runs. There's
  no equivalent of an online RLHF update — only "cache + replay".
- **Context window is finite.** Every token injected at SessionStart or
  UserPromptSubmit reduces the budget for the real task. Hard cap on
  every adaptive feature.
- **Hook latency budgets are real.** SessionStart hook timeout is 20 s
  today; UserPromptSubmit is 5 s. Anything heavier than that breaks the
  session.
- **Skills auto-load consumes prompt tokens.** 17 skills × ~3 KB =
  ~51 KB if all of them auto-load. Path-scoping + max-2-task-mode caps
  needed.
- **Wire format compat.** Anything that writes to `BoltDB` persisted
  formats (snapshots, debt, memex, knowledge store) must preserve
  field names + JSON tags — operators have TUI / dashboard consumers
  depending on those.

## Tools that DEFINITELY reach the agent (the "push" tier)

In order of effectiveness (= the tools we should weaponise):

1. **SessionStart context** — auto-loaded before the first prompt.
   Shapes the entire session. Highest leverage.
2. **UserPromptSubmit hook context** — fires on every user turn.
   Effective when the hint is task-relevant; noise when not.
3. **`CLAUDE.md` (project + global)** — loaded once at boot.
4. **Path-scoped skills** — appear in system prompt when paths match.
5. **Tool-return text** — embedded in tool response; the agent reads
   it as part of the result.
6. **PreToolUse / PostToolUse hooks** — only fire on the matched tool
   event; useful for surgical reminders.

Tools that only reach the agent if it *pulls* them (the "pull" tier):

7. Synced directives (`.claude/rules/neo-synced-directives.md`) —
   loaded by Claude Code skills layer, but agents compete with 57+
   entries and may skim.
8. `memex_buffer` / consolidated HNSW lessons — reachable only via
   explicit `neo_memory(action:search)`.
9. BoltDB historicals (`neo_tool_stats`, `neo_debt list`, snapshots) —
   reachable only via explicit tool call.

**The adaptive-runtime thesis:** *move tools 7-9 into the 1-2
push-tier whenever the workspace state makes them relevant.*

## Feature pipeline (sequenced, measure-then-ship)

Each feature is a sub-branch off `feature/adaptive-runtime` (NOT off
`develop`). Each ships back to `feature/adaptive-runtime` only after
its primary metric moves. The whole branch graduates to `develop` once
B1-B3 all proven.

### B1 — `feature/adaptive-briefing-diff`
**Scope:** extend `briefing.sh` (SessionStart hook) with a behavior-diff
section. Format: "last N sessions you invoked X/15 tools, Y/23 intents;
top 3 underused for your typical task type: A, B, C".

**Implementation outline:**

- Add a wrapper around `neo_tool_stats` that returns "tool diversity
  score" + "intent coverage %" + "underused list" for the rolling
  7-session window.
- Format ≤200 tokens, attached as a separate block in the briefing
  output so it's visually distinct from runtime state.
- Filter underused-list by success-attached metric (commit landed +
  tests pass during that session) to avoid surfacing tools that the
  agent already tried-and-failed.

**Primary metric:** *tool diversity score* — distinct neo_radar intents
invoked per session. Baseline = current ~8. Target = ≥12 sustained
across 5 sessions.

**Ship criterion:** metric moves to ≥10 average over 5 sessions with no
SessionStart timing regression (<2 s).

### B2 — `feature/intent-classifier-hints` (only if B1 ships)
**Scope:** add `userprompt-intent-hints.sh` (PreToolUse:UserPromptSubmit
hook). Regex / keyword classifier on the prompt → inject a small task-
relevant tool hint.

**Implementation outline:**

- Catalogue 6-8 task intents with regex matchers (e.g. "HTTP edit",
  "config field add", "debug perf issue", "doc refresh").
- Each intent has a hint definition in `.claude/rules/intent-hints/`
  mapping → tool sequence ("for HTTP edit: AST_AUDIT first, then
  chaos_drill capa 3 post-edit").
- Single hint per turn (highest-confidence match); silent below
  confidence threshold ≥0.7.
- Hook timeout ≤200 ms (regex only, no embedding inference).

**Primary metric:** *ratio bash:neo* — bash invocations / neo-tool
invocations per session. Baseline = ~12:1. Target = ≤5:1.

**Ship criterion:** ratio drops to ≤8:1 over 5 sessions and at least
70 % of injected hints are followed (the agent actually invokes the
suggested tool within 3 turns).

### B3 — `feature/lessons-replay` (only if B1 + B2 ship)
**Scope:** at SessionStart, query `memex_buffer` (and consolidated HNSW
lessons) for entries semantically matching the current workspace state
(open debt, master_plan phase, recent commits). Inject top-3 relevant
lessons.

**Implementation outline:**

- Compute a workspace state digest (open debt titles + master_plan
  phase + last-5-commit subjects).
- Embed digest via Ollama, query the memex HNSW for nearest-neighbour
  lessons.
- Cap injection at ≤300 tokens. Decay weight: lessons older than 30
  days are downranked.

**Primary metric:** *anti-pattern recurrence rate* — how often the
agent repeats a mistake captured as a memex lesson. Baseline = TBD
(need to define a few cardinal anti-patterns first).

**Ship criterion:** recurrence rate halves across 5 sessions, no
SessionStart timing regression, lesson relevance ≥60 % subjective
operator rating.

### B4 — Deferred research

Online feedback loop. Only if B1-B3 ship and prove the adaptive layer
delivers. Requires honest naming: "lesson cache + replay", not "RLHF"
(no online weight updates possible).

## Graduation rule to `develop`

This branch merges back to `develop` once **all three of B1-B3** have
shipped within `feature/adaptive-runtime` AND a final acceptance run
shows:

- Tool diversity score ≥12 sustained
- Ratio bash:neo ≤5:1 sustained
- Anti-pattern recurrence -50 %
- No regression in baseline workflow timings
- Operator subjective accept

If any B fails to ship, that B is rolled back; the branch holds the B's
that did ship and waits for the next iteration on the failed one. The
branch is **not** time-boxed — it ships features when they earn it.

## How to work on this branch

1. Check out `feature/adaptive-runtime`.
2. Create sub-branch `feature/<descriptive-name>` off it.
3. Implement the smallest possible vertical slice for that B feature.
4. Open a focal commit + measurement record (results of N-session
   trial).
5. When primary metric is hit, merge sub-branch back to
   `feature/adaptive-runtime`. Otherwise discard.
6. After B3 ships, raise the umbrella merge to `develop`.

## Out of scope (explicitly NOT building here)

- Big-bang refactor of the existing MCP. The adaptive layer is
  **additive**.
- New deferred tools or duplicate plugin instances. The 15 existing
  tools are the surface.
- Online RLHF. Not possible with current Opus 4.7 inference path.
- Custom embedding model. Ollama nomic-embed-text stays.

## Open questions

1. **Metric capture instrumentation:** does `neo_tool_stats` already
   record per-session granularity, or do we need a new aggregator?
   (To be answered in B1.)
2. **Cold-start (session #1):** how do we render a behavior diff when
   there's no prior history? (Show "tool inventory + suggested
   priorities" instead.)
3. **Cross-workspace adaptive state:** does the diff include only the
   active workspace or aggregate across all federated workspaces?
   ~~(Start with active-only, expand if useful.)~~
   **Answered 2026-05-15 during port to siblings:** `neo_tool_stats` is
   **Nexus-global** — a single counter ring buffer in the orchestrator,
   not per-workspace. Verified by calling `neo_tool_stats` targeting
   `strategos` vs `strategosia_frontend` and getting byte-identical
   responses (e.g. `neo_sre_certify_mutation: 237 calls` in both). The
   `target_workspace` header routes the call but doesn't scope the
   counter. Implications:
   - The **mirror display** is worth replicating across workspaces, so
     the agent sees the nudge wherever it boots. Done for `strategos`;
     skipped for `strategosia_frontend` (no pre-existing hook system
     — bootstrap > B1 scope).
   - The **CSV measurement** stays centralised in
     `neoanvil/.neo/b1-measurements.csv` with a new `workspace_boot`
     column tagging which workspace fired the snapshot. Per-workspace
     duplicate CSVs would be redundant.
   - A per-workspace `neo_tool_stats` (with scope=workspace actually
     honored) would be a separate cleanup ticket, not part of B1.
4. **Operator opt-out:** environment variable to disable adaptive
   layer for a session? (`NEO_ADAPTIVE_DISABLE=1`.)

---

*Charter end. Edits welcome via commit on `feature/adaptive-runtime`.*
