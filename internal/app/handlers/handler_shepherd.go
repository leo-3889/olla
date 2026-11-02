package handlers

// Shepherd (WOL) settings API: the read/write surface behind the dashboard's
// power toggle. The shepherd-watch cron watcher runs OUTSIDE the repo (its
// deployed config/state live in a directory mounted into the container), so
// this handler is a thin, sanitising file bridge: it reads the watcher's own
// files and writes exactly two files, the enable flag the watcher checks at
// run start and the wake-trigger overrides the watcher applies over its YAML.
// Absent flag => the watcher records action=disabled and exits 0 doing nothing
// else, which makes this endpoint the authoritative switch.
//
// Disclosure rule: nothing here may leak a full MAC, any IP, any filesystem
// path, or the shutdown ssh target. Only the fields below are ever copied out
// of the watcher's YAML, the MAC is masked (first two octets + last octet),
// and every failure path reports availability - never a path or an OS error.

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/thushan/olla/internal/core/constants"
	"gopkg.in/yaml.v3"
)

const (
	// shepherdDirEnv points at the mounted watcher directory. Unset (or a
	// directory that does not exist) is the normal state for a deployment
	// without the watcher: the API answers available=false, and the UI hides
	// the toggle rather than showing a switch that controls nothing.
	shepherdDirEnv     = "OLLA_SHEPHERD_DIR"
	shepherdDirDefault = "/config/watch"

	shepherdFlagFile      = "shepherd-enabled"
	shepherdStateFile     = "shepherd-watch-state.json"
	shepherdYAMLFile      = "shepherd-watch.yaml"
	shepherdOverridesFile = "shepherd-overrides.json"

	// Wake-trigger bounds, shared by the POST validator and published in
	// the GET payload's constraints so the UI renders honest inputs.
	shepherdWakeWindowMin    = 30
	shepherdWakeWindowMax    = 3600
	shepherdKeepAsleepMax    = 16
	shepherdKeepAsleepPatter = `^[A-Za-z0-9._-]{1,128}$`
)

// shepherdKeepAsleepRE is the compiled form of the pattern above: endpoint
// names only - the list is published, so shell/URL/quote characters are
// rejected by shape, not by deny-list.
var shepherdKeepAsleepRE = regexp.MustCompile(shepherdKeepAsleepPatter)

// ShepherdConfig is the operator-visible subset of shepherd-watch.yaml.
// Deliberately a allow-list, not a mirror of the config: mac is published
// masked and broadcast/ssh/endpoint are absent by design.
type ShepherdConfig struct {
	NodeName            string `json:"node_name"`
	MACMasked           string `json:"mac_masked"`
	IdleShutdownSeconds int    `json:"idle_shutdown_seconds"`
	WakeWindowSeconds   int    `json:"wake_window_seconds"`
}

// ShepherdState mirrors the watcher's last-known activity. Empty string means
// "not recorded", never a fabricated timestamp.
type ShepherdState struct {
	LastCheck    string `json:"last_check"`
	LastAction   string `json:"last_action"`
	LastWake     string `json:"last_wake"`
	LastShutdown string `json:"last_shutdown"`
	LastError    string `json:"last_error"`
}

// ShepherdTriggers is the GUI-editable wake-trigger pair - and the ONLY part
// of the watcher config the API writes. Demand models, idle_shutdown_seconds,
// MACs, IPs, endpoints and enforce mode are deliberately absent: they stay
// YAML/site territory and never appear in the editable object.
type ShepherdTriggers struct {
	WakeWindowSeconds int      `json:"wake_window_seconds"`
	KeepAsleepIfUp    []string `json:"keep_asleep_if_up"`
}

// ShepherdResponse is the JSON contract of GET /internal/shepherd (and the
// body returned by POST /internal/shepherd/enabled, POST and DELETE
// /internal/shepherd/config).
type ShepherdResponse struct {
	Config         ShepherdConfig `json:"config"`
	State          ShepherdState  `json:"state"`
	Available      bool           `json:"available"`
	Enabled        bool           `json:"enabled"`
	DryRun         bool           `json:"dry_run"`
	ConfigReadable bool           `json:"config_readable"`
	// Editable = EFFECTIVE triggers: YAML with shepherd-overrides.json
	// applied (missing file => pure YAML). Defaults = the YAML-only values,
	// so the UI can show what an override changed. Constraints = the exact
	// bounds POST enforces.
	Editable    ShepherdTriggers `json:"editable"`
	Defaults    ShepherdTriggers `json:"defaults"`
	Constraints map[string]any   `json:"constraints"`
}

