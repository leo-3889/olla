# Area: resilience

Validates health checking, failover, recovery, sticky repinning and proxy
behaviour under backend failure. Target: olla-main on
`http://127.0.0.1:41141`. **This is the only wave-2 agent: you ARE allowed to
inject faults via `POST /_mock/behaviour` and `POST /_mock/reset`.**

Rules of engagement:
- Fault at most ONE mock at a time. Never fault mock-a and mock-b together
  (they are the only openai-compatible pair - faulting both removes all
  failover headroom).
- After EVERY scenario: `POST /_mock/reset` on the faulted mock, then poll
  `/internal/status/endpoints` until all 7 endpoints are healthy again before
  starting the next scenario. The recovery itself is an assertion each time
  (healthy within ~60s of reset = pass).
- Health probe timing: despite `check_interval: 2s` in the config, the health
  checker runs on a global 30s ticker (`DefaultHealthCheckInterval` in
  `internal/adapter/health/checker.go`), so allow up to **40s** for any
  health transition driven by probes. Request-path connection failures mark
  an endpoint unhealthy immediately - those are fast.
- `/olla/openai/` and `/olla/openai-compatible/` routes are inclusive by
  design: any OpenAI-compatible endpoint may serve them. Failover assertions
  on those routes must check "not the faulted endpoint", not a specific
  survivor.

Documented current behaviour (do not "fix" expectations to taste):
- Retry applies to **connection failures** (refused/reset/timeout), not HTTP
  responses: an upstream HTTP 5xx with a healthy connection is **forwarded to
  the client** (known gap, issue #144). Assert accordingly.
- 429/401/403 from a backend must NOT trip health/circuit breaking.

## Quick checklist (~3–5 min; dominated by the 30s probe tick)

1. Baseline: all 7 endpoints healthy in `/internal/status/endpoints`.
2. Health-fail failover: set `{"fail_health":true}` on mock-b (19432) →
   within 40s mock-openai-b goes non-routable. While it is down, 10 requests
   to `/olla/openai-compatible/v1/chat/completions` → all 200, none served
   by mock-b.
3. During the transition window itself, issue requests continuously (one per
   250ms from the moment you set the fault until the endpoint is marked
   down): fail_health only fails the probe route, so every request should
   still return 200 throughout. Any client error here = FAIL.
4. Recovery: reset mock-b → all endpoints healthy within 60s; subsequent
   unique-session requests reach mock-b again (loop until you see its marker,
   max 40 tries).
5. Request-time 5xx (healthy connection): set
   `{"mode":"error","error_status":500}` on mock-c (19433) → a request to
   `/olla/ollama/api/chat` returns 5xx to the client (current contract);
   olla-main itself stays healthy (`/internal/health` 200) and other routes
   are unaffected. Reset.

## Nightly additions

6. Connection-refused failover: ask the orchestrator's PID table - kill the
   mock-b process outright. Immediately issue 20 requests to
   `/olla/openai-compatible/` → all 200, none from mock-b (retry-on-
   connection-failure must hide the dead backend; brief first-request latency
   is fine). `/internal/status/endpoints` marks mock-openai-b unhealthy
   (request-path failures mark immediately). Restart mock-b with its original
   command line; healthy within 60s; traffic returns.
7. Sticky repin: pin session `validate-repin-1` (two turns, note the
   endpoint). Fault that endpoint's mock with `fail_health`. Wait for
   non-routable, then send turn 3 with the same session → 200 from a
   different endpoint and `X-Olla-Sticky-Session` is `repin` (or `miss` -
   record which; a 5xx or routing to the dead backend = FAIL). Reset.
8. Rate-limit semantics: set `{"mode":"error","error_status":429}` on mock-d
   → requests to `/olla/lmstudio/` surface 429; the endpoint must NOT be
   marked unhealthy/offline in `/internal/status/endpoints` (RateLimited is
   not a health failure). Reset. Same check with 401 (ConfigError class).
9. Hang/timeout: set `{"mode":"hang","latency_ms":30000}` on mock-c → a
   request to `/olla/ollama/api/chat` fails within the proxy response
   timeout (60s config; expect an error well before 70s - record actual) and
   olla-main remains responsive to other routes throughout (probe every 2s
   while waiting). Reset.
10. Mid-stream drop: set `{"drop_mid_stream":true}` on mock-e (19435) → a
    streaming request to `/olla/vllm/v1/chat/completions` terminates
    (truncated is acceptable; an indefinite hang or olla crash = FAIL);
    olla-main healthy afterwards; next streaming request after reset
    completes normally.
11. Flaky backend: set `{"mode":"flaky","error_rate":0.5,"error_status":500}`
    on mock-e, send 30 requests to `/olla/vllm/` → roughly half fail with
    5xx (current 5xx-forwarding contract), olla stays healthy, no goroutine
    runaway (`/internal/process` before/after within 25%). Reset.
12. Repeated kill/restore cycling (mini-chaos): 5 cycles of
    fail_health(mock-b) → wait down → reset → wait up (each cycle can take
    ~2 min given the 30s probe tick - budget ~10 min). After the 5th cycle,
    all endpoints healthy, sticky stats endpoint still serves, goroutines
    within 25% of your baseline from check 1.
13. Final state assertion: all seven mocks report default behaviour
    (`GET /_mock/behaviour`), all 7 endpoints healthy, `/internal/status`
    200. This is mandatory - you must leave the fleet clean.
