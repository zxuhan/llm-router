#!/usr/bin/env python3
"""Plot TTFT and total-latency CDFs from raw per-request JSONL.

Usage:
    python3 scripts/plot/plot.py [--input bench/results/raw] [--out docs/images/cdf.png]

Reads <input>/<strategy>.jsonl files (one JSON object per line, as
emitted by `cmd/bench --raw-results`) and writes a single PNG with two
panels: TTFT CDF on the left, total-latency CDF on the right.

Dependencies: matplotlib (plus numpy via matplotlib). Install with:
    pip install matplotlib

The script intentionally avoids any non-stdlib code outside matplotlib
so it stays portable. It exits non-zero with a clear message if the
input directory is missing or empty.
"""
from __future__ import annotations

import argparse
import json
import os
import sys
from pathlib import Path

try:
    import matplotlib
    matplotlib.use("Agg")  # avoid requiring a display
    import matplotlib.pyplot as plt
except ImportError as e:
    sys.stderr.write(
        "matplotlib is required: pip install matplotlib\n"
        f"  (import error: {e})\n"
    )
    sys.exit(2)


STRATEGIES = ["roundrobin", "random", "leastloaded", "prefixaware"]
COLORS = {
    "roundrobin": "#888888",
    "random": "#1f77b4",
    "leastloaded": "#2ca02c",
    "prefixaware": "#d62728",
}


def load_samples(paths: list[Path]) -> tuple[list[float], list[float]]:
    """Pool (ttft_seconds, total_seconds) across the given JSONL files."""
    ttft, total = [], []
    for jsonl_path in paths:
        with jsonl_path.open() as f:
            for line in f:
                line = line.strip()
                if not line:
                    continue
                row = json.loads(line)
                if row.get("err"):
                    continue
                t = row.get("ttft", 0)  # nanoseconds
                o = row.get("total", 0)
                if t > 0:
                    ttft.append(t / 1e9)
                if o > 0:
                    total.append(o / 1e9)
    return ttft, total


def discover(in_dir: Path, strategy: str) -> list[Path]:
    """Find every <strategy>.jsonl under in_dir.

    Supports two layouts:
      <in_dir>/<strategy>.jsonl                           (single-run)
      <in_dir>/raw/<strategy>.jsonl                       (single-run via real-llm.sh)
      <in_dir>/raw-run*/<strategy>.jsonl                  (multi-run via real-llm.sh)
    """
    out = []
    direct = in_dir / f"{strategy}.jsonl"
    if direct.exists():
        out.append(direct)
    for sub in sorted(in_dir.glob("raw*")):
        if sub.is_dir():
            cand = sub / f"{strategy}.jsonl"
            if cand.exists():
                out.append(cand)
    return out


def plot_cdf(ax, samples: list[float], label: str, color: str) -> None:
    if not samples:
        return
    samples = sorted(samples)
    n = len(samples)
    # CDF: y = (i+1)/n at sample i
    ys = [(i + 1) / n for i in range(n)]
    ax.plot(samples, ys, label=label, color=color, linewidth=1.8)


def main() -> int:
    p = argparse.ArgumentParser()
    p.add_argument("--input", default="bench/results/raw",
                   help="directory containing <strategy>.jsonl files")
    p.add_argument("--out", default="docs/images/cdf.png",
                   help="output PNG path")
    p.add_argument("--title", default="LLM router latency CDF (Qwen2.5-1.5B, 3 workers, M1 Pro)",
                   help="figure title")
    args = p.parse_args()

    in_dir = Path(args.input)
    if not in_dir.is_dir():
        sys.stderr.write(f"input directory not found: {in_dir}\n")
        return 1

    available: dict[str, tuple[list[float], list[float]]] = {}
    for strat in STRATEGIES:
        paths = discover(in_dir, strat)
        if paths:
            available[strat] = load_samples(paths)
    if not available:
        sys.stderr.write(f"no <strategy>.jsonl files under {in_dir}\n")
        return 1
    # Print a one-line summary per strategy so callers see what got pooled.
    for strat, (ttft, _) in available.items():
        sys.stderr.write(f"  {strat}: {len(ttft)} TTFT samples\n")

    fig, (ax_ttft, ax_total) = plt.subplots(1, 2, figsize=(12, 5))

    for strat, (ttft, total) in available.items():
        plot_cdf(ax_ttft, ttft, strat, COLORS.get(strat, "black"))
        plot_cdf(ax_total, total, strat, COLORS.get(strat, "black"))

    for ax, title in [(ax_ttft, "TTFT"), (ax_total, "Total latency")]:
        ax.set_xlabel("seconds")
        ax.set_ylabel("CDF")
        ax.set_title(title)
        ax.set_ylim(0, 1.02)
        ax.grid(True, linestyle=":", alpha=0.5)
        ax.legend(loc="lower right")

    fig.suptitle(args.title)
    fig.tight_layout()
    out = Path(args.out)
    out.parent.mkdir(parents=True, exist_ok=True)
    fig.savefig(out, dpi=140)
    print(f"wrote {out}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
