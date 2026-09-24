package handlers

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/thushan/olla/internal/core/domain"
)

func TestRuntimeMetricsVLLMEpochAggregates(t *testing.T) {
	t.Parallel()
	s := newEndpointRuntimeMetricsSampler(nil, nil)
	ep := &domain.Endpoint{Name: "vllm", URLString: "http://vllm", Type: "vllm", Status: domain.StatusHealthy}
	start := time.Date(2026, 9, 24, 14, 0, 0, 0, time.UTC)
	s.record(ep.URLString, start, parseRuntimeCounters("vllm", `
vllm:generation_tokens_total 0
vllm:request_decode_time_seconds_sum 0
vllm:request_prefill_kv_computed_tokens_sum 0
vllm:request_prefill_time_seconds_sum 0
vllm:prefix_cache_hits_total 0
vllm:prefix_cache_queries_total 0
vllm:kv_cache_usage_ratio 0.37
`))
	s.record(ep.URLString, start.Add(10*time.Second), parseRuntimeCounters("vllm", `
vllm:generation_tokens_total 700
vllm:request_decode_time_seconds_sum 10
vllm:request_prefill_kv_computed_tokens_sum 964
vllm:request_prefill_time_seconds_sum 1.92
vllm:prefix_cache_hits_total 981
vllm:prefix_cache_queries_total 1000
vllm:kv_cache_usage_ratio 0.42
`))
	got := s.Snapshot(ep)
	require.NotNil(t, got)
	assert.Equal(t, start, got.AvailableSince)
	require.NotNil(t, got.GenerationTPS)
	assert.InDelta(t, 70, *got.GenerationTPS.Current, .001)
	assert.InDelta(t, 70, *got.GenerationTPS.Avg, .001)
	assert.InDelta(t, 70, *got.GenerationTPS.Max, .001)
	require.NotNil(t, got.PrefillTPS)
	assert.InDelta(t, 964/1.92, *got.PrefillTPS.Current, .001)
	require.NotNil(t, got.CacheHitPercent)
	assert.InDelta(t, 98.1, *got.CacheHitPercent.Current, .001)
	require.NotNil(t, got.KVCacheUsagePercent)
	assert.InDelta(t, 42, *got.KVCacheUsagePercent, .001)
}

func TestRuntimeMetricsHalogenCacheUsesTokenWeightedDemand(t *testing.T) {
	t.Parallel()
	s := newEndpointRuntimeMetricsSampler(nil, nil)
	ep := &domain.Endpoint{Name: "halogen", URLString: "http://halogen", Type: "halogen", Status: domain.StatusHealthy}
	at := time.Now().UTC()
	s.record(ep.URLString, at, parseRuntimeCounters("llamacpp", `
llamacpp:tokens_predicted_total 0
llamacpp:tokens_predicted_seconds_total 0
llamacpp:prompt_tokens_total 0
llamacpp:prompt_seconds_total 0
halogen:prompt_tokens_cached_total 0
`))
	s.record(ep.URLString, at.Add(time.Second), parseRuntimeCounters("llamacpp", `
llamacpp:tokens_predicted_total 33
llamacpp:tokens_predicted_seconds_total 1
llamacpp:prompt_tokens_total 20
llamacpp:prompt_seconds_total 5
halogen:prompt_tokens_cached_total 981
`))
	got := s.Snapshot(ep)
	require.NotNil(t, got.CacheHitPercent)
	// Hit rate is cached / (cached + computed): 981 / (981 + 20) = 98.0%.
	assert.InDelta(t, 98.0, *got.CacheHitPercent.Current, .01)
	assert.InDelta(t, 33, *got.GenerationTPS.Current, .001)
	assert.InDelta(t, 4, *got.PrefillTPS.Current, .001)
}

// TestRuntimeMetricsHonoursPerEndpointTimeout verifies that a per-endpoint
// runtime_metrics.timeout larger than the sampler's old 3s client cap is
// honoured: a backend that takes ~4s to respond is scraped successfully when
// the endpoint's timeout is 5s. The per-scrape context (not a fixed
// client-level timeout) bounds the request, so a configured timeout above the
// former cap now works.
func TestRuntimeMetricsHonoursPerEndpointTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(4 * time.Second):
		case <-r.Context().Done():
			return
		}
		_, _ = w.Write([]byte("llamacpp:tokens_predicted_total 10\nllamacpp:tokens_predicted_seconds_total 1\n"))
	}))
	defer server.Close()

	ep := &domain.Endpoint{Name: "halogen-slow", URLString: server.URL, Type: "halogen", Status: domain.StatusHealthy, RuntimeMetricsEnabled: true, RuntimeMetricsInterval: time.Nanosecond, RuntimeMetricsTimeout: 5 * time.Second}
	s := newEndpointRuntimeMetricsSampler(&mockStatusEndpointRepository{endpoints: []*domain.Endpoint{ep}}, nil)
	s.Refresh(t.Context())
	// A single scrape yields no throughput (rates need counter movement across
	// two samples), so we assert the scrape itself succeeded: a non-nil, fresh
	// snapshot. Under the old fixed 3s client timeout this 4s backend would
	// time out and produce no snapshot at all.
	got := s.Snapshot(ep)
	require.NotNil(t, got)
	assert.False(t, got.Stale)
}

