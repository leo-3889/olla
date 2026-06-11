# Area: core-routing

Validates the proxy core: provider-scoped routes, model routing, response
headers, load balancing and sticky sessions. Target: olla-main on
`http://127.0.0.1:41141`. **Read-only — never call `/_mock/behaviour`.**

Expected backend ownership (assert via `X-Olla-Endpoint` header and the
`BACKEND:<name>` marker in response content):

| Route prefix | Eligible endpoints | Marker(s) |
|---|---|---|
| `/olla/proxy/` | all seven | any |
| `/olla/openai/`, `/olla/openai-compatible/` | inclusive **by design**: any endpoint whose profile declares OpenAI compatibility; with model `test-model` that narrows to mock-openai-a/b, mock-vllm-e, mock-litellm-f | mock-a, mock-b, mock-e, mock-f |
| `/olla/ollama/` | mock-ollama-c | mock-c |
| `/olla/lmstudio/` (+ `/olla/lm-studio/`, `/olla/lm_studio/`) | mock-lmstudio-d | mock-d |
| `/olla/vllm/` | mock-vllm-e | mock-e |
| `/olla/litellm/` | mock-litellm-f | mock-f |
| `/olla/llamacpp/` | mock-llamacpp-g | mock-g |

Standard chat body (OpenAI-shaped routes):
`{"model":"test-model","messages":[{"role":"user","content":"ping"}],"max_tokens":20,"stream":false}`
Ollama route body (`/olla/ollama/api/chat`):
`{"model":"llama3.1:8b","messages":[{"role":"user","content":"ping"}],"stream":false}`

## Quick checklist

1. `POST /olla/proxy/v1/chat/completions` → 200; body contains `BACKEND:`;
   headers `X-Olla-Endpoint`, `X-Olla-Model`, `X-Olla-Backend-Type`,
   `X-Olla-Request-ID`, `X-Olla-Response-Time` all present and non-empty.
2. Each provider route in the table above → 200 AND the serving endpoint is in
   that route's eligible set (both header and body marker agree). For ollama
   use the native body/path. One request per route is enough.
3. Provider aliases: `/olla/lm-studio/v1/chat/completions` and
   `/olla/lm_studio/v1/chat/completions` both 200 from mock-d.
4. `X-Olla-Backend-Type` matches the provider for at least the ollama,
   lm-studio and vllm routes.
5. Model routing on `/olla/proxy/`: model `llama3.1:8b` → served by
   mock-ollama-c; model `phi-4` → served by mock-d or mock-g (the only
   hosts). If `X-Olla-Routing-*`
   headers are present, record their values; FAIL only if the request lands on
   an endpoint that does not serve the model.
6. Sticky: two POSTs to `/olla/proxy/v1/chat/completions` with header
   `X-Olla-Session-ID: validate-core-1` → turn 1 `X-Olla-Sticky-Session: miss`
   + `X-Olla-Sticky-Key-Source: session_header`; turn 2 `hit` + same
   `X-Olla-Endpoint` + same body marker. `X-Olla-Session-ID` echoed back.
7. Distribution sanity: 10 requests to `/olla/openai-compatible/...` with
   *unique* session IDs → at least 2 distinct endpoints observed (candidates
   for `test-model`: mock-a/b/e/f). WARN (not FAIL) if all 10 land on one.
8. `GET /olla/nonexistent/v1/models` → 404 (clean error, no hang).

## Nightly additions

9. Repeat check 2 with `"stream":true` on every route (ollama native streams
   NDJSON; others SSE) — each stream completes with its terminator and carries
   the right backend marker.
10. Model `shared-model` 15× on `/olla/proxy/` → only ever served by
    mock-a / mock-c / mock-d / mock-e / mock-g (never mock-b or mock-f,
    which don't host it).
11. Model `beta-model` 5× → only mock-b or mock-f.
12. Sticky prefix_hash fallback: two identical POSTs (same messages, no
    session header, no auth header) → turn 2 `X-Olla-Sticky-Session: hit` with
    `X-Olla-Sticky-Key-Source: prefix_hash`.
13. Balance distribution: 100 unique-session requests on
    `/olla/openai-compatible/` with model `test-model` → at least 3 of the
    four candidates (mock-a/b/e/f) receive traffic and no single endpoint
    serves more than 60%.
14. Concurrency: 50 parallel POSTs to `/olla/proxy/` → all 200, all carry
    unique `X-Olla-Request-ID` values.
15. `GET /olla/proxy/v1/models` → 200, contains `test-model`, `shared-model`,
    `beta-model`, `llama3.1:8b`, `phi-4`.
16. Cross-check `/_mock/stats` on each mock (`GET` only — allowed) against
    where you sent traffic: every mock you observed in markers shows non-zero
    counts on the paths you exercised.
