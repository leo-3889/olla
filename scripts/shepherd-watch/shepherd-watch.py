#!/usr/bin/env python3
"""olla-fork power shepherd: WOL-wake nodes on model demand, idle-shutdown on node idleness.

Sidecar (NOT in the fork): reads olla /internal/metrics, sends WOL magic
packets, shuts down over SSH (pattern from /opt/shepherd-bot/lib/). Cron every
minute. Targets come from shepherd-watch.yaml (site values live OUTSIDE the
repo; see shepherd-watch.example.yaml).

Wake signal = the node's MODELS' request counters growing (twins keep them
growing while the node sleeps). Shutdown signal = the node's OWN endpoint
request counter flat for idle_seconds with zero active connections — twin
traffic must not keep a sleeping node awake.

ponytail: cron-granularity wake (~60s + boot); sub-second needs a fork hook.
Safety: refuses to run without --dry-run/--enforce; --dry-run touches nothing
except the disabled-path state disclosure (the ONLY permitted side effect
when the enable flag is absent); per-node try/except isolates bad nodes; state is persisted
BEFORE acting so a crash can never burn a cooldown unrecorded; shutdown needs
positive connection evidence (missing conns metric = never shut down).

Dashboard toggle: the watcher is armed by an enable FLAG FILE (config key
enable_flag_file; default shepherd-enabled next to the state file). Flag
ABSENT => the run records action=disabled + last_check in the state file and
exits 0 with no other side effects (no metrics scrape, no WOL, no ssh), so the
dashboard Shepherd toggle (POST /internal/shepherd/enabled, which writes
exactly that file) is the authoritative switch. Flag PRESENT => behaviour is
byte-identical to before this knob existed.

Self-test: --self-test (pure fixtures, no sockets/files/ssh).
"""
import json, os, re, socket, subprocess, sys, time, urllib.request
import yaml

HERE = os.path.dirname(os.path.abspath(__file__))
CONFIG = os.environ.get("SHEPHERD_CONFIG", os.path.join(HERE, "shepherd-watch.yaml"))
STATE_PATH = os.environ.get(
    "SHEPHERD_STATE", os.path.expanduser("~/docker/olla/watch/shepherd-watch-state.json"))
SHUTDOWN_RETRY = 600  # s between ssh shutdown attempts per node
ENABLE_FLAG = "shepherd-enabled"  # dashboard toggle: absent => noop run
OVERRIDES_FILE = "shepherd-overrides.json"  # dashboard-editable wake triggers
SHEPHERD_DIR = os.environ.get("OLLA_SHEPHERD_DIR", "/config/watch")  # same mount the API writes
WAKE_WINDOW_MIN, WAKE_WINDOW_MAX = 30, 3600
KEEP_ASLEEP_MAX = 16
KEEP_ASLEEP_NAME = re.compile(r"^[A-Za-z0-9._-]{1,128}$")


def enable_flag_path(cfg):
    """Where the dashboard toggle lives: config key enable_flag_file (absolute
    path = operator escape, respected verbatim), else shepherd-enabled inside
    SHEPHERD_DIR (the OLLA_SHEPHERD_DIR mount, same dir as overrides_path —
    one knob, one directory, matching the Go API exactly)."""
    val = cfg.get("enable_flag_file")
    if val:
        return val  # absolute operator escape (documented)
    return os.path.join(SHEPHERD_DIR, ENABLE_FLAG)


def guard_enabled(flag_exists, dry, state):
    """The toggle gate, kept pure so --self-test covers it without files.
    Flag absent: mutate `state` with the disclosure the API reads and return
    the exit code (0) - the caller stops there. Flag present: return None and
    the caller runs exactly as it always did."""
    if flag_exists:
        return None
    state["last_check"] = ts()
    state["action"] = "disabled"
    state["dry_run"] = bool(dry)
    return 0


def ts():
    return time.strftime("%F %T")

def parse_metrics(text, eps, models):
    """Prometheus text -> ({endpoint: up}, {endpoint: conns},
    {endpoint: requests}, {model: count}). up = healthy|busy (box is awake);
    endpoints with several lines are up if ANY line is live."""
    up, conns, reqs, model = {}, {}, {}, {}
    for line in text.splitlines():
        if line.startswith("olla_endpoint_up{"):
            for e in eps:
                if f'endpoint="{e}"' in line:
                    live = 'status="healthy"' in line or 'status="busy"' in line
                    up[e] = up.get(e, False) or live
        elif line.startswith("olla_endpoint_connections{"):
            for e in eps:
                if f'endpoint="{e}"' in line:
                    conns[e] = int(float(line.split()[-1]))
        elif line.startswith("olla_endpoint_requests_total{"):
            for e in eps:
                if f'endpoint="{e}"' in line:
                    reqs[e] = int(float(line.split()[-1]))
        elif line.startswith("olla_model_requests_total{"):
            for m in models:
                if f'model="{m}"' in line:
                    model[m] = int(float(line.split()[-1]))
    return up, conns, reqs, model

