package handlers

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/thushan/olla/internal/core/domain"
)

type stubFluxQuerier struct {
	calls           int
	queries         []string // every flux script, in call order
	lastRows        []map[string]any
	gaugeMeanRows   []map[string]any
	gaugeMaxRows    []map[string]any
	counterMeanRows []map[string]any
	counterMaxRows  []map[string]any
	// Lifetime-tokens leg (costs bucket, measurement llm_endpoint_cost_v3),
	// dispatched by measurement like the observe stub harness.
	tokenRows []map[string]any
	tokenErr  error
	err       error
	legErrs   map[string]error // fail ONE leg: "last","gaugeMean","gaugeMax","counterMean","counterMax"
}

func (s *stubFluxQuerier) query(_ context.Context, flux string) ([]map[string]any, error) {
	s.calls++
	s.queries = append(s.queries, flux)
	if s.err != nil {
		return nil, s.err
	}
	legError := func(leg string) error { return s.legErrs[leg] }
	switch {
	case strings.Contains(flux, "llm_endpoint_cost_v3"):
		if s.tokenErr != nil {
			return nil, s.tokenErr
		}
		return s.tokenRows, nil
	case strings.Contains(flux, "|> last()"):
		if e := legError("last"); e != nil {
			return nil, e
		}
		return s.lastRows, nil
	case strings.Contains(flux, "decode_tokens_per_second") && strings.Contains(flux, "fn:mean"):
		if e := legError("gaugeMean"); e != nil {
			return nil, e
		}
		return s.gaugeMeanRows, nil
	case strings.Contains(flux, "decode_tokens_per_second") && strings.Contains(flux, "fn:max"):
		if e := legError("gaugeMax"); e != nil {
			return nil, e
		}
		return s.gaugeMaxRows, nil
	case strings.Contains(flux, "fn:mean"):
		if e := legError("counterMean"); e != nil {
			return nil, e
		}
		return s.counterMeanRows, nil
	default:
		if e := legError("counterMax"); e != nil {
			return nil, e
		}
		return s.counterMaxRows, nil
	}
}

func influxRow(endpoint, field string, value any, at time.Time) map[string]any {
	return map[string]any{"endpoint": endpoint, "_field": field, "_value": value, "_time": at.UTC().Format(time.RFC3339Nano)}
}

