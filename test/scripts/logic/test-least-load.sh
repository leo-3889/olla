#!/bin/bash
# Least-load dispatch e2e (fork feature: proxy.load_balancer least-load + proxy.load_probe).
#
# Proves, against live ollamock engines and a live olla:
#   1. NEW conversations (sticky misses) go to the node with the fewest
#      admitted requests - measured from the ENGINE's /health in_flight, so
#      load the gateway does not proxy itself is visible (direct hits on a
#      mock), which a gateway-connection-count balancer cannot see.
#   2. Sessions stay sticky: a pinned conversation keeps its node even when
#      the other node is strictly emptier.
#   3. When the busy node drains, new conversations move to it (least-load,
#      not affinity-to-first-node).
#   4. When engines expose NO load field in /health (ollamock
#      --no-health-inflight, and a probe pointed at a field nothing serves),
#      the gateway resolves load through the /metrics gauges and still
#      routes least-load (phase 5).
#
# Hardening rules for every phase: any load-state claim a dispatch relies on
# is RE-ASSERTED AT DISPATCH TIME (same-tick, both endpoints) and a lost
# premise is a loud FAIL, never a || true fallthrough - otherwise a transient
# curl miss turns the phase into a vacuous pass.
#
# Usage: ./test-least-load.sh            (builds binaries itself)
#        OLLA_BIN=/path BIN_DIR=... ./test-least-load.sh
set -euo pipefail

GREEN='\033[0;32m'; RED='\033[0;31m'; YELLOW='\033[1;33m'; RESET='\033[0m'
PASS=0; FAIL=0
ok()   { echo -e "${GREEN}PASS${RESET} $1"; PASS=$((PASS+1)); }
bad()  { echo -e "${RED}FAIL${RESET} $1"; FAIL=$((FAIL+1)); }
die()  { bad "$1"; exit 1; }
note() { echo -e "${YELLOW}..${RESET} $1"; }

WORK=$(mktemp -d /tmp/least-load.XXXXXX)
MOCK_A_PORT=19501; MOCK_B_PORT=19502; OLLA_PORT=19500
GW2_PORT=19503; MOCK_C_PORT=19504; MOCK_D_PORT=19505
MOCK_A="http://127.0.0.1:${MOCK_A_PORT}"; MOCK_B="http://127.0.0.1:${MOCK_B_PORT}"
MOCK_C="http://127.0.0.1:${MOCK_C_PORT}"; MOCK_D="http://127.0.0.1:${MOCK_D_PORT}"
GW="http://127.0.0.1:${OLLA_PORT}"; GW2="http://127.0.0.1:${GW2_PORT}"
PIDS=()

cleanup() {
  for p in "${PIDS[@]:-}"; do kill "$p" 2>/dev/null || true; done
  wait 2>/dev/null || true
  rm -rf "$WORK"
}
trap cleanup EXIT

# --- pre-flight: every test port must be free, or a stale run poisons the
#     whole scenario with confusing mid-phase failures ---
port_free() { # port
  if command -v ss >/dev/null 2>&1; then
    ! ss -ltn 2>/dev/null | grep -qE ":$1([^0-9]|$)"
  else
    ! curl -s -o /dev/null --max-time 1 "http://127.0.0.1:$1"
  fi
}
for p in "$OLLA_PORT" "$MOCK_A_PORT" "$MOCK_B_PORT" "$GW2_PORT" "$MOCK_C_PORT" "$MOCK_D_PORT"; do
  port_free "$p" || die "preflight: port $p already in use - a leftover run or foreign process holds it; stop it and re-run"
done

# --- build ---
REPO_ROOT="${REPO_ROOT:-$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)}"
cd "$REPO_ROOT"
OLLAMOCK_BIN="${OLLAMOCK_BIN:-$WORK/ollamock}"
OLLA_BIN="${OLLA_BIN:-$WORK/olla}"
GOFLAGS_DEFAULT="-buildvcs=false"
note "building ollamock + olla"
GOFLAGS=${GOFLAGS:-$GOFLAGS_DEFAULT} go build -o "$OLLAMOCK_BIN" ./test/cmd/ollamock
GOFLAGS=${GOFLAGS:-$GOFLAGS_DEFAULT} go build -o "$OLLA_BIN" .

# --- engines: equal priority, TTFT hold so admitted load is observable ---
# 2.5 s TTFT: direct requests stay admitted long enough to steer around.
"$OLLAMOCK_BIN" -addr "127.0.0.1:${MOCK_A_PORT}" -name node-a -models test-model -ttft-ms 2500 > "$WORK/mock-a.log" 2>&1 & PIDS+=($!)
"$OLLAMOCK_BIN" -addr "127.0.0.1:${MOCK_B_PORT}" -name node-b -models test-model -ttft-ms 2500 > "$WORK/mock-b.log" 2>&1 & PIDS+=($!)

