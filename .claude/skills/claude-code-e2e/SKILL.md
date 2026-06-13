---
name: claude-code-e2e
description: >
  End-to-end proof that Claude Code works turn-by-turn through Olla's Anthropic
  surface against a real Ollama backend. Boots a fresh Olla in front of a
  user-supplied Ollama endpoint, then exercises both legs - native Anthropic
  passthrough and forced OpenAI<->Anthropic translation - with two tiers:
  Tier-A direct-wire protocol assertions (curl + jq against Olla) and Tier-B a
  real headless Claude Code run implementing a fixed feature in a fresh clone of
  thushan/smash. Use to validate the Anthropic translation/passthrough layer
  under genuine Claude Code traffic. Needs a working Ollama and the claude CLI;
  not CI-safe (real LLM, real network). Runs on Sonnet.
argument-hint: "[--ollama-endpoint URL] [--ollama-model NAME] [--repo URL] [--repo-sha SHA] [--skip-tier-b] [--profile]"
model: sonnet
---

# /claude-code-e2e - Claude Code through Olla, end to end

Proves the thing unit tests cannot: that a real Claude Code instance completes
multi-turn tool loops through Olla's Anthropic endpoints, against a real model,
on both the passthrough and translation paths.

Two models are in play - keep them straight:
- **System under test**: a fresh `claude` CLI -> Olla -> Ollama -> the local
  model. This is what we are proving works.
- **This orchestrator**: Sonnet (`model: sonnet`), drives the phases, runs the
  curl/jq assertions, writes the report. Never delegate the whole run to one
  subagent - orchestration stays here.

## Two legs, two tiers

| | Passthrough leg | Translation leg |
|---|---|---|
| Olla config | `passthrough_enabled: true` | `passthrough_enabled: false` |
| Path exercised | native Anthropic forwarded to Ollama `/v1/messages` | Anthropic -> OpenAI -> Ollama `/v1/chat/completions` |

| Tier | What | Gates? |
|---|---|---|
| A - protocol | direct curl + jq against Olla's `/olla/anthropic/*` | **yes** |
| B - agent | headless Claude Code implements a fixed feature in a smash clone | no (model-dependent; reported) |

Run order per invocation: fitness preflight -> build Olla -> clone smash ->
**leg A (passthrough)**: Tier-A then Tier-B -> reset clone, restart Olla ->
**leg B (translation)**: Tier-A then Tier-B -> report.

## Arguments and defaults

- `--ollama-endpoint URL` - default `http://localhost:11434`. If that is not
  reachable and no flag was given, **ask the user** (AskUserQuestion) for an
  endpoint rather than failing silently.
- `--ollama-model NAME` - only used if no model is already loaded. The skill
  prefers the first model reported loaded by `/api/ps`.