// shepherdDir resolves the watcher directory knob. Read per request (not at
// boot) so the handler is testable without process-level config plumbing, and
// so a bind-mount added in a compose file takes effect on restart only, which
// is what a container-mounted directory means anyway.
func shepherdDir() string {
	if dir := os.Getenv(shepherdDirEnv); dir != "" {
		return dir
	}
	return shepherdDirDefault
}

// shepherdAvailable reports whether the watcher directory is present. Only a
// real directory counts: a dangling symlink or a file named like the mount
// must not turn into a half-populated payload.
func shepherdAvailable(dir string) bool {
	info, err := os.Stat(dir)
	return err == nil && info.IsDir()
}

// maskMAC keeps the first two octets and the last (b0:82:**:**:**:9a): enough
// for an operator to recognise the box, not enough to send a magic packet.
// Any other length yields "" - a partial/odd MAC must never be echoed.
func maskMAC(mac string) string {
	parts := strings.FieldsFunc(mac, func(r rune) bool { return r == ':' || r == '-' })
	if len(parts) != 6 {
		return ""
	}
	return parts[0] + ":" + parts[1] + ":**:**:**:" + parts[5]
}

// shepherdYAML is the strict allow-list read of the watcher config. Unlisted
// keys (mac in full, broadcast, ssh, endpoint) are decoded into nothing at
// all, so they cannot leak through a future field addition by accident.
// Multiple nodes: the first node is the one published - the contract is
// single-node shaped; a multi-node deployment shows its primary and the
// watcher applies its own per-node rules.
type shepherdYAML struct {
	IdleSeconds      int `yaml:"idle_seconds"`
	WakeDemandWindow int `yaml:"wake_demand_window"`
	Nodes            []struct {
		Name           string   `yaml:"name"`
		MAC            string   `yaml:"mac"`
		KeepAsleepIfUp []string `yaml:"keep_asleep_if_up"`
	} `yaml:"nodes"`
}

func shepherdConfigFrom(dir string) (ShepherdConfig, bool) {
	var out ShepherdConfig
	raw, err := os.ReadFile(filepath.Join(dir, shepherdYAMLFile))
	if err != nil {
		return out, false
	}
	var cfg shepherdYAML
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return out, false
	}
	out.IdleShutdownSeconds = cfg.IdleSeconds
	out.WakeWindowSeconds = cfg.WakeDemandWindow
	if len(cfg.Nodes) > 0 {
		out.NodeName = cfg.Nodes[0].Name
		out.MACMasked = maskMAC(cfg.Nodes[0].MAC)
	}
	return out, true
}

// validateWakeWindow / validateKeepAsleep return "" when the value is legal
// and a human reason otherwise (the POST body answers {"errors":{field:
// reason}}; the override reader logs the same reason).
func validateWakeWindow(n int) string {
	if n < shepherdWakeWindowMin || n > shepherdWakeWindowMax {
		return "must be between " + strconv.Itoa(shepherdWakeWindowMin) +
			" and " + strconv.Itoa(shepherdWakeWindowMax)
	}
	return ""
}

func validateKeepAsleep(list []string) string {
	if len(list) > shepherdKeepAsleepMax {
		return "at most " + strconv.Itoa(shepherdKeepAsleepMax) + " items"
	}
	for _, name := range list {
		if !shepherdKeepAsleepRE.MatchString(name) {
			return "each item must match " + shepherdKeepAsleepPatter
		}
	}
	return ""
}

