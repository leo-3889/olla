package handlers

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/thushan/olla/internal/app/handlers/runtime_dialect"
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

// uiPollIntervalsFrom publishes the configured dashboard poll intervals in
// milliseconds; unset (zero or negative) entries fall back to the exact
// defaults the dashboard's poll stores use, so the manifest never lies to a
// client about what cadence is actually running.
func uiPollIntervalsFrom(cfg *config.Config) uiPollIntervals {
	poll := uiPollIntervals{Overview: 5000, Endpoints: 5000, Models: 15000, Observe: 10000}
	if cfg == nil {
		return poll
	}
	p := cfg.Dashboard.Polling
	if p.OverviewInterval > 0 {
		poll.Overview = int(p.OverviewInterval / time.Millisecond)
	}
	if p.EndpointsInterval > 0 {
		poll.Endpoints = int(p.EndpointsInterval / time.Millisecond)
	}
	if p.ModelsInterval > 0 {
		poll.Models = int(p.ModelsInterval / time.Millisecond)
	}
	if p.ObserveInterval > 0 {
		poll.Observe = int(p.ObserveInterval / time.Millisecond)
	}
	return poll
}

// uiRuntimeColumnsFrom derives the endpoints-table columns from the dialects
// the configured fleet can actually sample: every opted-in endpoint resolves
// its kind (pinned runtime_metrics.kind, then endpoint type) and the union of
// those dialects' declared columns is published, de-duplicated in stable
// order. A fleet of sampleable backends therefore never advertises a column
// it can only dash, and a new dialect brings its own columns without a
// dashboard edit. An empty list (no opted-in endpoints) makes the UI fall
// back to its built-in defaults, byte-identical to the pre-registry manifest.
func uiRuntimeColumnsFrom(cfg *config.Config) []uiMetricColumn {
	kinds := make([]string, 0, 4)
	for _, ep := range cfg.Discovery.Static.Endpoints {
		if ep.RuntimeMetrics == nil || !ep.RuntimeMetrics.Enabled {
			continue
		}
		if kind := resolveRuntimeKind(ep.RuntimeMetrics.Kind, ep.Type); kind != "" {
			kinds = append(kinds, kind)
		}
	}
	var out []uiMetricColumn
	for _, col := range runtime_dialect.UnionColumns(kinds) {
		out = append(out, uiMetricColumn{Key: col.Key, Label: col.Label, Unit: col.Unit, Window: col.Windowed})
	}
	return out
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
		PollMS:        uiPollIntervalsFrom(cfg),
		Sections: []uiSection{
			{ID: "overview", Enabled: true},
			{ID: "endpoints", Enabled: true},
			{ID: "models", Enabled: modelsEnabled},
			{ID: "power", Enabled: powerEnabled, Reason: reasonIfDisabled(powerEnabled, "requires metrics_source.mode=influx")},
			{ID: "cost", Enabled: influx, Reason: reasonIfDisabled(influx, "requires metrics_source.mode=influx")},
			{ID: "gpu", Enabled: gpuEnabled, Reason: reasonIfDisabled(gpuEnabled, "requires metrics_source.mode=influx and metrics_source.influx.gpu_endpoint")},
		},
		RuntimeMetricColumns: uiRuntimeColumnsFrom(cfg),
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