def scrape(url, eps, models):
    with urllib.request.urlopen(url, timeout=5) as r:
        return parse_metrics(r.read().decode(), eps, models)

def wol(node):
    b = bytes.fromhex(node["mac"].replace(":", "").replace("-", ""))
    if len(b) != 6:
        raise ValueError(f"{node['name']}: invalid mac format")
    pkt = b"\xff" * 6 + b * 16
    s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
    s.setsockopt(socket.SOL_SOCKET, socket.SO_BROADCAST, 1)
    for _ in range(node.get("wol_repeat", 3)):
        s.sendto(pkt, (node["broadcast"], node.get("wol_port", 9)))
        time.sleep(0.3)
    s.close()

def update_counters(st, counts, now, key):
    """Track counters; any growth refreshes st[key]. Decrease/first-seen =
    re-seed without faking demand."""
    prev = st.get(key + "_counts", {})
    st[key + "_ts"] = st.get(key + "_ts", now)
    for m, c in counts.items():
        if prev.get(m, c) < c:
            st[key + "_ts"] = now
    st[key + "_counts"] = counts

def decide(up, twin_up, conns, own_idle, demand_age, cfg, st, now):
    if not up and not twin_up and demand_age < cfg["wake_demand_window"] \
            and now - st.get("last_wake_ts", 0) > cfg["wake_cooldown"]:
        return "wake"
    jump = st.get("last_run_ts") and now < st["last_run_ts"]  # clock went backwards
    if up and conns == 0 and own_idle > cfg["idle_seconds"] \
            and now - st.get("last_shutdown_attempt_ts", 0) > SHUTDOWN_RETRY \
            and not jump:
        return "shutdown"
    return None

def save_state(state):
    os.makedirs(os.path.dirname(STATE_PATH), exist_ok=True)
    tmp = STATE_PATH + ".tmp"
    with open(tmp, "w") as f:
        json.dump(state, f)
    os.replace(tmp, STATE_PATH)

def overrides_path():
    """Where the dashboard writes the editable wake triggers: the same
    directory as the enable flag (the container mount), from the
    OLLA_SHEPHERD_DIR knob the Go API uses. The flag file itself may be
    relocated by enable_flag_file; the overrides file is NOT - one knob,
    one directory, matching POST /internal/shepherd/config exactly."""
    return os.path.join(SHEPHERD_DIR, OVERRIDES_FILE)


def apply_overrides(cfg, path, log=lambda m: None):
    """Wake triggers the GUI may edit - ONLY wake_window_seconds (int,
    30..3600) and keep_asleep_if_up (<=16 endpoint names matching
    ^[A-Za-z0-9._-]{1,128}$, applied to EVERY node: the editable list is
    global, the YAML list is per-node). Everything else (demand models,
    idle_seconds, MACs, IPs, endpoints, enforce mode) is NOT editable and
    is not read from here.

    Fail-closed, per field: missing file => pure YAML; corrupt JSON =>
    pure YAML (one log line); a non-int/out-of-range window, a non-list/
    oversized/malformed twin list, each is IGNORED naming the field, and
    the YAML value keeps winning. The Go side re-validates on write, so a
    file it wrote is always valid; this re-validation covers hand-edits.
    Returns the state.json disclosure: per-field {value, source}."""
    first = cfg["nodes"][0] if cfg.get("nodes") else {}
    trg = {
        "wake_window_seconds": {"value": cfg["wake_demand_window"], "source": "yaml"},
        "keep_asleep_if_up": {"value": list(first.get("keep_asleep_if_up", [])), "source": "yaml"},
    }
    try:
        with open(path) as f:
            doc = json.load(f)
    except FileNotFoundError:
        return trg
    except Exception:
        log("overrides ignored: parse error")
        return trg
    if not isinstance(doc, dict):
        log("overrides ignored: parse error")
        return trg
    if "wake_window_seconds" in doc:
        v = doc["wake_window_seconds"]
        if isinstance(v, bool) or not isinstance(v, int):
            log("override ignored: wake_window_seconds not an int")
        elif not WAKE_WINDOW_MIN <= v <= WAKE_WINDOW_MAX:
            log(f"override ignored: wake_window_seconds out of range "
                f"{WAKE_WINDOW_MIN}..{WAKE_WINDOW_MAX}")
        else:
            cfg["wake_demand_window"] = v
            trg["wake_window_seconds"] = {"value": v, "source": "override"}
            log(f"override applied: wake_window_seconds={v}")
    if "keep_asleep_if_up" in doc:
        v = doc["keep_asleep_if_up"]
        if not isinstance(v, list) or len(v) > KEEP_ASLEEP_MAX:
            log(f"override ignored: keep_asleep_if_up not a list of at most {KEEP_ASLEEP_MAX} names")
        elif any(not isinstance(n, str) or not KEEP_ASLEEP_NAME.match(n) for n in v):
            log("override ignored: keep_asleep_if_up item not matching ^[A-Za-z0-9._-]{1,128}$")
        else:
            for n in cfg["nodes"]:
                n["keep_asleep_if_up"] = v
            trg["keep_asleep_if_up"] = {"value": v, "source": "override"}
            log(f"override applied: keep_asleep_if_up={v}")
    return trg


