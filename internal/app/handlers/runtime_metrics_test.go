package handlers

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/thushan/olla/internal/core/domain"
)

// radianceT0/radianceT1 are trimmed captures of the radiance exposition
// shape: a `# TYPE` line per metric, a single logical engine="0" series each
// (TP exposes one logical engine), and a two-series vLLM-compat KV gauge.
const radianceT0 = `
# TYPE radiance:decode_tokens_total counter
radiance:decode_tokens_total{engine="0"} 0
# TYPE radiance:engine_step_seconds_decode_total counter
radiance:engine_step_seconds_decode_total{engine="0"} 0
# TYPE radiance:prefill_tokens_total counter
radiance:prefill_tokens_total{engine="0"} 0
# TYPE radiance:engine_step_seconds_prefill_total counter
radiance:engine_step_seconds_prefill_total{engine="0"} 0
# TYPE radiance:attn_cached_tokens_total counter
radiance:attn_cached_tokens_total{engine="0"} 0
# TYPE radiance:linear_checkpoints_written_total counter
radiance:linear_checkpoints_written_total{engine="0"} 0
# TYPE radiance:decode_tokens_per_second gauge
radiance:decode_tokens_per_second{engine="0"} 288.5
# TYPE radiance:prefill_tokens_per_second gauge
radiance:prefill_tokens_per_second{engine="0"} 1893.2
# TYPE radiance:linear_prefix_cache_hit_rate gauge
radiance:linear_prefix_cache_hit_rate{engine="0"} 0.76
# TYPE vllm:kv_cache_usage_perc gauge
vllm:kv_cache_usage_perc{engine="0"} 0.3
vllm:kv_cache_usage_perc{engine="1"} 0.35
# TYPE radiance:http_queue_depth gauge
radiance:http_queue_depth{engine="0"} 4
# TYPE vllm:num_requests_running gauge
vllm:num_requests_running{engine="0"} 2
# TYPE radiance:draft_acceptance_ratio gauge
radiance:draft_acceptance_ratio{engine="0"} 0.5
`

const radianceT1 = `
# TYPE radiance:decode_tokens_total counter
radiance:decode_tokens_total{engine="0"} 700
# TYPE radiance:engine_step_seconds_decode_total counter
radiance:engine_step_seconds_decode_total{engine="0"} 10
# TYPE radiance:prefill_tokens_total counter
radiance:prefill_tokens_total{engine="0"} 964
# TYPE radiance:engine_step_seconds_prefill_total counter
radiance:engine_step_seconds_prefill_total{engine="0"} 1.92
# TYPE radiance:attn_cached_tokens_total counter
radiance:attn_cached_tokens_total{engine="0"} 981
# TYPE radiance:linear_checkpoints_written_total counter
radiance:linear_checkpoints_written_total{engine="0"} 1000
# TYPE radiance:decode_tokens_per_second gauge
radiance:decode_tokens_per_second{engine="0"} 301.5
# TYPE radiance:prefill_tokens_per_second gauge
radiance:prefill_tokens_per_second{engine="0"} 1901.7
# TYPE radiance:linear_prefix_cache_hit_rate gauge
radiance:linear_prefix_cache_hit_rate{engine="0"} 0.78
# TYPE vllm:kv_cache_usage_perc gauge
vllm:kv_cache_usage_perc{engine="0"} 0.4
vllm:kv_cache_usage_perc{engine="1"} 0.42
# TYPE radiance:http_queue_depth gauge
radiance:http_queue_depth{engine="0"} 7
# TYPE vllm:num_requests_running gauge
vllm:num_requests_running{engine="0"} 3
# TYPE radiance:draft_acceptance_ratio gauge
radiance:draft_acceptance_ratio{engine="0"} 0.55
`

func TestRuntimeMetricsVLLMEpochAggregates(t *testing.T) {
	t.Parallel()
	s := newEndpointRuntimeMetricsSampler(nil, nil)
	ep := &domain.Endpoint{Name: "vllm", URLString: "http://vllm", Type: "vllm", Status: domain.StatusHealthy, RuntimeMetricsEnabled: true}
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
	require.NotNil(t, envMetric(t, got, runtimeMetricKeyGenerationTPS))
	assert.InDelta(t, 70, *envNum(t, got, runtimeMetricKeyGenerationTPS, runtimeMetricPartCurrent), .001)
	assert.InDelta(t, 70, *envNum(t, got, runtimeMetricKeyGenerationTPS, runtimeMetricPartAvg), .001)
	assert.InDelta(t, 70, *envNum(t, got, runtimeMetricKeyGenerationTPS, runtimeMetricPartMax), .001)
	require.NotNil(t, envMetric(t, got, runtimeMetricKeyPrefillTPS))
	assert.InDelta(t, 964/1.92, *envNum(t, got, runtimeMetricKeyPrefillTPS, runtimeMetricPartCurrent), .001)
	require.NotNil(t, envMetric(t, got, runtimeMetricKeyCacheHitPercent))
	assert.InDelta(t, 98.1, *envNum(t, got, runtimeMetricKeyCacheHitPercent, runtimeMetricPartCurrent), .001)
	require.NotNil(t, envNum(t, got, runtimeMetricKeyKvCacheUsagePercent, runtimeMetricPartCurrent))
	assert.InDelta(t, 42, *envNum(t, got, runtimeMetricKeyKvCacheUsagePercent, runtimeMetricPartCurrent), .001)
}

func TestRuntimeMetricsHalogenCacheUsesTokenWeightedDemand(t *testing.T) {
	t.Parallel()
	s := newEndpointRuntimeMetricsSampler(nil, nil)
	ep := &domain.Endpoint{Name: "halogen", URLString: "http://halogen", Type: "halogen", Status: domain.StatusHealthy, RuntimeMetricsEnabled: true}
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
	require.NotNil(t, envMetric(t, got, runtimeMetricKeyCacheHitPercent))
	// Hit rate is cached / (cached + computed): 981 / (981 + 20) = 98.0%.
	assert.InDelta(t, 98.0, *envNum(t, got, runtimeMetricKeyCacheHitPercent, runtimeMetricPartCurrent), .01)
	assert.InDelta(t, 33, *envNum(t, got, runtimeMetricKeyGenerationTPS, runtimeMetricPartCurrent), .001)
	assert.InDelta(t, 4, *envNum(t, got, runtimeMetricKeyPrefillTPS, runtimeMetricPartCurrent), .001)
	// Radiance-only saturation/spec fields stay nil for other kinds.
	assert.Nil(t, envNum(t, got, runtimeMetricKeyQueueDepth, runtimeMetricPartCurrent))
	assert.Nil(t, envNum(t, got, runtimeMetricKeyRequestsRunning, runtimeMetricPartCurrent))
	assert.Nil(t, envMetric(t, got, runtimeMetricKeySpecAcceptPercent))
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
	ep := &domain.Endpoint{URLString: "x", Type: "vllm", Status: domain.StatusHealthy, RuntimeMetricsEnabled: true}
	got := s.Snapshot(ep)
	require.NotNil(t, got)
	assert.Equal(t, at.Add(time.Second), got.AvailableSince)
	assert.Nil(t, envMetric(t, got, runtimeMetricKeyGenerationTPS))
}

