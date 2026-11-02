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
(incl. state); per-node try/except isolates bad nodes; state is persisted
BEFORE acting so a crash can never burn a cooldown unrecorded; shutdown needs
positive connection evidence (missing conns metric = never shut down).

Self-test: --self-test (pure fixtures, no sockets/files/ssh).
"""
import json, os, socket, subprocess, sys, time, urllib.request
import yaml

HERE = os.path.dirname(os.path.abspath(__file__))
CONFIG = os.environ.get("SHEPHERD_CONFIG", os.path.join(HERE, "shepherd-watch.yaml"))
STATE_PATH = os.environ.get(
    "SHEPHERD_STATE", os.path.expanduser("~/docker/olla/watch/shepherd-watch-state.json"))
SHUTDOWN_RETRY = 600  # s between ssh shutdown attempts per node

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
        raise ValueError(f"{node['name']}: bad mac {node['mac']}")
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

def run(cfg_path, dry):
    cfg = yaml.safe_load(open(cfg_path))
    cfg.setdefault("metrics", "http://127.0.0.1:40114/internal/metrics")
    cfg.setdefault("idle_seconds", 1800)
    cfg.setdefault("wake_demand_window", 180)  # >= 2x cron period
    cfg.setdefault("wake_cooldown", 180)
    nodes = cfg["nodes"]
    eps = {n["endpoint"] for n in nodes} | {t for n in nodes for t in n.get("keep_asleep_if_up", [])}
    try:
        up, conns, reqs, model = scrape(cfg["metrics"], eps,
                                        sorted({m for n in nodes for m in n["models"]}))
    except Exception as e:
        print(f"{ts()} scrape failed: {e} - no action")
        return 1
    try:
        state = json.load(open(STATE_PATH))
    except Exception:
        state = {}
    now = time.time()
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
                    p = subprocess.run(
                        ["ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=5",
                         n["ssh"], "sudo", "-n", "shutdown", "-h", "now"], timeout=30)
                    print(f"{ts()} {n['name']}: ssh shutdown rc={p.returncode}")
                    if p.returncode != 0:
                        rc = 1
            st["last_run_ts"] = now
        except Exception as e:
            print(f"{ts()} {n['name']}: ERROR {type(e).__name__}: {e} - skipping node")
            rc = 1

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
    print("SELF-TEST OK (11 cases)")

if __name__ == "__main__":
    if "--self-test" in sys.argv:
        self_test(); sys.exit(0)
    dry = "--dry-run" in sys.argv
    if not dry and "--enforce" not in sys.argv:
        raise SystemExit("refusing to run without --dry-run or --enforce")
    sys.exit(run(CONFIG, dry))