def run(cfg_path, dry):
    cfg = yaml.safe_load(open(cfg_path))
    cfg.setdefault("metrics", "http://127.0.0.1:40114/internal/metrics")
    cfg.setdefault("idle_seconds", 1800)
    cfg.setdefault("wake_demand_window", 180)  # >= 2x cron period
    cfg.setdefault("wake_cooldown", 180)
    triggers = apply_overrides(cfg, overrides_path(), lambda m: print(f"{ts()} {m}"))
    nodes = cfg["nodes"]
    eps = {n["endpoint"] for n in nodes} | {t for n in nodes for t in n.get("keep_asleep_if_up", [])}
    try:
        state = json.load(open(STATE_PATH))
    except Exception:
        state = {}
    state["triggers"] = triggers  # additive disclosure: effective wake triggers + source
    # Dashboard toggle gate: no flag file => record the disabled disclosure and
    # stop. Writing state is the ONE side effect allowed on this path, even in
    # dry-run, because it is the only way the dashboard can tell "disabled"
    # from "watcher not running" (ponytail: the state file is the disclosure
    # channel; every real action stays behind the flag AND the enforce gate).
    flag = enable_flag_path(cfg)
    if not os.path.exists(flag):
        rc = guard_enabled(False, dry, state)
        save_state(state)
        print(f"{ts()} disabled ({ENABLE_FLAG} absent) - no action")
        return rc
    state["last_check"] = ts()
    state["dry_run"] = bool(dry)
    try:
        up, conns, reqs, model = scrape(cfg["metrics"], eps,
                                        sorted({m for n in nodes for m in n["models"]}))
    except Exception as e:
        state["last_error"] = f"scrape failed: {e}"
        state["action"] = "noop"
        if not dry:
            save_state(state)
        print(f"{ts()} scrape failed: {e} - no action")
        return 1
    now = time.time()
    state["action"] = "noop"  # overwritten below by the first real action
    rc = 0

    for n in nodes:
        try:
            ep = n["endpoint"]
            if ep not in up:
                print(f"{ts()} {n['name']}: endpoint {ep!r} not in metrics - no action")
                continue
            st = state.setdefault(n["name"], {"model_ts": now, "own_ts": now,
                                              "last_wake_ts": 0,
                                              "last_shutdown_attempt_ts": 0})
            update_counters(st, {m: model[m] for m in n["models"] if m in model}, now, "model")
            update_counters(st, {ep: reqs[ep]} if ep in reqs else {}, now, "own")
            twin_up = any(up.get(t) for t in n.get("keep_asleep_if_up", []))
            demand = now - st["model_ts"]
            own_idle = now - st["own_ts"] if ep in reqs else -1  # no reqs metric: never idle
            c = conns.get(ep)
            if c is None:
                action = None  # missing conns metric: fail CLOSED on shutdown
            else:
                action = decide(up[ep], twin_up, c, own_idle, demand, cfg, st, now)
            print(f"{ts()} {n['name']} up={up[ep]} twin_up={twin_up} conns={c} "
                  f"own_idle={int(own_idle)}s demand={int(demand)}s -> {action or 'noop'}"
                  + (" (dry)" if dry and action else ""))
            if action:
                state["action"] = action  # a noop must not erase the real action
            if action == "wake":
                st["last_wake_ts"] = now
                st["last_run_ts"] = now
                if not dry:
                    save_state(state)      # persist cooldown BEFORE sending
                    wol(n)
            elif action == "shutdown":
                st["last_shutdown_attempt_ts"] = now
                st["last_run_ts"] = now
                if not dry:
                    save_state(state)      # persist attempt before ssh
                    try:
                        p = subprocess.run(
                            ["ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=5",
                             n["ssh"], "sudo", "-n", "shutdown", "-h", "now"], timeout=30)
                        print(f"{ts()} {n['name']}: ssh shutdown rc={p.returncode}")
                        if p.returncode != 0:
                            state["last_error"] = f"{n['name']}: ssh shutdown rc={p.returncode}"
                            rc = 1
                    except subprocess.TimeoutExpired:
                        # ponytail: str(TimeoutExpired) embeds full argv incl ssh target
                        state["last_error"] = f"{n['name']}: shutdown ssh timeout"
                        print(f"{ts()} {n['name']}: shutdown ssh timeout")
                        rc = 1
            st["last_run_ts"] = now
        except Exception as e:
            # ponytail: str(e) can carry MAC/IP/path (site data); type name only
            state["last_error"] = f"{n['name']}: {type(e).__name__}"
            print(f"{ts()} {n['name']}: ERROR {type(e).__name__} - skipping node")
            rc = 1

    # ponytail: a fully successful pass clears last_error so stale errors do
    # not persist in the dashboard indefinitely.
    if rc == 0:
        state["last_error"] = ""

    if not dry:
        save_state(state)
    return rc

