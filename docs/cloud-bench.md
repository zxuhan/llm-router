# Cloud benchmark runbook

End-to-end recipe for the headline benchmark: a concurrency sweep at two
model sizes (Qwen2.5-7B and Qwen2.5-14B) on a 4× A100 80GB SXM pod with
vLLM 0.6.4. Same router code as the local bench; the only change is the
upstream (vLLM with `--enable-prefix-caching` instead of `llama-server`).
Total wall time ~3.5 hours, total cost ~$25.

---

## Before you click "Deploy" on RunPod

Have these ready on your laptop so you don't waste pod time:

- [ ] **SSH key**: `~/.ssh/id_ed25519.pub` exists (`ssh-keygen -t ed25519` if not). The pubkey goes into RunPod's SSH key field; the private key stays on your laptop. Without this you cannot `scp`.
- [ ] **An SSH config entry** (optional but nice): once you have the pod, add to `~/.ssh/config`:
  ```
  Host runpod
    HostName <pod-host-from-runpod-ui>
    Port <pod-port-from-runpod-ui>
    User root
    IdentityFile ~/.ssh/id_ed25519
  ```
  Then `ssh runpod` and `scp -r runpod:/root/llm-router/bench/results-sweep .` Just Work.
- [ ] **Local matplotlib venv** for re-rendering figures after scp:
  ```bash
  python3 -m venv /tmp/plot-venv && /tmp/plot-venv/bin/pip install matplotlib --quiet
  ```

---

## 1. Rent the pod

| | Choice | Why |
| --- | --- | --- |
| Provider | **RunPod** (community cloud) | cheapest 4-GPU A100 pods, fast boot |
| GPU | **4 × A100 80GB SXM** (~$5.96/hr) or **4 × A100 PCIe** (~$5.56/hr) | enough memory for 7-14B FP16 with the KV cache budget the bench needs |
| Container disk | **50 GB** | one model on disk at a time (7B ~14 GB, 14B ~28 GB) plus ~10 GB env |
| Persistent volume | 0 GB (skip) | one-shot bench |
| Template | "PyTorch 2.x" (CUDA 12, Python 3.10+) | vLLM lives on top |
| SSH | enable, paste your `~/.ssh/id_ed25519.pub` | required for scp |

H100 PCIe (~$2.39/hr) or H100 SXM (~$2.99/hr) per GPU also works; absolute
TTFT is roughly 2× lower, but the algorithmic gap is preserved.

## 2. SSH in and clone

```bash
# from your laptop:
ssh -p <pod-port> root@<pod-host>

# inside the pod:
git clone https://github.com/zxuhan/llm-router.git
cd llm-router
```

## 3. One-shot install

```bash
bash scripts/install-cloud.sh
```

Installs Go 1.23, `vllm==0.6.4.post1` with the `transformers==4.46.3` /
`huggingface_hub==0.26.5` pins vLLM 0.6.4 needs, builds the router
binaries, downloads `Qwen/Qwen2.5-7B-Instruct` (~14 GB) into
`models/qwen2.5-7b/`. Runs in 6-10 minutes. Idempotent: re-running skips
already-finished steps.

The Qwen2.5-14B model is downloaded later by `full-bench.sh` itself (one
model on disk at a time keeps the 50 GB container disk sufficient).

## 4. Run the full sweep, inside tmux

`tmux` is non-negotiable: a brief SSH disconnect or RunPod pod
preemption will kill any foreground bench started directly from your
shell.

```bash
tmux new -s bench

# inside tmux:
bash scripts/bench/full-bench.sh

# detach: press Ctrl+b, release, press d.
# reattach later from any SSH session: tmux attach -t bench
# read-only peek without attaching:    tmux capture-pane -t bench -p | tail -30
```

`full-bench.sh` runs the matrix that produces the README hero chart:
{Qwen2.5-7B, Qwen2.5-14B} × SESSIONS={4, 8, 12, 16, 24}, three seeds per
point, four strategies per seed. For each model it auto-downloads the
weights, runs the sweep, then deletes the weights to free disk for the
next model.

