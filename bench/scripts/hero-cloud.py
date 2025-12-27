#!/usr/bin/env python3
"""
Hero chart: side-by-side concurrency sweep at two model sizes.

Reads two bench-results-sweep/<model>/sessions=N/ trees (one per model size)
and renders a single PNG with two subplots, so the README shows the same
"PA stays flat under load" property at two scales.

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


# (color, marker, label, zorder, linewidth)
STRATEGY_STYLE = {
    "roundrobin":  ("#888888", "o", "round-robin",         2, 1.5),
    "random":      ("#bbbbbb", "s", "random",              2, 1.5),
    "leastloaded": ("#555555", "^", "least-loaded",        3, 1.5),
    "prefixaware": ("#0f9d8a", "D", "prefix-aware (ours)", 5, 2.8),
}


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
    """Return {strategy -> ([sessions...], [p50_ms...], [stddev_ms...])}."""
    points = []
    for d in sorted(model_root.iterdir(), key=lambda d: sessions_from_dirname(d.name) or 0):
        n = sessions_from_dirname(d.name)
        if n is None:
            continue
        points.append((n, d))

    series: dict[str, tuple[list[int], list[float], list[float]]] = {}
    for strategy in STRATEGY_STYLE:
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


def draw(ax, model_root: Path, title: str):
    series = gather_series(model_root)
    for strategy, (color, marker, label, zorder, lw) in STRATEGY_STYLE.items():
        xs, ys, errs = series[strategy]
        if not xs:
            continue
        ax.errorbar(xs, ys, yerr=errs, color=color, marker=marker, label=label,
                    linewidth=lw, capsize=4, zorder=zorder, markersize=6.5)
    ax.set_xlabel("Concurrent sessions (4 workers)", fontsize=11)
    ax.set_ylabel("TTFT p50 (ms)  --  lower is better", fontsize=11)
    ax.set_title(title, fontsize=13, fontweight="bold")
    ax.grid(True, alpha=0.25)
    ax.set_xticks([4, 8, 12, 16, 24])
    # Annotate the rightmost PA point so the eye lands on the headline.
    if series.get("prefixaware") and series["prefixaware"][0]:
        x_last = series["prefixaware"][0][-1]
        y_last = series["prefixaware"][1][-1]
        ax.annotate(f"PA p50 = {y_last:.0f} ms",
                    xy=(x_last, y_last), xytext=(-90, -22),
                    textcoords="offset points",
                    fontsize=10, color="#0f9d8a", fontweight="bold",
                    arrowprops=dict(arrowstyle="-", color="#0f9d8a", lw=1, alpha=0.6))


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--left",  required=True, help="path to first model's sweep dir")
    ap.add_argument("--left-title",  default="Left")
    ap.add_argument("--right", required=True, help="path to second model's sweep dir")
    ap.add_argument("--right-title", default="Right")
    ap.add_argument("--out",   required=True, help="output PNG path")
    args = ap.parse_args()

    fig, (ax_l, ax_r) = plt.subplots(1, 2, figsize=(14, 5.2), sharey=False)
    draw(ax_l, Path(args.left),  args.left_title)
    draw(ax_r, Path(args.right), args.right_title)

    # One legend, on the left subplot, top-left.
    ax_l.legend(loc="upper left", framealpha=0.95, fontsize=10)

    fig.suptitle("Routing strategy vs concurrency  --  same algorithm, two model sizes",
                 fontsize=13, y=1.02)
    fig.tight_layout()
    fig.savefig(args.out, dpi=160, bbox_inches="tight")
    print(f"wrote {args.out}")


if __name__ == "__main__":
    main()