# --- gateway config: least-load + probe + sticky ON ---
cat > "$WORK/config.yaml" <<YAML
server:
  host: "127.0.0.1"
  port: ${OLLA_PORT}
  request_logging: false
proxy:
  engine: "olla"
  load_balancer: "least-load"
  profile: "auto"
  load_probe:
    enabled: true
    timeout: 250ms
    fan_out_budget: 400ms
    health_path: "/health"
    active_field: "in_flight"
  sticky_sessions:
    enabled: true
    idle_ttl_seconds: 600
    max_sessions: 100
    key_sources: ["first_user_message"]
    prefix_hash_bytes: 512
    key_scan_bytes: 65536
  retry:
    enabled: false
discovery:
  type: "static"
  static:
    endpoints:
      - url: "${MOCK_A}"
        name: "node-a"
        type: "openai-compatible"
        priority: 100
        model_url: "/v1/models"
        health_check_url: "/health"
        check_interval: 2s
        check_timeout: 1s
      - url: "${MOCK_B}"
        name: "node-b"
        type: "openai-compatible"
        priority: 100
        model_url: "/v1/models"
        health_check_url: "/health"
        check_interval: 2s
        check_timeout: 1s
model_registry:
  type: "memory"
logging:
  level: "warn"
YAML

"$OLLA_BIN" -config "$WORK/config.yaml" > "$WORK/olla.log" 2>&1 & PIDS+=($!)

wait_http() { # url, label, logfile
  for _ in $(seq 1 50); do
    curl -fsS -o /dev/null --max-time 2 "$1" && return 0
    sleep 0.2
  done
  bad "startup: $2 never came up ($1)"
  echo "--- tail ${3:-<no log>} ---"; tail -n 40 "${3:-/dev/null}" 2>/dev/null || true
  exit 1
}
wait_http "${MOCK_A}/health" "mock A" "$WORK/mock-a.log"
wait_http "${MOCK_B}/health" "mock B" "$WORK/mock-b.log"
wait_http "${GW}/internal/health" "gateway" "$WORK/olla.log"

# --- model-readiness gate: BOTH endpoints registered AND the model listed.
# A single grep hit would pass with one dead endpoint; timeout is a loud
# FAIL, never a fallthrough into dispatch phases. ---
gw_ready() { # gw_url, nameA, nameB
  local eps models
  eps=$(curl -fsS --max-time 2 "$1/internal/status/endpoints" 2>/dev/null || true)
  models=$(curl -fsS --max-time 2 "$1/olla/openai/v1/models" 2>/dev/null || true)
  grep -Eq "\"name\":[[:space:]]*\"$2\"" <<<"$eps" \
    && grep -Eq "\"name\":[[:space:]]*\"$3\"" <<<"$eps" \
    && grep -q 'test-model' <<<"$models"
}
gw_gate() { # gw_url, nameA, nameB, logfile
  local a="$1" b="$2" c="$3" log="$4"
  for _ in $(seq 1 100); do
    gw_ready "$a" "$b" "$c" && return 0
    sleep 0.2
  done
  bad "readiness: $a never registered BOTH endpoints ($b + $c) with test-model - aborting before any dispatch"
  echo "--- tail $log ---"; tail -n 40 "$log" 2>/dev/null || true
  exit 1
}
gw_gate "$GW" node-a node-b "$WORK/olla.log"

chat() { # text [gw-url] -> body
  curl -fsS --max-time 30 -H 'Content-Type: application/json' -X POST \
    -d "{\"model\":\"test-model\",\"messages\":[{\"role\":\"user\",\"content\":\"$1\"}]}" \
    "${2:-$GW}/olla/openai/v1/chat/completions"
}
direct_load() { # mock_url - hold one admitted request (bypasses the gateway)
  curl -fsS --max-time 30 -H 'Content-Type: application/json' -X POST \
    -d '{"model":"test-model","stream":true,"messages":[{"role":"user","content":"direct-load"}]}' \
    "$1/v1/chat/completions" -o /dev/null &
}
served_by() { grep -o 'BACKEND:[A-Za-z0-9._-]*' <<<"$1" | head -1 | cut -d: -f2- || true; }