// TestRuntimeMetricsETagIgnoresSampleTimestampsButTracksValues pins the O2
// contract: re-sampling an endpoint whose values did not move must not change
// either status ETag (SampledAt/AvailableSince are excluded), while any
// rendered value - or the stale flag - must change both.
func TestRuntimeMetricsETagIgnoresSampleTimestampsButTracksValues(t *testing.T) {
	t0 := time.Date(2026, 9, 24, 13, 0, 0, 0, time.UTC)
	t1 := time.Date(2026, 9, 24, 14, 0, 0, 0, time.UTC)
	gen := func(current *float64) RuntimeMetric {
		return RuntimeMetric{Key: runtimeMetricKeyGenerationTPS, Unit: runtimeMetricUnitTokS, Current: current}
	}
	seven, eight := 70.0, 71.0
	zero := 0.0
	base := EndpointRuntimeMetrics{SampledAt: t1, AvailableSince: t0, Source: runtimeMetricsSourceSampler, Metrics: []RuntimeMetric{gen(&seven)}}
	hashes := func(m *EndpointRuntimeMetrics) (string, string) {
		endpoints := EndpointStatusResponse{Endpoints: []EndpointSummary{{ID: "ep", RuntimeMetrics: m}}}
		status := StatusResponse{Endpoints: []EndpointResponse{{ID: "ep", RuntimeMetrics: m}}}
		return hashEndpointStatusResponse(&endpoints), hashStatusResponse(&status)
	}
	resampled := base
	resampled.SampledAt = t1.Add(10 * time.Second)
	resampled.AvailableSince = t0.Add(time.Minute)
	updated := base
	updated.Metrics = []RuntimeMetric{gen(&eight)}
	staled := base
	staled.Stale = true
	six := 6.0
	withQueue := base
	withQueue.Metrics = append(append([]RuntimeMetric{}, base.Metrics...), RuntimeMetric{Key: runtimeMetricKeyQueueDepth, Unit: runtimeMetricUnitCount, Current: &six})
	sourceFlip := base
	sourceFlip.Source = runtimeMetricsSourceInflux
	zeroed := base
	zeroed.Metrics = []RuntimeMetric{gen(&zero)}
	nilCleared := base
	nilCleared.Metrics = []RuntimeMetric{gen(nil)}
	noteFlip := base
	noteFlip.Metrics = []RuntimeMetric{gen(&seven)}
	noteFlip.Metrics[0].Note = runtimeMetricNoteSamplerWindow
	scopeFlip := base
	scopeFlip.Metrics = []RuntimeMetric{gen(&seven)}
	scopeFlip.Metrics[0].Scope = runtimeMetricScopeEngine

	baseE, baseS := hashes(&base)
	resampledE, resampledS := hashes(&resampled)
	assert.Equal(t, baseE, resampledE, "timestamp-only change must not churn the /internal/status/endpoints ETag")
	assert.Equal(t, baseS, resampledS, "timestamp-only change must not churn the /internal/status ETag")
	for name, mutate := range map[string]func(m *EndpointRuntimeMetrics){
		"updated":   func(m *EndpointRuntimeMetrics) { *m = updated },
		"staled":    func(m *EndpointRuntimeMetrics) { *m = staled },
		"withQueue": func(m *EndpointRuntimeMetrics) { *m = withQueue },
		"source":    func(m *EndpointRuntimeMetrics) { *m = sourceFlip },
		"zeroed":    func(m *EndpointRuntimeMetrics) { *m = zeroed },
		"nilValue":  func(m *EndpointRuntimeMetrics) { *m = nilCleared },
		"note":      func(m *EndpointRuntimeMetrics) { *m = noteFlip },
		"scope":     func(m *EndpointRuntimeMetrics) { *m = scopeFlip },
	} {
		flipped := base
		mutate(&flipped)
		e, st := hashes(&flipped)
		assert.NotEqual(t, baseE, e, name+" must churn the endpoints ETag")
		assert.NotEqual(t, baseS, st, name+" must churn the status ETag")
	}
	identical := base
	identical.SampledAt = t1
	identicalE, identicalS := hashes(&identical)
	assert.Equal(t, baseE, identicalE, "equal snapshots must hash equal")
	assert.Equal(t, baseS, identicalS)
	noneE, noneS := hashes(nil)
	assert.NotEqual(t, baseE, noneE)
	assert.NotEqual(t, baseS, noneS)
}

func TestRuntimeMetricsPrometheusOutputOmitsUnsupportedValues(t *testing.T) {
	t.Parallel()
	s := newEndpointRuntimeMetricsSampler(nil, nil)
	ep := &domain.Endpoint{Name: "vllm", URLString: "x", Type: "vllm", Status: domain.StatusHealthy, RuntimeMetricsEnabled: true}
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

	ep := &domain.Endpoint{Name: "halogen-395", Type: "openai-compatible", RuntimeMetricsKind: "llamacpp", URLString: server.URL, Status: domain.StatusHealthy, RuntimeMetricsEnabled: true, RuntimeMetricsInterval: time.Nanosecond, Headers: map[string]string{"X-Olla-Test": "metrics"}, AuthHeaderName: "Authorization", AuthHeaderValue: "Bearer test-token"}
	s := newEndpointRuntimeMetricsSampler(&mockStatusEndpointRepository{endpoints: []*domain.Endpoint{ep}}, nil)
	s.Refresh(t.Context())
	s.Refresh(t.Context())
	got := s.Snapshot(ep)
	require.NotNil(t, got)
	require.NotNil(t, envMetric(t, got, runtimeMetricKeyGenerationTPS))
	assert.InDelta(t, 70, *envNum(t, got, runtimeMetricKeyGenerationTPS, runtimeMetricPartCurrent), .001)
	s.Refresh(t.Context())
	got = s.Snapshot(ep)
	require.NotNil(t, got)
	assert.True(t, got.Stale)
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
		{Name: "vllm", URLString: "x", Type: "vllm", Status: domain.StatusHealthy, RuntimeMetricsEnabled: true},
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
	ep := &domain.Endpoint{URLString: "x", Type: "vllm", Status: domain.StatusHealthy, RuntimeMetricsEnabled: true}
	got := s.Snapshot(ep)
	require.NotNil(t, got)
	require.NotNil(t, envNum(t, got, runtimeMetricKeyKvCacheUsagePercent, runtimeMetricPartCurrent))
	assert.InDelta(t, 26.068376068376065, *envNum(t, got, runtimeMetricKeyKvCacheUsagePercent, runtimeMetricPartCurrent), .001)
}