| Var | Default | Meaning |
| --- | --- | --- |
| `MODELS_TSV` | Qwen2.5-7B, Qwen2.5-14B | newline-separated `<HF_REPO_ID> <LOCAL_DIR>` pairs |
| `SESSIONS_LIST` | `4 8 12 16 24` | concurrency points to sweep |
| `RUNS` | 3 | seeds per (strategy, sessions) point |
| `TURNS` | 8 | turns per session |
| `SYS_LEN` | 6144 | shared system-prompt length in chars |
| `MAX_TOKENS` | 64 | upstream `max_tokens` |
| `SATURATION_INFLIGHT` | 4 | prefix-aware safety-valve threshold |

Total wall time on 4× A100 SXM: ~1.5 hours per model size, ~3.5 hours
total. Most of that is vLLM rebooting between (strategy, seed) combos so
KV caches start empty; the bench itself is fast.

When the matrix finishes you'll see:

```
################################################################################
  ✅  FULL-BENCH DONE  --  SAFE TO TERMINATE THE POD AFTER scp.
################################################################################
```

The banner prints the exact `scp` command for your pod's IP, including
the `rm -rf` step that prevents nested-results bugs.

If you only want one model size, run `concurrency-sweep.sh` directly:

```bash
MODEL_DIR=models/qwen2.5-7b bash scripts/bench/concurrency-sweep.sh
```

If you only want a single concurrency point at one model size, run the
underlying `cloud-vllm.sh`:

```bash
SESSIONS=12 MODEL_DIR=models/qwen2.5-7b bash scripts/bench/cloud-vllm.sh
```

## 5. Copy results back to your laptop

**Step 5a: get your pod's SSH host and port from the RunPod dashboard.**

In the RunPod web UI: your pod page > "Connect" button > the
`ssh root@... -p <port>` snippet. Copy the host and port.

**Step 5b: run scp from your laptop** (NOT inside the pod). Always
`rm -rf` the local destination first so `scp -r` does not nest a
`results-sweep/` inside an existing directory.

```bash
# replace <pod-host> and <pod-port> with what RunPod gave you
rm -rf ./bench-results-sweep-7B ./bench-results-sweep-14B

scp -P <pod-port> -r root@<pod-host>:/root/llm-router/bench/results-sweep ./bench-results-sweep
```

If RunPod gave you a `<pod-id>@ssh.runpod.io` style address, use that
form instead:

```bash
scp -P <pod-port> -r <pod-id>@ssh.runpod.io:/root/llm-router/bench/results-sweep ./bench-results-sweep
```

The transfer is small (~200 KB per concurrency point across all JSONLs +
the per-point `real.md`).

**Step 5c: verify the data arrived.**

```bash
ls bench-results-sweep/qwen2.5-7b/sessions=*/real.md   # 5 files
ls bench-results-sweep/qwen2.5-14b/sessions=*/real.md  # 5 files
head -15 bench-results-sweep/qwen2.5-14b/sessions=24/real.md
```

