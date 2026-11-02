package config

import (
	"fmt"
	"time"
)

// MetricsSourceConfig selects where the admin dashboard's runtime metric
// tiles get their numbers. "sampler" (default, also the empty value) keeps
// olla's own engine scraper; "influx" reads the canonical series that the
// ollametrics stack writes to InfluxDB, so the dashboard, ollametrics and
// Grafana show one truth. Routing truth (health, models, traffic) always
// stays in olla - only the engine-runtime tiles move.
type MetricsSourceConfig struct {
	Mode   string             `yaml:"mode,omitempty"` // "", "sampler" or "influx"
	Influx InfluxSourceConfig `yaml:"influx,omitempty"`
}

// InfluxSourceConfig points the influx provider at the ollametrics database.
// The token is referenced by ENVIRONMENT VARIABLE NAME (token_env), never
// inline: the dashboard config file is mounted read-only into the container
// and gets copied around for backups, while the secret stays in .env.
type InfluxSourceConfig struct {
	// PowerAttribution maps olla endpoint names to the power_target_id of
	// the meter that measures that endpoint's box. Power is attributed per
	// meter, never guessed from a device list.
	PowerAttribution map[string]string `yaml:"power_attribution,omitempty"`
	URL              string            `yaml:"url,omitempty"`          // e.g. http://influxdb:8086 (docker-internal DNS)
	Org              string            `yaml:"org,omitempty"`          // Influx org name
	TokenEnv         string            `yaml:"token_env,omitempty"`    // name of the env var holding the API token
	Bucket           string            `yaml:"bucket,omitempty"`       // runtime/power bucket, e.g. llm/metrics
	CostsBucket      string            `yaml:"costs_bucket,omitempty"` // cost rollups, e.g. llm/costs (default: Bucket)
	// GPUEndpoints are the Influx endpoint tags carrying GPU device series
	// (one per GPU exporter target, e.g. [radiance-gpus, radiance-gpus-128]).
	// Grouping is endpoint+gpu because device tags collide across boxes.
	// Empty disables the GPU section.
	GPUEndpoints []string `yaml:"gpu_endpoints,omitempty"`
	// StaleAfter marks tiles stale once the newest Influx point for an
	// endpoint is older than this (default 60s).
	StaleAfter time.Duration `yaml:"stale_after,omitempty"`
	// TokenLookback bounds the lifetime-token window on the endpoints table:
	// per endpoint and PER COUNTER FIELD, the INTEGRAL of the llm_runtime token
	// counters over this window, integrated per UTC day so a restart cannot
	// corrupt the delta (default 180d = the llm/metrics retention). Generation
	// (generation_tokens_total) and prefill (prompt_tokens_total) are summed
	// SEPARATELY into the two token columns; cached_prompt_tokens_total is
	// summed the same way only as the numerator of the KV-reuse ratio on engines
	// that publish no prefix_cache_hit_ratio gauge (its denominator is the same
	// window's prefill sum). The cost rollups are deliberately NOT the source:
	// they are a lossy downstream derivation, and a dropped or incomplete cost
	// interval silently shrinks the lifetime totals (measured ~9% under the true
	// counter integral over 24h). Rows are per endpoint id: an endpoint rename
	// is NOT aliased, pre-rename rows stay under the old id (honest split).
	TokenLookback time.Duration `yaml:"token_lookback,omitempty"`
}

const (
	MetricsSourceModeSampler = "sampler"
	MetricsSourceModeInflux  = "influx"

	defaultInfluxStaleAfter    = 60 * time.Second
	defaultInfluxTokenLookback = 180 * 24 * time.Hour
)

// InfluxEnabled reports whether metrics should be read from InfluxDB. A
// misconfigured or token-less influx setup falls back to the sampler at
// startup (see the application wiring); this method only reports intent.
func (m MetricsSourceConfig) InfluxEnabled() bool {
	return m.Mode == MetricsSourceModeInflux
}

// Validate whitelists the mode, requires the connection fields when influx
// is selected, and fills defaults. Defaults are applied here because
// Validate runs on every load path including tests.
func (m *MetricsSourceConfig) Validate() error {
	switch m.Mode {
	case "", MetricsSourceModeSampler, MetricsSourceModeInflux:
	default:
		return fmt.Errorf("unknown mode %q (want sampler or influx)", m.Mode)
	}
	if m.Mode == "" {
		m.Mode = MetricsSourceModeSampler
	}
	if m.Mode != MetricsSourceModeInflux {
		return nil
	}
	for _, f := range []struct {
		name, value string
	}{
		{"url", m.Influx.URL},
		{"org", m.Influx.Org},
		{"token_env", m.Influx.TokenEnv},
		{"bucket", m.Influx.Bucket},
	} {
		if f.value == "" {
			return fmt.Errorf("influx: %s is required when mode is influx", f.name)
		}
	}
	if m.Influx.StaleAfter <= 0 {
		m.Influx.StaleAfter = defaultInfluxStaleAfter
	}
	if m.Influx.CostsBucket == "" {
		m.Influx.CostsBucket = m.Influx.Bucket
	}
	if m.Influx.TokenLookback < 0 {
		return fmt.Errorf("influx: token_lookback must be positive, got %s", m.Influx.TokenLookback)
	}
	if m.Influx.TokenLookback == 0 {
		m.Influx.TokenLookback = defaultInfluxTokenLookback
	}
	return nil
}
