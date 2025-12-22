# Cloud benchmark runbook

End-to-end recipe for running the benchmark on a real GPU pod (RunPod or
similar) with **vLLM serving Qwen2.5-7B on N × A100 / H100**.

The same algorithm and the same router code as the local benchmark; the
only thing that changes is the upstream (vLLM with `--enable-prefix-caching`
instead of `llama-server`). Numbers in this regime are production-shaped:
TTFT in the tens-to-low-hundreds of milliseconds, throughput in real
tokens-per-second.

---

## Before you click "Deploy" on RunPod

Have these ready on your laptop so you don't waste pod time:

- [ ] **SSH key**: `~/.ssh/id_ed25519.pub` exists (`ssh-keygen -t ed25519` if not). The pubkey will go into RunPod's SSH key field; the private key stays on your laptop. Without this you're stuck in Jupyter and can't `scp`.
- [ ] **The repo is on GitHub at `https://github.com/zxuhan/llm-router`** so the pod can `git clone` it.
- [ ] **An SSH config entry** (optional but nice): once you have the pod, add to `~/.ssh/config`:
  ```
  Host runpod
    HostName <pod-host-from-runpod-ui>
    Port <pod-port-from-runpod-ui>
    User root
    IdentityFile ~/.ssh/id_ed25519
  ```
  Then `ssh runpod` and `scp -r runpod:/root/llm-router/bench/results .` Just Work.
- [ ] **Local matplotlib venv** for re-rendering figures after scp:
  ```bash
  python3 -m venv /tmp/plot-venv && /tmp/plot-venv/bin/pip install matplotlib --quiet
  ```

Total time on the pod (with all of the above prepared): ~75-90 minutes,
~$8-12 spent. Without these prepared first, expect to lose 20-30 minutes
fumbling on a billed clock.

---

## 1. Rent the pod

Recommended:

| | Choice | Why |
| --- | --- | --- |
| Provider | **RunPod** (community cloud) | cheapest 4-GPU A100 pods, fast boot, Stripe / Alipay payment |
| GPU | **4 × A100 80GB SXM** (~$5.96/hr) or **4 × A100 PCIe** if available (~$5.56/hr) | enough memory for 7-13B FP16 with big KV cache; reviewers know A100 = "production" |
| Container disk | **50 GB** (default 30 GB is tight) | model file (~14 GB) + vLLM cache (~14 GB transient) + Python deps (~5 GB) |
| Persistent volume | 0 GB (skip) | one-shot bench; nothing to keep |
| Template | "PyTorch 2.x" (or any with CUDA 12 + Python 3.10+) | vLLM lives on top |
| SSH | enable, paste your `~/.ssh/id_ed25519.pub` | Jupyter is fine, SSH is comfier |

H100 PCIe (~$2.39/hr) or H100 SXM (~$2.99/hr) per GPU also fine; absolute
TTFT will be ~2× lower, but the algorithmic gap is preserved.

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

Installs Go 1.23, `vllm==0.6.4.post1`, builds the router binaries, downloads
`Qwen/Qwen2.5-7B-Instruct` (~14 GB) into `models/qwen2.5-7b/`. ~6-10 minutes
including model download (HuggingFace transfer is fast inside the
datacenter). Idempotent: re-running skips already-finished steps.

Override the model with env:

```bash
MODEL_ID="meta-llama/Llama-3.1-8B-Instruct" MODEL_DIR=models/llama-8b \
  bash scripts/install-cloud.sh
```

## 4. Run the multi-seed bench

```bash
bash bench/scripts/cloud-vllm.sh
```

Defaults:

| Var | Default | Meaning |
| --- | --- | --- |
| `MODEL_DIR` | `models/qwen2.5-7b` | directory containing the model |
| `N_WORKERS` | auto-detect from `nvidia-smi` | one vLLM server per GPU |
| `START_PORT` | 8001 | workers go on `8001..800N` |
| `GPU_MEM_UTIL` | 0.90 | passed to vLLM |
| `MAX_MODEL_LEN` | 8192 | KV-cache cap |
| `RUNS` | 3 | seeds (each strategy runs N times with fresh workers) |
| `SESSIONS` | 12 | trace sessions per run |
| `TURNS` | 4 | turns per session |
| `SYS_LEN` | 2048 | shared system-prompt length in chars |
| `MAX_TOKENS` | 16 | upstream `max_tokens` |
| `SEED` | 17 | base RNG seed (each run uses `SEED + run - 1`) |

12 sessions × 4 turns × 3 runs × 4 strategies = **576 real chat
completions** through 4 vLLM workers. Each (strategy, run) gets fresh
vLLM workers booted with empty KV caches. The bench tears workers down
between runs so cache state never carries over.

Total wall time: ~30-45 minutes on 4× A100 SXM (the model is small;
each request is sub-second; restarting vLLM per run dominates).

When the script finishes you'll see a clear banner:

```
================================================================================
  ✅  DONE  --  bench finished cleanly. SAFE TO TERMINATE THE POD AFTER scp.
================================================================================
```

## 5. Copy results back to your laptop

**Step 5a: get your pod's SSH host and port from the RunPod dashboard.**