// TestRuntimeMetricsVLLMGPUCacheUsagePerCIsNormalised verifies O9: the
// gpu_cache_usage_perc fallback is normalised like every other KV gauge -
// ratio-scale readings are scaled to percent, percent-scale readings pass
// through unchanged.
func TestRuntimeMetricsVLLMGPUCacheUsagePerCIsNormalised(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ in, want float64 }{{0.55, 55}, {96.5, 96.5}} {
		c := parseRuntimeCounters("vllm", "# TYPE vllm:gpu_cache_usage_perc gauge\nvllm:gpu_cache_usage_perc "+strconv.FormatFloat(tc.in, 'g', -1, 64))
		require.NotNil(t, c.KVUsage)
		assert.InDelta(t, tc.want, *c.KVUsage, .001)
	}
}

// TestRuntimeMetricsWindowAveragesAndMaxEvictsOldSamples verifies that avg
// and max cover only the 10-minute reporting window: samples older than the
// window are evicted, and current is always the latest sample.
func TestRuntimeMetricsWindowAveragesAndMaxEvictsOldSamples(t *testing.T) {
	t.Parallel()
	s := newEndpointRuntimeMetricsSampler(nil, nil)
	ep := &domain.Endpoint{URLString: "x", Type: "vllm", Status: domain.StatusHealthy, RuntimeMetricsEnabled: true}
	at := time.Date(2026, 9, 24, 14, 0, 0, 0, time.UTC)
	// A slow sample (10 tok/s) that must age out of the window.
	s.record("x", at, parseRuntimeCounters("vllm", "vllm:generation_tokens_total 0\nvllm:request_decode_time_seconds_sum 0"))
	s.record("x", at.Add(time.Second), parseRuntimeCounters("vllm", "vllm:generation_tokens_total 10\nvllm:request_decode_time_seconds_sum 1"))
	// A fast sample (100 tok/s) 11 minutes later: outside the window of the
	// first sample, so the window holds only this one.
	s.record("x", at.Add(11*time.Minute), parseRuntimeCounters("vllm", "vllm:generation_tokens_total 110\nvllm:request_decode_time_seconds_sum 2"))
	got := s.Snapshot(ep)
	require.NotNil(t, got)
	require.NotNil(t, envMetric(t, got, runtimeMetricKeyGenerationTPS))
	assert.InDelta(t, 100, *envNum(t, got, runtimeMetricKeyGenerationTPS, runtimeMetricPartCurrent), .001)
	assert.InDelta(t, 100, *envNum(t, got, runtimeMetricKeyGenerationTPS, runtimeMetricPartAvg), .001)
	assert.InDelta(t, 100, *envNum(t, got, runtimeMetricKeyGenerationTPS, runtimeMetricPartMax), .001)
}

func TestParsePrometheusExpositionAggregatesByDeclaredType(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		exposition string
		want       map[string]float64
	}{
		{"counter sums across series", "# TYPE foo_total counter\nfoo_total{a=\"1\"} 5\nfoo_total{a=\"2\"} 7\n", map[string]float64{"foo_total": 12}},
		{"gauge takes max, never the sum", "# TYPE g gauge\ng{a=\"1\"} 0.4\ng{a=\"2\"} 0.7\ng{a=\"3\"} 0.1\n", map[string]float64{"g": 0.7}},
		{"single-series gauge equals its value", "# TYPE s gauge\ns{engine=\"0\"} 0.25\n", map[string]float64{"s": 0.25}},
		{"undecorated names keep summing", "u 2\nu 3\n", map[string]float64{"u": 5}},
		{"zero gauge still registers presence", "# TYPE z gauge\nz 0\n", map[string]float64{"z": 0}},
		{"histogram suffixes are not gauges", "# TYPE h histogram\nh_sum{le=\"x\"} 5\nh_sum 3\n", map[string]float64{"h_sum": 8}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := parsePrometheusSums(tc.exposition)
			for name, want := range tc.want {
				v, ok := got[name]
				require.True(t, ok, "metric %s missing from %v", name, got)
				assert.InDelta(t, want, v, 1e-9)
			}
			assert.Len(t, got, len(tc.want))
		})
	}
}

func TestRuntimeMetricsKindDetection(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, epName, typ, cfgKind, want string
	}{
		{"vllm type", "ep", "vllm", "", "vllm"},
		{"radiance type", "ep", "radiance", "", "radiance"},
		{"halogen type folds to llamacpp", "ep", "halogen", "", "llamacpp"},
		{"llama.cpp spelling folds too", "ep", "llama.cpp", "", "llamacpp"},
		{"name no longer decides kind", "radiance-qfn", "openai-compatible", "", ""},
		{"halogen name no longer decides kind", "halogen-395", "openai-compatible", "", ""},
		{"generic name has no kind", "ollama-prod", "ollama", "", ""},
		{"config kind wins over type", "ep", "openai-compatible", "radiance", "radiance"},
		{"config kind is canonicalised", "ep", "ollama", "llama-cpp", "llamacpp"},
		{"config kind case-insensitive", "ep", "ollama", "VLLM", "vllm"},
		{"unknown config kind passes through for the log", "ep", "ollama", "sglang", "sglang"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ep := &domain.Endpoint{Name: tc.epName, Type: tc.typ, RuntimeMetricsKind: tc.cfgKind}
			assert.Equal(t, tc.want, runtimeMetricsKind(ep))
		})
	}
	assert.Equal(t, "", runtimeMetricsKind(nil))
}

// TestRuntimeMetricsUnknownKindParsesNothing pins O3: an unrecognised kind
// samples nothing instead of silently falling back to llama.cpp names.
func TestRuntimeMetricsUnknownKindParsesNothing(t *testing.T) {
	t.Parallel()
	got := parseRuntimeCounters("sglang", "llamacpp:tokens_predicted_total 5\nllamacpp:tokens_predicted_seconds_total 1")
	assert.Equal(t, runtimeCounters{}, got)
}

