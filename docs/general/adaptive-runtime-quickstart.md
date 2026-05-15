# Adaptive Runtime — Quickstart (B1)

**Branch:** `feature/adaptive-briefing-diff`
**Full protocol:** `docs/general/b1-measurement-protocol.md`
**Charter:** `docs/general/adaptive-runtime-charter.md`

This page gets you running the B1 A/B test in three steps. The
measurement is **automatic** — Claude Code's `SessionStart` and `Stop`
hooks snapshot the tool-discipline state for you on every session.

---

## What B1 is

A SessionStart-injected "Tool Discipline Mirror" that shows the agent
which canonical neo tools/intents it hasn't invoked yet. The conjecture:
seeing the gap makes the agent invoke more of them next session.

It's an A/B test. Two arms:

| Arm | Env | Behavior |
|---|---|---|
| baseline | `NEO_BRIEFING_DIFF_DISABLE=1` | briefing only |
| treatment | env unset | briefing + mirror |

We capture pre/post-session snapshots from `neo_tool_stats` and compute
the per-arm average. **Ship criterion:** treatment arm shows ≥ +2 intents
on average vs baseline, sustained over ≥ 5 sessions per arm.

---

## Operator workflow (3 steps per session)

### 1. Set the arm before opening Claude Code

```bash
# Baseline run:
export NEO_BRIEFING_DIFF_DISABLE=1

# Treatment run:
unset NEO_BRIEFING_DIFF_DISABLE
```

That's it for setup. The hooks below handle the rest.

### 2. Open Claude Code, do your normal work

`SessionStart` hook fires automatically and:

- Runs the standard `BRIEFING` (always)
- Appends the Tool Discipline Mirror (treatment arm only)
- Triggers `b1-snapshot.sh pre` in background to capture pre-session
  state to `.neo/b1-measurements.csv`

When the session ends (Stop event), the `Stop` hook fires
`b1-snapshot.sh post` to capture post-session state.

You don't run anything manually. Just work.

### 3. After ≥ 5 sessions per arm, check the trajectory

```bash
./scripts/b1-measurement.sh report
```

Output is markdown. It renders per-arm averages, last 10 rows, and a
cross-arm comparison with the ship-criterion verdict.

---

## What "session" means here

A Claude Code session is the lifetime of one CLI instance.
`SessionStart` fires on `startup`, `resume`, `clear`, or `compact`;
`Stop` fires when the CLI exits or when you `/clear`. Each session
produces one pre + one post snapshot in the CSV.

`make rebuild-restart` does NOT start a new Claude Code session — it
restarts the MCP backend that Claude Code talks to. The same Claude
Code session continues across MCP restarts.

---

## Opt-outs

| Env var | Effect |
|---|---|
| `NEO_BRIEFING_DIFF_DISABLE=1` | Disables the mirror (= baseline arm) |
| `NEO_B1_SNAPSHOT_DISABLE=1` | Disables the snapshot hooks (no logging) |
| `NEO_READ_HOOK_DISABLE=1` | Disables the pre-Read large-file warning |

Disabling the snapshot hooks does NOT disable the mirror itself —
you can run an A/B trial that's invisible to logging, or log without
the mirror active. They're independent.

---

## Where things live

| Artifact | Path |
|---|---|
| Hook: snapshot trigger | `.claude/hooks/b1-snapshot.sh` |
| Hook: SessionStart briefing | `.claude/hooks/briefing.sh` (delegates) |
| Hook: tool-discipline mirror | `.claude/hooks/briefing-behavior-diff.sh` |
| Hook: Claude Code wiring | `.claude/settings.json` |
| Helper: snapshot/report CLI | `scripts/b1-measurement.sh` |
| CSV log (gitignored) | `.neo/b1-measurements.csv` |
| Per-session arm config | `NEO_BRIEFING_DIFF_DISABLE` env var |
| Full protocol | `docs/general/b1-measurement-protocol.md` |
| Charter (why this exists) | `docs/general/adaptive-runtime-charter.md` |

### Cross-workspace coverage (2026-05-15)

| Workspace | Mirror display | Snapshot writer | CSV destination |
|---|---|---|---|
| neoanvil | ✓ local hooks | ✓ local hooks | `neoanvil/.neo/b1-measurements.csv` |
| strategos | ✓ local hooks (port) | ✓ delegates to neoanvil's script | same shared CSV (tagged `workspace_boot=strategos`) |
| strategosia_frontend | ✗ not ported | ✗ not ported | n/a |

The CSV gained a `workspace_boot` column to disambiguate which workspace
fired a snapshot. The underlying `neo_tool_stats` counter is
Nexus-global, so the per-workspace mirror is a *display* push, not an
independent measurement source.

---

## Manual snapshot (still supported)

The hooks auto-snapshot, but you can also tag a manual checkpoint
mid-session — useful when you want to label something specific:

```bash
NEO_B1_NOTE="midpoint: finished phase 2.6" \
  ./scripts/b1-measurement.sh snapshot
```

The CSV `notes` column lets you correlate snapshots with what you were
doing at the time.

---

## What to do when the trial finishes

Per-arm avg sustained ≥ +2 intents → **graduate** B1 to
`feature/adaptive-runtime`, plan B2 (intent classifier hints).

Per-arm avg < +2 after 5+5 sessions → **discard** B1, return to
the charter, redesign or pivot.

Either way: open a memex commit summarizing the trial outcome
(`neo_memory action:commit topic:b1-trial-outcome ...`).