# Every poll below is fault-tolerant INSIDE the assignment: a transient curl
# or grep miss yields an empty reading that the loop simply retries - under
# set -euo pipefail a bare `n=$(curl|grep|cut)` would silently kill the run.
get_inflight() { curl -fsS --max-time 2 "$1/health" 2>/dev/null | grep -o '"in_flight":[0-9]*' | head -1 | cut -d: -f2 || true; }
get_queue_gauge() { curl -fsS --max-time 2 "$1/metrics" 2>/dev/null | sed -n 's/^radiance:http_queue_depth{[^}]*} \([0-9][0-9.]*\).*/\1/p' | head -1 || true; }
wait_inflight() { # mock_url min-n - caller MUST hard-fail on 1
  local n
  for _ in $(seq 1 100); do
    n=$(get_inflight "$1")
    if [[ "$n" =~ ^[0-9]+$ ]] && (( n >= $2 )); then return 0; fi
    sleep 0.1
  done
  return 1
}
wait_gauge() { # mock_url min-n (radiance:http_queue_depth via /metrics)
  local n
  for _ in $(seq 1 100); do
    n=$(get_queue_gauge "$1")
    if [[ "$n" =~ ^[0-9]+ ]] && (( ${n%.*} >= $2 )); then return 0; fi
    sleep 0.1
  done
  return 1
}
_cmp() { # value ge|eq target
  case "$2" in
    ge) (( $1 >= $3 )) ;;
    eq) (( $1 == $3 )) ;;
    *)  return 2 ;;
  esac
}
wait_loads() { # aUrl aCmp aN bUrl bCmp bN - BOTH claims true in ONE poll tick
  local a b
  for _ in $(seq 1 100); do
    a=$(get_inflight "$1"); b=$(get_inflight "$4")
    if [[ "$a" =~ ^[0-9]+$ && "$b" =~ ^[0-9]+$ ]] \
      && _cmp "$a" "$2" "$3" && _cmp "$b" "$5" "$6"; then
      return 0
    fi
    sleep 0.1
  done
  return 1
}

# ================= phase 1: gateway-blind load on A =================
note "phase 1: 3 direct (gateway-blind) generations on A"
for _ in 1 2 3; do direct_load "$MOCK_A"; done
wait_inflight "$MOCK_A" 3 || die "phase 1: A never reported in_flight>=3 to direct load"
wait_loads "$MOCK_B" eq 0 "$MOCK_A" ge 3 \
  || die "phase 1: B never read 0 in the same tick A held >=3"
ok "engine /health exposes admitted load the gateway does not proxy"

# ================= phase 2: new conversations must all go to B =================
note "phase 2: 3 fresh conversations while A=3, B=0"
P2_OK=0
for i in 1 2 3; do
  # Re-assert the claimed load state at dispatch time, hard.
  wait_loads "$MOCK_A" ge 3 "$MOCK_B" eq 0 \
    || die "phase 2: load premise A>=3/B==0 not live before dispatch $i"
  body=$(chat "least-load-fresh-$i") || die "phase 2: dispatch $i failed (chat curl error)"
  if [[ "$(served_by "$body")" == "node-b" ]]; then P2_OK=$((P2_OK+1)); fi
done
if [[ $P2_OK -eq 3 ]]; then
  ok "all 3 new conversations chose the emptier node (B)"
else
  bad "least-load misdispatched: $P2_OK/3 on B while A held 3 requests"
fi

# ================= phase 3: sticky beats load (after A drains) ==========
wait_loads "$MOCK_A" eq 0 "$MOCK_B" ge 0 || die "phase 3: A never drained to 0"
note "phase 3: A drained, now B carries the pinned conversations - repeat one"
# Load B so load alone says go to A - and prove it: B>=1 AND A==0 in the
# SAME tick at dispatch time, so "the busier node kept the pin" is real,
# not a <=1-request accident (un-admitted curl, 0-vs-0 tie).
direct_load "$MOCK_B"
wait_loads "$MOCK_B" ge 1 "$MOCK_A" eq 0 \
  || die "phase 3: never observed B>=1 AND A==0 in one tick - busy-node premise not established"
body=$(chat "least-load-fresh-2") || die "phase 3: chat dispatch failed"
if [[ "$(served_by "$body")" == "node-b" ]]; then
  ok "pinned conversation stayed on its warm node despite it being the busier one"
else
  bad "sticky lost the pin: conversation moved despite first_user_message affinity"
fi

# ================= phase 4: fresh conversation now goes to the emptier A =
wait_loads "$MOCK_B" eq 0 "$MOCK_A" ge 0 || die "phase 4: B never drained to 0"
note "phase 4: B drained, two direct loads on B, fresh conversation must move to A"
# TWO loads, hard-waited at >=2: with a single load the curl may not be
# admitted yet at dispatch time, and then A-vs-B is a 0-vs-0 tie the
# balancer wins on its lowest-URL tie-break - the old phase could pass
# without least-load ever seeing a difference. B>=2 vs A==0 asserted in one
# tick makes "A is strictly emptier" true at dispatch, not accidental.
direct_load "$MOCK_B"; direct_load "$MOCK_B"
wait_loads "$MOCK_B" ge 2 "$MOCK_A" eq 0 \
  || die "phase 4: B never held >=2 admitted with A==0 in one tick"