- `--repo URL` - default `https://github.com/thushan/smash`.
- `--repo-sha SHA` - default `057ad782490b2162d77923316a623101666eea1a` (main).
- `--skip-tier-b` - run only the Tier-A protocol gate (fast).
- `--profile` - boot Olla with pprof and run the performance phase (CPU/allocs
  profile of Olla's hot path under load + per-leg goroutine/heap leak check).
  Off by default; numbers are informational and never gate (leak growth -> WARN).

The Tier-B task is fixed and defined in `tasks/totalsize.md` (relative to this
skill file). Read it before leg A.

## Output discipline (per the skills spec)

No `set -e` - keep gathering evidence past failures. `curl -sf` for quiet
success, capture full body on failure. **Every JSON assertion goes through `jq`
- never `grep` a JSON body.** Status codes are categorised, not "non-200 =
fail": 5xx / connection error (000) -> FAIL; a 404 on `count_tokens` -> FAIL
(it is a regression, see Tier-A); model declining to call a tool -> WARN (model
capability, not an Olla bug). Echo `PASS:` / `FAIL:` / `WARN:` on every
assertion so the run is scannable.

## Phase 0 - Setup, OS detection, cleanup trap

Anchor at repo root and stay anchored in every phase. Forward slashes
throughout, no absolute paths.

```bash
ROOT="$(git rev-parse --show-toplevel)"; cd "$ROOT"

# --- OS portability: the only OS-specific bits are the Go binary suffix and
# the kill command. `go env GOEXE` yields the suffix on every platform; kill_pid
# tries POSIX kill then Windows taskkill - so no $OSTYPE branching is needed and
# nothing OS-specific is hard-coded below. ---
EXE=$(go env GOEXE)                       # ".exe" on Windows, "" elsewhere
kill_pid() { kill "$1" 2>/dev/null || taskkill //F //PID "$1" 2>/dev/null || true; }
TMPL="$ROOT/.claude/skills/claude-code-e2e/config.regression.yaml.tmpl"

# --- args (defaults, then parse the user's flags over them) ---
OLLAMA="${OLLAMA_ENDPOINT:-http://localhost:11434}"   # --ollama-endpoint
OLLAMA_MODEL_FLAG=""                                   # --ollama-model
REPO="https://github.com/thushan/smash"                # --repo
REPO_SHA="057ad782490b2162d77923316a623101666eea1a"    # --repo-sha
SKIP_TIER_B=0
PROFILE=0                                                # --profile: pprof Olla + perf phase
MAXT=180                                                # curl --max-time on model-path calls (s)
PROMPT_FILE="$ROOT/.claude/skills/claude-code-e2e/tasks/totalsize.prompt.txt"
while [ "$#" -gt 0 ]; do
  case "$1" in
    --ollama-endpoint) OLLAMA="$2"; shift 2 ;;
    --ollama-model)    OLLAMA_MODEL_FLAG="$2"; shift 2 ;;
    --repo)            REPO="$2"; shift 2 ;;
    --repo-sha)        REPO_SHA="$2"; shift 2 ;;
    --skip-tier-b)     SKIP_TIER_B=1; shift ;;
    --profile)         PROFILE=1; shift ;;
    *) echo "WARN: ignoring unknown arg: $1"; shift ;;
  esac
done

OLLA_PORT=41151
BASE="http://127.0.0.1:${OLLA_PORT}/olla/anthropic"
TOKEN="olla-regression-token"            # dummy bearer; Olla->Ollama needs no auth
PPROF="http://127.0.0.1:19841/debug/pprof"   # Olla's pprof server (hardcoded addr, on with -profile)

RUN_TS=$(date -u +%Y%m%d-%H%M%S)
GIT_SHA=$(git rev-parse --short HEAD)
RESULTS=test/results
# NB: not named TMP - that collides with the Windows %TMP% env var and would be
# inherited by `go test`/`go build` as a bogus relative temp dir.
REGRESSION_TMP=test/regression/tmp
RUNDIR="${REGRESSION_TMP}/run-${RUN_TS}"
CLONE="${RUNDIR}/smash"
LOGDIR="${RESULTS}/logs/claude-code-e2e-${RUN_TS}"
REPORT="${RESULTS}/claude-code-e2e-${RUN_TS}.md"
mkdir -p "$RUNDIR" "$LOGDIR" "$RESULTS"

PASS_COUNT=0 FAIL_COUNT=0 WARN_COUNT=0
WARNS=""
assert_pass() { echo "PASS: $1"; PASS_COUNT=$((PASS_COUNT + 1)); }
assert_fail() { echo "FAIL: $1"; FAIL_COUNT=$((FAIL_COUNT + 1)); }
assert_warn() {  # build WARNS as a string (no Bash-4 arrays); avoid a leading blank line
  echo "WARN: $1"; WARN_COUNT=$((WARN_COUNT + 1))
  [ -n "$WARNS" ] && WARNS="${WARNS}\n  - $1" || WARNS="  - $1"
}

OLLA_PID=""
cleanup() {
  echo "--- claude-code-e2e cleanup ---"
  if [ -n "$OLLA_PID" ]; then
    kill_pid "$OLLA_PID" && echo "INFO: killed Olla PID ${OLLA_PID}" || echo "WARN: could not kill Olla PID ${OLLA_PID}"
  fi
  wait 2>/dev/null || true
  # Throwaway clone + rendered configs go; report + logs are preserved.
  rm -rf "$RUNDIR" 2>/dev/null || true
  echo "PASS: cleanup complete -- port ${OLLA_PORT} released, clone removed (preserved: ${REPORT}, ${LOGDIR})"
  OVERALL=$([ "$FAIL_COUNT" -gt 0 ] && echo FAIL || echo PASS)
  printf '%s %s | /claude-code-e2e | %s | %dP/%dF/%dW | %s | report:%s\n' \
    "${RUN_TS:0:8}" "${RUN_TS:9:6}" "$OVERALL" "$PASS_COUNT" "$FAIL_COUNT" "$WARN_COUNT" "$GIT_SHA" "$REPORT" \
    >> "${RESULTS}/last-runs.md"
}
trap 'cleanup' EXIT INT TERM
```

If port `41151` is already bound, kill the stale Olla from a previous run or
abort with a clear message before continuing.

## Phase 1 - Required tools + fitness preflight (hard gate)

Required tools - `command -v` each, FAIL with an install hint if missing:
`go`, `git`, `jq`, `curl`, and **`claude`** (the CLI under test - if absent,
abort: "Claude Code CLI not on PATH; install it before running this skill").

Then the **fitness gate** - this is your "stop the run if it doesn't meet the
rigour" check. Abort (exit non-zero so the trap cleans up) on any hard fail:

```bash
# 1) endpoint reachable; if not and no --ollama-endpoint was given, ASK the user.
if ! curl -sf --connect-timeout 5 "${OLLAMA}/api/version" >/dev/null; then
  echo "FAIL: Ollama not reachable at ${OLLAMA}"   # -> AskUserQuestion for an endpoint, retry once
  exit 1
fi

# 2) choose the model: first loaded model wins, else the --ollama-model flag.
PS=$(curl -sf "${OLLAMA}/api/ps")
MODEL=$(echo "$PS" | jq -r '.models[0].name // empty')
if [ -z "$MODEL" ]; then
  [ -n "$OLLAMA_MODEL_FLAG" ] || { echo "FAIL: no model loaded and --ollama-model not given"; exit 1; }
  MODEL="$OLLAMA_MODEL_FLAG"
  # warm-load it so /api/ps reports its real context_length
  curl -sf "${OLLAMA}/api/generate" -d "{\"model\":\"${MODEL}\",\"prompt\":\"hi\",\"stream\":false}" >/dev/null
  PS=$(curl -sf "${OLLAMA}/api/ps")
fi

# 3) tool capability is mandatory - Tier-B and Tier-A tool checks need it.
# Prefer /api/tags; fall back to /api/show (capabilities surface there too on
# newer Ollama) before hard-failing a model as non-tool-capable.
CAPS=$(curl -sf "${OLLAMA}/api/tags" | jq -r --arg m "$MODEL" '.models[] | select(.name==$m) | .capabilities[]?')
if ! echo "$CAPS" | grep -qx tools; then
  CAPS=$(curl -sf "${OLLAMA}/api/show" -d "{\"name\":\"${MODEL}\"}" | jq -r '.capabilities[]?' 2>/dev/null)
fi
if echo "$CAPS" | grep -qx tools; then
  assert_pass "model ${MODEL} advertises tools capability"
else
  assert_fail "model ${MODEL} has no tools capability (caps: $(echo "$CAPS" | tr '\n' ',')) - cannot exercise tool loops"
  exit 1
fi
# Thinking models let A7 verify reasoning survives the translation path.
MODEL_THINKS=0; echo "$CAPS" | grep -qx thinking && MODEL_THINKS=1

# 4) context window - the killer constraint. /api/ps reports context_length for
# the loaded model on Ollama 0.30+; fall back to /api/show for older servers.
CTX=$(echo "$PS" | jq -r --arg m "$MODEL" '.models[] | select(.name==$m) | .context_length // empty')
if [ -z "$CTX" ]; then
  CTX=$(curl -sf "${OLLAMA}/api/show" -d "{\"name\":\"${MODEL}\"}" \
        | jq -r '[.model_info[] ] as $_ | (.model_info | to_entries[] | select(.key|endswith(".context_length")) | .value) // empty' 2>/dev/null | head -1)
fi
CTX=${CTX:-0}
if [ "$CTX" -lt 16384 ]; then
  assert_fail "context_length ${CTX} < 16384 - too small for Claude Code's system prompt + tools; raise OLLAMA_CONTEXT_LENGTH and reload"
  exit 1
elif [ "$CTX" -lt 32768 ]; then
  assert_warn "context_length ${CTX} (<32768) - Tier-B may truncate on larger files"
else
  assert_pass "context_length ${CTX} sufficient"
fi
echo "INFO: model=${MODEL} ctx=${CTX} endpoint=${OLLAMA}"
```

## Phase 2 - Build a fresh Olla

```bash
# Log to a file and check the exit directly - piping to tee would mask a build
# failure behind tee's exit status and leave a stale/absent binary.
if go build -o "build/regression/olla${EXE}" . > "${LOGDIR}/build.log" 2>&1; then
  assert_pass "fresh Olla built"
else
  assert_fail "Olla build failed"; tail -20 "${LOGDIR}/build.log"; exit 1
fi
```

## Phase 3 - Clone smash at the pinned SHA

```bash
git clone --quiet "$REPO" "$CLONE" || { assert_fail "clone $REPO failed"; exit 1; }
git -C "$CLONE" checkout --quiet "$REPO_SHA" || { assert_fail "checkout $REPO_SHA failed"; exit 1; }
( cd "$CLONE" && go mod download ) > "${LOGDIR}/go-mod-download.log" 2>&1 \
  || { assert_fail "go mod download failed"; tail -20 "${LOGDIR}/go-mod-download.log"; exit 1; }
# baseline must be green before we let an agent touch it
( cd "$CLONE" && go test ./pkg/analysis/... ) >"${LOGDIR}/baseline-test.log" 2>&1 \
  && assert_pass "smash baseline pkg/analysis tests green at ${REPO_SHA}" \
  || { assert_fail "smash baseline tests not green - bad SHA or toolchain"; exit 1; }
```

## Phase 4 - Render config + run each leg

Define two reusable helpers, then call them once per leg.

### Render + boot Olla for a leg

```bash
render_config() {  # $1 = passthrough (true|false) -> echoes rendered path
  local pt="$1" out="${RUNDIR}/config.${1}.yaml"
  sed -e "s|__OLLA_PORT__|${OLLA_PORT}|g" \
      -e "s|__OLLAMA_URL__|${OLLAMA}|g" \
      -e "s|__PASSTHROUGH__|${pt}|g" \
      "$TMPL" > "$out"
  echo "$out"
}

boot_olla() {  # $1 = rendered config path
  # -profile turns on Olla's pprof server (localhost:19841) when --profile is set.
  local prof=""; [ "$PROFILE" = 1 ] && prof="-profile"
  build/regression/olla${EXE} $prof --config "$1" > "${LOGDIR}/olla-${LEG}.log" 2>&1 &
  OLLA_PID=$!
  # Readiness: Olla healthy, then model discovery has populated the Anthropic
  # models list. Assert the list is non-empty rather than matching an exact id -
  # the unifier may surface the model under an alias, so a name match is brittle.
  local ok=0 i=0
  while [ "$i" -lt 60 ]; do
    if curl -sf "http://127.0.0.1:${OLLA_PORT}/internal/health" >/dev/null \
       && curl -sf "${BASE}/v1/models" | jq -e '(.data | length) > 0' >/dev/null 2>&1; then
      ok=1; break
    fi
    i=$((i + 1)); sleep 1
  done
  [ "$ok" = 1 ] || { assert_fail "[${LEG}] Olla not ready in 60s (see ${LOGDIR}/olla-${LEG}.log)"; return 1; }
  if [ "$PROFILE" = 1 ]; then
    local j=0; while [ "$j" -lt 15 ]; do curl -sf --max-time 3 "${PPROF}/" >/dev/null 2>&1 && break; j=$((j + 1)); sleep 1; done
  fi
  assert_pass "[${LEG}] Olla up and models discovered"
}

stop_olla() { [ -n "$OLLA_PID" ] && kill_pid "$OLLA_PID"; OLLA_PID=""; wait 2>/dev/null || true; }
```

**Execution note (read carefully):** run the whole per-leg driver (the
`for LEG ...` loop below, with the helper functions sourced ahead of it) as a
single **foreground** Bash tool call. Do *not* use the Bash tool's
`run_in_background` for Olla - backgrounding happens *inside* the script via the
`&` in `boot_olla`, so `OLLA_PID=$!` captures the pid in the same shell where
`stop_olla` and the cleanup trap can reach it. `run_in_background` would spawn a
detached shell and the pid would be lost, leaving Olla orphaned between legs.

### Tier-A - direct-wire protocol assertions

`tier_a` runs against `$BASE` for the current `$LEG`; `$PT` is the leg's
passthrough flag. The path each leg claims to exercise is **proved**, not
assumed, by two checks: **A2b** asserts the `X-Olla-Mode` response header
(`passthrough` present on the passthrough leg, absent on translation), and
**A6** asserts the matching `/internal/stats/translators` counter advanced over
the leg. Together they make "I flipped the config" verifiable rather than
implicit. The `count_tokens` check (A5) is the same on both legs - it is a
separate translator handler, not the message route.

```bash
# post: write the JSON body to a file, echo ONLY the HTTP code. Avoids any
# body/code string-splitting (Go encodes a trailing newline on every response).
#   usage: code=$(post URL BODY OUTFILE); then jq over OUTFILE.
post() {  # $1=url $2=body $3=outfile -> echoes http_code; response headers -> $3.hdr
  curl -s -o "$3" -D "${3}.hdr" -w '%{http_code}' --connect-timeout 5 --max-time "$MAXT" \
    -X POST "$1" -H "content-type: application/json" -H "x-api-key: ${TOKEN}" \
    -H "anthropic-version: 2023-06-01" -d "$2"
}

tier_a() {
  local code f
  local STATS="http://127.0.0.1:${OLLA_PORT}/internal/stats/translators"

  # Snapshot translator routing counters before any message traffic, so A6 can
  # prove which path actually executed (not just "I flipped the config").
  local sb=$(curl -sf --max-time 10 "$STATS")
  local PB=$(echo "$sb" | jq -r '.summary.total_passthrough // 0')
  local TB=$(echo "$sb" | jq -r '.summary.total_translations // 0')

  # A1 - models listing (non-empty data array)
  f="${LOGDIR}/${LEG}-models.json"
  code=$(curl -s -o "$f" -w '%{http_code}' --connect-timeout 5 --max-time 30 "${BASE}/v1/models" -H "x-api-key: ${TOKEN}")
  if [ "$code" = 200 ] && jq -e '(.data | length) > 0' "$f" >/dev/null 2>&1; then
    assert_pass "[${LEG}] A1 /v1/models lists $(jq -r '.data | length' "$f") model(s)"
  else
    assert_fail "[${LEG}] A1 /v1/models (code ${code})"
  fi

  # A2 - non-streaming message: well-formed envelope
  f="${LOGDIR}/${LEG}-nonstream.json"
  # max_tokens=256: thinking models exhaust a 64-token budget inside reasoning before
  # emitting any text block; 256 clears the reasoning phase on a trivial prompt.
  code=$(post "${BASE}/v1/messages" \
    "{\"model\":\"${MODEL}\",\"max_tokens\":256,\"messages\":[{\"role\":\"user\",\"content\":\"Reply with exactly one word: pong\"}]}" "$f")
  # Use select() rather than [0] so thinking-model responses (where content[0].type=="thinking")
  # don't false-fail; we require at least one text block, not that the first block is text.
  if [ "$code" = 200 ] \
     && [ "$(jq -r '.type' "$f")" = message ] \
     && [ "$(jq -r '[.content[]? | select(.type=="text")] | length' "$f")" -ge 1 ] \
     && [ "$(jq -r '.stop_reason // empty' "$f")" != "" ] \
     && [ "$(jq -r '.usage.output_tokens // 0' "$f")" -gt 0 ]; then
    assert_pass "[${LEG}] A2 non-stream message well-formed (stop_reason=$(jq -r '.stop_reason' "$f"))"
  elif [ "${code:0:1}" = 5 ] || [ "$code" = 000 ]; then
    assert_fail "[${LEG}] A2 non-stream upstream/proxy error (code ${code})"
  else
    assert_fail "[${LEG}] A2 non-stream malformed (code ${code}); body in $f"
  fi

  # A2b - X-Olla-Mode header proves the path. Olla sets it to 'passthrough' only
  # on the passthrough route; it is absent on translation. (Header grep, not JSON.)
  local mode=$(grep -i '^x-olla-mode:' "${f}.hdr" 2>/dev/null | tr -d '\r' | awk '{print $2}')
  if [ "$PT" = true ]; then
    [ "$mode" = passthrough ] && assert_pass "[${LEG}] A2b X-Olla-Mode: passthrough present" \
      || assert_fail "[${LEG}] A2b expected X-Olla-Mode: passthrough, got '${mode:-<absent>}'"
  else
    [ -z "$mode" ] && assert_pass "[${LEG}] A2b X-Olla-Mode absent (translation path)" \
      || assert_fail "[${LEG}] A2b expected no X-Olla-Mode on translation, got '${mode}'"
  fi

  # A3 - streaming SSE event order + no mid-stream error event
  # max_tokens=512: thinking models (e.g. gemma4) spend many tokens on reasoning before
  # emitting visible content; 64 exhausts the budget during thinking and produces empty output.
  curl -sN --connect-timeout 5 --max-time "$MAXT" -X POST "${BASE}/v1/messages" \
    -H "content-type: application/json" -H "x-api-key: ${TOKEN}" -H "anthropic-version: 2023-06-01" \
    -d "{\"model\":\"${MODEL}\",\"max_tokens\":512,\"stream\":true,\"messages\":[{\"role\":\"user\",\"content\":\"Count: one two three\"}]}" \
    > "${LOGDIR}/${LEG}-stream.sse" 2>/dev/null
  local f="${LOGDIR}/${LEG}-stream.sse"
  local n_start=$(grep -c '^event: message_start' "$f")
  local n_delta=$(grep -c '^event: content_block_delta' "$f")
  local n_stop=$(grep -c '^event: message_stop' "$f")
  local n_err=$(grep -c '^event: error' "$f")
  if [ "$n_start" -ge 1 ] && [ "$n_delta" -ge 1 ] && [ "$n_stop" -ge 1 ] && [ "$n_err" -eq 0 ]; then
    assert_pass "[${LEG}] A3 SSE sequence intact (start/${n_start} delta/${n_delta} stop/${n_stop})"
  elif [ "$n_err" -gt 0 ]; then
    assert_fail "[${LEG}] A3 mid-stream 'event: error' x${n_err} (panic-mid-stream regression?); see ${f}"
  else
    assert_fail "[${LEG}] A3 SSE sequence broken (start/${n_start} delta/${n_delta} stop/${n_stop})"
  fi

  # A4 - tool_use round-trip. Model declining is a WARN, not a FAIL.
  f="${LOGDIR}/${LEG}-tool.json"
  code=$(post "${BASE}/v1/messages" \
    "{\"model\":\"${MODEL}\",\"max_tokens\":256,\"tools\":[{\"name\":\"get_weather\",\"description\":\"Get weather for a city\",\"input_schema\":{\"type\":\"object\",\"properties\":{\"city\":{\"type\":\"string\"}},\"required\":[\"city\"]}}],\"messages\":[{\"role\":\"user\",\"content\":\"Use the get_weather tool for Sydney.\"}]}" "$f")
  if [ "$code" != 200 ]; then
    assert_fail "[${LEG}] A4 tool request failed (code ${code}); see $f"
  elif [ "$(jq -r '[.content[]? | select(.type=="tool_use")] | length' "$f")" -ge 1 ]; then
    if [ "$(jq -r '[.content[]? | select(.type=="tool_use")][0].id // empty' "$f")" != "" ] \
       && [ "$(jq -r '[.content[]? | select(.type=="tool_use")][0].name' "$f")" = get_weather ] \
       && [ "$(jq -r '[.content[]? | select(.type=="tool_use")][0].input | type=="object"' "$f")" = true ]; then
      assert_pass "[${LEG}] A4 tool_use block well-formed (stop_reason=$(jq -r '.stop_reason' "$f"))"
    else
      assert_fail "[${LEG}] A4 tool_use block malformed; see $f"
    fi
  else
    assert_warn "[${LEG}] A4 model did not emit a tool_use block (model capability, not Olla)"
  fi

  # A5 - count_tokens. This is a separately-registered translator handler, not
  # the /v1/messages passthrough route, so it proves Claude Code's token-count
  # compatibility (Claude Code calls it for real) rather than message routing.
  # It must answer, not 404 - Ollama itself declares token_counting_404, so a
  # 404 here means the dedicated handler isn't intercepting.
  f="${LOGDIR}/${LEG}-count.json"
  code=$(post "${BASE}/v1/messages/count_tokens" \
    "{\"model\":\"${MODEL}\",\"messages\":[{\"role\":\"user\",\"content\":\"how many tokens is this\"}]}" "$f")
  if [ "$code" = 404 ]; then
    assert_fail "[${LEG}] A5 count_tokens 404 - limitation filter not intercepting (Ollama 404 leaked through)"
  elif [ "$code" = 200 ] && [ "$(jq -r '.input_tokens // 0' "$f")" -gt 0 ]; then
    assert_pass "[${LEG}] A5 count_tokens -> input_tokens=$(jq -r '.input_tokens' "$f")"
  else
    assert_fail "[${LEG}] A5 count_tokens unexpected (code ${code}); see $f"
  fi

  # A6 - routing proof via translator stats delta. The /v1/messages calls above
  # must have incremented the leg's own counter; if the wrong counter moved, the
  # config flip did not actually change the path.
  local sa=$(curl -sf --max-time 10 "$STATS")
  local PA=$(echo "$sa" | jq -r '.summary.total_passthrough // 0')
  local TA=$(echo "$sa" | jq -r '.summary.total_translations // 0')
  if [ "$PT" = true ]; then
    [ "$PA" -gt "$PB" ] && assert_pass "[${LEG}] A6 passthrough counter advanced (+$((PA - PB)))" \
      || assert_fail "[${LEG}] A6 no passthrough delta (before=${PB} after=${PA}); leg may have translated"
  else
    [ "$TA" -gt "$TB" ] && assert_pass "[${LEG}] A6 translation counter advanced (+$((TA - TB)))" \
      || assert_fail "[${LEG}] A6 no translation delta (before=${TB} after=${TA}); leg may have passed through"
  fi

  # A7 - reasoning survives translation. Only meaningful on the translation leg
  # with a thinking-capable model: confirms the OpenAI reasoning/reasoning_content
  # field is mapped to Anthropic thinking blocks rather than dropped. WARN (not
  # FAIL) if absent - the model may not emit reasoning for a trivial prompt - so
  # model non-determinism never fails the gate; the unit tests are the hard guard.
  if [ "$PT" = false ] && [ "${MODEL_THINKS:-0}" = 1 ]; then
    if grep -q 'thinking_delta' "${LOGDIR}/${LEG}-stream.sse" 2>/dev/null; then
      assert_pass "[${LEG}] A7 reasoning preserved as thinking blocks through translation"
    else
      assert_warn "[${LEG}] A7 no thinking blocks in translated stream (model may not have reasoned; see ${LEG}-stream.sse)"
    fi
  fi
}
```

### Tier-B - real Claude Code run (non-gating)

Read `tasks/totalsize.md` for the prompt and assertion. Reset the clone to the
pinned baseline first so each leg starts identical.

```bash
tier_b() {
  [ "$SKIP_TIER_B" = 1 ] && { assert_warn "[${LEG}] Tier-B skipped (--skip-tier-b)"; return 0; }
  git -C "$CLONE" reset --hard --quiet "$REPO_SHA" && git -C "$CLONE" clean -xfd --quiet

  # Single source of truth - the prompt lives in tasks/totalsize.prompt.txt so it
  # cannot drift from the documented task. (totalsize.md describes the assertions.)
  local PROMPT=$(cat "$PROMPT_FILE")

  # Fresh, non-interactive Claude Code, all traffic pinned to Olla.
  ( cd "$CLONE" && \
    ANTHROPIC_BASE_URL="$BASE" \
    ANTHROPIC_AUTH_TOKEN="$TOKEN" \
    ANTHROPIC_MODEL="$MODEL" \
    ANTHROPIC_SMALL_FAST_MODEL="$MODEL" \
    DISABLE_AUTOUPDATER=1 DISABLE_TELEMETRY=1 DISABLE_ERROR_REPORTING=1 \
    CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1 \
    claude -p "$PROMPT" --model "$MODEL" --output-format stream-json --verbose \
      --dangerously-skip-permissions --max-turns 40 \
  ) > "${LOGDIR}/claude-${LEG}.jsonl" 2>"${LOGDIR}/claude-${LEG}.err"

  # Capture what the agent actually did BEFORE the clone is torn down, into the
  # preserved log dir, so a failed/odd run can be inspected after the fact:
  #  - a human-readable transcript (assistant text, tool calls, tool results, result)
  #  - the full diff vs baseline, including any new files (non-invasive: no staging,
  #    so the B3/B4 porcelain checks below still see the working-tree changes).
  jq -r '
    if .type=="assistant" then (.message.content[]? |
          if .type=="text" then "ASSISTANT: " + (.text // "")
          elif .type=="tool_use" then "TOOL_USE[" + (.name // "?") + "]: " + ((.input // {}) | tostring)
          else empty end)
    elif .type=="user" then (.message.content[]? |
          if .type=="tool_result" then "TOOL_RESULT" + (if .is_error == true then "(ERR)" else "" end) + ": " +
            ((.content) | if type=="array" then (map(.text // "") | join(" ")) else ((. // "") | tostring) end)
          else empty end)
    elif .type=="result" then "--- RESULT is_error=" + (.is_error|tostring) + " ---\n" + (.result // "")
    else empty end
  ' "${LOGDIR}/claude-${LEG}.jsonl" > "${LOGDIR}/tierb-transcript-${LEG}.txt" 2>/dev/null
  { git -C "$CLONE" --no-pager diff "$REPO_SHA";
    for nf in $(git -C "$CLONE" ls-files --others --exclude-standard); do
      echo "=== NEW FILE: ${nf} ==="; cat "${CLONE}/${nf}"; done
  } > "${LOGDIR}/tierb-diff-${LEG}.patch" 2>/dev/null

  # B1 - the child reported success (jq-select the result event, not grep)
  local result=$(jq -c 'select(.type=="result")' "${LOGDIR}/claude-${LEG}.jsonl" 2>/dev/null | tail -1)
  # .is_error is a JSON boolean; jq's // operator treats false as falsy so
  # `false // true` returns true - use an explicit if/else to avoid the trap.
  local is_err=$(echo "$result" | jq -r 'if .is_error == false then "false" else "true" end')
  [ "$is_err" = false ] && assert_pass "[${LEG}] B1 Claude Code reported success" \
                        || assert_warn "[${LEG}] B1 Claude Code did not report success (is_error=${is_err}) - model capability; see claude-${LEG}.jsonl"

  # B2 - tests green
  if ( cd "$CLONE" && go test ./pkg/analysis/... ) >"${LOGDIR}/tierb-test-${LEG}.log" 2>&1; then
    assert_pass "[${LEG}] B2 go test ./pkg/analysis/... green after edit"
  else
    assert_warn "[${LEG}] B2 tests not green after edit (model capability); see tierb-test-${LEG}.log"
  fi

  # B3 - a real change landed on summary.go (porcelain catches both modified and
  # newly-added paths, unlike a commit-to-worktree diff).
  if git -C "$CLONE" status --porcelain | grep -q 'pkg/analysis/summary.go'; then
    assert_pass "[${LEG}] B3 summary.go modified by the agent"
  else
    assert_warn "[${LEG}] B3 no change to summary.go (agent produced nothing usable)"
  fi

  # B4 - the requested TEST was actually added (not just the method). Guards the
  # B2/B3 hole where a model adds the method, skips the test, and rides the
  # pre-existing suite to green. (grep on a Go source file, not JSON.)
  if git -C "$CLONE" status --porcelain | grep -q 'pkg/analysis/summary_test.go' \
     && grep -q 'TotalSize' "${CLONE}/pkg/analysis/summary_test.go" 2>/dev/null; then
    assert_pass "[${LEG}] B4 a TotalSize test was added"
  else
    assert_warn "[${LEG}] B4 no TotalSize test added (method may be untested)"
  fi

  echo "INFO: [${LEG}] Tier-B artifacts in ${LOGDIR}: tierb-transcript-${LEG}.txt, tierb-diff-${LEG}.patch ($(grep -c '^' "${LOGDIR}/tierb-diff-${LEG}.patch" 2>/dev/null) diff lines), claude-${LEG}.jsonl"
}
```

### Performance profiling (optional, `--profile`)

Gauges Olla's own hot path under load, *after* functional validation has passed -
numbers never gate the run (a goroutine leak is the only WARN). Two ideas:

1. **Two burst modes per leg.** `perf_burst micro` floods `count_tokens` +
   `/v1/models` - served entirely by Olla (estimator, translation envelope,
   unifier/registry) with **no backend inference wait** - so it isolates Olla's
   own CPU. `perf_burst stream` drives real streaming `/v1/messages` at low
   concurrency to profile the **realistic** translation/proxy + SSE path
   (backend-bound, so fewer requests, but it captures the true streaming
   allocation profile, not the micro-request one). Compare the two: micro
   over-weights per-request middleware overhead; stream shows what real Claude
   Code traffic actually allocates.
2. **Bracket each leg with goroutine/heap snapshots** to catch a streaming/proxy
   goroutine leak across a realistic session (incl. the Tier-B multi-turn run).

Raw `.pb.gz` profiles are saved in `$LOGDIR` for interactive `go tool pprof`; the
headline (top cumulative CPU, top allocators) is auto-extracted into text.

```bash
pprof_goroutines() { curl -sf --max-time 5 "${PPROF}/goroutine?debug=1" | head -1 | grep -oE '[0-9]+' | head -1; }
pprof_heap_field() {  # $1 = HeapAlloc | NumGC
  curl -sf --max-time 5 "${PPROF}/heap?debug=1" | grep -E "^#[[:space:]]+$1[[:space:]]*=" | head -1 | grep -oE '[0-9]+' | head -1
}