// shepherdTriggersFrom computes the trigger pair: defaults are the YAML
// values (single-node shaped: the first node's twin list), editable adds the
// shepherd-overrides.json layer. Fail-closed rule: a missing file is pure
// YAML; a corrupt file, a non-matching type, an out-of-range value or an
// unknown top-level key is IGNORED with a log line naming the field, so the
// YAML value always wins on any doubt. The overrides file is machine-written
// by the POST handler; a hand-edit that names unknown keys simply has no
// effect (the two keys are the whole contract).
func (a *Application) shepherdTriggersFrom(dir string) (editable, defaults ShepherdTriggers) {
	var yamlCfg shepherdYAML
	if raw, err := os.ReadFile(filepath.Join(dir, shepherdYAMLFile)); err == nil {
		_ = yaml.Unmarshal(raw, &yamlCfg) // fields we need are typed; a bad file yields zero defaults
	}
	defaults.WakeWindowSeconds = yamlCfg.WakeDemandWindow
	if len(yamlCfg.Nodes) > 0 {
		defaults.KeepAsleepIfUp = yamlCfg.Nodes[0].KeepAsleepIfUp
	}
	editable = defaults

	raw, err := os.ReadFile(filepath.Join(dir, shepherdOverridesFile))
	if err != nil {
		return editable, defaults
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		a.logger.Info("shepherd overrides ignored: parse error")
		return editable, defaults
	}
	if v, ok := doc["wake_window_seconds"]; ok {
		var n int
		switch reason := "not an int"; {
		case json.Unmarshal(v, &n) != nil:
			a.logger.Info("shepherd override ignored: wake_window_seconds " + reason)
		case validateWakeWindow(n) != "":
			a.logger.Info("shepherd override ignored: wake_window_seconds " + validateWakeWindow(n))
		default:
			editable.WakeWindowSeconds = n
		}
	}
	if v, ok := doc["keep_asleep_if_up"]; ok {
		var list []string
		switch reason := "not a list of strings"; {
		case json.Unmarshal(v, &list) != nil:
			a.logger.Info("shepherd override ignored: keep_asleep_if_up " + reason)
		case validateKeepAsleep(list) != "":
			a.logger.Info("shepherd override ignored: keep_asleep_if_up " + validateKeepAsleep(list))
		default:
			editable.KeepAsleepIfUp = list
		}
	}
	return editable, defaults
}

func shepherdConstraints() map[string]any {
	return map[string]any{
		"wake_window_seconds": map[string]int{
			"min": shepherdWakeWindowMin, "max": shepherdWakeWindowMax,
		},
		"keep_asleep_if_up": map[string]any{
			"max_items": shepherdKeepAsleepMax, "pattern": shepherdKeepAsleepPatter,
		},
	}
}

// shepherdStateFrom maps the watcher's state file onto the published fields.
// Two shapes are accepted: the current top-level disclosures (last_check,
// action / last_action, last_wake, last_shutdown, last_error) and the
// per-node epoch-seconds bookkeeping the watcher has always written
// (last_run_ts, last_wake_ts, last_shutdown_attempt_ts), reduced to the latest
// value across nodes. Missing keys stay empty.
func shepherdStateFrom(dir string) (ShepherdState, bool) {
	state, dryRun := ShepherdState{}, true
	raw, err := os.ReadFile(filepath.Join(dir, shepherdStateFile))
	if err != nil {
		return state, dryRun
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return state, dryRun
	}
	// dry_run disclosure: the state key wins; the default is true because a
	// watcher that has not published the key has not declared enforcement,
	// and the live deployment runs dry (no --enforce).
	if d, ok := doc["dry_run"].(bool); ok {
		dryRun = d
	}
	state.LastCheck = jsonString(doc["last_check"])
	state.LastAction = firstNonEmpty(jsonString(doc["last_action"]), jsonString(doc["action"]))
	state.LastWake = jsonString(doc["last_wake"])
	state.LastShutdown = jsonString(doc["last_shutdown"])
	state.LastError = sanitizeLastError(jsonString(doc["last_error"]))

	latest := func(key string) string {
		var best float64
		for _, v := range doc {
			node, ok := v.(map[string]any)
			if !ok {
				continue
			}
			if ts, ok := node[key].(float64); ok && ts > best {
				best = ts
			}
		}
		if best <= 0 {
			return ""
		}
		return time.Unix(int64(best), 0).UTC().Format(time.RFC3339)
	}
	if state.LastCheck == "" {
		state.LastCheck = latest("last_run_ts")
	}
	if state.LastWake == "" {
		state.LastWake = latest("last_wake_ts")
	}
	if state.LastShutdown == "" {
		state.LastShutdown = latest("last_shutdown_attempt_ts")
	}
	return state, dryRun
}