In the RunPod web UI: your pod page > "Connect" button > look for the
`ssh root@... -p <port>` snippet. Copy the host and port.

It usually looks like one of:

```
ssh root@213.181.122.5 -p 22013       # IP + non-22 port
ssh <pod-id>@ssh.runpod.io -p 22      # SSH proxy variant
```

**Step 5b: run scp from your laptop** (NOT inside the pod). The bench
script's banner prints the exact line; it looks like:

```bash
# replace <pod-host> and <pod-ssh-port> with what RunPod gave you
scp -P <pod-ssh-port> -r root@<pod-host>:/root/llm-router/bench/results ./bench-results-cloud
```

If RunPod gave you a `<pod-id>@ssh.runpod.io` style address, use that
form instead:

```bash
scp -P <pod-ssh-port> -r <pod-id>@ssh.runpod.io:/root/llm-router/bench/results ./bench-results-cloud
```

Total transfer is small (~100-500 KB across all JSONLs + the report).
Takes seconds.

**Step 5c: verify the results actually arrived.**

```bash
ls bench-results-cloud/real.md           # must exist
cat bench-results-cloud/real.md | head -15   # must show all 4 strategies
ls bench-results-cloud/raw-run*/prefixaware.jsonl   # one per seed
```

Only after these three commands succeed are you safe to terminate the
pod (terminate destroys the pod's disk; anything you didn't copy is
gone forever).

## 6. Regenerate the figures and commit

These all run on your laptop, in the repo root, with the local
matplotlib venv from earlier:

```bash
# regenerate the headline visuals from cloud data
python3 bench/scripts/hero.py --input bench-results-cloud --out docs/hero-cloud.png
python3 bench/scripts/plot.py --input bench-results-cloud --out docs/cdf-cloud.png

# move the multi-seed Markdown report into docs/
cp bench-results-cloud/real.md docs/results-cloud.md

# commit + push
git add docs/hero-cloud.png docs/cdf-cloud.png docs/results-cloud.md
git commit -m "bench: cloud results on 4× GPU + vLLM + Qwen2.5-7B"
git push
```

The cloud results live alongside the local ones (`docs/hero.png`,
`docs/cdf.png`, `docs/results.md`) rather than overwriting them. You
can decide later which set the README's hero should point at; my
suggestion is to swap the README hero to `docs/hero-cloud.png` once
you have the cloud numbers (the local M1 Pro numbers stay around as a
"smaller-scale reproducibility" reference).

> **Heads-up:** if the matplotlib venv from your earlier local runs is
> gone, recreate it with:
>
> ```bash
> python3 -m venv /tmp/plot-venv && /tmp/plot-venv/bin/pip install matplotlib --quiet
> /tmp/plot-venv/bin/python bench/scripts/hero.py --input bench-results-cloud --out docs/hero-cloud.png
> ```

## 7. Terminate the pod

**This is the one easy-to-forget step that costs real money.**

The bench script ends with a "SAFE TO TERMINATE" banner. That's your
signal. Two ways to stop:

- **RunPod web dashboard** → your pod → click **Terminate** (not just
  Stop; Stop keeps the disk billed). Takes 5 seconds.
- **`runpodctl stop pod <pod-id>`** if you have the CLI installed.

Want a belt-and-suspenders auto-shutdown? Run this **inside the pod**
right after you SSH in, before `install-cloud.sh`:

```bash
sudo shutdown -h +90  # auto-shutdown in 90 minutes
```

The pod will hard-stop in 90 minutes regardless of what you're doing.
Cancel with `sudo shutdown -c` if you want more time.

---

## How to know it's safe to stop

Three signals in order of confidence:

1. **The bench script printed the green banner** ("DONE -- SAFE TO
   TERMINATE THE POD AFTER scp"). This is the strongest signal.
2. **`bench/results/real.md` exists and has all four strategies in the
   table**. Check with `cat bench/results/real.md | head -20`.
3. **You've successfully `scp`d the results to your laptop** and `ls
   bench-results-cloud/real.md` shows them locally.

If any of those three is missing, **don't terminate yet** -- the pod
disk is destroyed on terminate; results are gone.

---

## Troubleshooting

**vLLM OOM at startup.** Lower `GPU_MEM_UTIL=0.85` or `MAX_MODEL_LEN=4096`
and rerun.

**Worker never becomes healthy** (script bails after 10 min of polling).
Check the per-worker log: `tail -50 /tmp/vllm-logs/w0.log`. Usually
either OOM (see above) or HuggingFace hub auth issue (set
`export HF_TOKEN=...` if the model is gated).

**"cached_tokens not in usage."** vLLM reports this when
`--enable-prefix-caching` is on AND the build is recent (>= 0.6.x).
If you see zeros, double-check the flag is in `cloud-vllm.sh`'s vLLM
launch args (it is, by default).

**Script crashed mid-run.** vLLM workers may still be alive. Clean up:
`pkill -f vllm.entrypoints` (or just terminate the pod and start over).

**You're being charged but think you stopped the pod.** "Stop" keeps
the disk and bills storage hourly. Use **Terminate** to actually stop
spending money.