perf_request() {  # one request appropriate to the burst mode (subshells inherit this fn)
  case "$1" in
    micro)  # pure-Olla endpoints, no backend inference wait - isolates Olla's CPU
      curl -s -o /dev/null --max-time 10 -X POST "${BASE}/v1/messages/count_tokens" \
        -H "content-type: application/json" -H "x-api-key: ${TOKEN}" -H "anthropic-version: 2023-06-01" \
        -d "{\"model\":\"${MODEL}\",\"messages\":[{\"role\":\"user\",\"content\":\"profile the hot path with a body large enough to exercise tokenisation and the translation envelope build\"}]}"
      curl -s -o /dev/null --max-time 10 "${BASE}/v1/models" -H "x-api-key: ${TOKEN}" ;;
    stream) # real streaming messages - exercises the true translation/proxy + SSE path
      curl -sN -o /dev/null --max-time 30 -X POST "${BASE}/v1/messages" \
        -H "content-type: application/json" -H "x-api-key: ${TOKEN}" -H "anthropic-version: 2023-06-01" \
        -d "{\"model\":\"${MODEL}\",\"max_tokens\":128,\"stream\":true,\"messages\":[{\"role\":\"user\",\"content\":\"Briefly list three colours.\"}]}" ;;
  esac
}

perf_burst() {  # $1 = mode: micro (high-concurrency Olla-bound) | stream (realistic, backend-bound)
  [ "$PROFILE" = 1 ] || return 0
  local mode="$1" bin="build/regression/olla${EXE}" secs workers
  case "$mode" in
    micro)  secs=20; workers=8 ;;   # Olla-bound endpoints sustain high concurrency
    stream) secs=25; workers=3 ;;   # streaming is backend-bound; keep concurrency low
    *) return 0 ;;
  esac
  local cpu="${LOGDIR}/cpu-${LEG}-${mode}.pb.gz" allocs="${LOGDIR}/allocs-${LEG}-${mode}.pb.gz"
  # No -f: on a non-2xx we still want the body (error text) on disk so an empty
  # capture is diagnosable; the HTTP status is saved alongside. (Go's CPU profiler
  # can be flaky on Windows under heavy syscall load - allocs/goroutine are not.)
  curl -s --max-time $((secs + 15)) -w '%{http_code}' "${PPROF}/profile?seconds=${secs}" \
    -o "$cpu" > "${LOGDIR}/cpu-${LEG}-${mode}.status" 2>/dev/null &
  local prof_pid=$!
  local deadline=$(( $(date +%s) + secs )) i=0 worker_pids=""
  while [ "$i" -lt "$workers" ]; do
    ( while [ "$(date +%s)" -lt "$deadline" ]; do perf_request "$mode"; done ) &
    worker_pids="$worker_pids $!"
    i=$((i + 1))
  done
  wait "$prof_pid" 2>/dev/null
  curl -sf --max-time 10 "${PPROF}/allocs" -o "$allocs" 2>/dev/null
  # Wait only for the worker subshells - bare 'wait' would also wait for the Olla
  # background process (started in boot_olla) and hang until the server exits.
  # shellcheck disable=SC2086
  wait $worker_pids 2>/dev/null || true
  go tool pprof -top -cum -nodecount=12 "$bin" "$cpu" > "${LOGDIR}/cpu-${LEG}-${mode}-top.txt" 2>/dev/null
  go tool pprof -top -alloc_objects -nodecount=12 "$bin" "$allocs" > "${LOGDIR}/allocs-${LEG}-${mode}-top.txt" 2>/dev/null
  # Report CPU and allocs independently - allocs is the reliable signal on Windows.
  if [ -s "${LOGDIR}/cpu-${LEG}-${mode}-top.txt" ]; then
    assert_pass "[${LEG}/${mode}] PERF CPU profile captured"
    echo "INFO: [${LEG}/${mode}] top CPU (cum):"; sed -n '1,9p' "${LOGDIR}/cpu-${LEG}-${mode}-top.txt"
  else
    assert_warn "[${LEG}/${mode}] PERF CPU profile empty (http=$(tr -d '\r\n' < "${LOGDIR}/cpu-${LEG}-${mode}.status" 2>/dev/null); CPU profiling can be flaky on Windows)"
  fi
  if [ -s "${LOGDIR}/allocs-${LEG}-${mode}-top.txt" ]; then
    assert_pass "[${LEG}/${mode}] PERF allocs profile captured"
    echo "INFO: [${LEG}/${mode}] top allocators:"; sed -n '1,9p' "${LOGDIR}/allocs-${LEG}-${mode}-top.txt"
  else
    assert_warn "[${LEG}/${mode}] PERF allocs profile empty"
  fi
}

