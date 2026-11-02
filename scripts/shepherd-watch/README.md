# shepherd-watch

Cron watcher (sidecar, NOT part of the Go binary): WOL-wakes nodes on model
demand, idle-shuts them down over SSH. Targets and site secrets live in
`shepherd-watch.yaml` (template: `shepherd-watch.example.yaml`); the design
notes and the pure `--self-test` harness are in `shepherd-watch.py`.

## Wake-trigger overrides (GUI-editable)

Two wake triggers are editable from the dashboard
(GET /internal/shepherd, POST/DELETE /internal/shepherd/config):
`wake_window_seconds` (int, 30..3600) and `keep_asleep_if_up` (max 16
endpoint names matching `^[A-Za-z0-9._-]{1,128}$`, applied to EVERY node -
the editable list is global, the YAML list is per-node).

The dashboard writes them to `shepherd-overrides.json` - same directory as
`shepherd-enabled` (env `OLLA_SHEPHERD_DIR`, default `/config/watch`),
format exactly `{"wake_window_seconds": int, "keep_asleep_if_up": [...]}`,
atomic tmp+rename. The watcher applies the file over `shepherd-watch.yaml`
on every run: missing file = pure YAML; corrupt JSON and out-of-range values
are ignored with a log line naming the field, so the YAML value wins on any
doubt (fail-closed). Everything else in the YAML - demand models,
idle_seconds, MACs, IPs, endpoints, enforce mode - is NOT editable and never
appears in the API editable object. Effective values with per-field source
(`yaml` | `override`) are disclosed under `triggers` in
`shepherd-watch-state.json` (additive key; existing shape unchanged).
