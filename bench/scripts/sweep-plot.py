#!/usr/bin/env python3
"""
Plot TTFT-vs-concurrency from a concurrency-sweep result tree.

The wrapper bench/scripts/concurrency-sweep.sh lays results out as:

    bench-results-sweep/<model>/sessions=N/
        real-roundrobin-run1.json
        real-roundrobin-run2.json
        ...
        real-prefixaware-runN.json

Each *.json is one Summary (see internal/trace/report.go::Summary). We
aggregate across runs (mean + stddev for error bars) and plot one line per
strategy with concurrency on the X axis.

Usage:
    python3 bench/scripts/sweep-plot.py \\
        --input bench-results-sweep/qwen2.5-7b \\
        --out docs/sweep-qwen2.5-7b.png

The chart visualizes the cache-vs-load trade-off:
- At low concurrency, prefixaware should win (cache benefit > queuing).
- At high concurrency, prefixaware should tie roundrobin (safety valve
  spills enough to spread load).
"""

from __future__ import annotations

import argparse
import json
import re
import statistics
from pathlib import Path

import matplotlib.pyplot as plt


STRATEGY_STYLE = {
    # (color, marker, label, zorder)
    "roundrobin":  ("#888888", "o", "round-robin",        2),
    "random":      ("#aaaaaa", "s", "random",             2),
    "leastloaded": ("#666666", "^", "least-loaded",       2),
    "prefixaware": ("#0f9d8a", "D", "prefix-aware (ours)", 5),
}


def sessions_from_dirname(name: str) -> int | None:
    m = re.match(r"sessions=(\d+)", name)
    return int(m.group(1)) if m else None


def load_strategy_runs(sessions_dir: Path, strategy: str) -> list[dict]:
    """Return the list of Summary dicts for one (sessions, strategy) point."""
    out = []
    for f in sorted(sessions_dir.glob(f"real-{strategy}-run*.json")):
        with open(f) as fh:
            doc = json.load(fh)
        for s in doc.get("summaries", []):
            if s.get("strategy") == strategy:
                out.append(s)
    return out


def ttft_p50_ms(summary: dict) -> float:
    """TTFT p50 in milliseconds. Internal field is nanoseconds."""
    return summary["ttft"]["p50"] / 1e6


def main() -> None:
    ap = argparse.ArgumentParser()
    ap.add_argument("--input", required=True,
                    help="path to bench-results-sweep/<model>/")
    ap.add_argument("--out", required=True, help="output PNG path")
    ap.add_argument("--metric", default="ttft_p50",
                    choices=["ttft_p50", "ttft_p95", "throughput_rps"])
    args = ap.parse_args()

    root = Path(args.input)
    if not root.is_dir():
        raise SystemExit(f"input dir does not exist: {root}")

    # Discover available concurrency points.
    points = []
    for d in sorted(root.iterdir(), key=lambda d: sessions_from_dirname(d.name) or 0):
        n = sessions_from_dirname(d.name)
        if n is None:
            continue
        points.append((n, d))
    if not points:
        raise SystemExit(f"no sessions=* subdirs under {root}")

    fig, ax = plt.subplots(figsize=(8, 5.2))

    for strategy, (color, marker, label, zorder) in STRATEGY_STYLE.items():
        xs, ys, errs = [], [], []
        for sessions, sd in points:
            summaries = load_strategy_runs(sd, strategy)
            if not summaries:
                continue
            if args.metric == "ttft_p50":
                values = [ttft_p50_ms(s) for s in summaries]
            elif args.metric == "ttft_p95":
                values = [s["ttft"]["p95"] / 1e6 for s in summaries]
            else:
                values = [s["throughput_rps"] for s in summaries]

            xs.append(sessions)
            ys.append(statistics.fmean(values))
            # Sample stddev when we have >=2 runs; 0 otherwise.
            errs.append(statistics.stdev(values) if len(values) >= 2 else 0.0)

        if not xs:
            continue
        linewidth = 2.5 if strategy == "prefixaware" else 1.5
        ax.errorbar(xs, ys, yerr=errs, color=color, marker=marker, label=label,
                    linewidth=linewidth, capsize=4, zorder=zorder)

    ax.set_xlabel("Concurrent sessions (across 4 workers)")
    if args.metric == "ttft_p50":
        ax.set_ylabel("TTFT p50 (ms, lower is better)")
    elif args.metric == "ttft_p95":
        ax.set_ylabel("TTFT p95 (ms, lower is better)")
    else:
        ax.set_ylabel("Throughput (RPS, higher is better)")
    ax.set_title(f"{root.name}: routing strategy vs concurrency")
    ax.grid(True, alpha=0.3)
    ax.legend(loc="best", framealpha=0.95)

    fig.tight_layout()
    fig.savefig(args.out, dpi=150)
    print(f"wrote {args.out}")


if __name__ == "__main__":
    main()
