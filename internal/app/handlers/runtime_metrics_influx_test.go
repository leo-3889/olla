package handlers

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/thushan/olla/internal/core/domain"
)

type stubFluxQuerier struct {
	calls           int
	lastRows        []map[string]any
	gaugeMeanRows   []map[string]any
	gaugeMaxRows    []map[string]any
	counterMeanRows []map[string]any
	counterMaxRows  []map[string]any
	err             error
}

func (s *stubFluxQuerier) query(_ context.Context, flux string) ([]map[string]any, error) {
	s.calls++
	if s.err != nil {
		return nil, s.err
	}
	switch {
	case strings.Contains(flux, "|> last()"):
		return s.lastRows, nil
	case strings.Contains(flux, "decode_tokens_per_second") && strings.Contains(flux, "fn:mean"):
		return s.gaugeMeanRows, nil
	case strings.Contains(flux, "decode_tokens_per_second") && strings.Contains(flux, "fn:max"):
		return s.gaugeMaxRows, nil
	case strings.Contains(flux, "fn:mean"):
		return s.counterMeanRows, nil
	default:
		return s.counterMaxRows, nil
	}
}

func influxRow(endpoint, field string, value any, at time.Time) map[string]any {
	return map[string]any{"endpoint": endpoint, "_field": field, "_value": value, "_time": at.UTC().Format(time.RFC3339Nano)}
}

func newProviderForTest(stub *stubFluxQuerier, endpoints []*domain.Endpoint, now func() time.Time) *influxMetricsProvider {
	return &influxMetricsProvider{
		repository: &mockStatusEndpointRepository{endpoints: endpoints},
		client:     stub,
		bucket:     "llm/metrics",
		staleAfter: 60 * time.Second,
		now:        now,
		states:     map[string]*influxEndpointState{},
		firstSee:   map[string]time.Time{},
	}
}

