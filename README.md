# olla — fork

Fork of [thushan/olla](https://github.com/thushan/olla) at upstream `bb06121`
(2026-09-22). `main` here = upstream + **66 commits** of production hardening
and features, developed against a live 6-endpoint GPU fleet (vLLM/radiance,
llama.cpp, ComfyUI, TTS/STT voice lanes) running 24/7 for weeks.

For the project itself (what olla is, quick start, concepts), see
[upstream README](https://github.com/thushan/olla#readme).
This file documents **what the fork adds** and **where it lives**.

## What's on `main` vs upstream

| Area | What the fork adds |
|---|---|
| **Routing / dispatch** | Smooth weighted round-robin tier rotation (replaces weighted-random coin-flip; equal twins split 50/50, recovered endpoints pay back deficit) · per-conversation sticky key (`first_user_message`, Anthropic top-level system folded) replacing prompt-template keys that pinned whole agents · pins survive endpoint blips (no KV-affinity loss) · startup validation of `key_sources` (unknown name = boot error) · `key_scan_bytes` for bodies behind multi-KB system prompts |
| **Failover** | `proxy.retry_on_status` — backend 429/503 rotates to the next endpoint **before any byte reaches the client** (buffering writer; flush = commit; idempotency + response-started suppressions verbatim; exhaustion passes the refusal through honestly) |
| **Health** | Health modes incl. model-mode verdicts · `check_timeout` made effective per probe (was capped by a shared 5 s client) · load-aware `Busy` grading (`proxy.busy_queue_depth`) from live engine queue depth · **Busy stays routable** (graded weights 1.0/0.3/0.1 in dispatch; healthy-only dispatch turned load into 404 storms) · probe-429 classification · capability-WARN log-once |
| **Runtime metrics** | Sampler with per-backend dialect registry (vllm / radiance / llamacpp; add-a-dialect = one file, e2e-proven) · explicit runtime kinds (name heuristics deleted) · honest idle/opt-out semantics |
| **Embedded dashboard** | `metrics_source: sampler\|influx` — runtime envelope, power (plug meters), cost (EUR rollups) and GPU sections served from InfluxDB with source/age honesty badges · per-endpoint generation/prefill trend chart with per-endpoint history rings · Tokens (lifetime) column (exact lifetime sums from cost rollups; unmetered endpoints render an honest dash, never a fake 0) · ETag/304 semantics, negative-cache hygiene, degraded-shell rendering |
| **Vision + audio** | Anthropic-format image pass-through (was a silent drop) · registry-visible capabilities for engines that advertise none (per-model config map + health probe, stamped only on live models) · audio vocabulary (`audio-speech` / `audio-transcription`) · multipart model-field routing · 413 cap over the model-scan ceiling |
| **Access** | `dashboard.gate_internal_api` wiring (default OFF = byte-identical): CIDR + Host policy over `/internal/*` (health exempt) + `/version` |
| **Quality / CI** | Full-repo `golangci-lint` v2 at 0 issues, `betteralign`, windows/darwin builds green · `Dockerfile.test` with a **test stage inside the image build** (a successful build IS the proof the suite ran) · ~120 new tests incl. full-mux route-wiring, fake-dialect e2e, red-proven regression tests |

Every new knob ships **default-off / byte-identical** to upstream behaviour.

## Feature branches

Each is origin/main-based, self-contained, gate-green (`go test -race`,
gofmt/vet, golangci v2 on touched scope), and maps to upstream issues:

| Branch | Feature | Upstream issue |
|---|---|---|
| `pr/balancer-tier-rotation` | SWRR tier rotation + tie-break + drift prune | #84 |
| `pr/sticky-conversation-key` | conversation-keyed sticky + pin persistence + stats | #216 #226 |
| `pr/health-load-aware` | Busy grading + effective check_timeout + routable dispatch | #216 #226 #166 |
| `pr/retry-on-status` | rotate on backend 429/503 before first byte | #193 |
| `pr/gate-internal-api` | gate the operational API behind one access policy | #214 |
| `pr/anthropic-vision` | Anthropic image blocks + registry-visible vision | #115 |
| `pr/audio-multipart` | audio capability vocabulary + multipart routing + 413 cap | — |

## Gates (fork-pinned)

Containers only; as your uid (root false-fails the envresolver test):

```
docker run --rm -u $(id -u):$(id -g) -e HOME=/home/build -v $PWD/.gocache:/home/build \
  -v $PWD:/app -v olla-gomod:/go/pkg/mod -w /app -e GOFLAGS=-buildvcs=false \
  golang:1.24.13-bookworm go test -race -short -count=1 ./...

docker run --rm -u $(id -u):$(id -g) -v $PWD:/app -w /app/web/dashboard \
  oven/bun:1.3.5 sh -c "bun run test && bun run check"
```

Web note: the real-timer test class needs the 15 s vitest ceiling (in `main`);
under parallel container load expect flakes — authoritative runs are solo.