func TestRuntimeMetricsCounterResetStartsNewAvailabilityEpoch(t *testing.T) {
	t.Parallel()
	s := newEndpointRuntimeMetricsSampler(nil, nil)
	at := time.Now().UTC()
	first := parseRuntimeCounters("vllm", "vllm:generation_tokens_total 100\nvllm:request_decode_time_seconds_sum 1")
	s.record("x", at, first)
	s.record("x", at.Add(time.Second), parseRuntimeCounters("vllm", "vllm:generation_tokens_total 5\nvllm:request_decode_time_seconds_sum 0.1"))
	ep := &domain.Endpoint{URLString: "x", Type: "vllm", Status: domain.StatusHealthy}
	got := s.Snapshot(ep)
	require.NotNil(t, got)
	assert.Equal(t, at.Add(time.Second), got.AvailableSince)
	assert.Nil(t, got.GenerationTPS)
}

func TestRuntimeMetricsPrometheusOutputOmitsUnsupportedValues(t *testing.T) {
	t.Parallel()
	s := newEndpointRuntimeMetricsSampler(nil, nil)
	ep := &domain.Endpoint{Name: "vllm", URLString: "x", Type: "vllm", Status: domain.StatusHealthy}
	at := time.Now()
	s.record("x", at, parseRuntimeCounters("vllm", "vllm:generation_tokens_total 0\nvllm:request_decode_time_seconds_sum 0"))
	s.record("x", at.Add(time.Second), parseRuntimeCounters("vllm", "vllm:generation_tokens_total 70\nvllm:request_decode_time_seconds_sum 1"))
	var b strings.Builder
	writeEndpointRuntimePrometheusMetrics(&b, []*domain.Endpoint{ep}, s)
	assert.Contains(t, b.String(), `olla_endpoint_generation_tokens_per_second{endpoint="vllm"} 70`)
	assert.NotContains(t, b.String(), `olla_endpoint_prefill_tokens_per_second{endpoint="vllm"}`)
}

func TestRuntimeMetricsRefreshScrapesHalogenWithConfiguredHeadersAndMarksFailuresStale(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/metrics", r.URL.Path)
		assert.Equal(t, "Bearer test-token", r.Header.Get("Authorization"))
		assert.Equal(t, "metrics", r.Header.Get("X-Olla-Test"))
		calls++
		if calls == 3 {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		if calls == 1 {
			_, _ = w.Write([]byte("llamacpp:tokens_predicted_total 0\nllamacpp:tokens_predicted_seconds_total 0\n"))
			return
		}
		_, _ = w.Write([]byte("llamacpp:tokens_predicted_total 70\nllamacpp:tokens_predicted_seconds_total 1\n"))
	}))
	defer server.Close()

	ep := &domain.Endpoint{Name: "halogen-395", Type: "openai-compatible", URLString: server.URL, Status: domain.StatusHealthy, RuntimeMetricsEnabled: true, RuntimeMetricsInterval: time.Nanosecond, Headers: map[string]string{"X-Olla-Test": "metrics"}, AuthHeaderName: "Authorization", AuthHeaderValue: "Bearer test-token"}
	s := newEndpointRuntimeMetricsSampler(&mockStatusEndpointRepository{endpoints: []*domain.Endpoint{ep}}, nil)
	s.Refresh(t.Context())
	s.Refresh(t.Context())
	got := s.Snapshot(ep)
	require.NotNil(t, got)
	require.NotNil(t, got.GenerationTPS)
	assert.InDelta(t, 70, *got.GenerationTPS.Current, .001)
	s.Refresh(t.Context())
	got = s.Snapshot(ep)
	require.NotNil(t, got)
	assert.True(t, got.Stale)
}