// TestRuntimeMetricsVLLMPrefillFallsBackToPromptTokens verifies O9: an older
// vLLM without the V1-only request_prefill_kv_computed_tokens_sum still gets
// prefill throughput from prompt_tokens_total over request_prefill_time.
func TestRuntimeMetricsVLLMPrefillFallsBackToPromptTokens(t *testing.T) {
	t.Parallel()
	s := newEndpointRuntimeMetricsSampler(nil, nil)
	s.record("x", time.Unix(0, 0), parseRuntimeCounters("vllm", "vllm:prompt_tokens_total 0\nvllm:request_prefill_time_seconds_sum 0"))
	s.record("x", time.Unix(10, 0), parseRuntimeCounters("vllm", "vllm:prompt_tokens_total 1000\nvllm:request_prefill_time_seconds_sum 2"))
	ep := &domain.Endpoint{URLString: "x", Type: "vllm", Status: domain.StatusHealthy, RuntimeMetricsEnabled: true}
	got := s.Snapshot(ep)
	require.NotNil(t, envMetric(t, got, runtimeMetricKeyPrefillTPS))
	assert.InDelta(t, 500, *envNum(t, got, runtimeMetricKeyPrefillTPS, runtimeMetricPartCurrent), .001)
}

func TestRuntimeMetricsRadianceCounterDeltasAndSaturationFields(t *testing.T) {
	t.Parallel()
	s := newEndpointRuntimeMetricsSampler(nil, nil)
	ep := &domain.Endpoint{Name: "radiance-qfn", URLString: "http://radiance", Type: "openai-compatible", RuntimeMetricsKind: "radiance", Status: domain.StatusHealthy, RuntimeMetricsEnabled: true}
	kind := runtimeMetricsKind(ep)
	require.Equal(t, "radiance", kind)
	at := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	s.record(ep.URLString, at, parseRuntimeCounters(kind, radianceT0))
	first := s.Snapshot(ep)
	require.NotNil(t, first)
	assert.Nil(t, envMetric(t, first, runtimeMetricKeyGenerationTPS), "a single scrape cannot yield a delta rate")
	s.record(ep.URLString, at.Add(10*time.Second), parseRuntimeCounters(kind, radianceT1))
	got := s.Snapshot(ep)
	require.NotNil(t, got)
	// Counter deltas win over the native rate gauges (301.5/1901.7) in the
	// same payload, and the gauge never pollutes the window.
	require.NotNil(t, envMetric(t, got, runtimeMetricKeyGenerationTPS))
	assert.InDelta(t, 70, *envNum(t, got, runtimeMetricKeyGenerationTPS, runtimeMetricPartCurrent), .001)
	assert.InDelta(t, 70, *envNum(t, got, runtimeMetricKeyGenerationTPS, runtimeMetricPartMax), .001)
	require.NotNil(t, envMetric(t, got, runtimeMetricKeyPrefillTPS))
	assert.InDelta(t, 964/1.92, *envNum(t, got, runtimeMetricKeyPrefillTPS, runtimeMetricPartCurrent), .001)
	require.NotNil(t, envMetric(t, got, runtimeMetricKeyCacheHitPercent))
	// 78% from the linear hit-rate gauge: the two radiance cache counters in
	// the payload (981 tokens / 1000 checkpoints) are NOT a hit/queries pair
	// and must never form a ratio (live incident: reported 5,996,000%).
	assert.InDelta(t, 78, *envNum(t, got, runtimeMetricKeyCacheHitPercent, runtimeMetricPartCurrent), .001)
	// The two-series KV gauge takes the worst engine (0.42), never the sum.
	require.NotNil(t, envNum(t, got, runtimeMetricKeyKvCacheUsagePercent, runtimeMetricPartCurrent))
	assert.InDelta(t, 42, *envNum(t, got, runtimeMetricKeyKvCacheUsagePercent, runtimeMetricPartCurrent), .001)
	require.NotNil(t, envNum(t, got, runtimeMetricKeyQueueDepth, runtimeMetricPartCurrent))
	assert.Equal(t, 7.0, *envNum(t, got, runtimeMetricKeyQueueDepth, runtimeMetricPartCurrent))
	require.NotNil(t, envNum(t, got, runtimeMetricKeyRequestsRunning, runtimeMetricPartCurrent))
	assert.Equal(t, 3.0, *envNum(t, got, runtimeMetricKeyRequestsRunning, runtimeMetricPartCurrent))
	require.NotNil(t, envMetric(t, got, runtimeMetricKeySpecAcceptPercent))
	assert.InDelta(t, 55, *envNum(t, got, runtimeMetricKeySpecAcceptPercent, runtimeMetricPartCurrent), .001)
	assert.InDelta(t, 52.5, *envNum(t, got, runtimeMetricKeySpecAcceptPercent, runtimeMetricPartAvg), .001)
}

const radianceGaugeOnlyT0 = `
# TYPE radiance:decode_tokens_per_second gauge
radiance:decode_tokens_per_second{engine="0"} 288.5
# TYPE radiance:prefill_tokens_per_second gauge
radiance:prefill_tokens_per_second{engine="0"} 1893.2
# TYPE radiance:linear_prefix_cache_hit_rate gauge
radiance:linear_prefix_cache_hit_rate{engine="0"} 0.76
# TYPE vllm:kv_cache_usage_perc gauge
vllm:kv_cache_usage_perc{engine="0"} 0.2
# TYPE radiance:http_queue_depth gauge
radiance:http_queue_depth{engine="0"} 2
# TYPE vllm:num_requests_running gauge
vllm:num_requests_running{engine="0"} 1
# TYPE radiance:draft_acceptance_ratio gauge
radiance:draft_acceptance_ratio{engine="0"} 0.5
`

const radianceGaugeOnlyT1 = `
# TYPE radiance:decode_tokens_per_second gauge
radiance:decode_tokens_per_second{engine="0"} 300.5
# TYPE radiance:prefill_tokens_per_second gauge
radiance:prefill_tokens_per_second{engine="0"} 1901.7
# TYPE radiance:linear_prefix_cache_hit_rate gauge
radiance:linear_prefix_cache_hit_rate{engine="0"} 0.76
# TYPE vllm:kv_cache_usage_perc gauge
vllm:kv_cache_usage_perc{engine="0"} 0.2
# TYPE radiance:http_queue_depth gauge
radiance:http_queue_depth{engine="0"} 2
# TYPE vllm:num_requests_running gauge
vllm:num_requests_running{engine="0"} 1
# TYPE radiance:draft_acceptance_ratio gauge
radiance:draft_acceptance_ratio{engine="0"} 0.5
`

