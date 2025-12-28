#!/usr/bin/env python3
"""
Vertically-stacked concurrency-sweep chart, two model sizes.

Reads two bench-results-sweep/<model>/sessions=N/ trees and renders a single
PNG with two stacked panels (7B on top, 14B below). Vertical orientation
keeps each panel wide so axis labels and legends are readable inline in a
README. Styling targets the visual language of modern AI infra blog posts:
no top/right/left spines, soft horizontal grid only, muted slate baselines,
single teal accent for the headline series.

Usage:
    python3 bench/scripts/hero-cloud.py \\
        --top    bench-results-sweep-7B/qwen2.5-7b   --top-title  "Qwen2.5-7B" \\
        --bottom bench-results-sweep-14B/qwen2.5-14b --bottom-title "Qwen2.5-14B" \\
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
LINEWIDTH = {"roundrobin": 1.6, "random": 1.6, "leastloaded": 1.6, "prefixaware": 3.0}


def apply_modern_style():
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
        "axes.labelsize": 13,
        "axes.titlesize": 16,
        "axes.titleweight": "semibold",
        "axes.titlecolor": "#0f172a",
        "xtick.color": "#475569",
        "ytick.color": "#475569",
        "xtick.labelsize": 12,
        "ytick.labelsize": 12,
        "xtick.major.size": 0,
        "ytick.major.size": 0,
        "xtick.major.pad": 8,
        "ytick.major.pad": 8,
        "legend.fontsize": 12,
        "legend.frameon": False,
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


def headline_for(model_root: Path) -> str:
    series = gather_series(model_root)
    xs, ys, _ = series["prefixaware"]
    if not xs or len(ys) < 2:
        return ""
    slope = ys[-1] - ys[0]
    return f"prefix-aware: +{slope:.0f} ms across 6x concurrency"


def draw(ax, model_root: Path, title: str, show_legend: bool, headline_text: str):
    series = gather_series(model_root)
    for strategy in PALETTE:
        xs, ys, errs = series[strategy]
        if not xs:
            continue
        ax.errorbar(
            xs, ys, yerr=errs,
            color=PALETTE[strategy], marker=MARKERS[strategy], label=LABELS[strategy],
            linewidth=LINEWIDTH[strategy], capsize=4, zorder=ZORDER[strategy],
            markersize=8, markeredgewidth=0,
            elinewidth=1.0, ecolor=PALETTE[strategy], alpha=0.95,
        )

    ax.set_title(title, pad=14, loc="left")
    ax.set_xlabel("Concurrent sessions across 4 workers")
    ax.set_ylabel("TTFT p50 (ms)")
    ax.set_xticks([4, 8, 12, 16, 24])

    ax.text(
        0.985, 0.06, headline_text,
        transform=ax.transAxes,
        ha="right", va="bottom",
        fontsize=12, color="#0d9488", fontweight="semibold",
        bbox=dict(boxstyle="round,pad=0.55", facecolor="#f0fdfa",
                  edgecolor="#5eead4", linewidth=1.0),
    )

    if show_legend:
        leg = ax.legend(loc="upper left", borderpad=0.5, handletextpad=0.6,
                        labelspacing=0.5, ncol=4, columnspacing=1.6)
        for text in leg.get_texts():
            text.set_color("#1e293b")


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--top",    required=True)
    ap.add_argument("--top-title",    default="Top")
    ap.add_argument("--bottom", required=True)
    ap.add_argument("--bottom-title", default="Bottom")
    ap.add_argument("--out",    required=True)
    args = ap.parse_args()

    apply_modern_style()
    fig, (ax_top, ax_bot) = plt.subplots(2, 1, figsize=(11, 9.5))

    draw(ax_top, Path(args.top),    args.top_title,
         show_legend=True,  headline_text=headline_for(Path(args.top)))
    draw(ax_bot, Path(args.bottom), args.bottom_title,
         show_legend=False, headline_text=headline_for(Path(args.bottom)))

    fig.suptitle("Routing strategy vs concurrency on 4x A100 80GB SXM",
                 fontsize=17, fontweight="semibold", color="#0f172a", y=1.00)
    fig.text(
        0.5, -0.025,
        "vLLM 0.6.4 with prefix caching enabled. Three seeds per point. "
        "Mean and one sample stddev. Lower TTFT is better.",
        ha="center", fontsize=11, color="#64748b",
    )

    fig.tight_layout(h_pad=3.5, rect=[0, 0, 1, 0.99])
    fig.savefig(args.out)
    print(f"wrote {args.out}")


if __name__ == "__main__":
    main()
