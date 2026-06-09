# CLAUDE.md

Olla is a high-performance Go proxy and load balancer for LLM infrastructure. It routes requests across local inference nodes (Ollama, LM Studio, vLLM, vLLM-MLX, SGLang, llama.cpp, Lemonade, LMDeploy, Docker Model Runner, LiteLLM, oMLX, and OpenAI-compatible endpoints) and supports the Anthropic Messages API via passthrough or translation. Two proxy engines: **Olla** (high-performance, default) and **Sherpa** (simple, maintenance-mode — no new features).

Full documentation: https://thushan.github.io/olla/

## Commands

- `make ready` — pre-commit gate (test-short + test-race + fmt + vet + lint + align). Run before every commit.
- `make test` / `make test-race` / `make test-stress` — tests.
- `make bench` / `make bench-balancer` — benchmarks.
- `make build` / `make build-local` / `make run` / `make run-debug` — build and run.
- `make help` — all targets.

## Architecture (hexagonal)

- `internal/core/` — domain layer: entities (`domain/`), interfaces (`ports/`), constants (`constants/`).
- `internal/adapter/` — infrastructure: `proxy/{olla,sherpa,core}`, `balancer/` (incl. `sticky.go`), `health/` (checks + circuit breakers), `translator/` (+ `anthropic/`), `unifier/`, `registry/`, `discovery/`, `security/`, `stats/`.
- `internal/app/` — application layer: `handlers/` (routes registered in `server_routes.go`), `middleware/`, `services/`.
- `internal/config/` — config schema and loading. `config/profiles/*.yaml` — per-backend profiles (routing prefixes, allowed paths, `anthropic_support`).
- `main.go` at the repo root is the entry point.

### Key concepts

- **Translation / passthrough**: the translator layer converts Anthropic ↔ OpenAI. Backends with native Anthropic support (Ollama, LM Studio, vLLM, vLLM-MLX, llama.cpp, Docker Model Runner, oMLX) get zero-overhead **passthrough**; the rest get translation. The mode is chosen per request, filtering to the Anthropic-capable subset of endpoints (`handler_translation.go`, `translator/`).
- **Sticky sessions**: optional selector decorator that pins multi-turn conversations to one backend for KV-cache reuse. 64-bit FNV-1a keys, TTL + LRU bounded, purged on routable→non-routable health transitions (`balancer/sticky.go`).
- **Unifier**: deduplicates and merges models across endpoints into a unified catalogue (`unifier/`, `registry/`).
- **Load balancing**: `least-connections` (default), `round-robin`, `priority` (recommended for production).

## API surface

`internal/app/handlers/server_routes.go` is the source of truth for routes. In brief:

- Internal: `/internal/health`, `/internal/status[/endpoints|/models]`, `/internal/stats/{models,translators,sticky}`, `/internal/process`, `/version`.
- Unified models: `/olla/models`, `/olla/models/{id}`.
- Proxy: `/olla/proxy/`, `/olla/proxy/v1/models`, and per-backend prefixes `/olla/{provider}/` (e.g. `/olla/ollama/`, `/olla/openai/` — `type: "openai"` is an alias for `openai-compatible`).
- Anthropic translator: `/olla/anthropic/v1/{messages,models,messages/count_tokens}`.

Responses carry `X-Olla-*` headers (defined in `internal/core/constants/`): `Endpoint`, `Model`, `Backend-Type`, `Request-ID`, `Response-Time`, `Mode` (passthrough only), `Routing-{Strategy,Decision,Reason}`, `Sticky-Session`, `Sticky-Key-Source`, `Session-ID`.

## Conventions

- **Go 1.24 — do not bump to 1.25.** `golang.org/x/{sys,term,text,sync,time}` and `atomicgo.dev/keyboard` are pinned to their last Go-1.24-compatible versions (see `go.mod`); newer releases require Go 1.25 and `go get -u ./...` will silently bump the toolchain. Prefer `go get -u=patch ./...` and re-pin afterwards.
- Australian English (organise, colour, behaviour). No em-dashes. Comments explain **why**, not what.
- Always run `make ready` before reporting work complete.
- **Do not add dependencies** unless explicitly asked. Endorsed: `docker/go-units`, `json-iterator/go`, `puzpuzpuz/xsync/v4`, `tidwall/gjson`, `jellydator/ttlcache`, `rs/cors`, `golang.org/x/{sync,time}`.

## Sub-agent delegation

Always delegate to the appropriate subagent; use the main context only for orchestration and task decomposition.

- Code review / code changes → language subagent (e.g. Go Architect) or reviewer/implementer subagent.
- Research/exploration → explore subagent. Testing → test subagent.