// TestRuntimeMetricsRadianceGaugeFallbackWhenCountersAbsent covers engines
// that publish only the native rate gauges: values are meaningful from the
// first scrape and windowed for avg/max like delta samples.
func TestRuntimeMetricsRadianceGaugeFallbackWhenCountersAbsent(t *testing.T) {
	t.Parallel()
	s := newEndpointRuntimeMetricsSampler(nil, nil)
	at := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	s.record("x", at, parseRuntimeCounters("radiance", radianceGaugeOnlyT0))
	first := s.Snapshot(&domain.Endpoint{URLString: "x", Type: "vllm", Status: domain.StatusHealthy, RuntimeMetricsEnabled: true})
	require.NotNil(t, first)
	require.NotNil(t, envMetric(t, first, runtimeMetricKeyGenerationTPS), "gauge-only backends render from the first scrape")
	assert.InDelta(t, 288.5, *envNum(t, first, runtimeMetricKeyGenerationTPS, runtimeMetricPartCurrent), .001)
	require.NotNil(t, envMetric(t, first, runtimeMetricKeyCacheHitPercent))
	assert.InDelta(t, 76, *envNum(t, first, runtimeMetricKeyCacheHitPercent, runtimeMetricPartCurrent), .001, "hit-rate gauge x100")
	s.record("x", at.Add(10*time.Second), parseRuntimeCounters("radiance", radianceGaugeOnlyT1))
	got := s.Snapshot(&domain.Endpoint{URLString: "x", Type: "vllm", Status: domain.StatusHealthy, RuntimeMetricsEnabled: true})
	require.NotNil(t, envMetric(t, got, runtimeMetricKeyGenerationTPS))
	assert.InDelta(t, 300.5, *envNum(t, got, runtimeMetricKeyGenerationTPS, runtimeMetricPartCurrent), .001)
	assert.InDelta(t, 294.5, *envNum(t, got, runtimeMetricKeyGenerationTPS, runtimeMetricPartAvg), .001)
	assert.InDelta(t, 300.5, *envNum(t, got, runtimeMetricKeyGenerationTPS, runtimeMetricPartMax), .001)
	assert.InDelta(t, 1901.7, *s.states["x"].prefill.current, .001, "latest gauge sample wins as current")
}

// TestRuntimeMetricsRadianceCacheAlwaysFromGauge pins that the cache hit rate
// comes exclusively from the backend gauge: radiance's two cache counters are
// semantically unrelated (cached tokens vs checkpoint writes) and must never
// be combined, even when both series are present.
func TestRuntimeMetricsRadianceCacheAlwaysFromGauge(t *testing.T) {
	t.Parallel()
	s := newEndpointRuntimeMetricsSampler(nil, nil)
	const partial0 = `
# TYPE radiance:decode_tokens_total counter
radiance:decode_tokens_total{engine="0"} 0
# TYPE radiance:engine_step_seconds_decode_total counter
radiance:engine_step_seconds_decode_total{engine="0"} 0
# TYPE radiance:attn_cached_tokens_total counter
radiance:attn_cached_tokens_total{engine="0"} 0
# TYPE radiance:linear_prefix_cache_hit_rate gauge
radiance:linear_prefix_cache_hit_rate{engine="0"} 0.78
`
	const partial1 = `
# TYPE radiance:decode_tokens_total counter
radiance:decode_tokens_total{engine="0"} 50
# TYPE radiance:engine_step_seconds_decode_total counter
radiance:engine_step_seconds_decode_total{engine="0"} 1
# TYPE radiance:attn_cached_tokens_total counter
radiance:attn_cached_tokens_total{engine="0"} 1500
# TYPE radiance:linear_checkpoints_written_total counter
radiance:linear_checkpoints_written_total{engine="0"} 1000
# TYPE radiance:linear_prefix_cache_hit_rate gauge
radiance:linear_prefix_cache_hit_rate{engine="0"} 0.78
`
	at := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	s.record("x", at, parseRuntimeCounters("radiance", partial0))
	s.record("x", at.Add(time.Second), parseRuntimeCounters("radiance", partial1))
	got := s.Snapshot(&domain.Endpoint{URLString: "x", Type: "vllm", Status: domain.StatusHealthy, RuntimeMetricsEnabled: true})
	require.NotNil(t, envMetric(t, got, runtimeMetricKeyGenerationTPS))
	assert.InDelta(t, 50, *envNum(t, got, runtimeMetricKeyGenerationTPS, runtimeMetricPartCurrent), .001)
	require.NotNil(t, envMetric(t, got, runtimeMetricKeyCacheHitPercent))
	// 78% comes from the gauge; the (bogus) 1500/1000 counter pair is ignored.
	assert.InDelta(t, 78, *envNum(t, got, runtimeMetricKeyCacheHitPercent, runtimeMetricPartCurrent), .001)
}

// TestRuntimeMetricsRadianceCounterResetStartsNewEpoch verifies the reset
// detection covers the radiance counter spellings.
func TestRuntimeMetricsRadianceCounterResetStartsNewEpoch(t *testing.T) {
	t.Parallel()
	s := newEndpointRuntimeMetricsSampler(nil, nil)
	at := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	s.record("x", at, parseRuntimeCounters("radiance", radianceT0))
	s.record("x", at.Add(10*time.Second), parseRuntimeCounters("radiance", radianceT1))
	const reset = `
# TYPE radiance:decode_tokens_total counter
radiance:decode_tokens_total{engine="0"} 5
# TYPE radiance:engine_step_seconds_decode_total counter
radiance:engine_step_seconds_decode_total{engine="0"} 0.1
`
	s.record("x", at.Add(20*time.Second), parseRuntimeCounters("radiance", reset))
	got := s.Snapshot(&domain.Endpoint{URLString: "x", Type: "vllm", Status: domain.StatusHealthy, RuntimeMetricsEnabled: true})
	require.NotNil(t, got)
	assert.Equal(t, at.Add(20*time.Second), got.AvailableSince)
	assert.Nil(t, envMetric(t, got, runtimeMetricKeyGenerationTPS))
}

// TestRuntimeMetricsRadianceSaturationReExports checks the P2 Prometheus
// re-exports appear for radiance snapshots and stay absent (per endpoint, not
// per family) for kinds that do not publish them.
func TestRuntimeMetricsRadianceSaturationReExports(t *testing.T) {
	t.Parallel()
	s := newEndpointRuntimeMetricsSampler(nil, nil)
	radiance := &domain.Endpoint{Name: "radiance-qfn", URLString: "http://radiance", Type: "radiance", Status: domain.StatusHealthy, RuntimeMetricsEnabled: true}
	halogen := &domain.Endpoint{Name: "halogen-395", URLString: "http://halogen", Type: "halogen", Status: domain.StatusHealthy, RuntimeMetricsEnabled: true}
	at := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	s.record(radiance.URLString, at, parseRuntimeCounters("radiance", radianceT0))
	s.record(radiance.URLString, at.Add(10*time.Second), parseRuntimeCounters("radiance", radianceT1))
	s.record(halogen.URLString, at, parseRuntimeCounters("llamacpp", "llamacpp:tokens_predicted_total 0\nllamacpp:tokens_predicted_seconds_total 0"))
	s.record(halogen.URLString, at.Add(time.Second), parseRuntimeCounters("llamacpp", "llamacpp:tokens_predicted_total 40\nllamacpp:tokens_predicted_seconds_total 1"))
	var b strings.Builder
	writeEndpointRuntimePrometheusMetrics(&b, []*domain.Endpoint{radiance, halogen}, s)
	out := b.String()
	assert.Contains(t, out, `olla_endpoint_queue_depth{endpoint="radiance-qfn"} 7`)
	assert.Contains(t, out, `olla_endpoint_requests_running{endpoint="radiance-qfn"} 3`)
	assert.Contains(t, out, `olla_endpoint_spec_accept_percent{endpoint="radiance-qfn"} 55`)
	assert.Contains(t, out, `olla_endpoint_spec_accept_percent_avg{endpoint="radiance-qfn"} 52.5`)
	assert.Contains(t, out, `# TYPE olla_endpoint_queue_depth gauge`)
	assert.NotContains(t, out, `olla_endpoint_queue_depth{endpoint="halogen-395"}`)
	assert.NotContains(t, out, `olla_endpoint_requests_running{endpoint="halogen-395"}`)
	assert.NotContains(t, out, `olla_endpoint_spec_accept_percent{endpoint="halogen-395"}`)
}

