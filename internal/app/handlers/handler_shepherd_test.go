package handlers

// Shepherd (WOL) settings API: the file bridge in handler_shepherd.go is the
// dashboard power toggle's only surface, so the coverage is: unavailable when
// the mount is absent, a full payload (with a MASKED mac and no IPs, paths or
// ssh targets) from fixture files, the enable/disable write pair, 409 when the
// directory cannot be written, and the dashboard gate on both routes.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thushan/olla/internal/config"
)

const shepherdFixtureYAML = `metrics: "http://127.0.0.1:40114/internal/metrics"
idle_seconds: 1800
wake_demand_window: 90
wake_cooldown: 180
nodes:
  - name: gpu-box-1
    endpoint: gpu-box-1
    models: [qwen4exp]
    keep_asleep_if_up: [gpu-box-2]
    mac: "b0:82:aa:bb:cc:9a"
    broadcast: "192.168.1.255"
    wol_port: 9
    wol_repeat: 3
    ssh: "shepherd@192.168.1.50"
`

const shepherdFixtureState = `{"dry_run": false, "last_check": "2026-11-02 09:00:00", "action": "wake",
  "gpu-box-1": {"model_ts": 1762000000, "own_ts": 1761999500, "last_wake_ts": 1761999000,
    "last_shutdown_attempt_ts": 1761900000, "last_run_ts": 1762000060,
    "model_counts": {"qwen4exp": 7}}}`

