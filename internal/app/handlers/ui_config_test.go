package handlers

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/thushan/olla/internal/config"
)

func sectionEnabled(cfg UIConfig, id string) (bool, string) {
	for _, section := range cfg.Sections {
		if section.ID == id {
			return section.Enabled, section.Reason
		}
	}
	return false, "MISSING"
}

func TestUIConfigFromSamplerDefaults(t *testing.T) {
	cfg := uiConfigFrom(&config.Config{}, "", "")
	assert.Equal(t, "sampler", cfg.MetricsSource)
	for _, id := range []string{"overview", "endpoints"} {
		enabled, _ := sectionEnabled(cfg, id)
		assert.True(t, enabled, id)
	}
	enabled, _ := sectionEnabled(cfg, "models")
	assert.False(t, enabled, "no aliases and no model discovery")

	for id, reason := range map[string]string{
		"power": "requires metrics_source.mode=influx",
		"cost":  "requires metrics_source.mode=influx",
		"gpu":   "requires metrics_source.mode=influx and metrics_source.influx.gpu_endpoints",
	} {
		enabled, got := sectionEnabled(cfg, id)
		assert.False(t, enabled, id)
		assert.Equal(t, reason, got, "disabled sections must explain why")
	}

	cfgAliases := uiConfigFrom(&config.Config{ModelAliases: map[string][]string{"m": {"x"}}}, "", "")
	enabled, _ = sectionEnabled(cfgAliases, "models")
	assert.True(t, enabled, "aliases alone enable the models section")

	assert.Equal(t, 5000, cfg.PollMS.Overview)
	assert.Equal(t, 5000, cfg.PollMS.Endpoints, "the endpoints poll is 5s: the GUI clock follows the 5s health tick")
	assert.Equal(t, 15000, cfg.PollMS.Models)
	assert.Equal(t, 10000, cfg.PollMS.Observe)
	// With no opted-in endpoints there is nothing to promise: the manifest
	// advertises no columns and the UI falls back to its built-in defaults.
	assert.Empty(t, cfg.RuntimeMetricColumns)
}

func TestUIConfigFromInflux(t *testing.T) {
	cfg := uiConfigFrom(&config.Config{MetricsSource: config.MetricsSourceConfig{
		Mode:   "influx",
		Influx: config.InfluxSourceConfig{PowerAttribution: map[string]string{"a": "plug-a"}, GPUEndpoints: []string{"gpu-target"}},
	}}, "", "")
	assert.Equal(t, "influx", cfg.MetricsSource)
	for _, id := range []string{"power", "cost", "gpu"} {
		enabled, reason := sectionEnabled(cfg, id)
		assert.True(t, enabled, id)
		assert.Empty(t, reason)
	}

	noGpu := uiConfigFrom(&config.Config{MetricsSource: config.MetricsSourceConfig{Mode: "influx"}}, "", "")
	enabled, reason := sectionEnabled(noGpu, "power")
	assert.False(t, enabled)
	assert.Contains(t, reason, "influx")
	enabled, _ = sectionEnabled(noGpu, "cost")
	assert.True(t, enabled, "cost needs nothing but influx mode")
	enabled, _ = sectionEnabled(noGpu, "gpu")
	assert.False(t, enabled)
}

func TestUIConfigHandlerJSON(t *testing.T) {
	app := &Application{Config: &config.Config{}}
	rec := httptest.NewRecorder()
	app.uiConfigHandler(rec, httptest.NewRequest("GET", uiConfigRoute, nil))
	require.Equal(t, 200, rec.Code)
	assert.Equal(t, "no-cache", rec.Header().Get("Cache-Control"))
	assert.Empty(t, rec.Header().Get("ETag"))
	var body UIConfig
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, 1, body.Schema)
	assert.Equal(t, "sampler", body.MetricsSource)
	assert.NotZero(t, body.Sections)
}

func TestUIConfigPollIntervalsFromConfig(t *testing.T) {
	cfg := &config.Config{}
	cfg.Dashboard.Polling.OverviewInterval = 3 * time.Second
	cfg.Dashboard.Polling.ModelsInterval = 45 * time.Second
	cfg.Dashboard.Polling.ObserveInterval = -time.Second // unset, not an error
	body := uiConfigFrom(cfg, "", "")
	assert.Equal(t, 3000, body.PollMS.Overview)
	assert.Equal(t, 5000, body.PollMS.Endpoints) // untouched keeps default
	assert.Equal(t, 45000, body.PollMS.Models)
	assert.Equal(t, 10000, body.PollMS.Observe)
}

func optedKind(kind string) *config.RuntimeMetricsConfig {
	return &config.RuntimeMetricsConfig{Enabled: true, Kind: kind}
}

func columnKeys(cols []uiMetricColumn) []string {
	out := make([]string, 0, len(cols))
	for _, c := range cols {
		out = append(out, c.Key)
	}
	return out
}