// TestRuntimeMetricsScrapeFailureBackoffRetriesAndResets pins O5: failed
// scrapes double the retry delay per consecutive failure (capped at 60 s),
// and a success returns the endpoint to its normal interval.
func TestRuntimeMetricsScrapeFailureBackoffRetriesAndResets(t *testing.T) {
	var calls atomic.Int32
	var healthy atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if !healthy.Load() {
			http.Error(w, "down", http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte("llamacpp:tokens_predicted_total 40\nllamacpp:tokens_predicted_seconds_total 1\n"))
	}))
	defer server.Close()

	ep := &domain.Endpoint{Name: "halogen-down", Type: "halogen", URLString: server.URL, Status: domain.StatusHealthy, RuntimeMetricsEnabled: true, RuntimeMetricsInterval: 10 * time.Second}
	s := newEndpointRuntimeMetricsSampler(&mockStatusEndpointRepository{endpoints: []*domain.Endpoint{ep}}, nil)
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }

	s.Refresh(t.Context()) // failure #1: next attempt at +20 s
	assert.Equal(t, int32(1), calls.Load())
	now = now.Add(15 * time.Second)
	s.Refresh(t.Context())
	assert.Equal(t, int32(1), calls.Load(), "inside the 20 s backoff window")
	now = now.Add(5 * time.Second)
	s.Refresh(t.Context()) // failure #2: next attempt at +40 s
	assert.Equal(t, int32(2), calls.Load())
	now = now.Add(30 * time.Second)
	s.Refresh(t.Context())
	assert.Equal(t, int32(2), calls.Load(), "inside the 40 s backoff window")
	now = now.Add(10 * time.Second)
	s.Refresh(t.Context()) // failure #3: capped at +60 s
	assert.Equal(t, int32(3), calls.Load())
	now = now.Add(59 * time.Second)
	s.Refresh(t.Context())
	assert.Equal(t, int32(3), calls.Load(), "inside the 60 s capped window")
	now = now.Add(time.Second)
	healthy.Store(true)
	s.Refresh(t.Context()) // success clears the backoff
	assert.Equal(t, int32(4), calls.Load())
	got := s.Snapshot(ep)
	require.NotNil(t, got)
	assert.False(t, got.Stale)
	now = now.Add(10 * time.Second)
	s.Refresh(t.Context()) // back to the plain interval
	assert.Equal(t, int32(5), calls.Load())
}

func TestRuntimeMetricsRefreshPrunesStatesForRemovedEndpoints(t *testing.T) {
	t.Parallel()
	repo := &mockStatusEndpointRepository{endpoints: []*domain.Endpoint{{Name: "gone", URLString: "http://gone", Type: "vllm", Status: domain.StatusHealthy, RuntimeMetricsEnabled: true}}}
	s := newEndpointRuntimeMetricsSampler(repo, nil)
	s.record("http://gone", time.Now(), parseRuntimeCounters("vllm", "vllm:generation_tokens_total 1"))
	repo.endpoints = nil
	s.Refresh(t.Context())
	s.mu.RLock()
	_, ok := s.states["http://gone"]
	s.mu.RUnlock()
	assert.False(t, ok)
}

// TestRuntimeMetricsWakeDelayTracksNextDeadline pins O7's scheduling: sleep
// until the earliest deadline, bounded by the idle cap and floored so a
// due-now endpoint cannot spin the loop.
func TestRuntimeMetricsWakeDelayTracksNextDeadline(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	s := newEndpointRuntimeMetricsSampler(nil, nil)
	s.now = func() time.Time { return now }
	ep := &domain.Endpoint{Name: "v", URLString: "x", Type: "vllm", Status: domain.StatusHealthy, RuntimeMetricsEnabled: true, RuntimeMetricsInterval: 10 * time.Second}
	assert.Equal(t, runtimeMetricsIdleWake, s.wakeDelay(nil))
	assert.Equal(t, runtimeMetricsMinWake, s.wakeDelay([]*domain.Endpoint{ep}), "never sampled: due immediately, floored")
	s.record("x", now, parseRuntimeCounters("vllm", "vllm:generation_tokens_total 0"))
	assert.Equal(t, 10*time.Second, s.wakeDelay([]*domain.Endpoint{ep}))
	s.noteFailure(ep, "boom")
	assert.Equal(t, 20*time.Second, s.wakeDelay([]*domain.Endpoint{ep}), "backoff deadline takes over")
	assert.False(t, s.shouldRefresh(ep), "not due while backing off")
}

func TestRuntimeMetricsStaleSnapshotExpires(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	s := newEndpointRuntimeMetricsSampler(nil, nil)
	s.now = func() time.Time { return now }
	ep := &domain.Endpoint{URLString: "x", Type: "vllm", Status: domain.StatusHealthy, RuntimeMetricsEnabled: true}
	s.record("x", now, parseRuntimeCounters("vllm", "vllm:generation_tokens_total 1"))
	s.mu.Lock()
	s.states["x"].stale = true
	s.mu.Unlock()
	now = now.Add(14 * time.Minute)
	got := s.Snapshot(ep)
	require.NotNil(t, got)
	assert.True(t, got.Stale)
	now = now.Add(2 * time.Minute)
	assert.Nil(t, s.Snapshot(ep), "stale snapshots older than the max stale age are dropped")
}

