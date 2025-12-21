#!/usr/bin/env python3
"""Render the README hero image.

Two-panel composition designed for at-a-glance scanning:
  - Left panel: p95 TTFT (lower = better), bars per strategy with error bars
    from the multi-seed runs. Prefix-aware emphasised.
  - Right panel: upstream KV-cache hit rate (higher = better), same layout.

Uses the JSON summaries that bench/scripts/real-llm.sh writes (one per
(strategy, run) when RUNS > 1). No matplotlib styling rabbit-hole; just
clean, readable, professional.

Usage:
    python3 bench/scripts/hero.py [--input bench/results] [--out docs/hero.png]
"""
from __future__ import annotations

import argparse
import json
import math
import sys
from pathlib import Path

try:
    import matplotlib
    matplotlib.use("Agg")
    import matplotlib.pyplot as plt
except ImportError as e:
    sys.stderr.write(f"matplotlib required: pip install matplotlib (got: {e})\n")
    sys.exit(2)


STRATEGIES = ["roundrobin", "random", "leastloaded", "prefixaware"]
LABELS = {
    "roundrobin": "round-robin",
    "random": "random",
    "leastloaded": "least-loaded",
    "prefixaware": "prefix-aware",
}
# Muted greys for baselines, vibrant teal for the headline strategy.
BASELINE_FILL = "#9ea4ad"
BASELINE_EDGE = "#5b6068"
PA_FILL = "#0f9d8a"
PA_EDGE = "#057263"
PANEL_BG = "#fbfcfd"
INK = "#1f242b"
MUTED = "#5b6068"


def discover(in_dir: Path, strategy: str) -> list[Path]:
    """Return JSON summaries for `strategy` (single or multi-run layout)."""
    out = []
    for p in sorted(in_dir.glob(f"real-{strategy}-run*.json")):
        out.append(p)
    if not out:
        single = in_dir / f"real-{strategy}.json"
        if single.exists():
            out.append(single)
    return out


def load_summary(path: Path) -> dict:
    with path.open() as f:
        doc = json.load(f)
    if isinstance(doc, dict) and "summaries" in doc:
        return doc["summaries"][0]
    return doc


def mean_std(xs: list[float]) -> tuple[float, float]:
    if not xs:
        return 0.0, 0.0
    m = sum(xs) / len(xs)
    if len(xs) < 2:
        return m, 0.0
    var = sum((x - m) ** 2 for x in xs) / (len(xs) - 1)
    return m, math.sqrt(var)


def gather(in_dir: Path) -> dict[str, dict[str, tuple[float, float]]]:
    """Return {strategy: {metric: (mean, std)}}."""
    out: dict[str, dict[str, tuple[float, float]]] = {}
    for s in STRATEGIES:
        runs = [load_summary(p) for p in discover(in_dir, s)]
        if not runs:
            continue
        ttft_p95 = [r["ttft"]["p95"] / 1e9 for r in runs]   # ns -> s
        cache = [r["cache_token_rate"] * 100 for r in runs]
        rps = [r["throughput_rps"] for r in runs]
        out[s] = {
            "ttft_p95": mean_std(ttft_p95),
            "cache": mean_std(cache),
            "rps": mean_std(rps),
            "n_runs": (len(runs), 0),
        }
    return out


