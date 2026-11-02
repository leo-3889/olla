package handlers

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Gauge-derived percentages must render within [0,100]. A documented 0..1
// ratio gauge that reads 1.37 (a backend bug) x100 == 137 would otherwise
// surface "137% cache reuse" on the dashboard. All three gauge-derived
// percent metrics in influxSeries.envelope() go through clampKVReusePercent.
func TestInfluxEnvelopeGaugePercentsClampTo100(t *testing.T) {
	s := newInfluxSeries()
	const over = 1.37 // out-of-range ratio: x100 == 137
	s.last["prefix_cache_hit_ratio"] = over
	s.last["kv_cache_usage_ratio"] = over
	s.last["draft_acceptance_ratio"] = over

	env := s.envelope()

	find := func(key string) *RuntimeMetric {
		for i := range env {
			if env[i].Key == key {
				return &env[i]
			}
		}
		return nil
	}

	for _, key := range []string{
		runtimeMetricKeyCacheHitPercent,
		runtimeMetricKeyKvCacheUsagePercent,
		runtimeMetricKeySpecAcceptPercent,
	} {
		m := find(key)
		require.NotNil(t, m, "envelope must publish %s when its gauge is present", key)
		require.NotNil(t, m.Current, "%s Current must be set", key)
		assert.LessOrEqual(t, *m.Current, 100.0, "%s must clamp a 1.37 ratio gauge to <=100 (was %v)", key, *m.Current)
		assert.GreaterOrEqual(t, *m.Current, 0.0, "%s must clamp to >=0", key)
	}
}