func TestInfluxProviderEnvelopeGaugeFirstAndPercentising(t *testing.T) {
	newest := time.Date(2026, 10, 13, 10, 0, 0, 0, time.UTC)
	radiance := &domain.Endpoint{Name: "radiance-qfn", URLString: "http://radiance", Type: "openai-compatible", RuntimeMetricsKind: "radiance", Status: domain.StatusHealthy, RuntimeMetricsEnabled: true}
	stub := &stubFluxQuerier{
		lastRows: []map[string]any{
			influxRow("radiance-qfn", "decode_tokens_per_second", 40.0, newest),
			influxRow("radiance-qfn", "prefill_tokens_per_second", 10.0, newest),
			influxRow("radiance-qfn", "prefix_cache_hit_ratio", 0.5, newest),
			influxRow("radiance-qfn", "kv_cache_usage_ratio", 0.25, newest),
			influxRow("radiance-qfn", "queue_depth", 2.0, newest),
			influxRow("radiance-qfn", "requests_running", 1.0, newest),
			influxRow("radiance-qfn", "draft_acceptance_ratio", 0.6, newest),
			influxRow("radiance-qfn", "generation_tokens_total", 12345.0, newest),
		},
		gaugeMeanRows: []map[string]any{
			influxRow("radiance-qfn", "decode_tokens_per_second", 36.0, newest.Add(-time.Minute)),
			influxRow("radiance-qfn", "decode_tokens_per_second", 38.0, newest),
			influxRow("radiance-qfn", "prefix_cache_hit_ratio", 0.4, newest),
			influxRow("radiance-qfn", "draft_acceptance_ratio", 0.55, newest),
		},
		gaugeMaxRows: []map[string]any{influxRow("radiance-qfn", "decode_tokens_per_second", 50.0, newest)},
		// Counter rows exist too; the gauge family must win outright.
		counterMeanRows: []map[string]any{influxRow("radiance-qfn", "generation_tokens_total", 999.0, newest)},
	}
	p := newProviderForTest(stub, []*domain.Endpoint{radiance}, func() time.Time { return newest.Add(2 * time.Second) })
	p.refresh(context.Background())

	got := p.Snapshot(radiance)
	require.NotNil(t, got)
	assert.Equal(t, runtimeMetricsSourceInflux, got.Source)
	assert.Equal(t, "radiance", got.Kind)
	assert.False(t, got.Stale)
	require.NotNil(t, got.SourceAgeS)
	assert.InDelta(t, 2.0, *got.SourceAgeS, 0.01)

	require.NotNil(t, envMetric(t, got, runtimeMetricKeyGenerationTPS))
	assert.InDelta(t, 40.0, *envNum(t, got, runtimeMetricKeyGenerationTPS, runtimeMetricPartCurrent), .001, "gauge current wins")
	assert.InDelta(t, 37.0, *envNum(t, got, runtimeMetricKeyGenerationTPS, runtimeMetricPartAvg), .001, "avg of window means")
	assert.InDelta(t, 50.0, *envNum(t, got, runtimeMetricKeyGenerationTPS, runtimeMetricPartMax), .001)
	assert.Equal(t, runtimeMetricScopeEngine, envMetric(t, got, runtimeMetricKeyGenerationTPS).Scope)
	assert.Contains(t, envMetric(t, got, runtimeMetricKeyGenerationTPS).Note, runtimeMetricNoteEngineThroughput)
	assert.Contains(t, envMetric(t, got, runtimeMetricKeyGenerationTPS).Note, runtimeMetricNoteInfluxGauge)
	assert.InDelta(t, 37.0, *envNum(t, got, runtimeMetricKeyGenerationTPS, runtimeMetricPartAvg), .001, "counter rows must not leak into gauge windows")

	assert.InDelta(t, 50.0, *envNum(t, got, runtimeMetricKeyCacheHitPercent, runtimeMetricPartCurrent), .001, "ratio x100")
	assert.InDelta(t, 40.0, *envNum(t, got, runtimeMetricKeyCacheHitPercent, runtimeMetricPartAvg), .001)
	assert.InDelta(t, 25.0, *envNum(t, got, runtimeMetricKeyKvCacheUsagePercent, runtimeMetricPartCurrent), .001)
	assert.InDelta(t, 60.0, *envNum(t, got, runtimeMetricKeySpecAcceptPercent, runtimeMetricPartCurrent), .001)
	assert.Equal(t, runtimeMetricKeyGenerationTPS, envMetric(t, got, runtimeMetricKeySpecAcceptPercent).AttachTo)
	assert.InDelta(t, 2.0, *envNum(t, got, runtimeMetricKeyQueueDepth, runtimeMetricPartCurrent), .001)
}

func TestInfluxProviderCounterFallbackSemantics(t *testing.T) {
	newest := time.Date(2026, 10, 13, 10, 0, 0, 0, time.UTC)
	vllm := &domain.Endpoint{Name: "plain-vllm", URLString: "http://vllm", Type: "vllm", Status: domain.StatusHealthy, RuntimeMetricsEnabled: true}
	stub := &stubFluxQuerier{
		counterMeanRows: []map[string]any{
			influxRow("plain-vllm", "generation_tokens_total", 10.0, newest.Add(-time.Minute)),
			influxRow("plain-vllm", "generation_tokens_total", 20.0, newest),
		},
		counterMaxRows: []map[string]any{influxRow("plain-vllm", "generation_tokens_total", 25.0, newest)},
	}
	p := newProviderForTest(stub, []*domain.Endpoint{vllm}, func() time.Time { return newest })
	p.refresh(context.Background())

	got := p.Snapshot(vllm)
	require.NotNil(t, got)
	gen := envMetric(t, got, runtimeMetricKeyGenerationTPS)
	require.NotNil(t, gen)
	assert.InDelta(t, 20.0, *gen.Current, .001, "newest window is current")
	assert.InDelta(t, 15.0, *gen.Avg, .001)
	assert.InDelta(t, 25.0, *gen.Max, .001)
	assert.Empty(t, gen.Scope, "a derived rate is not the engine's own batch gauge")
	assert.Equal(t, runtimeMetricNoteInfluxCounter, gen.Note)
}

