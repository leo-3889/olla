package config

import (
	"strings"
	"testing"
	"time"
)

// TestLoadProbeConfig_DisabledIsInert pins the knob-off contract: with
// Enabled false the section must validate no matter how nonsensical the rest
// of it is, because the default config ships the section present and a
// zero-valued probe must never be able to break a startup that never uses it.
func TestLoadProbeConfig_DisabledIsInert(t *testing.T) {
	off := LoadProbeConfig{Enabled: false}
	if err := off.Validate(); err != nil {
		t.Fatalf("disabled load_probe must validate, got: %v", err)
	}

	nonsense := LoadProbeConfig{Enabled: false, Timeout: 0, FanOutBudget: 1, HealthPath: "health", ActiveField: ""}
	if err := nonsense.Validate(); err != nil {
		t.Fatalf("disabled load_probe must validate garbage, got: %v", err)
	}
}

func TestLoadProbeConfig_EnabledRejectsUnusableShapes(t *testing.T) {
	cases := []struct {
		name string
		cfg  LoadProbeConfig
		want string
	}{
		{"zero timeout cancels every probe", LoadProbeConfig{Enabled: true, ActiveField: "in_flight"}, "timeout"},
		{"budget below timeout", LoadProbeConfig{Enabled: true, Timeout: 250 * time.Millisecond, FanOutBudget: 10 * time.Millisecond, ActiveField: "in_flight"}, "fan_out_budget"},
		{"path without leading slash", LoadProbeConfig{Enabled: true, Timeout: time.Second, FanOutBudget: time.Second, HealthPath: "health", ActiveField: "in_flight"}, "health_path"},
		{"empty active field", LoadProbeConfig{Enabled: true, Timeout: time.Second, FanOutBudget: time.Second, HealthPath: "/health"}, "active_field"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cfg.Validate()
			if err == nil {
				t.Fatalf("expected a boot error naming %q, got nil", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error must name %q, got: %v", tc.want, err)
			}
		})
	}
}

// TestLoadProbeConfig_DefaultsValidate is the byte-identical guarantee in
// config form: the shipped defaults are valid, off, and point at the field the
// live fleet's engines actually answer with.
func TestLoadProbeConfig_DefaultsValidate(t *testing.T) {
	cfg := DefaultConfig()
	lp := cfg.Proxy.LoadProbe

	if lp.Enabled {
		t.Fatal("load_probe must default to disabled (least-load is opt-in)")
	}
	if err := cfg.Proxy.Validate(); err != nil {
		t.Fatalf("default proxy config must validate, got: %v", err)
	}

	enabled := lp
	enabled.Enabled = true
	if err := enabled.Validate(); err != nil {
		t.Fatalf("default load_probe values must be usable when switched on, got: %v", err)
	}
	if lp.Timeout <= 0 || lp.FanOutBudget < lp.Timeout {
		t.Fatalf("default budgets unusable: timeout=%s fan_out=%s", lp.Timeout, lp.FanOutBudget)
	}
	if lp.HealthPath != "/health" || lp.ActiveField != "in_flight" {
		t.Fatalf("defaults must name the live radiance shape (/health in_flight), got %q %q", lp.HealthPath, lp.ActiveField)
	}
}

// TestProxyConfig_ValidateSurfacesLoadProbe proves a bad load_probe section
// fails at the proxy gate, i.e. at boot - the precedent set by
// sticky_sessions key_sources and retry_on_status, where a silently inert
// knob reads in the field as "the feature is broken".
func TestProxyConfig_ValidateSurfacesLoadProbe(t *testing.T) {
	p := ProxyConfig{LoadProbe: LoadProbeConfig{Enabled: true, Timeout: 0, ActiveField: "in_flight"}}
	if err := p.Validate(); err == nil {
		t.Fatal("proxy.Validate must propagate a load_probe boot error")
	}
}
