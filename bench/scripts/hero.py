#!/usr/bin/env python3
"""Render the README hero image: pooled CDF on the left, killer numbers
on the right.

Usage:
    python3 bench/scripts/hero.py [--input bench/results] [--out docs/hero.png]
"""
from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path

try:
    import matplotlib
    matplotlib.use("Agg")
    import matplotlib.pyplot as plt
    from matplotlib.patches import FancyBboxPatch
except ImportError as e:
    sys.stderr.write(f"matplotlib required: pip install matplotlib (got: {e})\n")
    sys.exit(2)


STRATEGIES = ["roundrobin", "random", "leastloaded", "prefixaware"]
COLORS = {
    "roundrobin": "#9aa0a6",
    "random": "#1a73e8",
    "leastloaded": "#34a853",
    "prefixaware": "#ea4335",
}
LABELS = {
    "roundrobin": "round-robin",
    "random": "random",
    "leastloaded": "least-loaded",
    "prefixaware": "prefix-aware",
}


def load_samples(paths: list[Path]) -> list[float]:
    out = []
    for p in paths:
        with p.open() as f:
            for line in f:
                line = line.strip()
                if not line:
                    continue
                row = json.loads(line)
                if row.get("err"):
                    continue
                t = row.get("ttft", 0)
                if t > 0:
                    out.append(t / 1e9)
    return out


def discover(in_dir: Path, strategy: str) -> list[Path]:
    out = []
    for sub in sorted(in_dir.glob("raw*")):
        if sub.is_dir():
            cand = sub / f"{strategy}.jsonl"
            if cand.exists():
                out.append(cand)
    return out


def cdf(samples: list[float]) -> tuple[list[float], list[float]]:
    samples = sorted(samples)
    n = len(samples)
    ys = [(i + 1) / n for i in range(n)]
    return samples, ys


def main() -> int:
    p = argparse.ArgumentParser()
    p.add_argument("--input", default="bench/results")
    p.add_argument("--out", default="docs/hero.png")
    args = p.parse_args()

    in_dir = Path(args.input)
    samples: dict[str, list[float]] = {}
    for s in STRATEGIES:
        paths = discover(in_dir, s)
        if paths:
            samples[s] = load_samples(paths)
    if "prefixaware" not in samples or "roundrobin" not in samples:
        sys.stderr.write(f"missing prefixaware or roundrobin samples in {in_dir}\n")
        return 1

    fig = plt.figure(figsize=(14, 6.5), dpi=140, facecolor="white")
    gs = fig.add_gridspec(1, 5, wspace=0.05)
    ax_cdf = fig.add_subplot(gs[0, :3])
    ax_text = fig.add_subplot(gs[0, 3:])
    ax_text.axis("off")

    # CDF panel
    for s, ys in samples.items():
        xs, ps = cdf(ys)
        ax_cdf.plot(xs, ps, label=LABELS[s], color=COLORS[s],
                    linewidth=2.4 if s == "prefixaware" else 1.6,
                    zorder=3 if s == "prefixaware" else 2)
    ax_cdf.set_xlabel("TTFT (seconds)", fontsize=12)
    ax_cdf.set_ylabel("CDF", fontsize=12)
    ax_cdf.set_title("Time-to-first-token, pooled across 3 seeds (54 samples / strategy)",
                     fontsize=12, loc="left")
    ax_cdf.set_ylim(0, 1.02)
    ax_cdf.grid(True, linestyle=":", alpha=0.5)
    ax_cdf.legend(loc="lower right", framealpha=0.95)
    ax_cdf.spines["top"].set_visible(False)
    ax_cdf.spines["right"].set_visible(False)

    # Annotate PA tail
    pa_max = max(samples["prefixaware"])
    ax_cdf.axvline(pa_max, color=COLORS["prefixaware"], linestyle="--",
                   linewidth=1.0, alpha=0.6)
    ax_cdf.annotate(f"PA worst case\n~{pa_max:.1f}s",
                    xy=(pa_max, 0.96), xytext=(pa_max + 0.5, 0.62),
                    fontsize=10, color=COLORS["prefixaware"],
                    arrowprops=dict(arrowstyle="->", color=COLORS["prefixaware"],
                                    alpha=0.7, lw=1))

    # Numbers panel: clean stat cards
    pa = samples["prefixaware"]
    rr = samples["roundrobin"]

    def pct(v):
        return sorted(v)[int(0.95 * len(v))]
    pa_p95 = pct(pa)
    rr_p95 = pct(rr)
    delta = (1 - pa_p95 / rr_p95) * 100

    ax_text.text(0.5, 0.96, "Prefix-aware LLM routing",
                 ha="center", va="top", fontsize=22, fontweight="bold",
                 transform=ax_text.transAxes)
    ax_text.text(0.5, 0.88,
                 "Real llama-server fleet · Qwen2.5-1.5B · Apple M1 Pro",
                 ha="center", va="top", fontsize=11, color="#5f6368",
                 transform=ax_text.transAxes)

    cards = [
        (f"{delta:.0f}%", "lower p95 TTFT\nvs round-robin", COLORS["prefixaware"]),
        ("~10x", "tighter p95 stddev\nacross seeds", "#1a73e8"),
        ("+16pp", "upstream KV-cache\nhit-rate lift", "#34a853"),
        ("8 ns", "router decision\nzero-alloc fast path", "#fbbc04"),
    ]
    rows, cols = 2, 2
    cw, ch = 0.42, 0.30
    pad_x, pad_y = 0.05, 0.08
    start_x, start_y = 0.06, 0.66
    for i, (big, small, color) in enumerate(cards):
        r = i // cols
        c = i % cols
        x = start_x + c * (cw + pad_x)
        y = start_y - r * (ch + pad_y)
        bg = FancyBboxPatch((x, y - ch), cw, ch,
                            boxstyle="round,pad=0.005,rounding_size=0.02",
                            transform=ax_text.transAxes,
                            facecolor=color, edgecolor="none", alpha=0.10)
        ax_text.add_patch(bg)
        ax_text.text(x + cw / 2, y - 0.07, big,
                     transform=ax_text.transAxes,
                     ha="center", va="top",
                     fontsize=24, fontweight="bold", color=color)
        ax_text.text(x + cw / 2, y - 0.18, small,
                     transform=ax_text.transAxes,
                     ha="center", va="top",
                     fontsize=10, color="#3c4043", linespacing=1.3)

    ax_text.text(0.5, 0.02,
                 "github.com/xzhou/llm-router  ·  full methodology in docs/results.md",
                 ha="center", va="bottom", fontsize=9, color="#80868b",
                 transform=ax_text.transAxes)

    fig.tight_layout()
    out = Path(args.out)
    out.parent.mkdir(parents=True, exist_ok=True)
    fig.savefig(out, dpi=140, bbox_inches="tight", facecolor="white")
    print(f"wrote {out}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