// TestRuntimeMetricsHonoursConfiguredScrapePath pins O4: the endpoint's
// runtime_metrics.path replaces the default /metrics target.
func TestRuntimeMetricsHonoursConfiguredScrapePath(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/internal/prometheus" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte("llamacpp:tokens_predicted_total 1\n"))
	}))
	defer server.Close()
	ep := &domain.Endpoint{Name: "halogen-alt", Type: "halogen", URLString: server.URL, Status: domain.StatusHealthy, RuntimeMetricsEnabled: true, RuntimeMetricsInterval: time.Nanosecond, RuntimeMetricsPath: "/internal/prometheus"}
	s := newEndpointRuntimeMetricsSampler(&mockStatusEndpointRepository{endpoints: []*domain.Endpoint{ep}}, nil)
	s.Refresh(t.Context())
	got := s.Snapshot(ep)
	require.NotNil(t, got)
	assert.False(t, got.Stale)
}

// --- Audit regression tests (2026-10-06) ---

// TestRuntimeMetricsIdleScrapesNeverFreezeCurrent pins the two idle-engine
// defects: (radiance) an idle scrape must not fold the instantaneous rate
// gauge into the counter-delta window, and (vLLM) a rejected zero-delta
// sample must DROP the stale current instead of leaving last-burst numbers
// presented as live under a fresh sampled_at.
func TestRuntimeMetricsIdleScrapesNeverFreezeCurrent(t *testing.T) {
	t.Parallel()
	s := newEndpointRuntimeMetricsSampler(nil, nil)
	ep := &domain.Endpoint{Name: "radiance", URLString: "http://radiance", Type: "radiance", Status: domain.StatusHealthy, RuntimeMetricsEnabled: true}
	start := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	s.record(ep.URLString, start, parseRuntimeCounters("radiance", radianceT0))
	s.record(ep.URLString, start.Add(10*time.Second), parseRuntimeCounters("radiance", radianceT1))
	got := s.Snapshot(ep)
	require.NotNil(t, got)
	require.NotNil(t, envMetric(t, got, runtimeMetricKeyGenerationTPS))
	require.NotNil(t, envNum(t, got, runtimeMetricKeyGenerationTPS, runtimeMetricPartCurrent))
	assert.InDelta(t, 70, *envNum(t, got, runtimeMetricKeyGenerationTPS, runtimeMetricPartCurrent), 0.001) // 700 tokens / 10s

	// Idle: counters frozen (delta seconds == 0) while the engine still
	// publishes a non-zero rate gauge (in the T1 capture).
	s.record(ep.URLString, start.Add(20*time.Second), parseRuntimeCounters("radiance", radianceT1))
	got = s.Snapshot(ep)
	require.NotNil(t, envMetric(t, got, runtimeMetricKeyGenerationTPS))
	assert.Nil(t, envNum(t, got, runtimeMetricKeyGenerationTPS, runtimeMetricPartCurrent), "idle scrape must drop current, not freeze 70 or mix in the gauge")
	// The window keeps the one real counter-delta sample; the gauge value
	// never entered it (avg would move toward ~301 if it had).
	require.NotNil(t, envNum(t, got, runtimeMetricKeyGenerationTPS, runtimeMetricPartAvg))
	assert.InDelta(t, 70, *envNum(t, got, runtimeMetricKeyGenerationTPS, runtimeMetricPartAvg), 0.001)

	// Prolonged idle past the 10-minute window: every sample has decayed,
	// so the whole signal drops out of the payload (omitted, not zeroed).
	s.record(ep.URLString, start.Add(11*time.Minute), parseRuntimeCounters("radiance", radianceT1))
	got = s.Snapshot(ep)
	assert.Nil(t, envMetric(t, got, runtimeMetricKeyGenerationTPS))
}

func TestRuntimeMetricsVLLMIdleDropsStaleCurrent(t *testing.T) {
	t.Parallel()
	s := newEndpointRuntimeMetricsSampler(nil, nil)
	ep := &domain.Endpoint{URLString: "x", Type: "vllm", Status: domain.StatusHealthy, RuntimeMetricsEnabled: true}
	start := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	counters := func(tokens, seconds float64) runtimeCounters {
		return parseRuntimeCounters("vllm", "vllm:generation_tokens_total "+strconv.FormatFloat(tokens, 'f', -1, 64)+
			"\nvllm:request_decode_time_seconds_sum "+strconv.FormatFloat(seconds, 'f', -1, 64)+"\n")
	}
	s.record("x", start, counters(0, 0))
	s.record("x", start.Add(10*time.Second), counters(700, 10))
	require.NotNil(t, envNum(t, s.Snapshot(ep), runtimeMetricKeyGenerationTPS, runtimeMetricPartCurrent))
	s.record("x", start.Add(20*time.Second), counters(700, 10)) // idle, no rate gauges in vLLM
	got := s.Snapshot(ep)
	assert.Nil(t, envNum(t, got, runtimeMetricKeyGenerationTPS, runtimeMetricPartCurrent), "idle vLLM must not present the last burst rate as current")
	require.NotNil(t, envNum(t, got, runtimeMetricKeyGenerationTPS, runtimeMetricPartAvg))
	assert.InDelta(t, 70, *envNum(t, got, runtimeMetricKeyGenerationTPS, runtimeMetricPartAvg), 0.001)
}

// TestRuntimeMetricsOptOutInvalidatesSnapshot pins that disabling the
// opt-in (or losing dialect detection) invalidates the sampler state both
// through a refresh pass and through Snapshot itself - no frozen pre-opt-out
// snapshot may keep serving as fresh to the dashboard or /metrics.
func TestRuntimeMetricsOptOutInvalidatesSnapshot(t *testing.T) {
	t.Parallel()
	ep := &domain.Endpoint{Name: "v", URLString: "http://127.0.0.1:1", Type: "vllm", Status: domain.StatusHealthy, RuntimeMetricsEnabled: true}
	s := newEndpointRuntimeMetricsSampler(&mockStatusEndpointRepository{endpoints: []*domain.Endpoint{ep}}, nil)
	start := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	s.record(ep.URLString, start, parseRuntimeCounters("vllm", "vllm:generation_tokens_total 700\nvllm:request_decode_time_seconds_sum 10\n"))
	require.NotNil(t, s.Snapshot(ep))

	ep.RuntimeMetricsEnabled = false
	assert.Nil(t, s.Snapshot(ep), "Snapshot must refuse opted-out endpoints immediately")
	s.Refresh(t.Context())
	assert.Nil(t, s.Snapshot(ep))

	ep.RuntimeMetricsEnabled = true
	ep.Type = "ollama" // no dialect resolves for generic types without a name marker
	assert.Nil(t, s.Snapshot(ep), "Snapshot must refuse endpoints without a resolvable dialect")
}

