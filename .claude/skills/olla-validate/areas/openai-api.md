# Area: openai-api

Validates the OpenAI-compatible API surface and streaming behaviour through
olla-main on `http://127.0.0.1:41141`. **Read-only - never call
`/_mock/behaviour`** (the slow-stream check below uses ollamock's startup
pacing only in nightly via instructions to the orchestrator; if you cannot do
a check without mutating mock state, mark it skip with a note).

## Quick checklist

1. `GET /olla/proxy/v1/models` → 200, `object: "list"`, `data[]` non-empty,
   ids include `test-model`.
2. Non-stream chat: `POST /olla/proxy/v1/chat/completions` with
   `{"model":"test-model","messages":[{"role":"user","content":"say hi"}],"max_tokens":32}`
   → 200; `choices[0].message.role == "assistant"`; non-empty content;
   `choices[0].finish_reason == "stop"`; `usage.prompt_tokens > 0` and
   `usage.completion_tokens > 0`; response `model` echoes the request model.
3. Streaming chat: same body plus `"stream":true` → 200 with
   `Content-Type: text/event-stream`; multiple `data:` chunks of
   `chat.completion.chunk`; deltas assemble to non-empty content; a chunk with
   `finish_reason:"stop"`; terminates with `data: [DONE]`.
4. X-Olla headers present on the streaming response too (they are sent before
   the body).
5. Ollama native non-stream: `POST /olla/ollama/api/chat`
   (`"stream":false`) → 200, `done: true`, `message.content` non-empty,
   `prompt_eval_count`/`eval_count` > 0.
6. Malformed body to `/olla/proxy/v1/chat/completions` (`{"model":`) → must
   not 5xx and must not hang; record the observed status (2xx or 4xx are both
   acceptable). Olla is a transparent proxy and delegates body validation to
   the backend by design — it opportunistically extracts the model field and,
   on failure, forwards to a fallback backend. Only a 5xx or a hang is a FAIL.

## Nightly additions

7. Ollama native streaming: `POST /olla/ollama/api/chat` with stream
   defaulted (omit the field) → NDJSON lines, last line `done:true` with eval
   counts.
8. Ollama generate: `POST /olla/ollama/api/generate`
   `{"model":"llama3.1:8b","prompt":"ping"}` → streams, final `done:true`.
9. `GET /olla/ollama/api/tags` → 200, models array includes `llama3.1:8b`.
10. `POST /olla/openai/v1/completions` (legacy completions)
    `{"model":"test-model","prompt":"ping","max_tokens":16}` → 200 with a
    `choices[0].text`.
11. Concurrency under streaming: 20 parallel streaming chats → all complete
    with `[DONE]`, none hang past 30s, all content non-empty.
12. Large prompt: single message with ~3MB of text (under the 5MB cap) →
    200 (stream or non-stream). FAIL on 413 (cap is 5MB) or 5xx. Build the
    body in a file and send with `curl --data @file` (inline args this big
    fail on Git Bash/Windows).
13. Anthropic-format body sent to the OpenAI route (wrong-shape input:
    `{"model":"test-model","max_tokens":10,"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`)
    → must not crash Olla; record the status observed (2xx/4xx both
    acceptable; 5xx = FAIL).
14. Response-time sanity: median of 20 sequential non-stream requests
    < 250ms (mocks are instant; this catches gross proxy regressions).
    WARN, not FAIL, if exceeded - machines vary.
