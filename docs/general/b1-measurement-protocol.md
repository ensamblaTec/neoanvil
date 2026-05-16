# B1 Measurement Protocol

**Feature:** B1 Tool Discipline Mirror (`feature/adaptive-briefing-diff`)
**Charter:** `docs/general/adaptive-runtime-charter.md`
**Helper:** `scripts/b1-measurement.sh`
**Log:** `.neo/b1-measurements.csv` (gitignored — operator-local)

## The conjecture being tested

Does injecting a "Tool Discipline Mirror" at SessionStart change the
agent's tool-usage behavior over the following session?

The conjecture is testable because every `neo_radar` intent and every
neo-tool invocation is captured in `bucketToolAggregate` (BoltDB). A
snapshot before the session minus a snapshot after gives the per-session
diversity delta. Aggregated over several sessions per arm, the delta
indicates whether B1 moves the needle.

## A/B design

Two arms, each ~5 sessions:

| Arm | Env | Briefing output | Expected behavior |
|-----|-----|-----------------|-------------------|
| **baseline** | `NEO_BRIEFING_DIFF_DISABLE=1` | briefing only | agent unaware of its tool gap |
| **treatment** | env unset / `=0` | briefing + mirror | agent sees the gap on every boot |

Same agent (Claude Opus 4.7), same workspace, same general task mix.
Difference: the mirror.

## Workflow per session

Before opening a Claude Code session:

```bash
# Set the arm for THIS session.
export NEO_BRIEFING_DIFF_DISABLE=1   # baseline arm
# or
unset NEO_BRIEFING_DIFF_DISABLE      # treatment arm

# Capture pre-session snapshot.
NEO_B1_NOTE="pre-session" scripts/b1-measurement.sh snapshot
```

Then open the Claude Code session, do your normal work.

After the session ends:

```bash
# Capture post-session snapshot.
NEO_B1_NOTE="post-session: <one-line task summary>" \
  scripts/b1-measurement.sh snapshot
```

After every session, view the trajectory:

```bash
scripts/b1-measurement.sh report
```

## Ship criterion (per charter)

The B1 sub-branch graduates to `feature/adaptive-runtime` when the
treatment arm shows **≥ 2 additional intents on average** vs baseline,
sustained across the trial window (≥ 5 sessions per arm).

A delta of +2 intents means the mirror is genuinely moving Claude
toward tools it would otherwise skip — the entire point of the
adaptive layer.

If after 5+5 sessions the delta is ≤ 0 (or treatment underperforms),
discard B1, revisit charter, redesign or pivot.

## Caveats and honest limits

1. **N=5 per arm is small.** The metric will have noise. Don't over-
   interpret a single session. Look for the trend.

2. **Lifetime cumulative ≠ per-session.** The CSV records lifetime
   state at snapshot time. Per-session deltas are derived by
   subtracting consecutive snapshots from the SAME arm. The first
   snapshot in each arm establishes a per-arm baseline.

3. **Workspace effects are real.** A heavy refactor session naturally
   invokes more tools than a doc-edit session. Try to mix similar
   task types across arms.

4. **The agent might learn to game the mirror.** If Claude sees
   "neo_chaos_drill never invoked" it might invoke it once to clear
   the mark even when not appropriate. Measure tool-use VALUE, not
   just count (qualitative reading of session outcomes complements
   the quantitative diff).

5. **Cold-start renders identical to a fully-fresh state.** First
   session in the trial will look identical to a workspace bootstrap.
   That's by design — the mirror surfaces the inventory whether
   you've used it or not.

## Discard signals

Reasons to pull B1 even if metric improves:

- SessionStart timing exceeds 2 s consistently (the mirror is too
  expensive).
- Operator notices the mirror clutters the prompt and prefers it off.
- Treatment arm shows higher tool counts but no improved task
  outcomes (= noise hike without effectiveness change).
- The agent forms anti-patterns from the mirror (e.g. always
  invokes a tool once early to "tick the box").

## What measurement does NOT cover (deferred to later B's)

- Whether the agent makes BETTER decisions (only counts diversity).
- Cross-workspace behavior — measurement is active-workspace only.
- Per-task-type effectiveness — covered by B2's classifier.
- Latency of individual tool calls — `neo_tool_stats` already shows.