perf_leg_end() {  # leak/heap check after the full leg (incl. Tier-B), vs the boot baseline
  [ "$PROFILE" = 1 ] || return 0
  sleep 5   # settle so transient request goroutines unwind before sampling
  local gend=$(pprof_goroutines) hend=$(pprof_heap_field HeapAlloc) gc=$(pprof_heap_field NumGC)
  if [ -n "$GBASE" ] && [ -n "$gend" ]; then
    local gd=$((gend - GBASE))
    if [ "$gd" -gt 30 ]; then
      assert_warn "[${LEG}] PERF goroutines grew ${GBASE}->${gend} (+${gd}) after settle - possible leak"
    else
      assert_pass "[${LEG}] PERF goroutines stable ${GBASE}->${gend} (+${gd})"
    fi
  fi
  echo "INFO: [${LEG}] PERF heap ${HBASE:-?}->${hend:-?} bytes, NumGC=${gc:-?}"
}
```

### Drive both legs

```bash
for LEG in passthrough translation; do
  PT=$([ "$LEG" = passthrough ] && echo true || echo false)
  CFG=$(render_config "$PT")
  boot_olla "$CFG" || { stop_olla; continue; }
  GBASE=""; HBASE=""
  if [ "$PROFILE" = 1 ]; then GBASE=$(pprof_goroutines); HBASE=$(pprof_heap_field HeapAlloc); fi
  tier_a
  perf_burst micro  # no-op unless --profile; isolates Olla's CPU (no backend wait)
  perf_burst stream # no-op unless --profile; realistic streaming translation/proxy path
  tier_b
  perf_leg_end      # no-op unless --profile; goroutine/heap leak check vs baseline
  stop_olla
