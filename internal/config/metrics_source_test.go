package config

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMetricsSourceValidate(t *testing.T) {
	tests := []struct {
		name      string
		input     MetricsSourceConfig
		wantErr   string
		wantMode  string
		wantStale time.Duration
		wantCosts string
	}{
		{name: "empty is sampler", input: MetricsSourceConfig{}, wantMode: "sampler"},
		{name: "explicit sampler", input: MetricsSourceConfig{Mode: "sampler"}, wantMode: "sampler"},
		{name: "unknown mode rejected", input: MetricsSourceConfig{Mode: "graphite"}, wantErr: `unknown mode "graphite"`},
		{
			name: "influx complete fills defaults",
			input: MetricsSourceConfig{Mode: "influx", Influx: InfluxSourceConfig{
				URL: "http://influxdb:8086", Org: "org", TokenEnv: "INFLUX_TOKEN", Bucket: "llm/metrics",
				PowerAttribution: map[string]string{"ep": "plug"},
			}},
			wantMode: "influx", wantStale: 60 * time.Second, wantCosts: "llm/metrics",
		},
		{name: "influx missing url", input: MetricsSourceConfig{Mode: "influx", Influx: InfluxSourceConfig{Org: "o", TokenEnv: "T", Bucket: "b"}}, wantErr: "influx: url is required"},
		{name: "influx missing org", input: MetricsSourceConfig{Mode: "influx", Influx: InfluxSourceConfig{URL: "u", TokenEnv: "T", Bucket: "b"}}, wantErr: "influx: org is required"},
		{name: "influx missing token_env", input: MetricsSourceConfig{Mode: "influx", Influx: InfluxSourceConfig{URL: "u", Org: "o", Bucket: "b"}}, wantErr: "influx: token_env is required"},
		{name: "influx missing bucket", input: MetricsSourceConfig{Mode: "influx", Influx: InfluxSourceConfig{URL: "u", Org: "o", TokenEnv: "T"}}, wantErr: "influx: bucket is required"},
		{
			name:     "explicit costs bucket and stale kept",
			input:    MetricsSourceConfig{Mode: "influx", Influx: InfluxSourceConfig{URL: "u", Org: "o", TokenEnv: "T", Bucket: "b", CostsBucket: "costs", StaleAfter: 90 * time.Second}},
			wantMode: "influx", wantStale: 90 * time.Second, wantCosts: "costs",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := tc.input
			err := cfg.Validate()
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.wantMode, cfg.Mode)
			if tc.wantMode == "influx" {
				assert.Equal(t, tc.wantStale, cfg.Influx.StaleAfter)
				assert.Equal(t, tc.wantCosts, cfg.Influx.CostsBucket)
			}
		})
	}
}

func TestConfigValidateSurfacesMetricsSource(t *testing.T) {
	cfg := DefaultConfig()
	cfg.MetricsSource.Mode = "graphite"
	err := cfg.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "metrics_source config invalid")
}