body=$(chat "least-load-after-drain") || die "phase 4: chat dispatch failed"
if [[ "$(served_by "$body")" == "node-a" ]]; then
  ok "after drain, a fresh conversation moved to the now-emptier node (A)"
else
  bad "least-load stayed affinity-blind to the drained node"
fi

# ================= sticky-hit produced zero probe rounds? ===============
# (cheap smoke: repeated same conversation is answered without new dispatch
# churn - covered strictly by unit tests; here just re-verify stability)
body=$(chat "least-load-after-drain") || die "sticky-repeat dispatch failed"
if [[ "$(served_by "$body")" == "node-a" ]]; then
  ok "repeat turn kept its pin (no migration across turns)"
else
  bad "pin migrated between turns of one conversation"
fi

# ============ phase 5: /metrics fallback - health without in_flight ============
note "phase 5: engines whose /health omits in_flight must route via /metrics"
"$OLLAMOCK_BIN" -addr "127.0.0.1:${MOCK_C_PORT}" -name node-c -models test-model -ttft-ms 2500 -no-health-inflight > "$WORK/mock-c.log" 2>&1 & PIDS+=($!)
"$OLLAMOCK_BIN" -addr "127.0.0.1:${MOCK_D_PORT}" -name node-d -models test-model -ttft-ms 2500 -no-health-inflight > "$WORK/mock-d.log" 2>&1 & PIDS+=($!)
wait_http "${MOCK_C}/health" "mock C" "$WORK/mock-c.log"
wait_http "${MOCK_D}/health" "mock D" "$WORK/mock-d.log"

hc=$(curl -fsS --max-time 2 "${MOCK_C}/health" 2>/dev/null || true)
if grep -q '"in_flight"' <<<"$hc"; then
  die "phase 5: --no-health-inflight had no effect - /health still carries in_flight"
fi
if ! grep -q 'radiance:http_queue_depth' <<<"$(curl -fsS --max-time 2 "${MOCK_C}/metrics" 2>/dev/null || true)"; then
  die "phase 5: mock C /metrics must still serve radiance:http_queue_depth"
fi

# Second gateway whose probe asks for a field NO mock serves: both
# endpoints must resolve via the /metrics fallback (or end up unmeasured -
# which the dispatch checks below would catch as a misdispatch).
sed -e "s/port: ${OLLA_PORT}/port: ${GW2_PORT}/" \
    -e "s/:${MOCK_A_PORT}/:${MOCK_C_PORT}/g" -e "s/:${MOCK_B_PORT}/:${MOCK_D_PORT}/g" \
    -e "s/node-a/node-c/g" -e "s/node-b/node-d/g" \
    -e 's/active_field: "in_flight"/active_field: "probe_field_missing"/' \
    "$WORK/config.yaml" > "$WORK/config2.yaml"
"$OLLA_BIN" -config "$WORK/config2.yaml" > "$WORK/olla2.log" 2>&1 & PIDS+=($!)
wait_http "${GW2}/internal/health" "gateway 2" "$WORK/olla2.log"
gw_gate "$GW2" node-c node-d "$WORK/olla2.log"

# Busy node C - load only observable through the /metrics gauge.
direct_load "$MOCK_C"; direct_load "$MOCK_C"
wait_gauge "${MOCK_C}" 2 || die "phase 5: mock C /metrics gauge never reached 2 - busy premise not established"
hc=$(curl -fsS --max-time 2 "${MOCK_C}/health" 2>/dev/null || true)
if grep -q '"in_flight"' <<<"$hc"; then
  die "phase 5: flagged /health unexpectedly grew in_flight while busy"
fi
ok "engine /health stays load-blind while /metrics reports the same admitted load"

# Both fresh conversations must land on idle D. If the fallback were broken
# both probes would be unmeasured and the unmeasured tie-break cannot dodge
# C twice; if only D were measured, known-before-unknown sends it to D but
# the wait_gauge check above already proved C's gauge is live and nonzero.
P5_OK=0
for i in 1 2; do
  body=$(chat "metrics-fallback-fresh-$i" "$GW2") || die "phase 5: dispatch $i failed"
  if [[ "$(served_by "$body")" == "node-d" ]]; then P5_OK=$((P5_OK+1)); fi
done
if [[ $P5_OK -eq 2 ]]; then
  ok "metrics-fallback: fresh conversations routed around the /metrics-only busy node"
else
  bad "metrics-fallback misdispatched: $P5_OK/2 on the idle node - probe fallback broken"
fi

echo
echo "======================================"
if [[ $FAIL -eq 0 ]]; then
  echo -e "${GREEN}PASS${RESET} least-load e2e: $PASS/$((PASS+FAIL)) checks"
else
  echo -e "${RED}FAIL${RESET} least-load e2e: $PASS passed, $FAIL failed"
  exit 1
fi