done
```

(Set `LEG` and `PT` as shown; the orchestrator runs Olla in background and
records `OLLA_PID` so `stop_olla`/cleanup can kill it. Restarting between legs
is what flips passthrough<->translation against the one Ollama.)

## Phase 5 - WARN summary + report + verdict

Print the WARN summary block to the console first:

```bash
echo "===== WARN SUMMARY ====="
printf '%b\n' "$WARNS"
echo "========================"
```

Write `$REPORT`:

```markdown
# Claude Code E2E through Olla - <RUN_TS>
- Olla commit: <GIT_SHA>  Branch: <branch>
- Ollama: <OLLAMA>  Model: <MODEL>  Context: <CTX>
- Legs: passthrough, translation
- Verdict: PASS | FAIL
- Totals: <P>P / <F>F / <W>W

## Tier-A protocol (gate) - per leg
| Check | Passthrough | Translation |
|---|---|---|
| A1 models | ... | ... |
| A2 non-stream | ... | ... |
| A2b X-Olla-Mode header | ... | ... |
| A3 SSE order | ... | ... |
| A4 tool_use | ... | ... |
| A5 count_tokens | ... | ... |
| A6 routing stats delta | ... | ... |

## Tier-B agent (non-gating) - per leg
| Check | Passthrough | Translation |
|---|---|---|
| B1 claude success | ... | ... |
| B2 tests green | ... | ... |
| B3 summary.go changed | ... | ... |
| B4 TotalSize test added | ... | ... |