func TestInfluxProviderStaleAndBound(t *testing.T) {
	fresh := time.Date(2026, 10, 13, 10, 0, 0, 0, time.UTC)
	vllm := &domain.Endpoint{Name: "ep", URLString: "http://ep", Type: "vllm", Status: domain.StatusHealthy, RuntimeMetricsEnabled: true}
	now := fresh
	stub := &stubFluxQuerier{lastRows: []map[string]any{influxRow("ep", "queue_depth", 1.0, fresh)}}
	p := newProviderForTest(stub, []*domain.Endpoint{vllm}, func() time.Time { return now })
	p.refresh(context.Background())
	assert.False(t, p.Snapshot(vllm).Stale)

	now = fresh.Add(90 * time.Second)
	assert.True(t, p.Snapshot(vllm).Stale, "silence past stale_after marks stale")

	now = fresh.Add(16 * time.Minute)
	assert.Nil(t, p.Snapshot(vllm), "the max-stale bound eventually hides a dead source entirely")
}

func TestInfluxProviderFailureKeepsLastSamples(t *testing.T) {
	fresh := time.Date(2026, 10, 13, 10, 0, 0, 0, time.UTC)
	vllm := &domain.Endpoint{Name: "ep", URLString: "http://ep", Type: "vllm", Status: domain.StatusHealthy, RuntimeMetricsEnabled: true}
	stub := &stubFluxQuerier{lastRows: []map[string]any{influxRow("ep", "queue_depth", 1.0, fresh)}}
	p := newProviderForTest(stub, []*domain.Endpoint{vllm}, func() time.Time { return fresh })
	p.refresh(context.Background())
	require.NotNil(t, p.Snapshot(vllm))

	stub.err = errors.New("influx down")
	p.refresh(context.Background())
	got := p.Snapshot(vllm)
	require.NotNil(t, got, "a query failure must not erase the last honest samples")
	assert.False(t, got.Stale, "age has not crossed stale_after yet at same now")
}

func TestInfluxProviderNoTrackedEndpointsRunsNoQueries(t *testing.T) {
	off := &domain.Endpoint{Name: "ep", URLString: "http://ep", Type: "vllm", Status: domain.StatusHealthy, RuntimeMetricsEnabled: false}
	stub := &stubFluxQuerier{}
	p := newProviderForTest(stub, []*domain.Endpoint{off}, time.Now)
	p.refresh(context.Background())
	assert.Equal(t, 0, stub.calls)
}

func TestInfluxProviderOptOutInvalidatesState(t *testing.T) {
	newest := time.Date(2026, 10, 13, 10, 0, 0, 0, time.UTC)
	ep := &domain.Endpoint{Name: "ep", URLString: "http://ep", Type: "vllm", Status: domain.StatusHealthy, RuntimeMetricsEnabled: true}
	stub := &stubFluxQuerier{lastRows: []map[string]any{influxRow("ep", "queue_depth", 1.0, newest)}}
	p := newProviderForTest(stub, []*domain.Endpoint{ep}, func() time.Time { return newest })
	p.refresh(context.Background())
	require.NotNil(t, p.Snapshot(ep))

	ep.RuntimeMetricsEnabled = false
	p.refresh(context.Background())
	assert.Nil(t, p.Snapshot(ep), "opting out must invalidate the leftover state")
}

func TestInfluxProviderUnavailableEndpointExcludedFromQuerySet(t *testing.T) {
	newest := time.Date(2026, 10, 13, 10, 0, 0, 0, time.UTC)
	up := &domain.Endpoint{Name: "up", URLString: "http://up", Type: "vllm", Status: domain.StatusHealthy, RuntimeMetricsEnabled: true}
	down := &domain.Endpoint{Name: "down", URLString: "http://down", Type: "vllm", Status: domain.StatusUnhealthy, RuntimeMetricsEnabled: true}
	stub := &stubFluxQuerier{lastRows: []map[string]any{influxRow("up", "queue_depth", 1.0, newest)}}
	p := newProviderForTest(stub, []*domain.Endpoint{up, down}, func() time.Time { return newest })
	p.refresh(context.Background())
	require.NotZero(t, stub.calls)
	assert.Nil(t, p.Snapshot(down), "unroutable endpoints never surface runtime metrics")
}