// shepherdDirWith writes watcher-shaped fixture files and points the env knob
// at the directory.
func shepherdDirWith(t *testing.T, yamlBody, stateBody string) string {
	t.Helper()
	dir := t.TempDir()
	if yamlBody != "" {
		if err := os.WriteFile(filepath.Join(dir, shepherdYAMLFile), []byte(yamlBody), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if stateBody != "" {
		if err := os.WriteFile(filepath.Join(dir, shepherdStateFile), []byte(stateBody), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv(shepherdDirEnv, dir)
	return dir
}

func newShepherdApp() *Application {
	return &Application{Config: &config.Config{}, logger: &mockStyledLogger{}}
}

func getShepherd(t *testing.T, app *Application) ShepherdResponse {
	t.Helper()
	rec := httptest.NewRecorder()
	app.shepherdHandler(rec, httptest.NewRequest(http.MethodGet, "/internal/shepherd", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("content-type = %q, want application/json", ct)
	}
	var body ShepherdResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v (body %s)", err, rec.Body.String())
	}
	return body
}

func TestShepherdGETUnavailableWhenDirMissing(t *testing.T) {
	t.Setenv(shepherdDirEnv, filepath.Join(t.TempDir(), "not-mounted"))
	body := getShepherd(t, newShepherdApp())
	if body.Available {
		t.Fatal("available must be false when the watcher directory is absent")
	}
	if body.Enabled || body.DryRun || body.Config.NodeName != "" || body.State.LastCheck != "" {
		t.Fatalf("unavailable payload must be zeroed, got %+v", body)
	}
	// The default knob (/config/watch) is not mounted on a dev box: the same
	// answer, without the env set at all.
	t.Setenv(shepherdDirEnv, "")
	if shepherdDir() != shepherdDirDefault {
		t.Errorf("shepherdDir() = %q, want the documented default %q", shepherdDir(), shepherdDirDefault)
	}
}

func TestShepherdGETFullPayload(t *testing.T) {
	dir := shepherdDirWith(t, shepherdFixtureYAML, shepherdFixtureState)
	body := getShepherd(t, newShepherdApp())

	if !body.Available || body.Enabled {
		t.Fatalf("available=true, enabled=false expected (no flag file), got %+v", body)
	}
	if body.DryRun {
		t.Fatal("the state dry_run key (false) must win over the true default")
	}
	if body.Config.NodeName != "gpu-box-1" {
		t.Errorf("node_name = %q", body.Config.NodeName)
	}
	if body.Config.MACMasked != "b0:82:**:**:**:9a" {
		t.Errorf("mac_masked = %q, want b0:82:**:**:**:9a", body.Config.MACMasked)
	}
	if body.Config.IdleShutdownSeconds != 1800 || body.Config.WakeWindowSeconds != 90 {
		t.Errorf("config ints = %+v, want 1800/90", body.Config)
	}
	want := ShepherdState{
		LastCheck:    "2026-11-02 09:00:00",
		LastAction:   "wake",
		LastWake:     time.Unix(1761999000, 0).UTC().Format(time.RFC3339),
		LastShutdown: time.Unix(1761900000, 0).UTC().Format(time.RFC3339),
		LastError:    "",
	}
	if body.State != want {
		t.Errorf("state = %+v, want %+v", body.State, want)
	}

	// Disclosure: the serialised payload must carry no full MAC octets, no IP,
	// no ssh target and no filesystem path.
	raw := getShepherdRaw(t)
	for _, leak := range []string{"aa:bb:cc", "192.168", "127.0.0.1", "shepherd@", dir, shepherdYAMLFile} {
		if strings.Contains(raw, leak) {
			t.Errorf("payload leaks %q: %s", leak, raw)
		}
	}
}

func getShepherdRaw(t *testing.T) string {
	t.Helper()
	rec := httptest.NewRecorder()
	newShepherdApp().shepherdHandler(rec, httptest.NewRequest(http.MethodGet, "/internal/shepherd", nil))
	return rec.Body.String()
}

// The watcher has always written per-node epoch seconds and (in dry-run) no
// disclosures at all: the legacy shape must still render, and dry_run must
// default to true (a watcher that never declared enforcement has not enabled
// it; the live deployment runs dry).
func TestShepherdGETLegacyStateShape(t *testing.T) {
	shepherdDirWith(t, shepherdFixtureYAML,
		`{"gpu-box-1": {"last_run_ts": 1762000060, "last_wake_ts": 1761999000, "last_shutdown_attempt_ts": 1761900000}}`)
	body := getShepherd(t, newShepherdApp())
	if !body.DryRun {
		t.Error("dry_run must default to true when the state file has no dry_run key")
	}
	if body.State.LastCheck != time.Unix(1762000060, 0).UTC().Format(time.RFC3339) {
		t.Errorf("last_check = %q, want last_run_ts rendered", body.State.LastCheck)
	}
	if body.State.LastWake == "" || body.State.LastShutdown == "" {
		t.Errorf("per-node wake/shutdown timestamps must map through: %+v", body.State)
	}
	if body.State.LastAction != "" || body.State.LastError != "" {
		t.Errorf("absent keys must stay empty: %+v", body.State)
	}
}

// Missing config file: available (the mount is there) with an empty config
// block, not a 500 - the toggle is still usable.
func TestShepherdGETMissingConfigFiles(t *testing.T) {
	shepherdDirWith(t, "", "")
	body := getShepherd(t, newShepherdApp())
	if !body.Available {
		t.Fatal("a present directory with no files is available")
	}
	if body.Config != (ShepherdConfig{}) {
		t.Errorf("config = %+v, want zero", body.Config)
	}
	if !body.DryRun {
		t.Error("dry_run defaults true with no state file")
	}
}

func shepherdPost(t *testing.T, app *Application, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	app.shepherdEnabledHandler(rec, httptest.NewRequest(http.MethodPost, "/internal/shepherd/enabled", strings.NewReader(body)))
	return rec
}

func TestShepherdPOSTEnableThenDisable(t *testing.T) {
	dir := shepherdDirWith(t, shepherdFixtureYAML, shepherdFixtureState)
	app := newShepherdApp()
	flag := filepath.Join(dir, shepherdFlagFile)

	rec := shepherdPost(t, app, `{"enabled":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("enable status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(flag); err != nil {
		t.Fatalf("enable must create %s: %v", shepherdFlagFile, err)
	}
	var enabled ShepherdResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &enabled); err != nil {
		t.Fatal(err)
	}
	if !enabled.Enabled || !enabled.Available {
		t.Errorf("POST response must be the fresh GET payload: %+v", enabled)
	}

	rec = shepherdPost(t, app, `{"enabled":false}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("disable status = %d, want 200", rec.Code)
	}
	if _, err := os.Stat(flag); !os.IsNotExist(err) {
		t.Fatalf("disable must remove the flag file (stat err = %v)", err)
	}
	var disabled ShepherdResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &disabled); err != nil {
		t.Fatal(err)
	}
	if disabled.Enabled {
		t.Error("enabled must be false after disable")
	}
	// Idempotent disable: removing a flag that is already gone is success.
	if rec := shepherdPost(t, app, `{"enabled":false}`); rec.Code != http.StatusOK {
		t.Errorf("repeat disable = %d, want 200", rec.Code)
	}
}

func TestShepherdPOSTConflictWhenDirUnusable(t *testing.T) {
	app := newShepherdApp()

	// Directory absent (the knob unset/missing case).
	t.Setenv(shepherdDirEnv, filepath.Join(t.TempDir(), "not-mounted"))
	rec := shepherdPost(t, app, `{"enabled":true}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("missing dir status = %d, want 409", rec.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("409 must be JSON (%v): %s", err, rec.Body)
	}
	if v, ok := body["available"].(bool); !ok || v {
		t.Errorf("409 body = %s, want available=false", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "not-mounted") {
		t.Errorf("409 must not name a filesystem path: %s", rec.Body.String())
	}

	// Directory present but read-only: the write fails, same answer.
	dir := shepherdDirWith(t, shepherdFixtureYAML, shepherdFixtureState)
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Skipf("chmod not effective as uid %d", os.Getuid())
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	if rec := shepherdPost(t, app, `{"enabled":true}`); rec.Code != http.StatusConflict {
		t.Errorf("unwritable dir status = %d, want 409", rec.Code)
	}
}

func TestShepherdPOSTRejectsBadBodyAndMethod(t *testing.T) {
	shepherdDirWith(t, shepherdFixtureYAML, shepherdFixtureState)
	app := newShepherdApp()
	for _, body := range []string{`{}`, `not json`, `{"enabled":"yes"}`} {
		rec := shepherdPost(t, app, body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("body %q status = %d, want 400", body, rec.Code)
		}
		var m map[string]string
		if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil || m["error"] == "" {
			t.Errorf("body %q must get a JSON error, got %s", body, rec.Body.String())
		}
	}
	rec := httptest.NewRecorder()
	app.shepherdEnabledHandler(rec, httptest.NewRequest(http.MethodGet, "/internal/shepherd/enabled", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET on the POST route = %d, want 405", rec.Code)
	}
}

func TestShepherdMaskMAC(t *testing.T) {
	for mac, want := range map[string]string{
		"b0:82:aa:bb:cc:9a":   "b0:82:**:**:**:9a",
		"b0-82-aa-bb-cc-9a":   "b0:82:**:**:**:9a",
		"b0:82:aa:bb:cc":      "", // short: echo nothing
		"b0:82:aa:bb:cc:9a:1": "", // long: echo nothing
		"":                    "",
	} {
		if got := maskMAC(mac); got != want {
			t.Errorf("maskMAC(%q) = %q, want %q", mac, got, want)
		}
	}
}

// Route wiring: the shepherd pair must ride the SAME gate_internal_api
// wrapper as /internal/metrics and the dashboard routes, so an operator who
// has a perimeter cannot have a power switch flipped from outside it.
func TestShepherdRoutesRespectInternalAPIGate(t *testing.T) {
	mux := func(t *testing.T, gate bool) http.Handler {
		t.Helper()
		d := enabledDashboardCfg(t)
		d.GateInternalAPI = gate
		if err := d.Validate(); err != nil {
			t.Fatal(err)
		}
		_, reg := applicationWithStaticRouteTable(t, d)
		for _, path := range []string{"/internal/shepherd", "/internal/shepherd/enabled", "/internal/shepherd/config"} {
			if _, ok := reg.GetRoutes()[path]; !ok {
				t.Fatalf("%s not registered", path)
			}
		}
		m := http.NewServeMux()
		reg.WireUp(m)
		return m
	}
	hit := func(h http.Handler, method, path, ip string) int {
		req := httptest.NewRequest(method, path, strings.NewReader(`{"enabled":true}`))
		req.RemoteAddr = ip + ":33333"
		req.Host = "127.0.0.1:40114"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}

	t.Setenv(shepherdDirEnv, filepath.Join(t.TempDir(), "not-mounted"))
	on := mux(t, true)
	for _, probe := range []struct{ method, path string }{{http.MethodGet, "/internal/shepherd"}, {http.MethodPost, "/internal/shepherd/enabled"}, {http.MethodPost, "/internal/shepherd/config"}, {http.MethodDelete, "/internal/shepherd/config"}} {
		if code := hit(on, probe.method, probe.path, "192.0.2.7"); code != http.StatusForbidden {
			t.Errorf("gate on: %s %s from unlisted ip = %d, want 403", probe.method, probe.path, code)
		}
		if code := hit(on, probe.method, probe.path, "127.0.0.1"); code == http.StatusForbidden {
			t.Errorf("gate on: %s %s from allowed ip = 403, want pass-through", probe.method, probe.path)
		}
	}
	off := mux(t, false)
	for _, probe := range []struct{ method, path string }{{http.MethodGet, "/internal/shepherd"}, {http.MethodPost, "/internal/shepherd/enabled"}, {http.MethodPost, "/internal/shepherd/config"}, {http.MethodDelete, "/internal/shepherd/config"}} {
		if code := hit(off, probe.method, probe.path, "192.0.2.7"); code == http.StatusForbidden {
			t.Errorf("gate off: %s %s from unlisted ip = 403, want ungated (shipped default)", probe.method, probe.path)
		}
	}
}

// --- Wake-triggers API (editable/defaults/constraints + config route) -----

const shepherdFixtureOverrides = `{"wake_window_seconds":300,"keep_asleep_if_up":["gpu-box-3","gpu-box-4"]}`

func shepherdPostConfig(t *testing.T, app *Application, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	app.shepherdConfigHandler(rec, httptest.NewRequest(http.MethodPost, "/internal/shepherd/config", strings.NewReader(body)))
	return rec
}

// GET exposes the trigger trio: effective (yaml+override merged), defaults
// (yaml only) and the constraints POST enforces. No override file => the two
// value sets are equal (pure YAML).
func TestShepherdGETTriggersEffectiveWithOverride(t *testing.T) {
	dir := shepherdDirWith(t, shepherdFixtureYAML, shepherdFixtureState)
	if err := os.WriteFile(filepath.Join(dir, shepherdOverridesFile), []byte(shepherdFixtureOverrides), 0o644); err != nil {
		t.Fatal(err)
	}
	body := getShepherd(t, newShepherdApp())

	if body.Editable.WakeWindowSeconds != 300 || strings.Join(body.Editable.KeepAsleepIfUp, ",") != "gpu-box-3,gpu-box-4" {
		t.Errorf("editable = %+v, want 300/[gpu-box-3,gpu-box-4]", body.Editable)
	}
	if body.Defaults.WakeWindowSeconds != 90 || strings.Join(body.Defaults.KeepAsleepIfUp, ",") != "gpu-box-2" {
		t.Errorf("defaults = %+v, want yaml values 90/[gpu-box-2]", body.Defaults)
	}

	// Constraints published verbatim (the UI renders from these, not from
	// constants duplicated in JS).
	raw := getShepherdRaw(t)
	for _, want := range []string{`"min":30`, `"max":3600`, `"max_items":16`, `^[A-Za-z0-9._-]{1,128}$`} {
		if !strings.Contains(raw, want) {
			t.Errorf("constraints missing %q in %s", want, raw)
		}
	}
}

func TestShepherdGETTriggersPureYAMLWhenNoOverride(t *testing.T) {
	shepherdDirWith(t, shepherdFixtureYAML, shepherdFixtureState)
	body := getShepherd(t, newShepherdApp())
	if body.Editable.WakeWindowSeconds != body.Defaults.WakeWindowSeconds ||
		strings.Join(body.Editable.KeepAsleepIfUp, ",") != strings.Join(body.Defaults.KeepAsleepIfUp, ",") {
		t.Errorf("editable %+v must equal defaults %+v without an override file", body.Editable, body.Defaults)
	}
}

// Fail-closed: a corrupt overrides file, an out-of-range value or a bad name
// is IGNORED (YAML value keeps winning); a valid field in a partially-bad
// file still applies (per-field isolation, one log line per ignored field).
func TestShepherdGETTriggersOverrideFailClosed(t *testing.T) {
	cases := map[string]struct {
		overrides      string
		wantWindow     int
		wantKeepAsleep string // comma-joined
	}{
		"corrupt json":        {`{not json`, 90, "gpu-box-2"},
		"window out of range": {`{"wake_window_seconds":29,"keep_asleep_if_up":[]}`, 90, ""},
		"window over max":     {`{"wake_window_seconds":3601}`, 90, "gpu-box-2"},
		"bad name":            {`{"wake_window_seconds":120,"keep_asleep_if_up":["bad name;!"]}`, 120, "gpu-box-2"},
		"window not an int":   {`{"wake_window_seconds":"90"}`, 90, "gpu-box-2"},
		"unknown key only":    {`{"demand_models":["x"],"idle_shutdown_seconds":5}`, 90, "gpu-box-2"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			dir := shepherdDirWith(t, shepherdFixtureYAML, shepherdFixtureState)
			if err := os.WriteFile(filepath.Join(dir, shepherdOverridesFile), []byte(tc.overrides), 0o644); err != nil {
				t.Fatal(err)
			}
			body := getShepherd(t, newShepherdApp())
			if body.Editable.WakeWindowSeconds != tc.wantWindow {
				t.Errorf("editable window = %d, want %d (fail-closed to YAML)", body.Editable.WakeWindowSeconds, tc.wantWindow)
			}
			if got := strings.Join(body.Editable.KeepAsleepIfUp, ","); got != tc.wantKeepAsleep {
				t.Errorf("editable keep_asleep_if_up = %q, want %q", got, tc.wantKeepAsleep)
			}
		})
	}
}

// POST valid: file written with exactly the two-key contract (0644), payload
// reflects the merge, next watcher run applies it.
func TestShepherdPOSTConfigWritesOverrides(t *testing.T) {
	dir := shepherdDirWith(t, shepherdFixtureYAML, shepherdFixtureState)
	app := newShepherdApp()

	rec := shepherdPostConfig(t, app, `{"wake_window_seconds":300,"keep_asleep_if_up":["gpu-box-3"]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	raw, err := os.ReadFile(filepath.Join(dir, shepherdOverridesFile))
	if err != nil {
		t.Fatal(err)
	}
	var written map[string]any
	if err := json.Unmarshal(raw, &written); err != nil {
		t.Fatalf("overrides must be valid JSON: %v", err)
	}
	if len(written) != 2 || written["wake_window_seconds"].(float64) != 300 {
		t.Errorf("file carries the two-key contract exactly, got %s", raw)
	}
	if info, statErr := os.Stat(filepath.Join(dir, shepherdOverridesFile)); statErr != nil || info.Mode().Perm() != 0o644 {
		t.Errorf("overrides mode = %v, want 0644", info.Mode().Perm())
	}

	var body ShepherdResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Editable.WakeWindowSeconds != 300 || strings.Join(body.Editable.KeepAsleepIfUp, ",") != "gpu-box-3" {
		t.Errorf("POST response must be the fresh merged payload: %+v", body.Editable)
	}
	if body.Defaults.WakeWindowSeconds != 90 {
		t.Errorf("defaults must stay yaml-only: %+v", body.Defaults)
	}
}

func shepherdPOSTConfigError(t *testing.T, body string) map[string]string {
	t.Helper()
	shepherdDirWith(t, shepherdFixtureYAML, shepherdFixtureState)
	rec := shepherdPostConfig(t, newShepherdApp(), body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body %s)", rec.Code, rec.Body.String())
	}
	var m struct {
		Errors map[string]string `json:"errors"`
		Error  string            `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("400 must be JSON: %s", rec.Body.String())
	}
	if m.Error != "" {
		return map[string]string{"_": m.Error}
	}
	return m.Errors
}

func TestShepherdPOSTConfigValidation(t *testing.T) {
	cases := []struct {
		name, body, field, reasonSubstr string
	}{
		{"window too low", `{"wake_window_seconds":29,"keep_asleep_if_up":[]}`, "wake_window_seconds", "between 30 and 3600"},
		{"window too high", `{"wake_window_seconds":99999,"keep_asleep_if_up":[]}`, "wake_window_seconds", "between 30 and 3600"},
		{"too many items", `{"wake_window_seconds":90,"keep_asleep_if_up":["a1","a2","a3","a4","a5","a6","a7","a8","a9","b1","b2","b3","b4","b5","b6","b7","b8"]}`, "keep_asleep_if_up", "at most 16"},
		{"bad name", `{"wake_window_seconds":90,"keep_asleep_if_up":["bad name;!"]}`, "keep_asleep_if_up", "^[A-Za-z0-9._-]{1,128}$"},
		{"partial body", `{"wake_window_seconds":90}`, "_", "both fields required"},
		{"not json", `nope`, "_", "must be JSON"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			errs := shepherdPOSTConfigError(t, tc.body)
			got, ok := errs[tc.field]
			if !ok {
				t.Fatalf("errors = %v, want key %q", errs, tc.field)
			}
			if !strings.Contains(got, tc.reasonSubstr) {
				t.Errorf("reason %q must mention %q", got, tc.reasonSubstr)
			}
		})
	}
}

// A valid POST naming a missing directory is the mount's fault: 409 with
// available=false, no path in the body (same disclosure rule as /enabled).
func TestShepherdPOSTConfigConflictWhenDirMissing(t *testing.T) {
	app := newShepherdApp()
	t.Setenv(shepherdDirEnv, filepath.Join(t.TempDir(), "not-mounted"))
	rec := shepherdPostConfig(t, app, `{"wake_window_seconds":90,"keep_asleep_if_up":[]}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", rec.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("409 must be JSON: %s", rec.Body.String())
	}
	if v, ok := body["available"].(bool); !ok || v {
		t.Errorf("409 body = %s, want available=false", rec.Body.String())
	}
}

// DELETE removes the overrides file (revert to pure YAML) and is idempotent:
// deleting a file that was never written is success, not an error.
func TestShepherdDELETEConfigResetsToYAML(t *testing.T) {
	dir := shepherdDirWith(t, shepherdFixtureYAML, shepherdFixtureState)
	app := newShepherdApp()
	overrides := filepath.Join(dir, shepherdOverridesFile)
	if err := os.WriteFile(overrides, []byte(shepherdFixtureOverrides), 0o644); err != nil {
		t.Fatal(err)
	}

	del := func() *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		app.shepherdConfigHandler(rec, httptest.NewRequest(http.MethodDelete, "/internal/shepherd/config", nil))
		return rec
	}
	rec := del()
	if rec.Code != http.StatusOK {
		t.Fatalf("reset status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(overrides); !os.IsNotExist(err) {
		t.Fatalf("reset must remove the overrides file (stat err = %v)", err)
	}
	var body ShepherdResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Editable.WakeWindowSeconds != 90 || strings.Join(body.Editable.KeepAsleepIfUp, ",") != "gpu-box-2" {
		t.Errorf("after reset editable = %+v, want yaml values 90/[gpu-box-2]", body.Editable)
	}
	if rec := del(); rec.Code != http.StatusOK {
		t.Errorf("repeat reset = %d, want 200 (idempotent)", rec.Code)
	}
}

func TestShepherdConfigHandlerRejectsGET(t *testing.T) {
	shepherdDirWith(t, shepherdFixtureYAML, shepherdFixtureState)
	rec := httptest.NewRecorder()
	newShepherdApp().shepherdConfigHandler(rec, httptest.NewRequest(http.MethodGet, "/internal/shepherd/config", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET on the config route = %d, want 405", rec.Code)
	}
}