## Performance (--profile only, informational) - per leg
| Metric | Passthrough | Translation |
|---|---|---|
| goroutines baseline -> end (delta) | ... | ... |
| heap baseline -> end | ... | ... |
| NumGC over leg | ... | ... |
| top CPU (cum) path | ... | ... |
| top allocator | ... | ... |

Raw profiles per leg and mode: `cpu-<leg>-<micro|stream>.pb.gz`,
`allocs-<leg>-<micro|stream>.pb.gz` in the log dir (`go tool pprof
build/regression/olla <file>` to explore). Headline text in the matching
`*-top.txt`. Compare passthrough vs translation for the translation path's added
overhead, and micro vs stream to separate per-request middleware cost from the
real streaming allocation profile.

## Failures
<one block each: check, expected, actual, evidence/log path>

## Warnings / Notes
<the WARN summary; note any model-capability vs Olla-bug distinction>
```

`last-runs.md` line is written by the cleanup trap.

**Verdict rule:** any **Tier-A** FAIL, the fitness gate, the build, the clone
baseline, or a leg that never booted -> overall **FAIL**. Tier-B is reported
and shapes the headline but never fails the gate (the model, not Olla, is the
variable there). Finish by telling the user the verdict, the totals, the three
most important findings in plain sentences, and the report path.

## Verification protocol (per the skills spec - do before trusting a green)

1. Dispatch a **separate Sonnet agent** for a read-only audit against the
   golden-standard checklist (OS portability, no absolute paths, jq-only JSON,
   trap cleanup, status categorisation). Fix everything it flags.
2. Dispatch a **second Sonnet agent** to execute end-to-end against a real
   Ollama and fix the runtime issues it finds.
3. Iterate until a cold re-run yields the same verdict.