// TestRuntimeMetricsRoutableBusyKeepsMetrics pins that Busy/Warming - which
// the scheduler still routes to - keep being sampled and rendered: metrics
// must not vanish precisely when a backend is slow under load.
func TestRuntimeMetricsRoutableBusyKeepsMetrics(t *testing.T) {
	t.Parallel()
	ep := &domain.Endpoint{Name: "v", URLString: "http://127.0.0.1:1", Type: "vllm", Status: domain.StatusHealthy, RuntimeMetricsEnabled: true}
	s := newEndpointRuntimeMetricsSampler(&mockStatusEndpointRepository{endpoints: []*domain.Endpoint{ep}}, nil)
	start := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	clock := start.Add(time.Minute) // past both due deadlines; window still covers the samples
	s.now = func() time.Time { return clock }
	s.record(ep.URLString, start, parseRuntimeCounters("vllm", "vllm:generation_tokens_total 0\nvllm:request_decode_time_seconds_sum 0\n"))
	s.record(ep.URLString, start.Add(10*time.Second), parseRuntimeCounters("vllm", "vllm:generation_tokens_total 700\nvllm:request_decode_time_seconds_sum 10\n"))

	ep.Status = domain.StatusBusy
	got := s.Snapshot(ep)
	require.NotNil(t, got, "Busy endpoints still serve traffic and must keep their metrics")
	require.NotNil(t, envMetric(t, got, runtimeMetricKeyGenerationTPS))
	assert.InDelta(t, 70, *envNum(t, got, runtimeMetricKeyGenerationTPS, runtimeMetricPartAvg), 0.001)

	ep.Status = domain.StatusWarming
	require.NotNil(t, s.Snapshot(ep))

	// A refresh pass on the Busy endpoint must attempt the scrape (the dead
	// port turns it into a recorded failure/stale) rather than wiping the
	// epoch via markUnavailable: stale keeps the last good numbers rendered.
	s.Refresh(t.Context())
	got = s.Snapshot(ep)
	require.NotNil(t, got, "Busy endpoint scrape failure marks stale, not unavailable")
	assert.True(t, got.Stale)

	ep.Status = domain.StatusOffline
	assert.Nil(t, s.Snapshot(ep), "non-routable states still drop the snapshot")
}

// TestRuntimeMetricsSnapshotCarriesResolvedKind pins the payload field the
// dashboard uses to key batch-semantics caveats: the RESOLVED dialect, not
// the endpoint type (an openai-compatible endpoint pinned to radiance must
// report radiance).
func TestRuntimeMetricsSnapshotCarriesResolvedKind(t *testing.T) {
	t.Parallel()
	s := newEndpointRuntimeMetricsSampler(nil, nil)
	ep := &domain.Endpoint{Name: "r", URLString: "x", Type: "openai-compatible", RuntimeMetricsKind: "radiance", Status: domain.StatusHealthy, RuntimeMetricsEnabled: true}
	start := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	s.record("x", start, parseRuntimeCounters("radiance", radianceT0))
	got := s.Snapshot(ep)
	require.NotNil(t, got)
	assert.Equal(t, "radiance", got.Kind)
}

// TestRuntimeMetricsOversizedExpositionFailsLoud pins that a body over the
// scrape limit is a failure (backoff + stale), never a partial parse: half a
// counter family would read as a counter reset and wipe the window epoch.
func TestRuntimeMetricsOversizedExpositionFailsLoud(t *testing.T) {
	t.Parallel()
	filler := strings.Repeat("unrelated_metric_series_total{a=\"b\"} 1\n", 600*1024) // >4 MiB limit
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("vllm:generation_tokens_total 700\nvllm:request_decode_time_seconds_sum 10\n"))
		_, _ = w.Write([]byte(filler))
	}))
	defer server.Close()
	ep := &domain.Endpoint{Name: "v", URLString: server.URL, Type: "vllm", Status: domain.StatusHealthy, RuntimeMetricsEnabled: true, RuntimeMetricsInterval: time.Nanosecond}
	s := newEndpointRuntimeMetricsSampler(&mockStatusEndpointRepository{endpoints: []*domain.Endpoint{ep}}, nil)
	fixed := time.Date(2026, 10, 6, 12, 5, 0, 0, time.UTC) // past every deadline, inside the 15m stale age
	s.now = func() time.Time { return fixed }
	s.Refresh(t.Context())
	assert.Nil(t, s.Snapshot(ep), "an over-limit exposition must not produce a snapshot")

	// And after a good epoch, an over-limit scrape marks stale instead of
	// silently parsing the truncated prefix.
	start := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	s.record(ep.URLString, start, parseRuntimeCounters("vllm", "vllm:generation_tokens_total 0\nvllm:request_decode_time_seconds_sum 0\n"))
	s.record(ep.URLString, start.Add(10*time.Second), parseRuntimeCounters("vllm", "vllm:generation_tokens_total 700\nvllm:request_decode_time_seconds_sum 10\n"))
	require.NotNil(t, s.Snapshot(ep))
	s.Refresh(t.Context())
	got := s.Snapshot(ep)
	require.NotNil(t, got)
	assert.True(t, got.Stale, "over-limit scrape must mark the epoch stale, not wipe or silently trim it")
}

// envMetric/envNum read envelope metrics in assertions; nil means the
// metric is absent from the payload.
func envMetric(t *testing.T, m *EndpointRuntimeMetrics, key string) *RuntimeMetric {
	t.Helper()
	if m == nil {
		return nil
	}
	for _, metric := range m.Metrics {
		if metric.Key == key {
			found := metric
			return &found
		}
	}
	return nil
}

func envNum(t *testing.T, m *EndpointRuntimeMetrics, key, part string) *float64 {
	t.Helper()
	metric := envMetric(t, m, key)
	if metric == nil {
		return nil
	}
	return metric.value(part)
}

func TestRuntimeMetricsSampler_QueueDepthFor(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	s := newEndpointRuntimeMetricsSampler(nil, nil)
	s.now = func() time.Time { return now }
	ep := &domain.Endpoint{URLString: "http://engine:8000", RuntimeMetricsEnabled: true}

	_, ok := s.QueueDepthFor(ep)
	assert.False(t, ok, "no sample yet = not known")

	depth := 7.0
	s.states[ep.URLString] = &runtimeMetricsState{available: true, sampledAt: now.Add(-15 * time.Second), QueueDepth: &depth}
	q, ok := s.QueueDepthFor(ep)
	assert.True(t, ok)
	assert.Equal(t, 7.0, q)

	s.states[ep.URLString].stale = true
	_, ok = s.QueueDepthFor(ep)
	assert.False(t, ok, "stale samples must not steer routing")
	s.states[ep.URLString].stale = false
	s.states[ep.URLString].sampledAt = now.Add(-2 * time.Minute)
	_, ok = s.QueueDepthFor(ep)
	assert.False(t, ok, "samples older than 90s must not steer routing")

	assert.False(t, func() bool { _, ok := s.QueueDepthFor(nil); return ok }(), "nil endpoint is not known")
}
