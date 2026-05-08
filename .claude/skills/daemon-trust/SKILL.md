---
name: daemon-trust
description: Trust score dashboard for PILAR XXVII. Per-pattern α/β counters, tier history with demote/promote events, decay-aware lower bound, and a comparison "what trust said this morning vs now". Use when auditing model performance per (pattern, scope), debugging unexpected tier transitions, or preparing a trust review before activating daemon mode. Task-mode skill (invoke with `/daemon-trust [filter_pattern]`).
---

# Daemon Trust — Score Dashboard

> Read-only deep-dive into the trust system that gates daemon
> auto-approval. Where `/daemon-flow` shows "what the daemon sees right
> now," `/daemon-trust` shows "how each (pattern, scope) bucket got
> here."

## When to invoke

- Auditing model performance per (pattern, scope) over time
- Debugging "why did this pattern demote to L0?"
- Preparing a trust-state review before flipping NEO_SERVER_MODE=daemon
- Deciding whether to `TrustWarmup` a new pattern based on operator
  intuition rather than waiting for evidence
- Investigating a `skipped_corrupt > 0` signal from `trust_status`

## What the skill outputs

A markdown report with five sections per filtered pattern:

```
## Daemon Trust — refactor:.go:pkg/state

### Counters (decay-aware as of now)
- α (successes + prior):  41.0   (raw α=42.5; decay factor = 0.96 over 4d)
- β (failures + prior):    3.0   (raw β=3.0; no decay needed)
- TotalExecutions:        42
- ManualWarmup:           false

### Trust math
- Point estimate (α/(α+β)):                  0.93
- LowerBound (mean − 1.96·σ, 95% conf):      0.79  ← drives tier
- Tier band:  L1 (0.65 ≤ LB < 0.85)

### Tier band thresholds (for context)
| Band | LB threshold | Auto-approval |
|------|--------------|----------------|
| L3   | LB ≥ 0.95    | aggressive     |
| L2   | 0.85 ≤ LB    | tests must pass |
| L1   | 0.65 ≤ LB    | spot-check 1/5  |
| L0   | LB < 0.65    | always prompt   |

### Recent transitions (from daemon_results)
- 2026-04-30 14:22 — approve TASK-041 (operator: "looks correct")
- 2026-04-30 13:08 — approve TASK-040
- 2026-04-30 11:14 — reject  TASK-039 (quality: "wrong API call")
- 2026-04-30 09:55 — approve TASK-038
- 2026-04-30 08:21 — approve TASK-037
   ↳ tier promote: L0 → L1 at this point (gate cleared, LB ≥ 0.65)

### Compare vs 24h ago
- α:    36 → 41   (+5 successes)
- β:    3  → 3    (no change)
- LB:   0.74 → 0.79  (+0.05 — promotion to L2 at 0.85 in next ~10 successes if rate holds)
- Tier: L1 → L1 (no change yet)
```

## Argument

Optional `filter_pattern` substring (case-insensitive). Without it,
the skill shows the top 5 buckets ranked by total absolute change in
LB over the last 24h — most-active patterns first. With it, restricts
to matching patterns.

Examples:
- `/daemon-trust` — top 5 most-active patterns, full per-pattern detail
- `/daemon-trust refactor` — only refactor:* buckets
- `/daemon-trust unknown` — the catch-all migration bucket (138.C.7)

## How the skill maps to MCP actions

| Section            | MCP action                                     |
|--------------------|------------------------------------------------|
| Counters           | `neo_daemon(action: trust_status, top: 0)` for full state |
| Trust math         | computed client-side from α/β (no extra call)  |
| Tier thresholds    | static reference table                         |
| Recent transitions | `neo_daemon(action: trust_status)` exposes only current state — recent transitions need `daemon_results` bucket scan (deferred to dashboard implementation) |
| Compare vs 24h ago | snapshot diff requires a daemon_trust_history bucket (138.C.4 deferred sub-feature) |

**Implementation note:** sections 4-5 surface the limitations of the
current data model. The history is implicit (you can reconstruct it
by walking daemon_results sorted by completed_at, but that's
expensive). When the daemon gets heavy use, consider adding an
append-only `daemon_trust_history` bucket so this skill renders fast.

## Decay sensitivity

The skill highlights when `decay factor < 0.95` for any pattern —
that's a signal that the bucket has been idle long enough for
evidence to dampen toward the prior. Operators eyeballing
"this used to be L2 but now shows L0" will see the decay reason
called out:

```
⚠️ refactor:.go:pkg/foo idle 12d — decay factor 0.83 has degraded
   accumulated evidence. LB dropped from 0.82 (12d ago) to 0.61 (now)
   despite no new failures. Consider TrustWarmup if the pattern is
   still trustworthy.
```

## Pair-mode safety

`trust_status` (the only MCP action this skill calls) is read-only
and exempted from pair-mode prohibition. Safe to invoke at any time.

## Related

- `/daemon-flow` — operator UI for the live daemon loop
- `docs/pilar-xxvii-daemon-mcp.md` — operator runbook
- `docs/adr/ADR-009-daemon-trust-scoring.md` — design rationale of
  the Beta-distribution + decay + tier-gate stack
