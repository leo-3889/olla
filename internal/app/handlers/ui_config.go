package handlers

import (
	"encoding/json"
	"net/http"

	"github.com/thushan/olla/internal/config"
)

// uiConfigRoute is the dashboard's manifest: which metric source is live,
// which sections exist, which runtime columns to render, and the observe
// poll budget. It lives in the dashboard CIDR zone but OUTSIDE the static
// asset subtree (exact pattern beats the /internal/ui/ subtree in
// http.ServeMux), so it is registered via dashboard.RegisterRoutes extras.
const uiConfigRoute = "/internal/ui/config.json"

type uiSection struct {
	ID      string `json:"id"`
	Enabled bool   `json:"enabled"`
	// Reason explains a disabled section to the operator, not the patient:
	// "power needs metrics_source.mode=influx" is actionable, an absent
	// section is not.
	Reason string `json:"reason,omitempty"`
}

type uiMetricColumn struct {
	Key    string `json:"key"`
	Label  string `json:"label"`
	Unit   string `json:"unit"`
	Window bool   `json:"window"`
}

type uiPollIntervals struct {
	Overview  int `json:"overview"`
	Endpoints int `json:"endpoints"`
	Models    int `json:"models"`
	Observe   int `json:"observe"`
}

// UIConfig is the JSON contract of GET /internal/ui/config.json. The
// frontend falls back to an identical built-in default when this endpoint
// cannot be reached, so both copies must stay byte-equivalent in meaning.
type UIConfig struct {
	Schema               int              `json:"schema"`
	MetricsSource        string           `json:"metrics_source"`
	PollMS               uiPollIntervals  `json:"poll_ms"`
	Sections             []uiSection      `json:"sections"`
	RuntimeMetricColumns []uiMetricColumn `json:"runtime_metric_columns"`
}

func uiConfigFrom(cfg *config.Config) UIConfig {
	influx := cfg.MetricsSource.InfluxEnabled()
	mode := config.MetricsSourceModeSampler
	if influx {
		mode = config.MetricsSourceModeInflux
	}
	modelsEnabled := cfg.Discovery.ModelDiscovery.Enabled || len(cfg.ModelAliases) > 0
	powerEnabled := influx && len(cfg.MetricsSource.Influx.PowerAttribution) > 0
	gpuEnabled := influx && cfg.MetricsSource.Influx.GPUEndpoint != ""
	return UIConfig{
		Schema:        1,
		MetricsSource: mode,
		// Mirrors of the frontend stores' module constants (decision:
		// existing stores keep compile-time intervals; observe is served
		// from config because the store is new).
		PollMS: uiPollIntervals{Overview: 2000, Endpoints: 5000, Models: 10000, Observe: 10000},
		Sections: []uiSection{
			{ID: "overview", Enabled: true},
			{ID: "endpoints", Enabled: true},
			{ID: "models", Enabled: modelsEnabled},
			{ID: "power", Enabled: powerEnabled, Reason: reasonIfDisabled(powerEnabled, "requires metrics_source.mode=influx")},
			{ID: "cost", Enabled: influx, Reason: reasonIfDisabled(influx, "requires metrics_source.mode=influx")},
			{ID: "gpu", Enabled: gpuEnabled, Reason: reasonIfDisabled(gpuEnabled, "requires metrics_source.mode=influx and metrics_source.influx.gpu_endpoint")},
		},
		// Labels live here (not in the payload) so presentation changes
		// never touch the wire contract of runtime metrics.
		RuntimeMetricColumns: []uiMetricColumn{
			{Key: runtimeMetricKeyGenerationTPS, Label: "Generation", Unit: runtimeMetricUnitTokS, Window: true},
			{Key: runtimeMetricKeyPrefillTPS, Label: "Prefill", Unit: runtimeMetricUnitTokS, Window: true},
			{Key: runtimeMetricKeyCacheHitPercent, Label: "Prompt-cache hit rate", Unit: runtimeMetricUnitPercent, Window: true},
			{Key: runtimeMetricKeyKvCacheUsagePercent, Label: "KV-cache utilisation", Unit: runtimeMetricUnitPercent, Window: false},
			{Key: runtimeMetricKeyQueueDepth, Label: "Queue depth", Unit: runtimeMetricUnitCount, Window: false},
		},
	}
}

func reasonIfDisabled(enabled bool, reason string) string {
	if enabled {
		return ""
	}
	return reason
}

// uiConfigHandler serves the manifest. Static per process (config cannot
// change without a restart), so no ETag is warranted; no-cache keeps
// browsers honest about picking up a new mode after a redeploy.
func (a *Application) uiConfigHandler(w http.ResponseWriter, r *http.Request) {
	body, err := json.Marshal(uiConfigFrom(a.Config))
	if err != nil {
		http.Error(w, "config encode failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(body)
}
