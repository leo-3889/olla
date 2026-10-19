# Sticky Sessions

> :memo: **Default Configuration**
> ```yaml
> proxy:
>   sticky_sessions:
>     enabled: false
> ```
> Sticky sessions are **opt-in**. Set `enabled: true` under `proxy.sticky_sessions` to activate KV-cache affinity routing.
>
> **Environment Variable**: `OLLA_PROXY_STICKY_SESSIONS_ENABLED`

Sticky sessions route repeat turns in a multi-turn conversation to the same backend endpoint, maximising KV-cache reuse across turns. The feature wraps the configured load balancer as a decorator. The underlying strategy (priority, round-robin, least-connections) is unchanged for new sessions and fallback cases.

## Why sticky sessions

Modern LLM backends maintain a KV-cache for the token sequence they have already processed. When the next turn of a conversation lands on the **same** backend, the backend can skip re-ingesting the full context and jump straight to generating new tokens. For long conversations this produces a substantial reduction in both time-to-first-token and compute cost; the benefit scales with context length.

Without affinity, a load balancer may distribute turn N and turn N+1 to different backends. The receiving backend for turn N+1 has a cold cache and must process the entire prompt from scratch. For workloads with short prompts or single-turn completions this overhead is negligible; for chat-style applications with growing context it compounds with every turn.

## How it works