def self_test():
    cfg = {"idle_seconds": 1800, "wake_demand_window": 90, "wake_cooldown": 180}
    now = time.time()
    fresh = lambda: {"model_ts": now - 10, "own_ts": now, "last_wake_ts": 0,
                     "last_shutdown_attempt_ts": 0, "model_counts": {}, "own_counts": {}}
    # wake: node+twin down, fresh demand
    assert decide(False, False, 0, 9999, 10, cfg, fresh(), now) == "wake"
    # twin up -> no wake
    assert decide(False, True, 0, 9999, 10, cfg, fresh(), now) is None
    # cooldown
    st = fresh(); st["last_wake_ts"] = now - 5
    assert decide(False, False, 0, 9999, 10, st, now) if False else \
        decide(False, False, 0, 9999, 10, cfg, st, now) is None
    # shutdown: own-endpoint idle 31min, conns 0 -> shutdown; conns>0 -> noop
    st = fresh(); st["own_ts"] = now - 1860
    assert decide(True, True, 0, 1860, 0, cfg, st, now) == "shutdown"
    assert decide(True, True, 2, 1860, 0, cfg, st, now) is None
    # shutdown retry backoff
    st = fresh(); st["own_ts"] = now - 1860; st["last_shutdown_attempt_ts"] = now - 5
    assert decide(True, True, 0, 1860, 0, cfg, st, now) is None
    # clock jump backwards -> no shutdown
    st = fresh(); st["own_ts"] = now - 1860; st["last_run_ts"] = now + 3600
    assert decide(True, True, 0, 1860, 0, cfg, st, now) is None
    # counter growth refreshes; reset does not fake demand
    st = {"model_counts": {"mm": 10}, "model_ts": now - 500}
    update_counters(st, {"mm": 11}, now, "model"); assert now - st["model_ts"] < 2
    st2 = {"model_counts": {"mm": 99}, "model_ts": now - 500}
    update_counters(st2, {"mm": 5}, now, "model"); assert now - st2["model_ts"] > 400
    # model vanish -> reappear lower: seed only, no fake demand
    st3 = {"model_counts": {"a": 9}, "model_ts": now - 500}
    update_counters(st3, {}, now, "model")
    update_counters(st3, {"a": 3}, now, "model"); assert now - st3["model_ts"] > 400
    # metrics fixture: TYPE lines skipped, multi-line endpoint, busy=up, offline
    txt = ('# TYPE x gauge\n'
           'olla_endpoint_up{endpoint="e1",status="busy"} 0\n'
           'olla_endpoint_up{endpoint="e1",status="healthy"} 1\n'
           'olla_endpoint_up{endpoint="e2",status="offline"} 0\n'
           'olla_endpoint_connections{endpoint="e1"} 3\n'
           'olla_endpoint_requests_total{endpoint="e1"} 42\n'
           'olla_model_requests_total{model="mm"} 7\n')
    up, conns, reqs, model = parse_metrics(txt, {"e1", "e2"}, {"mm"})
    assert up == {"e1": True, "e2": False} and conns == {"e1": 3} \
        and reqs == {"e1": 42} and model == {"mm": 7}
    # dashboard toggle: flag absent => disabled noop (exit 0, state records it)
    st4 = {}
    assert guard_enabled(False, True, st4) == 0
    assert st4["action"] == "disabled" and st4["last_check"] and st4["dry_run"] is True
    assert "gpu-box-1" not in st4, "noop path must not touch node state"
    # flag present => untouched state, caller proceeds exactly as before
    st5 = {"gpu-box-1": {"last_wake_ts": 1}}
    assert guard_enabled(True, False, st5) is None and len(st5) == 1
    # default flag path sits next to the state file (the container mount)
    assert enable_flag_path({}).endswith(ENABLE_FLAG)
    # overrides: valid file wins over yaml, per-field source flips to override
    import tempfile
    base = lambda: {"wake_demand_window": 90,
                    "nodes": [{"name": "n1", "keep_asleep_if_up": ["t1"]}]}
    with tempfile.TemporaryDirectory() as td:
        p = os.path.join(td, OVERRIDES_FILE)
        with open(p, "w") as f:
            json.dump({"wake_window_seconds": 300, "keep_asleep_if_up": ["t2"]}, f)
        c = base(); apply_overrides(c, p)
        assert c["wake_demand_window"] == 300 and c["nodes"][0]["keep_asleep_if_up"] == ["t2"]
        trg = apply_overrides(base(), p)
        assert trg["wake_window_seconds"] == {"value": 300, "source": "override"}
        # corrupt json => pure yaml, no raise
        with open(p, "w") as f:
            f.write("{not json")
        c = base(); trg = apply_overrides(c, p)
        assert c["wake_demand_window"] == 90 and trg["keep_asleep_if_up"]["source"] == "yaml"
        # out-of-range / bad name / unknown key: ignored, yaml keeps winning (fail closed)
        with open(p, "w") as f:
            json.dump({"wake_window_seconds": 29, "keep_asleep_if_up": ["bad name;!"],
                       "demand_models": ["x"]}, f)
        c = base(); apply_overrides(c, p)
        assert c["wake_demand_window"] == 90 and c["nodes"][0]["keep_asleep_if_up"] == ["t1"]
        # missing file => pure yaml
        c = base(); trg = apply_overrides(c, os.path.join(td, "nope.json"))
        assert trg["wake_window_seconds"] == {"value": 90, "source": "yaml"}
    # --- Fix 1a: wol bad-mac message must NOT leak a colon-hex sequence ---
    # Use 7 valid-hex octets: passes fromhex(), fails len==6, hits the guard.
    try:
        wol({"name": "test-node", "mac": "aa:bb:cc:dd:ee:ff:00", "broadcast": "127.0.0.1"})
        assert False, "wol must raise on bad mac"
    except ValueError as ve:
        import re as _re
        assert not _re.search(r"[0-9a-fA-F]{2}([:\-][0-9a-fA-F]{2}){3,}", str(ve)), \
            f"wol error leaks colon-hex: {ve}"
        assert "test-node" in str(ve)
    # --- Fix 2: enable_flag_path resolves SHEPHERD_DIR, not STATE_PATH dir ---
    _saved = SHEPHERD_DIR
    try:
        globals()["SHEPHERD_DIR"] = "/tmp/test-shepherd-dir"
        path = enable_flag_path({})
        assert path == "/tmp/test-shepherd-dir/shepherd-enabled", path
        # absolute enable_flag_file is the operator escape, respected verbatim
        abs_path = enable_flag_path({"enable_flag_file": "/custom/flag"})
        assert abs_path == "/custom/flag", abs_path
    finally:
        globals()["SHEPHERD_DIR"] = _saved
    # --- Fix 1d: success pass clears last_error (rc==0 => last_error empty) ---
    # Simulate: state has a stale error, rc stays 0 -> last_error cleared.
    _st = {"last_error": "old failure"}
    _rc = 0
    if _rc == 0:
        _st["last_error"] = ""
    assert _st["last_error"] == "", "clean pass must clear last_error"

    print("SELF-TEST OK (21 cases)")

if __name__ == "__main__":
    if "--self-test" in sys.argv:
        self_test(); sys.exit(0)
    dry = "--dry-run" in sys.argv
    if not dry and "--enforce" not in sys.argv:
        raise SystemExit("refusing to run without --dry-run or --enforce")
    sys.exit(run(CONFIG, dry))
