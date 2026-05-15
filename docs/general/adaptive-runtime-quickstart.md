# Adaptive Runtime — Quickstart (B1)

**Branch:** `feature/adaptive-briefing-diff`
**Full protocol:** `docs/general/b1-measurement-protocol.md`
**Charter:** `docs/general/adaptive-runtime-charter.md`

This page describes the B1 A/B test. There are **zero operator steps** —
the trial runs itself. Claude Code's `SessionStart` and `Stop` hooks
auto-snapshot, auto-rotate the arm, and auto-tag every CSV row. You
work normally; check the report when curious.

---

## What B1 is

A SessionStart-injected "Tool Discipline Mirror" that shows the agent
which canonical neo tools/intents it hasn't invoked yet. The conjecture:
seeing the gap makes the agent invoke more of them next session.

It's an A/B test. Two arms, **auto-alternated by the script**:

| Arm | Trigger | Behavior |
|---|---|---|
| baseline | last `auto-post` row was `treatment` (or CSV empty) | briefing only, mirror silenced |
| treatment | last `auto-post` row was `baseline` | briefing + mirror |

We capture pre/post-session snapshots from `neo_tool_stats` and compute
the per-arm average. **Ship criterion:** treatment arm shows ≥ +2 intents
on average vs baseline, sustained over ≥ 5 sessions per arm.

---

## Operator workflow (1 step)

### Just work normally.

The trial runs itself. Every session alternates arm. Every pre + post
snapshot is tagged automatically. After ~10 sessions you have 5+5.

When you want to see how it's going:

```bash
./scripts/b1-measurement.sh report
```

Output is markdown. It renders per-arm averages, last 10 rows, and a
cross-arm comparison with the ship-criterion verdict.

If you want to force a specific arm for debugging or to extend one
arm beyond 5 sessions:

```bash
NEO_B1_FORCE_ARM=baseline   # next session forced to baseline
NEO_B1_FORCE_ARM=treatment  # next session forced to treatment
```

Set this in the shell **before** opening Claude Code if you want to
override. Unset for auto.

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

## Opt-outs (kill switches — not arm selectors)

| Env var | Effect |
|---|---|
| `NEO_BRIEFING_DIFF_DISABLE=1` | Hides the mirror display. Does NOT affect arm rotation or CSV logging — the snapshot still tags rows by the auto-derived arm. Use to silence visual noise without breaking the trial. |
| `NEO_B1_SNAPSHOT_DISABLE=1` | Disables CSV logging entirely. Arm rotation pauses (no new rows = no flip). Use when you want to run sessions outside the trial. |
| `NEO_READ_HOOK_DISABLE=1` | Disables the pre-Read large-file warning (unrelated to B1). |
| `NEO_B1_FORCE_ARM=baseline\|treatment` | Overrides auto-arm for one session. For debugging or extending an arm. |

Kill switches are independent. You can mirror-off + snapshot-on, or
vice versa.

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
doing at the time. Manual snapshots use the same auto-arm as the
session's automatic ones (won't break the rotation).

## How auto-rotation works (under the hood)

`./scripts/b1-measurement.sh arm` decides which arm the **next** session
belongs to by reading the CSV:

1. Look for `NEO_B1_FORCE_ARM` env var → if set, use it
2. Otherwise read CSV, find the most recent row with `notes` starting
   with `auto-post`
3. Return the **opposite** of that row's `treatment` column
4. Empty CSV → `baseline` (always start there)

Both the mirror hook and the snapshot hook query `arm` independently
within milliseconds of each other — they always agree because they
both see the same CSV state.

The PRE snapshot writes a row with the same arm; the POST snapshot
writes another row with the same arm (because between them, the CSV
only got `auto-pre` rows which `arm` ignores). After POST writes its
row, the *next* session sees the new `auto-post` and flips.

---

## What to do when the trial finishes

Run `./scripts/b1-measurement.sh report` to see the verdict.

- Per-arm avg sustained ≥ +2 intents → **graduate** B1 to
  `feature/adaptive-runtime`, plan B2 (intent classifier hints).
- Per-arm avg < +2 after 5+5 sessions → **discard** B1, return to
  the charter, redesign or pivot.

Either way: open a memex commit summarizing the trial outcome
(`neo_memory action:commit topic:b1-trial-outcome ...`).

The arm rotation keeps running even after 5+5. To formally stop the
trial, set `NEO_B1_SNAPSHOT_DISABLE=1` in your shell config — no
more CSV rows = no more rotation, but the mirror keeps rendering if
the most recent `auto-post` was a baseline (i.e. it lands on
treatment by default after a clean stop).
