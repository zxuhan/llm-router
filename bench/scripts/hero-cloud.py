#!/usr/bin/env python3
"""
Side-by-side concurrency-sweep chart, two model sizes.

Reads two bench-results-sweep/<model>/sessions=N/ trees and renders one PNG
with two subplots, styled to match modern AI-infra README aesthetics
(no top/right spines, soft grid, muted baselines, single accent series).

Usage:
    python3 bench/scripts/hero-cloud.py \\
        --left  bench-results-sweep-7B/qwen2.5-7b   --left-title  "Qwen2.5-7B" \\
        --right bench-results-sweep-14B/qwen2.5-14b --right-title "Qwen2.5-14B" \\
        --out docs/hero-cloud.png
"""

from __future__ import annotations

import argparse
import json
import re
import statistics
from pathlib import Path

import matplotlib.pyplot as plt
import matplotlib as mpl


# Visual language: muted greyscale baselines, single teal accent for ours.
# Keeps the eye on prefixaware without screaming.
PALETTE = {
    "roundrobin":  "#94a3b8",   # slate-400
    "random":      "#cbd5e1",   # slate-300
    "leastloaded": "#475569",   # slate-600
    "prefixaware": "#0d9488",   # teal-600
}
LABELS = {
    "roundrobin":  "round-robin",
    "random":      "random",
    "leastloaded": "least-loaded",
    "prefixaware": "prefix-aware",
}
MARKERS = {
    "roundrobin": "o", "random": "s", "leastloaded": "^", "prefixaware": "D",
}
ZORDER = {"roundrobin": 2, "random": 2, "leastloaded": 3, "prefixaware": 5}
LINEWIDTH = {"roundrobin": 1.4, "random": 1.4, "leastloaded": 1.4, "prefixaware": 2.6}


def apply_modern_style():
    """Install rcParams once. Closer to modern data-viz chart aesthetics."""
    mpl.rcParams.update({
        "font.family": "sans-serif",
        "font.sans-serif": ["Inter", "Helvetica Neue", "Arial", "DejaVu Sans"],
        "axes.spines.top": False,
        "axes.spines.right": False,
        "axes.spines.left": False,
        "axes.spines.bottom": True,
        "axes.edgecolor": "#cbd5e1",
        "axes.linewidth": 0.8,
        "axes.grid": True,
        "axes.grid.axis": "y",
        "grid.color": "#e2e8f0",
        "grid.linewidth": 0.7,
        "grid.alpha": 1.0,
        "axes.labelcolor": "#1e293b",
        "axes.labelsize": 11,
        "axes.titlesize": 13,
        "axes.titleweight": "semibold",
        "axes.titlecolor": "#0f172a",
        "xtick.color": "#475569",
        "ytick.color": "#475569",
        "xtick.labelsize": 10,
        "ytick.labelsize": 10,
        "xtick.major.size": 0,
        "ytick.major.size": 0,
        "xtick.major.pad": 6,
        "ytick.major.pad": 6,
        "legend.fontsize": 10,
        "legend.frameon": False,
        "legend.labelcolor": "#1e293b",
        "figure.facecolor": "white",
        "axes.facecolor": "white",
        "figure.dpi": 160,
        "savefig.dpi": 200,
        "savefig.bbox": "tight",
        "savefig.facecolor": "white",
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


def gather_series(model_root: Path):
    points = []
    for d in sorted(model_root.iterdir(), key=lambda d: sessions_from_dirname(d.name) or 0):
        n = sessions_from_dirname(d.name)
        if n is None:
            continue
        points.append((n, d))
    series = {}
    for strategy in PALETTE:
        xs, ys, errs = [], [], []
        for sessions, sd in points:
            summaries = load_strategy_runs(sd, strategy)
            if not summaries:
                continue
            values = [s["ttft"]["p50"] / 1e6 for s in summaries]
            xs.append(sessions)
            ys.append(statistics.fmean(values))
            errs.append(statistics.stdev(values) if len(values) >= 2 else 0.0)
        series[strategy] = (xs, ys, errs)
    return series


def draw(ax, model_root: Path, title: str, show_legend: bool, headline_text: str):
    series = gather_series(model_root)
    for strategy in PALETTE:
        xs, ys, errs = series[strategy]
        if not xs:
            continue
        ax.errorbar(
            xs, ys, yerr=errs,
            color=PALETTE[strategy], marker=MARKERS[strategy], label=LABELS[strategy],
            linewidth=LINEWIDTH[strategy], capsize=3, zorder=ZORDER[strategy],
            markersize=6.5, markeredgewidth=0,
            elinewidth=0.9, ecolor=PALETTE[strategy], alpha=0.95,
        )

    ax.set_title(title, pad=12, loc="left")
    ax.set_xlabel("Concurrent sessions (4 workers)")
    ax.set_ylabel("TTFT p50 (ms)")
    ax.set_xticks([4, 8, 12, 16, 24])

    # Headline text in the corner: cleaner than an annotation arrow.
    ax.text(
        0.98, 0.04, headline_text,
        transform=ax.transAxes,
        ha="right", va="bottom",
        fontsize=10, color="#0d9488", fontweight="semibold",
        bbox=dict(boxstyle="round,pad=0.4", facecolor="#f0fdfa",
                  edgecolor="#99f6e4", linewidth=0.8),
    )

    if show_legend:
        leg = ax.legend(loc="upper left", borderpad=0.4, handletextpad=0.5,
                        labelspacing=0.4)
        for text in leg.get_texts():
            text.set_color("#1e293b")


def headline_for(model_root: Path) -> str:
    """Compute 'PA p50 = X ms (slope +Y ms)' for the corner annotation."""
    series = gather_series(model_root)
    xs, ys, _ = series["prefixaware"]
    if not xs or len(ys) < 2:
        return ""
    slope = ys[-1] - ys[0]
    return f"prefix-aware: +{slope:.0f} ms across 6x concurrency"


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--left",  required=True)
    ap.add_argument("--left-title",  default="Left")
    ap.add_argument("--right", required=True)
    ap.add_argument("--right-title", default="Right")
    ap.add_argument("--out",   required=True)
    args = ap.parse_args()

    apply_modern_style()
    fig, (ax_l, ax_r) = plt.subplots(1, 2, figsize=(13, 4.6))

    draw(ax_l, Path(args.left),  args.left_title,
         show_legend=True,  headline_text=headline_for(Path(args.left)))
    draw(ax_r, Path(args.right), args.right_title,
         show_legend=False, headline_text=headline_for(Path(args.right)))

    fig.suptitle("Routing strategy vs concurrency, on 4x A100 80GB SXM",
                 fontsize=14, fontweight="semibold", color="#0f172a", y=1.02)
    fig.text(
        0.5, -0.06,
        "vLLM 0.6.4 with prefix caching, 3 seeds per point, mean +/- 1 stddev. Lower is better.",
        ha="center", fontsize=9.5, color="#64748b",
    )

    fig.tight_layout(w_pad=4.0)
    fig.savefig(args.out)
    print(f"wrote {args.out}")


if __name__ == "__main__":
    main()