func TestRuntimeMetricsChangeInvalidatesStatusETags(t *testing.T) {
	current := 70.0
	changed := 71.0
	base := EndpointRuntimeMetrics{SampledAt: time.Date(2026, 9, 24, 14, 0, 0, 0, time.UTC), AvailableSince: time.Date(2026, 9, 24, 13, 0, 0, 0, time.UTC), GenerationTPS: &RuntimeThroughput{Current: &current}}
	updated := base
	updated.GenerationTPS = &RuntimeThroughput{Current: &changed}

	endpointA := EndpointStatusResponse{Endpoints: []EndpointSummary{{ID: "ep", RuntimeMetrics: &base}}}
	endpointB := EndpointStatusResponse{Endpoints: []EndpointSummary{{ID: "ep", RuntimeMetrics: &updated}}}
	assert.NotEqual(t, hashEndpointStatusResponse(&endpointA), hashEndpointStatusResponse(&endpointB))

	statusA := StatusResponse{Endpoints: []EndpointResponse{{ID: "ep", RuntimeMetrics: &base}}}
	statusB := StatusResponse{Endpoints: []EndpointResponse{{ID: "ep", RuntimeMetrics: &updated}}}
	assert.NotEqual(t, hashStatusResponse(&statusA), hashStatusResponse(&statusB))
}

// TestRuntimeMetricsPrometheusOutputSkipsUncoveredEndpoints verifies that
// endpoints with no snapshot (offline, not opted in, or never sampled) are
// skipped by the Prometheus writer instead of panicking on a nil snapshot.
func TestRuntimeMetricsPrometheusOutputSkipsUncoveredEndpoints(t *testing.T) {
	t.Parallel()
	s := newEndpointRuntimeMetricsSampler(nil, nil)
	at := time.Now()
	s.record("x", at, parseRuntimeCounters("vllm", "vllm:generation_tokens_total 0\nvllm:request_decode_time_seconds_sum 0"))
	s.record("x", at.Add(time.Second), parseRuntimeCounters("vllm", "vllm:generation_tokens_total 70\nvllm:request_decode_time_seconds_sum 1"))
	endpoints := []*domain.Endpoint{
		{Name: "vllm", URLString: "x", Type: "vllm", Status: domain.StatusHealthy},
		{Name: "offline-ep", URLString: "y", Type: "vllm", Status: domain.StatusOffline},
		{Name: "not-opted-in", URLString: "z", Type: "ollama", Status: domain.StatusHealthy},
	}
	var b strings.Builder
	writeEndpointRuntimePrometheusMetrics(&b, endpoints, s)
	assert.Contains(t, b.String(), `olla_endpoint_generation_tokens_per_second{endpoint="vllm"} 70`)
	assert.NotContains(t, b.String(), `endpoint="offline-ep"`)
	assert.NotContains(t, b.String(), `endpoint="not-opted-in"`)
}

// TestRuntimeMetricsVLLMKVUsagePerCNormalised verifies that vLLM's
// kv_cache_usage_perc gauge (0..1, e.g. 0.26) is normalised to a percentage.
func TestRuntimeMetricsVLLMKVUsagePerCNormalised(t *testing.T) {
	t.Parallel()
	s := newEndpointRuntimeMetricsSampler(nil, nil)
	at := time.Now()
	s.record("x", at, parseRuntimeCounters("vllm", "vllm:kv_cache_usage_perc 0.26068376068376065"))
	ep := &domain.Endpoint{URLString: "x", Type: "vllm", Status: domain.StatusHealthy}
	got := s.Snapshot(ep)
	require.NotNil(t, got)
	require.NotNil(t, got.KVCacheUsagePercent)
	assert.InDelta(t, 26.068376068376065, *got.KVCacheUsagePercent, .001)
}

// TestRuntimeMetricsWindowAveragesAndMaxEvictsOldSamples verifies that avg
// and max cover only the 10-minute reporting window: samples older than the
// window are evicted, and current is always the latest sample.
func TestRuntimeMetricsWindowAveragesAndMaxEvictsOldSamples(t *testing.T) {
	t.Parallel()
	s := newEndpointRuntimeMetricsSampler(nil, nil)
	ep := &domain.Endpoint{URLString: "x", Type: "vllm", Status: domain.StatusHealthy}
	at := time.Date(2026, 9, 24, 14, 0, 0, 0, time.UTC)
	// A slow sample (10 tok/s) that must age out of the window.
	s.record("x", at, parseRuntimeCounters("vllm", "vllm:generation_tokens_total 0\nvllm:request_decode_time_seconds_sum 0"))
	s.record("x", at.Add(time.Second), parseRuntimeCounters("vllm", "vllm:generation_tokens_total 10\nvllm:request_decode_time_seconds_sum 1"))
	// A fast sample (100 tok/s) 11 minutes later: outside the window of the
	// first sample, so the window holds only this one.
	s.record("x", at.Add(11*time.Minute), parseRuntimeCounters("vllm", "vllm:generation_tokens_total 110\nvllm:request_decode_time_seconds_sum 2"))
	got := s.Snapshot(ep)
	require.NotNil(t, got)
	require.NotNil(t, got.GenerationTPS)
	assert.InDelta(t, 100, *got.GenerationTPS.Current, .001)
	assert.InDelta(t, 100, *got.GenerationTPS.Avg, .001)
	assert.InDelta(t, 100, *got.GenerationTPS.Max, .001)
}