func newProviderForTest(stub *stubFluxQuerier, endpoints []*domain.Endpoint, now func() time.Time) *influxMetricsProvider {
	return &influxMetricsProvider{
		repository:  &mockStatusEndpointRepository{endpoints: endpoints},
		client:      stub,
		bucket:      "llm/metrics",
		costsBucket: "llm/costs",
		tokenRange:  "-730d",
		tokenTTL:    lifetimeTokensTTL,
		staleAfter:  60 * time.Second,
		now:         now,
		states:      map[string]*influxEndpointState{},
		firstSee:    map[string]time.Time{},
		legFailing:  map[string]bool{},
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
	// The five RUNTIME legs must not fire for a fully opted-out fleet. The
	// lifetime-tokens leg is a separate, TTL-gated leg: cost rollups exist
	// for opted-out endpoints too (the endpoint still served traffic; only
	// the runtime tiles opted out), so it runs on the configured set.
	runtime := 0
	for _, q := range stub.queries {
		if !strings.Contains(q, "llm_endpoint_cost_v3") {
			runtime++
		}
	}
	assert.Equal(t, 0, runtime, "no runtime queries for an opted-out fleet")
	assert.NotEmpty(t, stub.queries, "the lifetime leg runs for configured endpoints")
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

func TestInfluxProviderQueueDepthFor(t *testing.T) {
	now := time.Unix(1700000000, 0)
	ep := &domain.Endpoint{Name: "ep1", URLString: "http://ep1:8000", Type: "openai-compatible", RuntimeMetricsKind: "radiance", Status: domain.StatusHealthy, RuntimeMetricsEnabled: true}
	provider := newProviderForTest(&stubFluxQuerier{}, []*domain.Endpoint{ep}, func() time.Time { return now })

	// Fresh queue sample: known.
	provider.states["ep1"] = &influxEndpointState{
		metrics: []RuntimeMetric{{Key: runtimeMetricKeyQueueDepth, Current: floatPtr(4)}},
		newest:  now.Add(-30 * time.Second),
		queueAt: now.Add(-30 * time.Second),
	}
	q, ok := provider.QueueDepthFor(ep)
	assert.True(t, ok)
	assert.InDelta(t, 4.0, q, 1e-9)

	// Queue sample older than the load-reader bound: not known (no steering
	// on old data) - even though the ENDPOINT's newest point is fresh, which
	// is exactly the case `newest`-gating let through: throughput refreshes
	// every scrape while the queue reading sits at its last change.
	provider.states["ep1"] = &influxEndpointState{
		metrics: []RuntimeMetric{{Key: runtimeMetricKeyQueueDepth, Current: floatPtr(4)}},
		newest:  now,
		queueAt: now.Add(-91 * time.Second),
	}
	_, ok = provider.QueueDepthFor(ep)
	assert.False(t, ok, "a queue reading older than 90s must not steer routing")

	// The mirror case: the queue gauge is fresh while the endpoint's newest
	// aggregate is older (1m-window rows land behind a last() point) - the
	// queue is current, so it steers.
	provider.states["ep1"] = &influxEndpointState{
		metrics: []RuntimeMetric{{Key: runtimeMetricKeyQueueDepth, Current: floatPtr(2)}},
		newest:  now.Add(-120 * time.Second),
		queueAt: now.Add(-10 * time.Second),
	}
	q, ok = provider.QueueDepthFor(ep)
	assert.True(t, ok, "a fresh queue reading steers even when other fields are older")
	assert.InDelta(t, 2.0, q, 1e-9)

	// Queue field never arrived: not known (pre-fix the zero-time newest gate
	// was the only defence; queueAt zero is the direct statement).
	provider.states["ep1"] = &influxEndpointState{
		metrics: []RuntimeMetric{{Key: runtimeMetricKeyKvCacheUsagePercent, Current: floatPtr(33)}},
		newest:  now,
	}
	_, ok = provider.QueueDepthFor(ep)
	assert.False(t, ok)

	// Endpoint outside the tracked set (state deleted by markUnavailable).
	_, ok = provider.QueueDepthFor(&domain.Endpoint{Name: "ep2", URLString: "http://ep2:8000", RuntimeMetricsKind: "radiance", Status: domain.StatusHealthy})
	assert.False(t, ok)
	_, ok = provider.QueueDepthFor(nil)
	assert.False(t, ok)
}

type influxLegLogEntry struct {
	msg  string
	args []any
}

type influxLegLogger struct {
	mockStyledLogger
	warns []influxLegLogEntry
	infos []influxLegLogEntry
}

func (l *influxLegLogger) Warn(msg string, args ...any) {
	l.warns = append(l.warns, influxLegLogEntry{msg, args})
}
func (l *influxLegLogger) Info(msg string, args ...any) {
	l.infos = append(l.infos, influxLegLogEntry{msg, args})
}

// A PARTIAL cycle failure (one window leg dies while last() lives) used to be
// fully silent: the all-fail warn only covers the total case, so blanked
// avg/max columns were indistinguishable from an idle engine. Legs are now
// logged on transition, once - the cycle runs every 10s.
func TestInfluxProviderPartialLegLoggedOnTransition(t *testing.T) {
	now := time.Unix(1700000000, 0)
	ep := &domain.Endpoint{Name: "ep1", URLString: "http://ep1:8000", Type: "openai-compatible", RuntimeMetricsKind: "radiance", Status: domain.StatusHealthy, RuntimeMetricsEnabled: true}
	stub := &stubFluxQuerier{
		lastRows: []map[string]any{influxRow("ep1", "queue_depth", 3.0, now.Add(-30*time.Second))},
		legErrs:  map[string]error{"gaugeMean": errors.New("gateway timeout")},
	}
	provider := newProviderForTest(stub, []*domain.Endpoint{ep}, func() time.Time { return now })
	rec := &influxLegLogger{}
	provider.logger = rec

	provider.refresh(context.Background())
	provider.refresh(context.Background())
	legWarns := 0
	for _, w := range rec.warns {
		if strings.Contains(w.msg, "query leg failing") {
			legWarns++
			assert.Contains(t, fmt.Sprint(w.args...), "gaugeMean")
		}
	}
	assert.Equal(t, 1, legWarns, "a persistently failing leg is logged on transition, not per cycle")

	// Recovery is disclosed once.
	stub.legErrs["gaugeMean"] = nil
	provider.refresh(context.Background())
	provider.refresh(context.Background())
	recoveries := 0
	for _, i := range rec.infos {
		if strings.Contains(i.msg, "query leg recovered") {
			recoveries++
		}
	}
	assert.Equal(t, 1, recoveries)
}

// tokenRow renders one summed row of the lifetime-tokens flux (the
// group(columns:["endpoint"]) |> sum() output: endpoint + _value per table).
func tokenRow(endpoint string, value any) map[string]any {
	return map[string]any{"endpoint": endpoint, "_value": value}
}

// P8 core: the lifetime-tokens leg surfaces the EXACT rollup sum per
// configured endpoint, ignores ghost ids, ships the contract flux, and is
// TTL-gated (the 10s refresh must not re-fan the costs query per cycle).
func TestInfluxProviderLifetimeTokensExactSumGhostsIgnoredTTL(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	r64 := &domain.Endpoint{Name: "radiance-9700-64", URLString: "http://r64", Type: "openai-compatible", RuntimeMetricsKind: "radiance", Status: domain.StatusHealthy, RuntimeMetricsEnabled: true}
	h395 := &domain.Endpoint{Name: "halogen-395", URLString: "http://h", Type: "llamacpp", RuntimeMetricsKind: "llamacpp", Status: domain.StatusHealthy, RuntimeMetricsEnabled: true}
	// radiance-9700-128: configured, ZERO cost rows (unmetered energy-only).
	r128 := &domain.Endpoint{Name: "radiance-9700-128", URLString: "http://r128", Type: "openai-compatible", RuntimeMetricsKind: "radiance", Status: domain.StatusHealthy, RuntimeMetricsEnabled: true}
	stub := &stubFluxQuerier{tokenRows: []map[string]any{
		tokenRow("radiance-9700-64", "2192454947"),
		tokenRow("halogen-395", "97467683"),
		// Ghost ids from the live data: the -64 pre-rename id and a retired
		// endpoint. The stub models influx passing them through (a server
		// that ignored contains()); the Go-side set guard must drop them.
		tokenRow("radiance-qfn", "1414912324"),
		tokenRow("vllm-qwen38", "104651862"),
	}}
	clock := now
	p := newProviderForTest(stub, []*domain.Endpoint{r64, h395, r128}, func() time.Time { return clock })
	p.refresh(context.Background())

	v, ok := p.LifetimeTokens("radiance-9700-64")
	require.True(t, ok)
	assert.Equal(t, int64(2192454947), v, "exact rollup sum, string-parsed")
	v, ok = p.LifetimeTokens("halogen-395")
	require.True(t, ok)
	assert.Equal(t, int64(97467683), v)
	_, ok = p.LifetimeTokens("radiance-9700-128")
	assert.False(t, ok, "no rows => not known (dash), never a fake 0")
	_, ok = p.LifetimeTokens("radiance-qfn")
	assert.False(t, ok, "ghost ids are never credited to a current endpoint")
	_, ok = p.LifetimeTokens("vllm-qwen38")
	assert.False(t, ok, "retired endpoint rows are dropped")

	var costQuery string
	for _, q := range stub.queries {
		if strings.Contains(q, "llm_endpoint_cost_v3") {
			costQuery = q
		}
	}
	require.NotEmpty(t, costQuery)
	assert.Contains(t, costQuery, `from(bucket:"llm/costs")`, "costs bucket, not the runtime bucket")
	assert.Contains(t, costQuery, "range(start:-730d)", "configured lookback rendered as a Flux duration")
	assert.Contains(t, costQuery, `r._measurement=="llm_endpoint_cost_v3"`)
	assert.Contains(t, costQuery, `r._field=="total_tokens"`)
	assert.Contains(t, costQuery, `r.period=="hour"`)
	assert.Contains(t, costQuery, `contains(set:["radiance-9700-64","halogen-395","radiance-9700-128"], value:r.endpoint)`)
	assert.Contains(t, costQuery, `group(columns:["endpoint"]) |> sum()`)

	// TTL gate: the 10s refresh cycle must reuse the cache; a second call
	// inside tokenTTL adds NO costs query.
	before := 0
	for _, q := range stub.queries {
		if strings.Contains(q, "llm_endpoint_cost_v3") {
			before++
		}
	}
	p.refresh(context.Background())
	after := 0
	for _, q := range stub.queries {
		if strings.Contains(q, "llm_endpoint_cost_v3") {
			after++
		}
	}
	assert.Equal(t, before, after, "the lifetime query is TTL-gated, not per-cycle")

	// Past the TTL it re-queries and the newer sum wins.
	stub.tokenRows = []map[string]any{tokenRow("radiance-9700-64", "2192455000")}
	clock = clock.Add(lifetimeTokensTTL + time.Second)
	p.refresh(context.Background())
	v, ok = p.LifetimeTokens("radiance-9700-64")
	require.True(t, ok)
	assert.Equal(t, int64(2192455000), v, "TTL expiry re-queries and refreshes the map")
}

// A failing lifetime query must CLEAR the cache (the column dashes; no stale
// pre-outage numbers) and must not touch the runtime states (partial-failure
// disclosure: one leg dying never blanks the other tiles).
func TestInfluxProviderLifetimeTokensFailureClearsAndIsIsolated(t *testing.T) {
	newest := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	ep := &domain.Endpoint{Name: "ep", URLString: "http://ep", Type: "vllm", Status: domain.StatusHealthy, RuntimeMetricsEnabled: true}
	stub := &stubFluxQuerier{
		lastRows:  []map[string]any{influxRow("ep", "decode_tokens_per_second", 40.0, newest)},
		tokenRows: []map[string]any{tokenRow("ep", "123")},
	}
	clock := newest
	p := newProviderForTest(stub, []*domain.Endpoint{ep}, func() time.Time { return clock })
	p.refresh(context.Background())
	require.True(t, func() bool { _, ok := p.LifetimeTokens("ep"); return ok }())
	require.NotNil(t, p.Snapshot(ep))

	stub.tokenErr = errors.New("costs bucket on fire")
	clock = clock.Add(lifetimeTokensTTL + time.Second)
	p.refresh(context.Background())
	_, ok := p.LifetimeTokens("ep")
	assert.False(t, ok, "a failed lifetime query must not keep serving the pre-outage sum")
	assert.NotNil(t, p.Snapshot(ep), "the runtime legs are unaffected by the costs leg failing")

	// Recovery re-populates without a restart.
	stub.tokenErr = nil
	clock = clock.Add(lifetimeTokensTTL + time.Second)
	p.refresh(context.Background())
	v, ok := p.LifetimeTokens("ep")
	assert.True(t, ok)
	assert.Equal(t, int64(123), v)
}

// The empty-set guard: a fleet with no endpoints never sends an empty
// contains(set:[]) filter (Flux compile hazard) and reports nothing.
func TestInfluxProviderLifetimeTokensEmptySetGuard(t *testing.T) {
	p := newProviderForTest(&stubFluxQuerier{}, nil, time.Now)
	p.refreshLifetimeTokens(context.Background())
	assert.Empty(t, p.tokens, "empty fleet yields an empty map, not a crash")
	for _, q := range p.client.(*stubFluxQuerier).queries {
		assert.NotContains(t, q, "contains(set:[],")
	}
}

func TestFluxDurationRange(t *testing.T) {
	assert.Equal(t, "-730d", fluxDurationRange(730*24*time.Hour))
	assert.Equal(t, "-1d", fluxDurationRange(24*time.Hour))
	assert.Equal(t, "-36h", fluxDurationRange(36*time.Hour))
	assert.Equal(t, "-90m", fluxDurationRange(90*time.Minute))
	assert.Equal(t, "-45s", fluxDurationRange(45*time.Second))
	assert.Equal(t, "-730d", fluxDurationRange(0), "non-positive falls back to the retention default")
	assert.Equal(t, "-730d", fluxDurationRange(-time.Hour))
}

// The sampler has NO honest lifetime source (engine counters reset on
// restart), so it must answer "not known" - the payload then omits the key
// and the UI dashes it.
func TestSamplerHasNoLifetimeSource(t *testing.T) {
	s := newEndpointRuntimeMetricsSampler(&mockStatusEndpointRepository{}, nil)
	v, ok := s.LifetimeTokens("anything")
	assert.False(t, ok)
	assert.Equal(t, int64(0), v)
}