func TestUIConfigRuntimeColumnsFromFleetDialects(t *testing.T) {
	cfg := &config.Config{}
	cfg.Discovery.Static.Endpoints = []config.EndpointConfig{
		{Name: "gw", Type: "openai-compatible", RuntimeMetrics: optedKind("llamacpp")},
		{Name: "plain", Type: "openai-compatible"},                                                  // not opted in
		{Name: "engine", Type: "vllm", RuntimeMetrics: &config.RuntimeMetricsConfig{Enabled: true}}, // kind from type
	}
	// llamacpp and vllm both expose running requests; only vllm has a
	// scheduler queue, and the union carries it. A non-opted endpoint
	// contributes nothing even if its type is known.
	assert.Equal(t, []string{"generation_tps", "prefill_tps", "cache_hit_percent", "requests_running", "queue_depth"}, columnKeys(uiRuntimeColumnsFrom(cfg)))
	assert.Equal(t, "Waiting", uiRuntimeColumnsFrom(cfg)[4].Label)

	cfg.Discovery.Static.Endpoints = append(cfg.Discovery.Static.Endpoints,
		config.EndpointConfig{Name: "rad", Type: "openai-compatible", RuntimeMetrics: optedKind("radiance")},
		config.EndpointConfig{Name: "odd", Type: "openai-compatible", RuntimeMetrics: optedKind("sglang")},
	)
	// radiance and vllm both fill the waiting queue (first dialect to
	// declare a key owns its label); an unregistered kind contributes
	// nothing (and the sampler reports it separately). Running renders
	// before Waiting: the union keeps each dialect's display order.
	cols := uiRuntimeColumnsFrom(cfg)
	assert.Equal(t, []string{"generation_tps", "prefill_tps", "cache_hit_percent", "requests_running", "queue_depth"}, columnKeys(cols))
	assert.Equal(t, "Running", cols[3].Label)
	assert.Equal(t, "count", cols[3].Unit)
	assert.False(t, cols[3].Window)
	assert.Equal(t, "Waiting", cols[4].Label)
	assert.True(t, cols[0].Window)
	// S8: every published runtime column carries non-empty header help.
	for _, c := range cols {
		assert.NotEmpty(t, c.Help, "column %s must ship help text", c.Key)
	}

	// Stable across calls regardless of endpoint declaration order.
	reordered := &config.Config{}
	reordered.Discovery.Static.Endpoints = []config.EndpointConfig{
		cfg.Discovery.Static.Endpoints[3],
		cfg.Discovery.Static.Endpoints[0],
		cfg.Discovery.Static.Endpoints[3],
		cfg.Discovery.Static.Endpoints[2],
	}
	assert.Equal(t, columnKeys(cols), columnKeys(uiRuntimeColumnsFrom(reordered)))
}

func TestUIConfigFromEffectiveModeOverride(t *testing.T) {
	// Intent is influx and every knob is set, but the process fell back to
	// the sampler at boot (missing token env): config.json must report the
	// EFFECTIVE mode and disable every influx-only section, or the UI would
	// advertise power/cost/GPU tiles that can never fill.
	cfg := uiConfigFrom(&config.Config{MetricsSource: config.MetricsSourceConfig{
		Mode:   "influx",
		Influx: config.InfluxSourceConfig{TokenEnv: "X", PowerAttribution: map[string]string{"a": "plug-a"}, GPUEndpoints: []string{"g"}},
	}}, "sampler", "")
	assert.Equal(t, "sampler", cfg.MetricsSource)
	for _, id := range []string{"power", "cost", "gpu"} {
		enabled, _ := sectionEnabled(cfg, id)
		assert.False(t, enabled, id)
	}
}

// The token columns are static: they are published on the manifest OUTSIDE the
// dialect union (the lifetime integrals and the KV-reuse basis are computed by
// the provider, not reported by an engine per endpoint), with window=false
// (a sum / a basis, never a windowed avg/max).
func TestUIConfig_StaticTokenColumns(t *testing.T) {
	want := []struct {
		key   string
		label string
		unit  string
	}{
		{"generation_tokens", "Tokens 180d (Gen)", "count"},
		{"prefill_tokens", "Tokens 180d (Prefill)", "count"},
		{"kv_cache_reuse_percent", "KV cache reuse", "percent"},
	}
	for _, mode := range []string{"sampler", "influx"} {
		ui := uiConfigFrom(&config.Config{MetricsSource: config.MetricsSourceConfig{Mode: mode}}, mode, "")
		require.Len(t, ui.StaticMetricColumns, len(want), mode)
		for i, w := range want {
			col := ui.StaticMetricColumns[i]
			assert.Equal(t, w.key, col.Key, mode)
			assert.Equal(t, w.label, col.Label, mode)
			assert.Equal(t, w.unit, col.Unit, mode)
			assert.False(t, col.Window, "%s is not a windowed stat", w.key, mode)
			assert.NotEmpty(t, col.Help, "column %s must ship help text", w.key)
		}
	}
	// T2: the label carries the EFFECTIVE window the caller publishes (the
	// retention-capped lookback), not a hardcoded 180d.
	capped := uiConfigFrom(&config.Config{}, "influx", "90d")
	assert.Equal(t, "Tokens 90d (Gen)", capped.StaticMetricColumns[0].Label)
	assert.Equal(t, "Tokens 90d (Prefill)", capped.StaticMetricColumns[1].Label)
	assert.Contains(t, capped.StaticMetricColumns[0].Help, "90d")
	// NOT part of the dialect union: the union never grows a lifetime key
	// (a dialect adding one would be claiming engine-reported lifetime, which
	// no engine can honestly provide).
	cfg := &config.Config{}
	cfg.Discovery.Static.Endpoints = []config.EndpointConfig{
		{Name: "a", Type: "openai-compatible", RuntimeMetrics: optedKind("radiance")},
	}
	for _, w := range want {
		assert.NotContains(t, columnKeys(uiRuntimeColumnsFrom(cfg)), w.key)
	}
}