On each request, Olla computes a **session key** from one of the configured key sources (see [Key sources](#key-sources) below). The key is looked up in an in-memory LRU/TTL store:

- **Hit**: the pinned backend is still routable; the request is sent there and the TTL is refreshed.
- **Miss**: no entry exists; the request is forwarded to the underlying balancer, and the selected backend is stored.
- **Repin**: an entry exists but the pinned backend is no longer routable; the underlying balancer selects a new backend and the entry is overwritten.
- **Disabled**: no key source produced a usable key (e.g. no `X-Olla-Session-ID` header and no other sources configured); the request is passed through to the underlying balancer without recording anything.

```mermaid
sequenceDiagram
    participant C as Client
    participant O as Olla
    participant Store as Session Store
    participant B as Backend

    C->>O: POST /olla/proxy/... (turn N+1)
    O->>O: Derive session key
    O->>Store: Lookup key
    alt Cache hit (backend routable)
        Store-->>O: Pinned backend URL
        O->>B: Forward to pinned backend
        B-->>O: Response
        O-->>C: Response + X-Olla-Sticky-Session: hit
    else Cache miss / repin
        Store-->>O: No entry (or dead backend)
        O->>O: Delegate to underlying balancer
        O->>Store: Store key → selected backend
        O->>B: Forward to selected backend
        B-->>O: Response
        O-->>C: Response + X-Olla-Sticky-Session: miss
    end
```

## Key sources

The `key_sources` list is evaluated in order; the first source that produces a non-empty value wins. All keys are scoped to the model name so the same client talking to different models maintains independent session state.

| Source | How the key is derived | When to prefer it | Caveats |
|---|---|---|---|
| `session_header` | 64-bit FNV-1a hash of the `X-Olla-Session-ID` request header | Explicit client opt-in; most reliable | Client must send the header consistently |
| `first_user_message` | 64-bit FNV-1a hash of the first **non-system** turn of `messages` (the raw `prompt` for completions bodies), key `turn:<model>:<hash>` | No client changes needed and one key per conversation: the first user turn is unique per conversation and is resent verbatim on every later turn | Needs `key_scan_bytes` large enough to reach past the system prompt; conversations that open with identical text collapse to one key |
| `prefix_hash` | 64-bit FNV-1a hash of the first `prefix_hash_bytes` bytes of the `messages` JSON field | Fallback for bodies with no user turn (system-only probes); the only source that gives shared-prefix affinity | Keys the prompt *template*, not the conversation: every conversation of one agent shares the key, so the whole agent flocks to one node |
| `auth_header` | 64-bit FNV-1a hash of the `Authorization` header value | Per-user affinity without client changes | Breaks if the token rotates mid-conversation; unreliable with shared tokens |
| `ip` | Client IP address (extracted via `net.SplitHostPort`) | Simple deployments with no NAT | Unreliable behind NAT, load balancers, or Docker networking |

All header and token values are hashed before storage; plaintext secrets are never written to the session store.

The default configuration enables `session_header`, `first_user_message`, `prefix_hash`, and `auth_header` (in that order) and comments out `ip` because it is unreliable behind typical container networking. Adjust the list to suit your deployment.

Names are validated at startup when the feature is enabled: an unrecognised entry is a boot error. It used to be skipped silently at request time, which meant a typo quietly deleted one layer of the cascade and looked like "sticky sessions do not work".

### Multipart uploads (audio)

The body-derived sources fingerprint a JSON body. OpenAI audio transcription requests are `multipart/form-data` - an audio file plus form fields - so there is no JSON body to fingerprint and every body-derived source declines the request. A transcription upload that sends no `session_header` is therefore reported as `sticky_source: no_key` with key source `none`. This is by design, not a sticky failure: each voice model is served by exactly one endpoint, so there is no fan-out for a pin to prevent and no multi-turn KV-cache state to preserve across turns. Routing such uploads is the inspector's job - it reads the `model` form field to select the endpoint - and the `no_key` counter honestly records that the cascade ran and legitimately had nothing to key on.

### Conversation-level vs template-level affinity

The two body-derived sources key at different granularities, and the difference decides where load lands.

`prefix_hash` hashes the opening bytes of the `messages` array. For agent clients (coding agents, bot gateways) those bytes are the **shared system prompt**, so every conversation that agent starts produces the *same* key. One key means one pin: the entire agent runs on the node that answered its first request, and because the TTL is sliding (refreshed on every hit) a busy agent never releases it. The balancer's per-request choice only ever applies to the handful of keys in the store, so an equal-priority tier never evens out. Measured on a two-node fleet: 10 191 sticky hits over 12 active sessions, with 7 288 requests on one node against 1 040 on the other - both endpoints healthy, both at priority 100.

`first_user_message` hashes the first non-system turn. It is unique per conversation (so new conversations are spread by the balancer) and stable across that conversation's later turns (clients resend the history verbatim), which is exactly the affinity KV-cache reuse wants. After the switch the same traffic produced 110 sessions for 110 conversations, with new conversations alternating across the tier and follow-up turns hitting their pinned node.

Shared-prefix affinity is not lost: because the tier rotates, every node serves conversations carrying the same system prompt, so each node keeps that prefix warm in its own cache.

Sessions do not live forever. Three mechanisms remove them:

**Sliding TTL**: every cache hit refreshes the expiry timer. A session that goes idle for longer than `idle_ttl_seconds` is expired automatically. Active conversations are never interrupted mid-session.

**LRU eviction**: when the store reaches `max_sessions`, the least-recently-used entry is evicted to make room. Under normal load this should never occur; it acts as a safety cap to bound memory usage.

**Health-based purge**: when the health checker transitions a backend to an unhealthy state, Olla immediately calls `PurgeDeadEndpoints` with the current routable set. Any session entry pointing to the now-dead backend is deleted without waiting for TTL. The next request for that session falls through to the underlying balancer and receives a `repin`.

!!! note "Busy endpoints are not purged"
    A backend in the **Busy** state is still considered routable (`IsRoutable() == true`). Sticky sessions are preserved through Busy transitions; the backend is overloaded but still serving. A purge fires only when an endpoint transitions **from** a routable state (Healthy, Busy, Warming) **to** any non-routable state (Unhealthy, Offline, Unknown, ConfigError, RateLimited). Unknown→Unhealthy does not trigger a purge because Unknown is already non-routable and nothing could have been pinned to it.

```mermaid
stateDiagram-v2
    [*] --> Active: First request (miss)
    Active --> Active: Subsequent requests (hit, TTL refreshed)
    Active --> Expired: Idle longer than idle_ttl_seconds
    Active --> Purged: Backend becomes unhealthy
    Active --> Evicted: LRU cap (max_sessions) reached
    Expired --> [*]
    Purged --> [*]
    Evicted --> [*]
```

## Response headers

Olla writes three response headers so clients and operators can observe affinity decisions:

| Header | Values | Meaning |
|---|---|---|
| `X-Olla-Sticky-Session` | `hit` / `miss` / `repin` / `disabled` | Outcome of the affinity lookup for this request |
| `X-Olla-Sticky-Key-Source` | `session_header` / `first_user_message` / `prefix_hash` / `auth_header` / `ip` / `none` | Which key source was used (absent when outcome is `disabled`) |
| `X-Olla-Session-ID` | _(echoed from request)_ | Present in the response only when the client sent `X-Olla-Session-ID`; lets stateless clients confirm the header was received |

Example: first request (miss), client provides explicit session ID:

```bash
curl -i -X POST http://localhost:40114/olla/proxy/api/chat \
  -H "X-Olla-Session-ID: conv-abc123" \
  -H "Content-Type: application/json" \
  -d '{"model":"llama3.2","messages":[{"role":"user","content":"Hello"}]}'
```

```http
HTTP/1.1 200 OK
X-Olla-Endpoint: gpu-server-1
X-Olla-Sticky-Session: miss
X-Olla-Sticky-Key-Source: session_header
X-Olla-Session-ID: conv-abc123
```

Subsequent request (hit):

```http
HTTP/1.1 200 OK
X-Olla-Endpoint: gpu-server-1
X-Olla-Sticky-Session: hit
X-Olla-Sticky-Key-Source: session_header
X-Olla-Session-ID: conv-abc123
```

## Configuration

All fields live under `proxy.sticky_sessions`:

```yaml
proxy:
  sticky_sessions:
    enabled: false              # opt-in: set true to activate affinity routing

    idle_ttl_seconds: 600       # sliding TTL in seconds; 0 = sessions never expire by TTL
                                # (not recommended, sessions accumulate until LRU eviction)

    max_sessions: 10000         # LRU capacity; oldest entries are evicted when full

    key_sources:                # ordered cascade, first match wins. Unknown names
                                # are a startup error.
      - "session_header"        # X-Olla-Session-ID header (explicit client opt-in)
      - "first_user_message"    # hash of the first non-system turn: one key per conversation
      - "prefix_hash"           # hash of first N bytes of messages JSON (fallback, and the
                                # only shared-prefix-affinity source)
      - "auth_header"           # hash of Authorization header (per-user affinity)
      # - "ip"                  # client IP, opt-in; unreliable behind NAT/Docker

    prefix_hash_bytes: 512      # bytes of the messages field to hash for prefix_hash source;
                                # larger values reduce false collisions at a small CPU cost
    key_scan_bytes: 65536       # leading body bytes the handler buffers to compute a key.
                                # The handler reads max(prefix_hash_bytes, key_scan_bytes)
                                # and restores the body. Required by first_user_message: a
                                # system prompt is routinely tens of kilobytes, so with a
                                # 512-byte read the source never reaches the user turn.
```

The only env var exposed for this feature is `OLLA_PROXY_STICKY_SESSIONS_ENABLED` (boolean). The remaining fields are configuration-file only.

## Observability

### Stats endpoint

```bash
curl http://localhost:40114/internal/stats/sticky
```

When sticky sessions are **enabled**:

```json
{
  "enabled": true,
  "active_sessions": 142,
  "insertions": 1500,
  "hits": 9231,
  "misses": 1500,
  "evictions": 0,
  "max_sessions": 10000,
  "idle_ttl_seconds": 600
}
```

When sticky sessions are **disabled** (stable shape for scripting):

```json
{
  "enabled": false
}
```

Scripts should branch on the `enabled` field; the endpoint always returns `200 OK` regardless of whether the feature is active.

## When NOT to use sticky sessions

Sticky sessions trade load distribution for cache locality. They are not always the right choice:

- **Stateless or single-turn workloads**: embeddings, one-shot completions, and batch jobs gain nothing from affinity; use the plain load balancer.
- **Model-routing-dominated traffic**: if requests are already hard-routed to specific endpoints by model routing, the sticky wrapper adds overhead with no benefit.
- **Single-endpoint deployments**: with one candidate there is nothing to spread and nothing to pin; the wrapper only adds a key computation.
- **Homogeneous short-prompt workloads**: when prompts are short and vary widely, KV-cache hit rates on the backend are already low; affinity provides little gain and reduces load distribution.
- **Deployments with aggressive autoscaling**: if backends are added and removed frequently, sessions will repin often and the affinity benefit is diluted.

## Developer notes

**Decorator pattern**: `StickySessionWrapper` in `internal/adapter/balancer/sticky.go` wraps any `domain.EndpointSelector` implementation. No factory or registry changes are needed to add a new inner balancer; the wrapper is applied in `ProxyServiceWrapper.applyStickySessions()` inside `internal/app/services/proxy.go`.

**Hashing**: 64-bit FNV-1a (`hash/fnv`) is used for all key derivation, including `first_user_message`. It is non-cryptographic and intended only as a compact routing hint; collisions are acceptable (two different sessions occasionally land on the same backend). Do not use these keys as security tokens.

**Import cycle avoidance**: `StickyOutcome` is defined in `internal/core/domain/routing.go` rather than in `internal/adapter/balancer/sticky.go`. This allows `internal/adapter/proxy/core` (the proxy engine shared layer) to read the outcome and write response headers without importing the balancer package, which would create a cycle.

**Purge wiring order**: `applyStickySessions()` assigns `s.stickyWrapper` and then immediately calls `s.discoverySvc.SetPurgeDeadEndpointsFn(s.PurgeDeadEndpoints)`. This ensures the write of `stickyWrapper` happens-before the health-checker goroutine can observe it via the purge callback. The registration happens inside `ProxyServiceWrapper.Start()`, not at construction time, to respect service startup ordering.

**Session store**: backed by `github.com/jellydator/ttlcache/v3` with `WithCapacity` (LRU) and `WithTTL` (sliding expiry). The `ttlcache.Get` call inside `Select` refreshes the TTL automatically on every hit.

Relevant source files: `internal/adapter/balancer/sticky.go`, `internal/core/domain/routing.go`, `internal/app/services/proxy.go`, `internal/app/services/discovery.go`, `internal/app/handlers/handler_proxy.go`, `internal/app/handlers/handler_stats_sticky.go`.

## See also

- [Load Balancing](load-balancing.md): underlying strategies that sticky sessions wrap
- [Health Checking](health-checking.md): health states and the routable concept
- [Configuration Reference](../configuration/reference.md#sticky-sessions): complete field reference

## Further Reading

- TensorFoundry's [What we found when we benchmarked Olla](https://tensorfoundry.io/blog/olla-performance-benchmarks) measures a 7.6x time-to-first-token improvement from sticky-session KV-cache affinity, for a 32k-token second turn routed across two backends - not a blanket figure across workloads.
