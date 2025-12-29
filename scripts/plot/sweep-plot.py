#!/usr/bin/env python3
"""
Single-panel concurrency-sweep chart for one model.

Usage:
    python3 scripts/plot/sweep-plot.py \\
        --input bench-results-sweep/qwen2.5-7b \\
        --out docs/images/sweep-7b.png
"""

from __future__ import annotations

import argparse
import json
import re
import statistics
from pathlib import Path

import matplotlib.pyplot as plt
import matplotlib as mpl


PALETTE = {
    "roundrobin":  "#94a3b8",
    "random":      "#cbd5e1",
    "leastloaded": "#475569",
    "prefixaware": "#0d9488",
}
LABELS = {
    "roundrobin":  "round-robin",
    "random":      "random",
    "leastloaded": "least-loaded",
    "prefixaware": "prefix-aware",
}
MARKERS  = {"roundrobin": "o", "random": "s", "leastloaded": "^", "prefixaware": "D"}
ZORDER   = {"roundrobin": 2, "random": 2, "leastloaded": 3, "prefixaware": 5}
LINEWIDTH = {"roundrobin": 1.4, "random": 1.4, "leastloaded": 1.4, "prefixaware": 2.6}


def apply_modern_style():
    mpl.rcParams.update({
        "font.family": "sans-serif",
        "font.sans-serif": ["Inter", "Helvetica Neue", "Arial", "DejaVu Sans"],
        "axes.spines.top": False, "axes.spines.right": False,
        "axes.spines.left": False, "axes.spines.bottom": True,
        "axes.edgecolor": "#cbd5e1", "axes.linewidth": 0.8,
        "axes.grid": True, "axes.grid.axis": "y",
        "grid.color": "#e2e8f0", "grid.linewidth": 0.7, "grid.alpha": 1.0,
        "axes.labelcolor": "#1e293b", "axes.labelsize": 11,
        "axes.titlesize": 13, "axes.titleweight": "semibold",
        "axes.titlecolor": "#0f172a",
        "xtick.color": "#475569", "ytick.color": "#475569",
        "xtick.labelsize": 10, "ytick.labelsize": 10,
        "xtick.major.size": 0, "ytick.major.size": 0,
        "xtick.major.pad": 6, "ytick.major.pad": 6,
        "legend.fontsize": 10, "legend.frameon": False,
        "figure.facecolor": "white", "axes.facecolor": "white",
        "figure.dpi": 160, "savefig.dpi": 200,
        "savefig.bbox": "tight", "savefig.facecolor": "white",
    })


def sessions_from_dirname(name: str) -> int | None:
    m = re.match(r"sessions=(\d+)", name)
    return int(m.group(1)) if m else None


def load_strategy_runs(sessions_dir: Path, strategy: str) -> list[dict]:
    out = []
    for f in sorted(sessions_dir.glob(f"real-{strategy}-run*.json")):
        with open(f) as fh:
            doc = json.load(fh)
        for s in doc.get("summaries", []):
            if s.get("strategy") == strategy:
                out.append(s)
    return out


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--input", required=True)
    ap.add_argument("--out", required=True)
    ap.add_argument("--metric", default="ttft_p50",
                    choices=["ttft_p50", "ttft_p95", "throughput_rps"])
    args = ap.parse_args()

    root = Path(args.input)
    if not root.is_dir():
        raise SystemExit(f"input dir does not exist: {root}")

    points = []
    for d in sorted(root.iterdir(), key=lambda d: sessions_from_dirname(d.name) or 0):
        n = sessions_from_dirname(d.name)
        if n is None:
            continue
        points.append((n, d))
    if not points:
        raise SystemExit(f"no sessions=* subdirs under {root}")

    apply_modern_style()
    fig, ax = plt.subplots(figsize=(8, 4.8))

    for strategy in PALETTE:
        xs, ys, errs = [], [], []
        for sessions, sd in points:
            summaries = load_strategy_runs(sd, strategy)
            if not summaries:
                continue
            if args.metric == "ttft_p50":
                values = [s["ttft"]["p50"] / 1e6 for s in summaries]
            elif args.metric == "ttft_p95":
                values = [s["ttft"]["p95"] / 1e6 for s in summaries]
            else:
                values = [s["throughput_rps"] for s in summaries]
            xs.append(sessions)
            ys.append(statistics.fmean(values))
            errs.append(statistics.stdev(values) if len(values) >= 2 else 0.0)

        if not xs:
            continue
        ax.errorbar(
            xs, ys, yerr=errs,
            color=PALETTE[strategy], marker=MARKERS[strategy], label=LABELS[strategy],
            linewidth=LINEWIDTH[strategy], capsize=3, zorder=ZORDER[strategy],
            markersize=6.5, markeredgewidth=0,
            elinewidth=0.9, ecolor=PALETTE[strategy], alpha=0.95,
        )

    ax.set_xlabel("Concurrent sessions (4 workers)")
    if args.metric == "ttft_p50":
        ax.set_ylabel("TTFT p50 (ms)")
    elif args.metric == "ttft_p95":
        ax.set_ylabel("TTFT p95 (ms)")
    else:
        ax.set_ylabel("Throughput (RPS)")
    ax.set_xticks([4, 8, 12, 16, 24])
    ax.set_title(f"{root.name}: routing strategy vs concurrency", loc="left", pad=12)
    ax.legend(loc="upper left", borderpad=0.4, handletextpad=0.5, labelspacing=0.4)

    fig.tight_layout()
    fig.savefig(args.out)
    print(f"wrote {args.out}")


if __name__ == "__main__":
    main()
