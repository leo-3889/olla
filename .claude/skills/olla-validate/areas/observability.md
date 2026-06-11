# Area: observability

Validates internal/status/stats endpoints, the unified model registry and
version info. Target: olla-main on `http://127.0.0.1:41141`. **Read-only —
never call `/_mock/behaviour`.** Other wave-1 agents are generating traffic
concurrently, so assert structure and plausibility, not exact counts.

## Quick checklist

1. `GET /internal/health` → 200, valid JSON.
2. `GET /internal/status` → 200, valid JSON; FAIL on any 5xx (this endpoint
   has a known history of panics — treat errors here as serious).
3. `GET /internal/status/endpoints` → 200; exactly 7 endpoints
   (mock-openai-a/b, mock-vllm-a, mock-litellm-b, mock-ollama-c,
   mock-lmstudio-d, mock-llamacpp-d); all healthy/routable.
4. `GET /internal/status/models` → 200, non-empty.
5. `GET /internal/stats/models` → 200, valid JSON.
6. `GET /internal/stats/sticky` → 200 and `enabled: true` (sticky is on in
   the harness config).
7. `GET /internal/stats/translators` → 200, anthropic translator listed.
8. `GET /internal/process` → 200; goroutines > 0, memory stats present.
9. `GET /version` → 200 with version info.
10. `GET /olla/models` → 200; includes a unified entry covering
    `shared-model`; that entry (or its detail view) references multiple
    endpoints/providers (it is served by mock-a, mock-c and mock-d).
11. `GET /olla/models/<id>` for one unified model id from check 10 → 200 with
    the same model; an unknown id → 404.
12. Two requests to `/internal/health` → distinct `X-Olla-Request-ID` values
    (if the header is set on internal routes; skip with note if not).

## Nightly additions

13. Unifier alias resolution: `llama3.1:8b` resolves via
    `/olla/models/llama3.1:8b` (or its documented alias lookup) to a unified
    model that lists the ollama endpoint. If alias lookup is not supported on
    that route, record actual behaviour as a note, not a FAIL.
14. Consistency: send 10 chat requests yourself to `/olla/proxy/` with model
    `test-model`, then confirm `/internal/stats/models` reflects activity for
    that model (counter increased vs your earlier reading).
15. `/internal/status/endpoints` per-endpoint stats: endpoints you know
    received traffic (cross-reference `GET /_mock/stats`) show non-zero
    request counts.
16. Sticky stats: after the core-routing agent has run (it always does in
    wave 1), `/internal/stats/sticky` shows `insertions > 0` and `hits > 0`.
    If you run before it finishes, generate two same-session requests
    yourself first.
17. Malformed queries: `/olla/models?bogus=1` and `/internal/status/<junk>` →
    no 5xx; sane 200/404.
18. Poll `/internal/process` 5 times over 2 minutes while wave-1 load runs →
    goroutine count is bounded (no monotonic climb of >50% across samples);
    record the series.
