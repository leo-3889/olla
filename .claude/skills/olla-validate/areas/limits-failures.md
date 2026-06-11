# Area: limits-failures

Validates request limits, rate limiting and bad-input handling. Two sections -
the orchestrator tells you which to run:

- **Client section** (wave 1, both modes): targets the dedicated tight-limits
  Olla instance on `http://127.0.0.1:41142` (256KB body cap, 30 req/min per-IP,
  burst 5, backed by mock-a). **No `/_mock/behaviour` calls.** Do not hammer
  olla-main on 41141 - its rate limits are shared with the other agents.
- **Upstream section** (nightly, wave 3, runs solo): mutates mock-a behaviour
  to test how Olla handles a misbehaving upstream. Targets 41142 (short 10s
  response timeout makes hang tests fast). Reset mock-a when done.

Large payloads: build them in a file and send with `curl --data @file` -
inlining hundreds of KB on the command line fails with "argument list too
long" on Git Bash/Windows.

## Client section

### Quick checklist

1. Baseline: `POST http://127.0.0.1:41142/olla/proxy/v1/chat/completions`
   with the standard small body → 200.
2. Body cap, declared length: same route with a ~300KB body (pad one message
   with filler) → rejected. Current contract: the security chain rejects a
   declared oversized Content-Length with a blanket **403** ("Security
   validation failed", `internal/app/handlers/application.go`); 413 is also
   acceptable. Any 2xx or 5xx = FAIL.
3. Just-under cap (~200KB) → 200.
4. 429 rate limit: fire 45 rapid sequential requests → at least one 429;
   record at which request it first appears (expect after roughly
   burst+window allowance) and whether a Retry-After or rate-limit header is
   present (note, don't fail on header absence).
5. Health exemption: while rate-limited, `GET /internal/health` on 41142
   still 200 (health has its own generous limit).
6. Malformed JSON (`{"model":`) → 4xx, not 5xx.
7. Empty body POST → 4xx, not 5xx.
8. Wrong method: `DELETE /olla/proxy/v1/chat/completions` → 404/405, no 5xx.

### Nightly additions

9. Oversized **chunked** body (no Content-Length, stream >256KB chunked;
   `curl -H 'Transfer-Encoding: chunked'` with `--data-binary @file`) →
   rejected with 413 once the cap is crossed mid-read (recent hardening;
   regression guard - this path must NOT be a 403, it is detected during
   streaming).
10. Header size: a single ~80KB header (cap 64KB) → 431 or connection
    rejection - record actual; 5xx or hang = FAIL.
11. Anthropic message size: `POST /olla/anthropic/v1/messages` on 41142 with
    a ~2MB message (translator cap 1MB) → 4xx, record the status.
12. 429 recovery: after check 4, wait ~65s → requests succeed again.
13. Burst parallelism: 20 parallel requests immediately after recovery →
    mix of 200s and 429s, never 5xx, olla-limits stays healthy.

## Upstream section (nightly, solo)

1. Malformed upstream JSON: set `{"malformed_json":true}` on mock-a →
   `POST 41142 /olla/proxy/v1/chat/completions`: record what the client gets
   (passthrough of garbage or a 502 are both observable outcomes - note
   which); olla must not crash and `/internal/health` stays 200. Reset.
2. Upstream hang: `{"mode":"hang","latency_ms":60000}` on mock-a → request
   fails within ~15s (10s response timeout + margin), client receives a
   gateway-style error, no goroutine leak across 10 repeats
   (`/internal/process` before/after within 25%). Reset.
3. Upstream slow first byte: `{"mode":"slow","latency_ms":3000}` → request
   succeeds (3s < 5s response_header_timeout on 41142); record latency.
   Then `{"mode":"slow","latency_ms":8000}` → fails with a timeout error,
   not a hang. Reset.
4. Upstream 503 with `fail_health` false: `{"mode":"error","error_status":503}`
   → 5xx surfaces to client (current contract), endpoint not flapped into
   permanent removal - after reset, next request 200 within 10s.
5. Final: `POST /_mock/reset` on mock-a; confirm default behaviour and a
   clean 200 on 41142. Mandatory.
