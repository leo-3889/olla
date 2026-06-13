# Area: anthropic

Validates the Anthropic Messages API surface: passthrough to natively capable
backends and (nightly) the forced translation path. Target: olla-main on
`http://127.0.0.1:41141`. **Read-only - never call `POST /_mock/behaviour`.**
`GET /_mock/stats` is allowed.

The orchestrator tells you which section to run: **passthrough** (default
config, `passthrough_enabled: true`) or **translation-forced** (Olla restarted
with `passthrough_enabled: false`). Run only that section.

Standard body:
`{"model":"test-model","max_tokens":32,"messages":[{"role":"user","content":"ping"}]}`
Headers: `Content-Type: application/json`, `x-api-key: validate`,
`anthropic-version: 2023-06-01`.

## Passthrough section

### Quick checklist

1. `POST /olla/anthropic/v1/messages` (non-stream) → 200;
   `type:"message"`, `role:"assistant"`, `content[0].type:"text"` with
   non-empty text containing `BACKEND:`; `stop_reason` set;
   `usage.input_tokens > 0` and `usage.output_tokens > 0`.
2. The same response carries `X-Olla-Mode: passthrough` plus the standard
   `X-Olla-Endpoint` / `X-Olla-Request-ID` headers, and the serving endpoint
   is an anthropic-capable one (not mock-litellm-f; litellm goes via
   translation).
3. Streaming (`"stream":true`) → 200 `text/event-stream`; events arrive in
   valid order: `message_start` → `content_block_start` →
   `content_block_delta`(×N) → `content_block_stop` → `message_delta` →
   `message_stop`; deltas assemble to non-empty text.
4. `GET /olla/anthropic/v1/models` → 200, real Anthropic wire format:
   - Top-level envelope has `has_more` (boolean) and `first_id`/`last_id`.
   - `data[]` is non-empty.
   - Each entry has `type: "model"` (not `"chat"` — the official Anthropic SDK
     uses this as a deserialise discriminator; a wrong value breaks client
     compatibility), a non-empty `display_name`, and `created_at` as an ISO
     8601 string.
   FAIL if any of the above are missing or wrong.
5. `POST /olla/anthropic/v1/messages/count_tokens` with the standard body →
   200 with `input_tokens > 0`.
6. Invalid body (`{"model":"test-model"}` - no messages/max_tokens) → 4xx
   with an Anthropic-style error object (`type:"error"` or similar); FAIL on
   5xx or hang.
7. Sticky on the translator route: two requests with
   `X-Olla-Session-ID: validate-anthropic-1` → miss then hit, same endpoint.

### Nightly additions

8. Confirm wire-level passthrough via mock stats: note `/_mock/stats` before
   and after a burst of 5 messages - the serving mock's `/v1/messages` count
   rises and its `/v1/chat/completions` count does not (for those requests).
9. 20 parallel non-stream messages → all 200, all passthrough.
10. Streaming with a multi-block conversation (system + 3 user/assistant
    turns) → valid event stream.
11. `/internal/stats/translators` → anthropic translator present; passthrough
    counter consistent with the traffic you sent (record numbers).

## Translation-forced section (nightly; orchestrator restarted Olla with passthrough_enabled: false and reset mock stats)

1. Non-stream message → 200 with a **valid Anthropic-shape** response
   (`type:"message"`, `content[0].text` non-empty, `usage` populated) even
   though the backend spoke OpenAI.
2. `X-Olla-Mode` is absent or not `passthrough`.
3. Wire-level proof via `/_mock/stats`: after your burst, the serving mock's
   `/v1/chat/completions` count rose; `/v1/messages` did not.
4. Streaming → 200 with a syntactically valid Anthropic SSE event sequence
   (same ordering rules as passthrough check 3) synthesised from OpenAI
   chunks; assembled text non-empty.
5. `count_tokens` and `/olla/anthropic/v1/models` still work (200).
6. `/internal/stats/translators` shows translation (non-passthrough) counts
   rising; record passthrough/translation split and any fallback reasons.
7. Invalid body → 4xx Anthropic-style error (no 5xx).
