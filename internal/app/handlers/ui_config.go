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
	ID string `json:"id"`
	// Reason explains a disabled section to the operator, not the patient:
	// "power needs metrics_source.mode=influx" is actionable, an absent
	// section is not.
	Reason  string `json:"reason,omitempty"`
	Enabled bool   `json:"enabled"`
}

type uiMetricColumn struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	Unit  string `json:"unit"`
	// Help is the column's explanatory header text (window, source,
	// aggregate, known ceiling), rendered as the header's native tooltip.
	// The dashboard's built-in defaults mirror these strings.
	Help   string `json:"help,omitempty"`
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
	MetricsSource string      `json:"metrics_source"`
	Sections      []uiSection `json:"sections"`
	// RuntimeMetricColumns is the per-ENGINE column union (dialect registry).
	RuntimeMetricColumns []uiMetricColumn `json:"runtime_metric_columns"`
	// StaticMetricColumns are endpoints-table columns that exist OUTSIDE
	// the dialect union because they are not engine-reported: the
	// lifetime-tokens column comes from cost rollups, so no dialect owns
	// it. Published so config.json is the complete column contract.
	StaticMetricColumns []uiMetricColumn `json:"static_metric_columns"`
	PollMS              uiPollIntervals  `json:"poll_ms"`
	Schema              int              `json:"schema"`
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
		out = append(out, uiMetricColumn{Key: col.Key, Label: col.Label, Unit: col.Unit, Help: col.Help, Window: col.Windowed})
	}
	return out
}

// uiStaticColumns is the static (non-dialect) column list: the lifetime
// token production split by counter plus the KV-cache-reuse ratio. window=false
// for all three - the tokens are lifetime sums over
// metrics_source.influx.token_lookback and the reuse ratio is a gauge / a
// window ratio, never a polled window average/maximum. They are static (not
// dialect columns) because the dialect union publishes per-scrape runtime
// signals; these three exist for every endpoint of every kind, filled by the
// metrics source, not by a dialect. The columns render in every mode -
// sampler mode simply dashes every cell (all three reads are "not known"
// there), which is the honest disclosure that no lifetime source is wired.
// kv_cache_reuse_percent is a PERCENT (0..100, the same unit the runtime
// cache tiles use) and its basis is per-engine (engine gauge vs counter
// ratio) - the payload field comment carries the disclosure the UI repeats.
func uiStaticColumns(tokenWindow string) []uiMetricColumn {
	if tokenWindow == "" {
		tokenWindow = "180d" // the config default (defaultInfluxTokenLookback)
	}
	windowHelp := "Window: the effective token lookback " + tokenWindow +
		" = min(configured token_lookback, the bucket's measured retention). Source: per-UTC-day integrals of the engine token counter via ollametrics/Influx - influx mode only; sampler mode dashes every cell. A missing day window shrinks the sum: the cell tooltip then says LOWER BOUND. Dash = no data, never a fabricated 0."
	return []uiMetricColumn{
		{Key: "generation_tokens", Label: "Tokens " + tokenWindow + " (Gen)", Unit: runtimeMetricUnitCount,
			Help: windowHelp + " Counter: generation_tokens_total.", Window: false},
		{Key: "prefill_tokens", Label: "Tokens " + tokenWindow + " (Prefill)", Unit: runtimeMetricUnitCount,
			Help: windowHelp + " Counter: prompt_tokens_total (llama.cpp-family counts COMPUTED prompt tokens).", Window: false},
		{Key: "kv_cache_reuse_percent", Label: "KV cache reuse", Unit: runtimeMetricUnitPercent,
			Help: "Per-engine basis, named in the cell tooltip: the engine's prefix-cache gauge (measured since engine boot, resets on every restart) or the ratio cached/(cached+computed) prompt tokens over the token window. Source: engine metrics via ollametrics/Influx; sampler mode dashes. Dash = the engine exposes no cache-reuse signal.", Window: false},
	}
}

func uiConfigFrom(cfg *config.Config, effectiveMode, tokenWindow string) UIConfig {
	// Effective mode wins: an influx intent that fell back at boot (missing
	// token env) must not advertise influx sections the process cannot serve.
	mode := effectiveMode
	if mode == "" {
		mode = cfg.MetricsSource.Mode
	}
	if mode == "" {
		mode = config.MetricsSourceModeSampler
	}
	influx := mode == config.MetricsSourceModeInflux
	modelsEnabled := cfg.Discovery.ModelDiscovery.Enabled || len(cfg.ModelAliases) > 0
	powerEnabled := influx && len(cfg.MetricsSource.Influx.PowerAttribution) > 0
	gpuEnabled := influx && len(cfg.MetricsSource.Influx.GPUEndpoints) > 0
	return UIConfig{
		Schema:        1,
		MetricsSource: mode,
		PollMS:        uiPollIntervalsFrom(cfg),
		Sections: []uiSection{
			{ID: "overview", Enabled: true},
			{ID: "endpoints", Enabled: true},
			{ID: "models", Enabled: modelsEnabled},
			{ID: "settings", Enabled: true},
			{ID: "power", Enabled: powerEnabled, Reason: reasonIfDisabled(powerEnabled, "requires metrics_source.mode=influx")},
			{ID: "cost", Enabled: influx, Reason: reasonIfDisabled(influx, "requires metrics_source.mode=influx")},
			{ID: "gpu", Enabled: gpuEnabled, Reason: reasonIfDisabled(gpuEnabled, "requires metrics_source.mode=influx and metrics_source.influx.gpu_endpoints")},
		},
		RuntimeMetricColumns: uiRuntimeColumnsFrom(cfg),
		StaticMetricColumns:  uiStaticColumns(tokenWindow),
	}
}

// tokenWindowLabel publishes the EFFECTIVE lifetime-token window (T2): the
// influx provider caps the configured token_lookback by the bucket
// retention it read at boot, so the "Tokens Nd" header never claims a
// window the data cannot cover (measured: a "180d" label over a 90d
// retention). Providers that cannot answer keep the configured default.
func (a *Application) tokenWindowLabel() string {
	if tw, ok := a.runtimeMetrics.(interface{ TokenWindowLabel() string }); ok {
		if label := tw.TokenWindowLabel(); label != "" {
			return label
		}
	}
	if a.Config != nil && a.Config.MetricsSource.Influx.TokenLookback > 0 {
		return tokenWindowLabel(a.Config.MetricsSource.Influx.TokenLookback)
	}
	return "180d"
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
	body, err := json.Marshal(uiConfigFrom(a.Config, a.metricsSourceMode, a.tokenWindowLabel()))
	if err != nil {
		http.Error(w, "config encode failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(body)
}
