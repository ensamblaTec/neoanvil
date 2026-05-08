#!/usr/bin/env python3
"""
bench-regression.py — PILAR LXIX / Épica 366.B.

Compares Go `go test -bench=` output against a JSON baseline to detect
performance regressions in PRs. Two modes:

  --capture <bench-output>       → write a new baseline JSON to stdout
  <baseline.json> <bench-output> → compare; exit 1 on regression

The baseline JSON format is intentionally minimal:

  {
    "commit": "<git sha>",
    "captured_at": "<RFC3339>",
    "benchmarks": {
      "BenchmarkHNSWSearch": {"ns_op": 234.5, "allocs_op": 3, "bytes_op": 1024},
      ...
    }
  }

Regression thresholds (defaults, CLI-overridable):
  - ns_op:    >5% slower   → FAIL
  - allocs:   any increase → FAIL
  - bytes:    >10% growth  → WARN only (some tests legit grow alloc)

Benchmarks not present in the baseline are skipped (new bench = no baseline).
Benchmarks present in baseline but absent from current = WARN (removed?).
"""

import argparse
import json
import re
import sys
import subprocess
from datetime import datetime, timezone
from typing import Dict, Optional


# Matches lines like: BenchmarkFoo-8    1000   1234 ns/op   5678 B/op   42 allocs/op
BENCH_RE = re.compile(
    r"^(Benchmark[\w/]+?)(?:-\d+)?\s+\d+\s+"
    r"([\d.]+)\s+ns/op"
    r"(?:\s+([\d.]+)\s+B/op)?"
    r"(?:\s+([\d.]+)\s+allocs/op)?"
)


def parse_bench_output(text: str) -> Dict[str, Dict[str, float]]:
    """Parse Go bench output text into {name → {ns_op, bytes_op, allocs_op}}."""
    out: Dict[str, Dict[str, float]] = {}
    for line in text.splitlines():
        m = BENCH_RE.match(line.strip())
        if not m:
            continue
        name, ns, bytes_op, allocs = m.group(1), m.group(2), m.group(3), m.group(4)
        entry: Dict[str, float] = {"ns_op": float(ns)}
        if bytes_op is not None:
            entry["bytes_op"] = float(bytes_op)
        if allocs is not None:
            entry["allocs_op"] = float(allocs)
        out[name] = entry
    return out


def capture_mode(bench_output_path: str) -> int:
    """Emit baseline JSON to stdout from a bench output file."""
    with open(bench_output_path, "r") as f:
        text = f.read()
    benches = parse_bench_output(text)
    if not benches:
        print("[bench-regression] no benchmarks parsed — check bench output format", file=sys.stderr)
        return 2
    commit = "unknown"
    try:
        commit = subprocess.check_output(
            ["git", "rev-parse", "HEAD"], stderr=subprocess.DEVNULL
        ).decode().strip()
    except Exception:
        pass
    baseline = {
        "commit": commit,
        "captured_at": datetime.now(timezone.utc).isoformat(),
        "benchmarks": benches,
    }
    json.dump(baseline, sys.stdout, indent=2, sort_keys=True)
    print(file=sys.stdout)  # trailing newline
    print(f"[bench-regression] captured {len(benches)} benchmarks from commit {commit[:8]}",
          file=sys.stderr)
    return 0


def compare_mode(
    baseline_path: str,
    current_output_path: str,
    ns_threshold: float,
    bytes_threshold: float,
) -> int:
    """Compare current bench output against baseline. Exit 1 on regression."""
    with open(baseline_path, "r") as f:
        baseline = json.load(f)
    baseline_benches: Dict[str, Dict[str, float]] = baseline.get("benchmarks", {})
    with open(current_output_path, "r") as f:
        current_benches = parse_bench_output(f.read())

    regressions = []
    warnings = []
    passed = []

    for name, base in baseline_benches.items():
        curr = current_benches.get(name)
        if curr is None:
            warnings.append(f"BENCH MISSING: {name} present in baseline but not in current run")
            continue

        # ns/op regression
        base_ns = base.get("ns_op", 0.0)
        curr_ns = curr.get("ns_op", 0.0)
        if base_ns > 0:
            delta_pct = (curr_ns - base_ns) / base_ns * 100
            if delta_pct > ns_threshold:
                regressions.append(
                    f"REGRESSION: {name} ns/op {base_ns:.1f} → {curr_ns:.1f} "
                    f"({delta_pct:+.1f}% > +{ns_threshold:.0f}%)"
                )
            elif abs(delta_pct) > 2:
                note = "faster" if delta_pct < 0 else "slower"
                passed.append(f"OK: {name} {delta_pct:+.1f}% ({note})")
            else:
                passed.append(f"OK: {name} {delta_pct:+.1f}%")

        # allocs/op: any increase is a regression
        base_allocs = base.get("allocs_op", 0.0)
        curr_allocs = curr.get("allocs_op", 0.0)
        if curr_allocs > base_allocs:
            regressions.append(
                f"REGRESSION: {name} allocs/op {base_allocs:.0f} → {curr_allocs:.0f} "
                f"(non-zero increase)"
            )

        # bytes/op: warn-only over threshold
        base_bytes = base.get("bytes_op", 0.0)
        curr_bytes = curr.get("bytes_op", 0.0)
        if base_bytes > 0:
            byte_delta_pct = (curr_bytes - base_bytes) / base_bytes * 100
            if byte_delta_pct > bytes_threshold:
                warnings.append(
                    f"WARN: {name} B/op {base_bytes:.0f} → {curr_bytes:.0f} "
                    f"({byte_delta_pct:+.1f}% > +{bytes_threshold:.0f}%)"
                )

    new_benches = set(current_benches.keys()) - set(baseline_benches.keys())
    if new_benches:
        warnings.append(f"NEW BENCHMARKS (not in baseline): {', '.join(sorted(new_benches))}")

    # Report
    for line in passed[:5]:  # sample of OK lines
        print(line)
    if len(passed) > 5:
        print(f"... and {len(passed) - 5} more OK benchmarks")
    for line in warnings:
        print(line, file=sys.stderr)
    for line in regressions:
        print(line, file=sys.stderr)

    if regressions:
        print(
            f"\n[bench-regression] \033[31m✗ {len(regressions)} regression(s)\033[0m "
            f"vs baseline ({len(passed)} ok, {len(warnings)} warnings)",
            file=sys.stderr,
        )
        return 1
    print(
        f"\n[bench-regression] \033[32m✓ no regressions\033[0m vs baseline "
        f"({len(passed)} ok, {len(warnings)} warnings)",
        file=sys.stderr,
    )
    return 0


def main() -> int:
    ap = argparse.ArgumentParser(description="Go benchmark regression checker")
    ap.add_argument("--capture", metavar="BENCH_OUT", help="write baseline JSON to stdout from bench output")
    ap.add_argument("--ns-threshold", type=float, default=5.0,
                    help="%% regression tolerance for ns/op (default 5.0)")
    ap.add_argument("--bytes-threshold", type=float, default=10.0,
                    help="%% regression tolerance for B/op (warn only, default 10.0)")
    ap.add_argument("positional", nargs="*",
                    help="baseline.json bench-output.txt (for compare mode)")
    args = ap.parse_args()

    if args.capture:
        return capture_mode(args.capture)

    if len(args.positional) != 2:
        ap.print_help(sys.stderr)
        return 2
    baseline_path, current_path = args.positional
    return compare_mode(baseline_path, current_path, args.ns_threshold, args.bytes_threshold)


if __name__ == "__main__":
    sys.exit(main())