You should see all four strategies in the table and PA's KV cached value
in the 94-95% range. Only after this passes are you safe to terminate
the pod (terminate destroys the pod's disk).

## 6. Regenerate the figures and commit

These all run on your laptop, in the repo root, with the local
matplotlib venv from earlier. The repo expects sweep data to live under
the laptop-side names `bench-results-sweep-7B` and `bench-results-sweep-14B`
(both are in `.gitignore`). Reorganize the scp output once:

```bash
mv bench-results-sweep/qwen2.5-7b  bench-results-sweep-7B/qwen2.5-7b   2>/dev/null || \
   mkdir -p bench-results-sweep-7B  && mv bench-results-sweep/qwen2.5-7b  bench-results-sweep-7B/
mv bench-results-sweep/qwen2.5-14b bench-results-sweep-14B/qwen2.5-14b 2>/dev/null || \
   mkdir -p bench-results-sweep-14B && mv bench-results-sweep/qwen2.5-14b bench-results-sweep-14B/
```

Then regenerate:

```bash
# headline: stacked 7B + 14B sweep (the README hero)
/tmp/plot-venv/bin/python scripts/plot/hero-cloud.py \
  --top    bench-results-sweep-7B/qwen2.5-7b   --top-title  "Qwen2.5-7B" \
  --bottom bench-results-sweep-14B/qwen2.5-14b --bottom-title "Qwen2.5-14B" \
  --out docs/images/hero-cloud.png

# single-panel versions
/tmp/plot-venv/bin/python scripts/plot/sweep-plot.py \
  --input bench-results-sweep-7B/qwen2.5-7b  --out docs/images/sweep-7b.png
/tmp/plot-venv/bin/python scripts/plot/sweep-plot.py \
  --input bench-results-sweep-14B/qwen2.5-14b --out docs/images/sweep-14b.png

# commit + push
git add docs/images/hero-cloud.png docs/images/sweep-7b.png docs/images/sweep-14b.png
git commit -m "bench: cloud sweep at 7B and 14B"
git push
```

The cloud images live alongside the local CDF (`docs/images/cdf.png`,
referenced from `docs/results.md`); the README hero points at the cloud
sweep.

> **Heads-up:** if the matplotlib venv is gone, recreate it:
>
> ```bash
> python3 -m venv /tmp/plot-venv && /tmp/plot-venv/bin/pip install matplotlib --quiet
> ```

## 7. Terminate the pod

The bench script ends with the SAFE-TO-TERMINATE banner. Two ways to
stop:

- **RunPod web dashboard** → your pod → click **Terminate** (not just
  Stop; Stop keeps the disk billed). Takes 5 seconds.
- **`runpodctl stop pod <pod-id>`** if you have the CLI installed.

Belt-and-suspenders auto-shutdown, run **inside the pod** right after
you SSH in:

```bash
sudo shutdown -h +240  # auto-shutdown in 4 hours
```

The pod hard-stops in 4 hours regardless of what you're doing. Cancel
with `sudo shutdown -c` if you need more time.

---

## How to know it's safe to stop

In order of confidence:

1. The bench script printed the green `FULL-BENCH DONE` banner.
2. `bench/results-sweep/qwen2.5-7b/sessions=*/real.md` and
   `bench/results-sweep/qwen2.5-14b/sessions=*/real.md` exist (10 files
   total) and the prefixaware row in each shows `KV cached` in the 94-95%
   range.
3. You have successfully `scp`d the data to your laptop and the
   `head -15` from step 5c looks right.

If any of those is missing, do not terminate yet. Pod termination
destroys the disk; anything not copied is gone forever.

---

## Troubleshooting

**vLLM OOM at startup.** Lower `GPU_MEM_UTIL=0.85` or `MAX_MODEL_LEN=4096`
and rerun.

**Worker never becomes healthy** (script bails after 10 minutes of
polling). Check the per-worker log: `tail -50 /tmp/vllm-logs/w0.log`.
Usually OOM (see above) or HuggingFace hub auth issue (set
`export HF_TOKEN=...` if the model is gated).

**`cached_tokens` reported as 0.** vLLM 0.6.4 only emits
`prompt_tokens_details.cached_tokens` when `--enable-prompt-tokens-details`
is passed. The flag is in `scripts/bench/cloud-vllm.sh`'s vLLM launch
args by default; if you removed it, put it back.

**Worker w0 returns 405 to every request.** Some RunPod base images run
nginx on `:8001`. The bench defaults to `START_PORT=18001` to avoid this
class of collision; if you override it, the preflight `ss -tln` check
hard-fails when a target port is already taken. Pick a different port
range or stop whatever is holding the conflicting one.

**Script crashed mid-run.** vLLM workers may still be alive. Clean up
with `pkill -f vllm.entrypoints` before rerunning. The bench scripts
self-clean their `OUT_DIR` at the start of every run, so partial state
from the prior run does not contaminate the next.

**You are being charged but think you stopped the pod.** "Stop" keeps
the disk and bills storage hourly. Use **Terminate** to stop spending.

**RunPod preempted your pod mid-bench.** Community cloud pods can be
reclaimed when capacity tightens. Anything not yet `scp`d is lost. For
sweep-style multi-hour runs, scp intermediate state periodically
(`bench/results-sweep/qwen2.5-7b/sessions=4/`, then `=8/`, ...) so a
preempt costs at most one concurrency point.