func jsonString(v any) string {
	s, _ := v.(string)
	return s
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// siteDataRE matches MAC-like colon-hex sequences (6+ groups) or IPv4 dotted
// quads in a LastError string — defense-in-depth: the watcher is sanitised
// upstream, this catches hand-edited state files and future regressions.
var siteDataRE = regexp.MustCompile(`[0-9a-fA-F]{2}([:\-][0-9a-fA-F]{2}){3,}|\d{1,3}(\.\d{1,3}){3}`)

// sanitizeLastError caps length and suppresses site data (MACs, IPs).
func sanitizeLastError(s string) string {
	if s == "" {
		return ""
	}
	if len(s) > 200 {
		s = s[:200]
	}
	if siteDataRE.MatchString(s) {
		return "error detail suppressed (site data)"
	}
	return s
}

// shepherdPayload assembles the GET payload. Any unreadable piece degrades to
// its zero value rather than an error: the toggle's availability is the only
// fact the UI needs to render honestly.
func (a *Application) shepherdPayload() ShepherdResponse {
	dir := shepherdDir()
	if !shepherdAvailable(dir) {
		return ShepherdResponse{}
	}
	state, dryRun := shepherdStateFrom(dir)
	editable, defaults := a.shepherdTriggersFrom(dir)
	cfg, readable := shepherdConfigFrom(dir)
	return ShepherdResponse{
		Available:      true,
		Enabled:        fileExists(filepath.Join(dir, shepherdFlagFile)),
		DryRun:         dryRun,
		ConfigReadable: readable,
		Config:         cfg,
		State:          state,
		Editable:       editable,
		Defaults:       defaults,
		Constraints:    shepherdConstraints(),
	}
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func writeShepherdJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set(constants.HeaderContentType, constants.ContentTypeJSON)
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		http.Error(w, "Internal server error", http.StatusInternalServerError)
	}
}

// shepherdHandler serves GET /internal/shepherd. Always 200 when reachable:
// "no watcher mounted" is a state (available=false), not a failure.
func (a *Application) shepherdHandler(w http.ResponseWriter, r *http.Request) {
	writeShepherdJSON(w, http.StatusOK, a.shepherdPayload())
}

