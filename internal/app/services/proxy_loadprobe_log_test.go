package services

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/thushan/olla/internal/adapter/balancer"
	appconfig "github.com/thushan/olla/internal/config"
	"github.com/thushan/olla/internal/logger"
)

// The fork's loud-inert doctrine (retry_on_status, sticky key_sources): a knob
// that cannot do its job must say so at boot. logLoadProbeConfig is the only
// signal an operator gets that load_probe and load_balancer disagree, so the
// exact shape of the four combinations is pinned here - including the two
// shapes that used to read as success ("Load probe armed" logged while nothing
// was wired) and as silence (least-load with the probe off, i.e. priority).

type loggedLine struct {
	Level string `json:"level"`
	Msg   string `json:"msg"`
}

func captureLoadProbeLog(t *testing.T, cfg appconfig.ProxyConfig) []loggedLine {
	t.Helper()

	var buf bytes.Buffer
	s := &ProxyServiceWrapper{
		config: &cfg,
		logger: logger.NewPlainStyledLogger(slog.New(slog.NewJSONHandler(&buf, nil))),
	}
	s.logLoadProbeConfig()

	var lines []loggedLine
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var l loggedLine
		if err := json.Unmarshal([]byte(line), &l); err != nil {
			t.Fatalf("unparseable log line %q: %v", line, err)
		}
		lines = append(lines, l)
	}
	return lines
}

func hasLine(lines []loggedLine, level, substr string) bool {
	for _, l := range lines {
		if l.Level == level && strings.Contains(l.Msg, substr) {
			return true
		}
	}
	return false
}

func TestLogLoadProbeConfig_OnlyArmedCombinationReportsSuccess(t *testing.T) {
	probe := appconfig.LoadProbeConfig{
		Enabled: true, HealthPath: "/health", ActiveField: "in_flight",
	}

	t.Run("enabled + least-load: armed, no warning", func(t *testing.T) {
		cfg := appconfig.ProxyConfig{LoadBalancer: balancer.DefaultBalancerLeastLoad, LoadProbe: probe}
		lines := captureLoadProbeLog(t, cfg)
		if !hasLine(lines, "INFO", "Load probe armed") {
			t.Fatalf("armed combination must log the armed Info, got %+v", lines)
		}
		if hasLine(lines, "WARN", "load_probe") {
			t.Fatalf("armed combination must not warn, got %+v", lines)
		}
	})

	t.Run("enabled + other balancer: warn only, never 'armed'", func(t *testing.T) {
		cfg := appconfig.ProxyConfig{LoadBalancer: balancer.DefaultBalancerPriority, LoadProbe: probe}
		lines := captureLoadProbeLog(t, cfg)
		if !hasLine(lines, "WARN", "has no effect") {
			t.Fatalf("inert probe must warn, got %+v", lines)
		}
		if hasLine(lines, "INFO", "Load probe armed") {
			t.Fatalf("a probe that nothing will call must not log 'armed', got %+v", lines)
		}
	})

	t.Run("disabled + least-load: warn that it runs as priority", func(t *testing.T) {
		cfg := appconfig.ProxyConfig{
			LoadBalancer: balancer.DefaultBalancerLeastLoad,
			LoadProbe:    appconfig.LoadProbeConfig{Enabled: false},
		}
		lines := captureLoadProbeLog(t, cfg)
		if !hasLine(lines, "WARN", "runs as") || !hasLine(lines, "WARN", "priority") {
			t.Fatalf("least-load without a probe must say it runs as priority, got %+v", lines)
		}
		if hasLine(lines, "INFO", "Load probe armed") {
			t.Fatalf("disabled probe must not log 'armed', got %+v", lines)
		}
	})

	t.Run("disabled + priority: total silence (knob-off byte-identical)", func(t *testing.T) {
		cfg := appconfig.ProxyConfig{
			LoadBalancer: balancer.DefaultBalancerPriority,
			LoadProbe:    appconfig.LoadProbeConfig{Enabled: false},
		}
		if lines := captureLoadProbeLog(t, cfg); len(lines) != 0 {
			t.Fatalf("the default configuration must log nothing, got %+v", lines)
		}
	})
}