def panel_bars(ax, data, metric, *, title, ylabel, value_fmt, lower_is_better):
    keys = [s for s in STRATEGIES if s in data]
    means = [data[s][metric][0] for s in keys]
    stds = [data[s][metric][1] for s in keys]
    colors = [PA_FILL if s == "prefixaware" else BASELINE_FILL for s in keys]
    edges = [PA_EDGE if s == "prefixaware" else BASELINE_EDGE for s in keys]
    pretty = [LABELS[s] for s in keys]

    bars = ax.bar(pretty, means, yerr=stds, capsize=5,
                  color=colors, edgecolor=edges, linewidth=1.4,
                  error_kw=dict(ecolor=MUTED, elinewidth=1.2, alpha=0.9),
                  zorder=3)
    for bar, m, s in zip(bars, means, stds):
        text = value_fmt(m, s)
        ax.text(bar.get_x() + bar.get_width() / 2,
                bar.get_height() + (max(means) * 0.02 if lower_is_better else 0.02),
                text, ha="center", va="bottom",
                fontsize=11, fontweight="bold", color=INK, zorder=4)

    ax.set_title(title, fontsize=13, color=INK, pad=14, loc="left")
    ax.set_ylabel(ylabel, fontsize=10, color=MUTED)
    ax.tick_params(axis="x", labelsize=11, colors=INK, length=0)
    ax.tick_params(axis="y", labelsize=9, colors=MUTED)
    ax.set_facecolor(PANEL_BG)
    ax.grid(axis="y", linestyle=":", alpha=0.45, zorder=1)
    ax.set_axisbelow(True)
    for spine in ("top", "right"):
        ax.spines[spine].set_visible(False)
    for spine in ("left", "bottom"):
        ax.spines[spine].set_color("#dadde2")

    # Annotate the headline gap with a bracket between baseline-best and PA.
    if metric == "ttft_p95":
        baseline_keys = [s for s in keys if s != "prefixaware"]
        if baseline_keys and "prefixaware" in keys:
            best_baseline = min(baseline_keys, key=lambda s: data[s][metric][0])
            base_mean = data[best_baseline][metric][0]
            pa_mean = data["prefixaware"][metric][0]
            delta_pct = (1 - pa_mean / base_mean) * 100
            ax.set_ylim(0, max(means) * 1.30)
            y_top = max(means) * 1.18
            i_base = keys.index(best_baseline)
            i_pa = keys.index("prefixaware")
            ax.annotate("", xy=(i_pa, y_top), xytext=(i_base, y_top),
                        arrowprops=dict(arrowstyle="<->", color=PA_EDGE, lw=1.6))
            mid = (i_base + i_pa) / 2
            ax.text(mid, y_top + max(means) * 0.025,
                    f"{delta_pct:.0f}% lower",
                    ha="center", va="bottom", fontsize=12, fontweight="bold",
                    color=PA_EDGE)
    else:
        ax.set_ylim(0, max(means) * 1.18)


def main() -> int:
    p = argparse.ArgumentParser()
    p.add_argument("--input", default="bench/results")
    p.add_argument("--out", default="docs/hero.png")
    args = p.parse_args()

    data = gather(Path(args.input))
    if "prefixaware" not in data or "roundrobin" not in data:
        sys.stderr.write(f"missing prefixaware or roundrobin in {args.input}\n")
        return 1
    n_runs = data["prefixaware"]["n_runs"][0]

    fig = plt.figure(figsize=(13, 5.0), dpi=150, facecolor="white")
    gs = fig.add_gridspec(1, 2, wspace=0.18, left=0.06, right=0.97,
                          top=0.81, bottom=0.13)
    ax_left = fig.add_subplot(gs[0, 0])
    ax_right = fig.add_subplot(gs[0, 1])

    panel_bars(ax_left, data, "ttft_p95",
               title="TTFT p95 (lower is better)",
               ylabel="seconds",
               value_fmt=lambda m, s: f"{m:.2f} s\n± {s:.2f} s" if s > 0 else f"{m:.2f} s",
               lower_is_better=True)

    panel_bars(ax_right, data, "cache",
               title="Upstream KV-cache hit rate (higher is better)",
               ylabel="cached_tokens / prompt_tokens",
               value_fmt=lambda m, s: f"{m:.1f}%\n± {s:.1f}%" if s > 0 else f"{m:.1f}%",
               lower_is_better=False)

    fig.suptitle("Prefix-aware LLM routing  ·  measured on real llama.cpp",
                 fontsize=17, fontweight="bold", color=INK, y=0.96)
    fig.text(0.5, 0.91,
             f"Qwen2.5-1.5B  ·  3 workers  ·  Apple M1 Pro  ·  "
             f"{n_runs} seeds × 18 requests  ·  fresh workers per (strategy, run)",
             ha="center", fontsize=10, color=MUTED)

    out = Path(args.out)
    out.parent.mkdir(parents=True, exist_ok=True)
    fig.savefig(out, dpi=150, bbox_inches="tight", facecolor="white")
    print(f"wrote {out}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