// shepherdEnabledHandler is POST /internal/shepherd/enabled: it writes or
// removes the enable flag and returns the fresh GET payload, so the UI needs
// no second round trip. A directory that is missing or unwritable answers 409
// with available=false - the cause is the mount, not the request, and naming
// the OS error would name a filesystem path.
func (a *Application) shepherdEnabledHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Enabled *bool `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Enabled == nil {
		writeShepherdJSON(w, http.StatusBadRequest, map[string]string{
			"error": `body must be JSON: {"enabled": true|false}`,
		})
		return
	}

	dir := shepherdDir()
	flag := filepath.Join(dir, shepherdFlagFile)
	var err error
	if *body.Enabled {
		// Content is a human-readable marker only; the watcher checks
		// presence, never the bytes - so the file is owner-only (0600):
		// nothing needs to read it across users, and the timestamp inside is
		// not a secret worth publishing.
		err = os.WriteFile(flag, []byte(time.Now().UTC().Format(time.RFC3339)+"\n"), 0o600)
	} else if rmErr := os.Remove(flag); rmErr != nil && !os.IsNotExist(rmErr) {
		err = rmErr
	}
	if err != nil {
		writeShepherdJSON(w, http.StatusConflict, ShepherdResponse{})
		return
	}

	a.logger.Info("shepherd enable flag updated", "enabled", *body.Enabled)
	writeShepherdJSON(w, http.StatusOK, a.shepherdPayload())
}

// shepherdConfigHandler is /internal/shepherd/config, the GUI-editable wake
// triggers (ONLY wake_window_seconds and keep_asleep_if_up - everything else
// in the watcher config is deliberately not writable through this API):
//
//	POST   {"wake_window_seconds":int,"keep_asleep_if_up":[...]}
//	       -> validate BOTH fields (partial body => 400 "both fields
//	          required"; violations => 400 {"errors":{"field":"reason"}})
//	       -> atomic tmp+rename write of shepherd-overrides.json (0644, the
//	          watcher applies it over its YAML on the next run)
//	       -> 200 with the full GET payload (effective values included).
//	DELETE -> remove the overrides file = revert to pure YAML, idempotent.
//
// DELETE chosen over POST {"reset":true}: a reset carries no value, and a
// body shape that is conditionally valid invites UI bugs; the method says it.
// A missing/unwritable directory answers 409 available=false, same rule as
// the enabled route: the mount is the cause, and OS errors name paths.
func (a *Application) shepherdConfigHandler(w http.ResponseWriter, r *http.Request) {
	dir := shepherdDir()

	switch r.Method {
	case http.MethodPost:
		var body struct {
			WakeWindowSeconds *int      `json:"wake_window_seconds"`
			KeepAsleepIfUp    *[]string `json:"keep_asleep_if_up"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeShepherdJSON(w, http.StatusBadRequest, map[string]string{
				"error": `body must be JSON: {"wake_window_seconds": int, "keep_asleep_if_up": [...]}`,
			})
			return
		}
		if body.WakeWindowSeconds == nil || body.KeepAsleepIfUp == nil {
			writeShepherdJSON(w, http.StatusBadRequest, map[string]string{
				"error": "both fields required: wake_window_seconds, keep_asleep_if_up",
			})
			return
		}
		errors := map[string]string{}
		if reason := validateWakeWindow(*body.WakeWindowSeconds); reason != "" {
			errors["wake_window_seconds"] = reason
		}
		if reason := validateKeepAsleep(*body.KeepAsleepIfUp); reason != "" {
			errors["keep_asleep_if_up"] = reason
		}
		if len(errors) > 0 {
			writeShepherdJSON(w, http.StatusBadRequest, map[string]any{"errors": errors})
			return
		}
		if !shepherdAvailable(dir) {
			writeShepherdJSON(w, http.StatusConflict, ShepherdResponse{})
			return
		}
		triggers := ShepherdTriggers{
			WakeWindowSeconds: *body.WakeWindowSeconds,
			KeepAsleepIfUp:    *body.KeepAsleepIfUp,
		}
		if triggers.KeepAsleepIfUp == nil {
			triggers.KeepAsleepIfUp = []string{}
		}
		if err := writeShepherdOverrides(dir, triggers); err != nil {
			writeShepherdJSON(w, http.StatusConflict, ShepherdResponse{})
			return
		}
		a.logger.Info("shepherd overrides written",
			"wake_window_seconds", triggers.WakeWindowSeconds,
			"keep_asleep_if_up", len(triggers.KeepAsleepIfUp))

	case http.MethodDelete:
		if !shepherdAvailable(dir) {
			writeShepherdJSON(w, http.StatusConflict, ShepherdResponse{})
			return
		}
		if rmErr := os.Remove(filepath.Join(dir, shepherdOverridesFile)); rmErr != nil && !os.IsNotExist(rmErr) {
			writeShepherdJSON(w, http.StatusConflict, ShepherdResponse{})
			return
		}
		a.logger.Info("shepherd overrides removed (reverted to YAML)")

	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	writeShepherdJSON(w, http.StatusOK, a.shepherdPayload())
}

// writeShepherdOverrides atomically replaces the overrides file: tmp in the
// same directory + rename, so the watcher (cron, every minute) never reads a
// half-written file. The bytes are the two-key contract only - unknown keys
// cannot be produced from a validated request.
func writeShepherdOverrides(dir string, triggers ShepherdTriggers) error {
	data, err := json.Marshal(triggers)
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, shepherdOverridesFile+".tmp")
	//nolint:gosec // G306: two bounded ints + endpoint names, all re-published by GET; 0644 matches the YAML it overrides
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, shepherdOverridesFile))
}
