# ADR 0002: Default to llama.cpp's HTTP server as the upstream backend

## Context

The router is OpenAI-compatible on the front. To demonstrate the routing
algorithm we need at least one upstream that:

1. Implements `/v1/chat/completions` with both streaming and non-streaming
   modes;
2. Has *explicit* prefix caching at the KV-cache level so that hits are
   actually cheap (the whole point);
3. Runs on Apple Silicon laptops without containerisation gymnastics;
4. Is small enough to ship two or three concurrent instances on a single
   machine for testing.

Candidates considered: llama.cpp's built-in server, mlx-lm.server, vLLM,
text-generation-inference, Ollama.

## Decision

Default to llama.cpp (`llama-server`). Treat any OpenAI-compatible upstream
as a drop-in alternative; mlx-lm.server is the explicit Apple Silicon
companion path.

## Rationale

- **Prefix cache is documented and observable.** llama.cpp's `prompt-cache`
  and KV-cache reuse are documented; we can see TTFT collapse on a warm
  prefix. mlx-lm has equivalent behaviour for Apple Silicon hardware paths.
- **OpenAI-compatible API.** Both surface `/v1/chat/completions` directly,
  so the router does not need bespoke per-backend wire code.
- **Local-only.** No accounts, no docker daemon, no GPU. A reviewer can run
  the demo with `brew install llama.cpp` plus a model file.
- **Process model is simple.** Each backend is its own OS process bound to
  a different port. The router talks HTTP. There is no shared memory and
  no inter-process coordination.

vLLM and TGI are stronger production options but require Python, NVIDIA
GPUs, and considerably more setup. Ollama wraps llama.cpp and is great for
demos but its API surface and prefix-cache semantics are less stable to
target as a routing primary.

## Consequences

- The `Backend` interface is intentionally tiny. Adding mlx-lm, vLLM, or a
  custom backend means writing a new constructor; the proxy and router do
  not change.
- We do not ship pre-baked benchmarks against a real GPU cluster. The
  `bench/` harness uses an in-process fake server; numbers are *relative*.
  See `docs/results.md` for caveats.
