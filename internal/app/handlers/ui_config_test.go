package handlers

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

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
	cfg := uiConfigFrom(&config.Config{})
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
		"gpu":   "requires metrics_source.mode=influx and metrics_source.influx.gpu_endpoint",
	} {
		enabled, got := sectionEnabled(cfg, id)
		assert.False(t, enabled, id)
		assert.Equal(t, reason, got, "disabled sections must explain why")
	}

	cfgAliases := uiConfigFrom(&config.Config{ModelAliases: map[string][]string{"m": {"x"}}})
	enabled, _ = sectionEnabled(cfgAliases, "models")
	assert.True(t, enabled, "aliases alone enable the models section")

	assert.Equal(t, 2000, cfg.PollMS.Overview)
	assert.Equal(t, 5000, cfg.PollMS.Endpoints)
	assert.Equal(t, 10000, cfg.PollMS.Models)
	assert.Equal(t, 10000, cfg.PollMS.Observe)
	assert.Len(t, cfg.RuntimeMetricColumns, 5)
	assert.Equal(t, runtimeMetricKeyGenerationTPS, cfg.RuntimeMetricColumns[0].Key)
	assert.Equal(t, "tok_s", cfg.RuntimeMetricColumns[0].Unit)
	assert.Equal(t, "Queue depth", cfg.RuntimeMetricColumns[4].Label)
}

func TestUIConfigFromInflux(t *testing.T) {
	cfg := uiConfigFrom(&config.Config{MetricsSource: config.MetricsSourceConfig{
		Mode:   "influx",
		Influx: config.InfluxSourceConfig{PowerAttribution: map[string]string{"a": "plug-a"}, GPUEndpoint: "gpu-target"},
	}})
	assert.Equal(t, "influx", cfg.MetricsSource)
	for _, id := range []string{"power", "cost", "gpu"} {
		enabled, reason := sectionEnabled(cfg, id)
		assert.True(t, enabled, id)
		assert.Empty(t, reason)
	}

	noGpu := uiConfigFrom(&config.Config{MetricsSource: config.MetricsSourceConfig{Mode: "influx"}})
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
