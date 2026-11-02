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
	// pairedRows answers the RAW saturation-pair leg (requests_running +
	// queue_depth, no aggregateWindow): the Waiting headline joins the queue
	// value at the running peak's exact timestamp.
	pairedRows []map[string]any
	// Lifetime-tokens leg (runtime bucket, per-day counter integral),
	// dispatched on the difference(nonNegative: true) step. tokenRows
	// answers every day window; tokenRowsByDay answers the window whose
	// flux contains the key (a day's "start:<RFC3339>" literal); tokenErr
	// fails every day window and tokenErrDay fails only the window whose
	// flux contains that key (the error-day-skip case).
	tokenRows      []map[string]any
	tokenRowsByDay map[string][]map[string]any
	tokenErr       error
	tokenErrDay    string
	// tokenErrField fails ONLY the day-window queries naming that counter
	// field: one field's failure fails the whole day (the sums of the other
	// fields drop with it and stay disclosed lower bounds).
	tokenErrField string
	err           error
	legErrs       map[string]error // fail ONE leg: "last","gaugeMean","gaugeMax","counterMean","counterMax","paired"

	// tokenDeadline / legDeadline record the context budget the provider
	// gave the LAST lifetime-leg / runtime-leg query (the day-window
	// integral must not share the 5s interactive-poll cap).
	tokenDeadline time.Duration
	legDeadline   time.Duration
}

func (s *stubFluxQuerier) query(ctx context.Context, flux string) ([]map[string]any, error) {
	s.calls++
	s.queries = append(s.queries, flux)
	if dl, ok := ctx.Deadline(); ok {
		if strings.Contains(flux, "difference(nonNegative: true)") {
			s.tokenDeadline = time.Until(dl)
		} else {
			s.legDeadline = time.Until(dl)
		}
	}
	if s.err != nil {
		return nil, s.err
	}
	legError := func(leg string) error { return s.legErrs[leg] }
	switch {
	case strings.Contains(flux, "difference(nonNegative: true)"):
		if s.tokenErrField != "" && strings.Contains(flux, `r._field=="`+s.tokenErrField+`"`) {
			return nil, errors.New("field window query failed")
		}
		if s.tokenErrDay != "" && strings.Contains(flux, s.tokenErrDay) {
			return nil, errors.New("day window query failed")
		}
		if s.tokenErr != nil {
			return nil, s.tokenErr
		}
		for key, rows := range s.tokenRowsByDay {
			if strings.Contains(flux, key) {
				// The lifetime leg is ONE tag-equality query per endpoint per
				// FIELD: answer with the rows whose endpoint AND field the
				// query names. A row without _field is field-agnostic (the
				// pre-split fixture: every counter returns the same number).
				filtered := make([]map[string]any, 0, len(rows))
				for _, row := range rows {
					name, _ := row["endpoint"].(string)
					if !strings.Contains(flux, `r.endpoint=="`+name+`"`) {
						continue
					}
					if field, tagged := row["_field"].(string); tagged && !strings.Contains(flux, `r._field=="`+field+`"`) {
						continue
					}
					filtered = append(filtered, row)
				}
				return filtered, nil
			}
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
	case strings.Contains(flux, `"requests_running"`) && !strings.Contains(flux, "aggregateWindow"):
		// The raw saturation-pair leg (gauge legs carry the same field
		// INSIDE aggregateWindow filters and matched above).
		if e := legError("paired"); e != nil {
			return nil, e
		}
		return s.pairedRows, nil
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
		repository:     &mockStatusEndpointRepository{endpoints: endpoints},
		client:         stub,
		bucket:         "llm/metrics",
		tokenLookback:  180 * 24 * time.Hour,
		tokenTTL:       lifetimeTokensTTL,
		staleAfter:     60 * time.Second,
		now:            now,
		kvBasisCounter: fmt.Sprintf("%s ratio of cached/(cached+computed) prompt counter integrals", tokenWindowLabel(180*24*time.Hour)),
		states:         map[string]*influxEndpointState{},
		firstSee:       map[string]time.Time{},
		legFailing:     map[string]bool{},
		tokenDays:      map[string]map[string]tokenDayPart{},
		tokenDayAt:     map[string]time.Time{},
		tokenDayFails:  map[string]tokenDayFailure{},
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
		if !strings.Contains(q, "difference(nonNegative: true)") {
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

// The all-fail branch used to Warn every 10s cycle while influx stayed down
// (6/hr log spam). It is now a leg of the same transition-disciplined noteLeg
// mechanism as the partial legs.
func TestInfluxProviderAllFailLoggedOnTransition(t *testing.T) {
	fresh := time.Date(2026, 10, 13, 10, 0, 0, 0, time.UTC)
	ep := &domain.Endpoint{Name: "ep", URLString: "http://ep", Type: "vllm", Status: domain.StatusHealthy, RuntimeMetricsEnabled: true}
	stub := &stubFluxQuerier{lastRows: []map[string]any{influxRow("ep", "queue_depth", 1.0, fresh)}}
	p := newProviderForTest(stub, []*domain.Endpoint{ep}, func() time.Time { return fresh })
	rec := &influxLegLogger{}
	p.logger = rec
	p.refresh(context.Background())

	// Every runtime leg fails while the tracked-endpoint probe still answers
	// (stub.err would fail that too, short-circuiting before the legs).
	five := map[string]error{"last": errors.New("down"), "gaugeMean": errors.New("down"), "gaugeMax": errors.New("down"), "counterMean": errors.New("down"), "counterMax": errors.New("down")}
	stub.legErrs = five
	p.refresh(context.Background())
	p.refresh(context.Background())
	p.refresh(context.Background())
	allFails := 0
	for _, w := range rec.warns {
		if strings.Contains(w.msg, "query leg failing") && strings.Contains(fmt.Sprint(w.args...), "legall") {
			allFails++
		}
	}
	assert.Equal(t, 1, allFails, "all-fail is logged on transition, not per cycle")

	stub.legErrs = map[string]error{}
	p.refresh(context.Background())
	p.refresh(context.Background())
	recoveries := 0
	for _, i := range rec.infos {
		if strings.Contains(i.msg, "query leg recovered") && strings.Contains(fmt.Sprint(i.args...), "legall") {
			recoveries++
		}
	}
	assert.Equal(t, 1, recoveries, "recovery of the all-fail leg is disclosed once")
}

// tokenRow renders one summed row of the day-window integral flux (the
// group(columns:["endpoint"]) |> sum() output: endpoint + _value per table).
// It is FIELD-AGNOSTIC: the stub answers every field's query with it, i.e. an
// engine whose counters all read the same number. Tests that need the fields
// to differ use tokenFieldRow.
func tokenRow(endpoint string, value any) map[string]any {
	return map[string]any{"endpoint": endpoint, "_value": value}
}

// tokenFieldRow is one field's summed day row: the stub answers ONLY the query
// naming that field, which is how the split-by-field contract is pinned.
func tokenFieldRow(endpoint, field string, value any) map[string]any {
	return map[string]any{"endpoint": endpoint, "_field": field, "_value": value}
}

// countFieldDayQueries counts the day-window queries for ONE counter field -
// the per-field fan-out (each field is its own pushdown-restored query).
func countFieldDayQueries(stub *stubFluxQuerier, dayStart, field string) int {
	n := 0
	for _, q := range stub.queries {
		if strings.Contains(q, "start:"+dayStart) && strings.Contains(q, `r._field=="`+field+`"`) {
			n++
		}
	}
	return n
}

// countDayQueries counts the lifetime-leg queries whose day window opens at
// the given UTC RFC3339 instant - the per-day cache arithmetic.
func countDayQueries(stub *stubFluxQuerier, dayStart string) int {
	n := 0
	for _, q := range stub.queries {
		if strings.Contains(q, "difference(nonNegative: true)") && strings.Contains(q, "start:"+dayStart) {
			n++
		}
	}
	return n
}

func countLifetimeQueries(stub *stubFluxQuerier) int {
	n := 0
	for _, q := range stub.queries {
		if strings.Contains(q, "difference(nonNegative: true)") {
			n++
		}
	}
	return n
}

func dayWindowEndpoint(name string) *domain.Endpoint {
	return &domain.Endpoint{Name: name, URLString: "http://" + name, Type: "openai-compatible", RuntimeMetricsKind: "radiance", Status: domain.StatusHealthy, RuntimeMetricsEnabled: true}
}

// The committed day-window flux: runtime bucket, ONE counter field per
// FULL tag identity in the group key (model/runtime tags split a series on
// engine restart and an interleaved series corrupts every later delta),
// difference(nonNegative: true) for restart-safe deltas (this influxd rejects
// difference(n:)), one summed row per endpoint (ONE tag-equality query per
// endpoint per field), and NO trace of the cost ledger.
func TestInfluxProviderLifetimeTokensFluxShape(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	stub := &stubFluxQuerier{}
	p := newProviderForTest(stub, []*domain.Endpoint{dayWindowEndpoint("radiance-9700-64")}, func() time.Time { return now })
	p.refresh(context.Background())

	var q string
	for _, s := range stub.queries {
		if strings.Contains(s, "difference(nonNegative: true)") {
			q = s
			break
		}
	}
	require.NotEmpty(t, q, "the lifetime leg ran")
	assert.Contains(t, q, `from(bucket:"llm/metrics")`, "the runtime bucket, not the cost ledger")
	assert.Contains(t, q, "range(start:2026-10-20T00:00:00Z, stop:2026-10-21T00:00:00Z)", "one UTC calendar day window")
	assert.Contains(t, q, `r._measurement=="llm_runtime"`)
	assert.Contains(t, q, `r.endpoint=="radiance-9700-64"`, "tag-equality endpoint filter (pushdown): one query per endpoint")
	assert.Contains(t, q, `filter(fn:(r)=>r._field=="generation_tokens_total")`, "ONE counter field as a single equality in its OWN filter")
	// The current day plus the one backfilled closed day: each day is queried
	// exactly once PER FIELD - never a combined sum, never a repeated field.
	for _, day := range []string{"2026-10-20T00:00:00Z", "2026-10-19T00:00:00Z"} {
		for _, field := range tokenDayFields {
			assert.Equal(t, 1, countFieldDayQueries(stub, day, field),
				"exactly one day-window query per field for %s, never a combined sum", day)
		}
	}
	assert.NotContains(t, q, "contains(", "NO function call in any filter: it disables pushdown for the whole filter (measured 44.45 CPU-s/day vs 1.62)")
	assert.Contains(t, q, `group(columns:["endpoint","_field","model","runtime","engine_id","engine","node","role","display_name"])`, "the full tag identity: a restart splits model/runtime tags")
	assert.Contains(t, q, `difference(nonNegative: true)`, "restart resets clamp to 0")
	assert.Contains(t, q, `sum(column:"_value") |> group(columns:["endpoint"]) |> sum()`)
	assert.NotContains(t, q, "llm_endpoint_cost_v3", "the cost ledger is no longer the token source")
	assert.NotContains(t, q, "period", "no cost-period filter survives")
}

// The runtime base window (all five legs): the endpoint set renders as an
// OR of tag equalities, never contains(set:[...]) - a function call in a
// filter expression disables pushdown for the WHOLE filter, so the read
// scans every llm_runtime series of the window. The empty set keeps the
// matches-nothing semantics of the old contains(set:[]) without the Flux
// compile hazard; refresh()'s len==0 early return is the empty-set policy.
func TestInfluxBaseRendersTagEqualityEndpointFilter(t *testing.T) {
	base := influxBase("llm/metrics", []string{"radiance-9700-64", "halogen-395"})
	assert.Contains(t, base, `filter(fn:(r)=>r._measurement=="llm_runtime")`)
	assert.Contains(t, base, `(r.endpoint=="radiance-9700-64" or r.endpoint=="halogen-395")`)
	assert.NotContains(t, base, "contains(", "no function call: pushdown of the comparisons must survive")
	assert.NotContains(t, base, " and ", "the endpoint set is one disjunction, not a conjunction")

	empty := influxBase("llm/metrics", nil)
	assert.Contains(t, empty, "filter(fn:(r)=>false)", "empty set matches nothing, exactly as contains(set:[]) did")
	assert.NotContains(t, empty, "set:[]")
}

// Ghost exclusion on the new source: rows whose endpoint is not configured
// are never credited (server-side contains() AND the Go set guard), and an
// endpoint with no counter rows (comfyui/voice endpoints export none) is not
// known - a dash, never a fabricated 0.
func TestInfluxProviderLifetimeTokensGhostExclusionAndPresence(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	r64 := dayWindowEndpoint("radiance-9700-64")
	h395 := &domain.Endpoint{Name: "halogen-395", URLString: "http://h", Type: "llamacpp", RuntimeMetricsKind: "llamacpp", Status: domain.StatusHealthy, RuntimeMetricsEnabled: true}
	// radiance-9700-128: configured, ZERO counter rows (comfyui/voice shape).
	r128 := dayWindowEndpoint("radiance-9700-128")
	stub := &stubFluxQuerier{tokenRowsByDay: map[string][]map[string]any{
		"start:2026-10-20T00:00:00Z": {
			tokenRow("radiance-9700-64", "2192454947"),
			tokenRow("halogen-395", "97467683"),
			// Ghost ids: the -64 pre-rename id and a retired endpoint.
			// Per-endpoint equality queries never NAME them (the stub
			// answers only what each query asks), and the Go-side set
			// guard is the second layer: they are never credited.
			tokenRow("radiance-qfn", "1414912324"),
			tokenRow("vllm-qwen38", "104651862"),
		},
	}}
	p := newProviderForTest(stub, []*domain.Endpoint{r64, h395, r128}, func() time.Time { return now })
	p.refresh(context.Background())

	v, ok := p.LifetimeGeneration("radiance-9700-64")
	require.True(t, ok)
	assert.Equal(t, int64(2192454947), v, "exact day integral, string-parsed")
	v, ok = p.LifetimeGeneration("halogen-395")
	require.True(t, ok)
	assert.Equal(t, int64(97467683), v)
	_, ok = p.LifetimeGeneration("radiance-9700-128")
	assert.False(t, ok, "no counter rows => not known (dash), never a fake 0")
	_, ok = p.LifetimeGeneration("radiance-qfn")
	assert.False(t, ok, "ghost ids are never credited to a current endpoint")
	_, ok = p.LifetimeGeneration("vllm-qwen38")
	assert.False(t, ok, "retired endpoint rows are dropped")
	_, ok = p.LifetimePrefill("radiance-qfn")
	assert.False(t, ok, "the ghost is dropped from the prefill sum too")
	_, ok = p.LifetimePrefill("radiance-9700-64")
	assert.True(t, ok, "a configured endpoint with counter rows is known")
}

// Day-part arithmetic: a CLOSED day is computed exactly ONCE (cached
// permanently - its data can no longer change), the CURRENT day refreshes at
// the TTL, the served number is the SUM of the parts inside token_lookback,
// and parts outside the lookback are evicted.
func TestInfluxProviderLifetimeTokensDayPartsCachedAndSummed(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	stub := &stubFluxQuerier{tokenRowsByDay: map[string][]map[string]any{
		"start:2026-10-20T00:00:00Z": {tokenRow("ep", 500)}, // current day
		"start:2026-10-19T00:00:00Z": {tokenRow("ep", 700)}, // newest closed
	}}
	clock := now
	p := newProviderForTest(stub, []*domain.Endpoint{dayWindowEndpoint("ep")}, func() time.Time { return clock })
	// A closed day integrated in an earlier life: permanent cache.
	p.tokenDays["2026-10-10"] = map[string]tokenDayPart{"ep": {gen: 1000, hasGen: true}}
	p.tokenDayAt["2026-10-10"] = clock.Add(-48 * time.Hour)
	// A part far outside the 180d lookback: evicted, never summed.
	p.tokenDays["2020-01-01"] = map[string]tokenDayPart{"ep": {gen: 999999, hasGen: true}}
	p.tokenDayAt["2020-01-01"] = clock.Add(-48 * time.Hour)

	p.refresh(context.Background())
	v, ok := p.LifetimeGeneration("ep")
	require.True(t, ok)
	assert.Equal(t, int64(2200), v, "seeded closed day + current day + one backfilled closed day")
	assert.Equal(t, len(tokenDayFields), countDayQueries(stub, "2026-10-19T00:00:00Z"), "one query per counter field")
	assert.Equal(t, len(tokenDayFields), countDayQueries(stub, "2026-10-20T00:00:00Z"))
	_, cached := p.tokenDays["2020-01-01"]
	assert.False(t, cached, "parts outside token_lookback are evicted from the cache")

	// Inside the TTL a refresh adds NO current-day query (and never a closed
	// -day one): the 10s cycle must not re-fan day windows.
	clock = clock.Add(10 * time.Second)
	p.refresh(context.Background())
	assert.Equal(t, len(tokenDayFields), countDayQueries(stub, "2026-10-20T00:00:00Z"), "the current day is TTL-gated")

	// Past the TTL the current day refreshes and the newer number wins; a
	// CLOSED day is never re-queried.
	clock = clock.Add(lifetimeTokensTTL)
	stub.tokenRowsByDay["start:2026-10-20T00:00:00Z"] = []map[string]any{tokenRow("ep", 800)}
	p.refresh(context.Background())
	v, ok = p.LifetimeGeneration("ep")
	require.True(t, ok)
	assert.Equal(t, int64(2500), v, "1000 + 700 + the refreshed current-day part 800")
	assert.Equal(t, 2*len(tokenDayFields), countDayQueries(stub, "2026-10-20T00:00:00Z"))
	assert.Equal(t, len(tokenDayFields), countDayQueries(stub, "2026-10-19T00:00:00Z"), "a CLOSED day is queried exactly ONCE per field")
}

// A day whose query ERRORS is SKIPPED from the sum - the number is then a
// best-effort LOWER bound, disclosed on transition - and a later cycle
// retries the day. One bad day never crashes the poll and never touches the
// runtime states (the observe partial-failure pattern).
func TestInfluxProviderLifetimeTokensErrorDaySkippedAndRetried(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	newest := now.Add(-5 * time.Second)
	stub := &stubFluxQuerier{
		lastRows: []map[string]any{influxRow("ep", "decode_tokens_per_second", 40.0, newest)},
		tokenRowsByDay: map[string][]map[string]any{
			"start:2026-10-20T00:00:00Z": {tokenRow("ep", 500)},
			"start:2026-10-19T00:00:00Z": {tokenRow("ep", 700)},
		},
	}
	clock := now
	p := newProviderForTest(stub, []*domain.Endpoint{dayWindowEndpoint("ep")}, func() time.Time { return clock })
	p.tokenLookback = 24 * time.Hour // window = 19th + 20th: a closable set
	rec := &influxLegLogger{}
	p.logger = rec

	// The newest closed day fails: cycle 1 serves the current day ALONE.
	stub.tokenErrDay = "start:2026-10-19T00:00:00Z"
	p.refresh(context.Background())
	v, ok := p.LifetimeGeneration("ep")
	require.True(t, ok, "a surviving day part keeps the column serving")
	assert.Equal(t, int64(500), v, "the failed day is skipped from the sum, never faked")
	assert.True(t, p.tokenGap, "the lower-bound state is disclosed")
	assert.NotNil(t, p.Snapshot(dayWindowEndpoint("ep")), "the runtime legs are unaffected by a failing day")

	assert.Equal(t, 1, countDayQueries(stub, "2026-10-19T00:00:00Z"), "the failing field leg aborts the day: the remaining fields are not queried")

	// Negative cache: the next cycle inside the backoff must NOT retry
	// the day (one broken day must not burn the cycle budget every 10s -
	// the measured retry storm), and the disclosure persists.
	clock = clock.Add(10 * time.Second)
	p.refresh(context.Background())
	assert.Equal(t, 1, countDayQueries(stub, "2026-10-19T00:00:00Z"), "a failed day is not retried inside the backoff")
	v, ok = p.LifetimeGeneration("ep")
	require.True(t, ok)
	assert.Equal(t, int64(500), v, "the day stays excluded while negative-cached")
	assert.True(t, p.tokenGap, "the lower bound persists while the day is unaccounted")

	// Past the backoff the day is retried: recovery joins the sum and
	// the disclosure clears.
	stub.tokenErrDay = ""
	clock = clock.Add(tokenDayRetryBackoff)
	p.refresh(context.Background())
	v, ok = p.LifetimeGeneration("ep")
	require.True(t, ok)
	assert.Equal(t, int64(1200), v, "the retried closed day joins the sum")
	assert.False(t, p.tokenGap, "every window day is integrated again")
	warns, infos := 0, 0
	for _, w := range rec.warns {
		if strings.Contains(w.msg, "best-effort") {
			warns++
		}
	}
	for _, in := range rec.infos {
		if strings.Contains(in.msg, "complete") {
			infos++
		}
	}
	assert.Equal(t, 1, warns, "the lower bound is logged on transition, not per cycle")
	assert.Equal(t, 1, infos, "the recovery is logged on transition")
}

// Negative-cache arithmetic: a failed day retries at exactly ONE attempt
// per tokenDayRetryBackoff, is abandoned at tokenDayMaxAttempts, and the
// honesty contract (day excluded, lower bound disclosed) holds throughout;
// success clears the record (covered by the recovery path above).
func TestInfluxProviderTokenDayNegativeCacheBackoffAndGiveUp(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	stub := &stubFluxQuerier{
		tokenRowsByDay: map[string][]map[string]any{
			"start:2026-10-20T00:00:00Z": {tokenRow("ep", 500)},
		},
		tokenErrDay: "start:2026-10-19T00:00:00Z",
	}
	clock := now
	p := newProviderForTest(stub, []*domain.Endpoint{dayWindowEndpoint("ep")}, func() time.Time { return clock })
	p.tokenLookback = 24 * time.Hour // window = 19th + 20th

	p.refresh(context.Background())
	v, ok := p.LifetimeGeneration("ep")
	require.True(t, ok)
	assert.Equal(t, int64(500), v)
	require.Equal(t, 1, p.tokenDayFails["2026-10-19"].attempts)
	assert.True(t, p.tokenDayFails["2026-10-19"].nextTry.Equal(now.Add(tokenDayRetryBackoff)), "retry arms at +1h")

	for i := 2; i <= tokenDayMaxAttempts; i++ {
		clock = clock.Add(tokenDayRetryBackoff)
		p.refresh(context.Background())
		assert.Equal(t, i, p.tokenDayFails["2026-10-19"].attempts, "one attempt per backoff")
		assert.Equal(t, i, countDayQueries(stub, "2026-10-19T00:00:00Z"), "one attempt per backoff (the failing leg aborts the day)")
		assert.Equal(t, int64(500), p.genTokens["ep"], "the failed day stays excluded from the sum")
		assert.True(t, p.tokenGap, "the lower bound is disclosed while the day is unaccounted")
	}

	// Give up: at the attempt cap the day is NEVER queried again - even
	// healthy - and the gap disclosure is the standing honest state.
	stub.tokenErrDay = ""
	clock = clock.Add(2 * tokenDayRetryBackoff)
	p.refresh(context.Background())
	assert.Equal(t, tokenDayMaxAttempts, countDayQueries(stub, "2026-10-19T00:00:00Z"), "a day abandoned at the cap is not retried")
	assert.True(t, p.tokenGap)
}

// The day leg gets its OWN budget: fluxClientTimeout (5s) bounds the
// interactive poll, and applying it to the background integral livelocked
// the leg (18s day query always cancelled -> influxd 499s -> retried every
// cycle forever). The day query must run under tokenDayTimeout; the
// runtime legs keep the interactive cap.
func TestInfluxProviderTokenDayOwnDeadlineBudget(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	stub := &stubFluxQuerier{}
	p := newProviderForTest(stub, []*domain.Endpoint{dayWindowEndpoint("ep")}, func() time.Time { return now })
	p.refresh(context.Background())
	require.NotZero(t, stub.tokenDeadline, "the day leg runs under an explicit deadline")
	assert.InDelta(t, float64(tokenDayTimeout), float64(stub.tokenDeadline), float64(2*time.Second), "the day gets the 60s background budget")
	require.NotZero(t, stub.legDeadline)
	assert.Less(t, stub.legDeadline, tokenDayTimeout/2, "runtime legs keep the 5s interactive budget")
}

// Cold boot with the source down: NO day part exists, so the column is not
// known at all (dash) - the provider never serves numbers from a window it
// cannot see, and the runtime tiles are unaffected.
func TestInfluxProviderLifetimeTokensFullFailureDashesAndIsIsolated(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	newest := now.Add(-5 * time.Second)
	stub := &stubFluxQuerier{
		lastRows: []map[string]any{influxRow("ep", "decode_tokens_per_second", 40.0, newest)},
		tokenErr: errors.New("influx down"),
	}
	clock := now
	p := newProviderForTest(stub, []*domain.Endpoint{dayWindowEndpoint("ep")}, func() time.Time { return clock })
	p.tokenLookback = 24 * time.Hour
	p.refresh(context.Background())
	_, ok := p.LifetimeGeneration("ep")
	assert.False(t, ok, "no day part at all => the key is absent (UI dash)")
	assert.NotNil(t, p.Snapshot(dayWindowEndpoint("ep")), "the runtime legs are unaffected")

	stub.tokenErr = nil
	clock = clock.Add(tokenDayRetryBackoff + lifetimeTokensTTL + 10*time.Second)
	stub.tokenRowsByDay = map[string][]map[string]any{"start:2026-10-20T00:00:00Z": {tokenRow("ep", 42)}}
	p.refresh(context.Background())
	v, ok := p.LifetimeGeneration("ep")
	assert.True(t, ok, "recovery re-populates without a restart")
	assert.Equal(t, int64(42), v)
}

// The cold-boot bound: at most tokenBackfillPerCycle CLOSED day windows are
// integrated per refresh. 180 day windows x seconds each must never fan in
// one cycle (the refresh loop would block for many minutes).
func TestInfluxProviderLifetimeTokensBackfillBudget(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	stub := &stubFluxQuerier{}
	clock := now
	p := newProviderForTest(stub, []*domain.Endpoint{dayWindowEndpoint("ep")}, func() time.Time { return clock })
	p.refresh(context.Background())
	assert.Equal(t, (1+tokenBackfillPerCycle)*len(tokenDayFields), countLifetimeQueries(stub), "the first cycle: (current day + one closed day) x fields")
	clock = clock.Add(10 * time.Second)
	p.refresh(context.Background())
	clock = clock.Add(10 * time.Second)
	p.refresh(context.Background())
	assert.Equal(t, (1+3*tokenBackfillPerCycle)*len(tokenDayFields), countLifetimeQueries(stub), "each later cycle adds exactly the backfill budget")
}

// The empty-set guard: a fleet with no endpoints never sends an empty
// contains(set:[]) filter (Flux compile hazard) and reports nothing.
func TestInfluxProviderLifetimeTokensEmptySetGuard(t *testing.T) {
	p := newProviderForTest(&stubFluxQuerier{}, nil, time.Now)
	p.refreshLifetimeTokens(context.Background())
	assert.Empty(t, p.genTokens, "empty fleet yields no sums, not a crash")
	assert.Empty(t, p.prefillTokens)
	assert.Empty(t, p.cachedTokens)
	for _, q := range p.client.(*stubFluxQuerier).queries {
		// Both argument orders the provider renders (the runtime legs use
		// contains(set:[...], value:), the lifetime flux contains(value:,
		// set:[...])): an empty set is the compile hazard under test.
		assert.NotContains(t, q, "set:[],")
	}
}

// Per-cycle dedup (plan P3): two refreshes inside one cache window issue
// every distinct Flux text ONCE at influxd - the shared llm_runtime window
// is the dominant influxd cost and must not multiply per consumer. The
// payload contract is untouched: the keyed envelope still renders from the
// cached rows.
func TestInfluxProviderCycleDedupsIdenticalFlux(t *testing.T) {
	now := time.Date(2026, 10, 13, 10, 0, 0, 0, time.UTC)
	ep := dayWindowEndpoint("ep")
	stub := &stubFluxQuerier{lastRows: []map[string]any{influxRow("ep", "decode_tokens_per_second", 40.0, now.Add(-time.Second))}}
	p := newProviderForTest(stub, []*domain.Endpoint{ep}, func() time.Time { return now })
	p.client = newFluxResultCache(stub, fluxResultCacheTTL)
	p.refresh(context.Background())
	first := stub.calls
	require.NotZero(t, first)

	p.refresh(context.Background())
	// The second cycle advances the LAZY day backfill - a NEW day window
	// is legitimately its own query - but NO text is ever issued twice
	// inside one cache window: the five runtime legs and the first-cycle
	// day windows are all served from the single-flight cache.
	seen := map[string]int{}
	for _, q := range stub.queries {
		seen[q]++
	}
	for q, n := range seen {
		assert.Equal(t, 1, n, "identical Flux issued %d times in one cache window: %s", n, q)
	}
	assert.Equal(t, first+len(tokenDayFields), stub.calls, "exactly the next backfill day's field queries are extra")
	got := p.Snapshot(ep)
	require.NotNil(t, got, "the envelope contract survives the cache")
	assert.NotNil(t, envMetric(t, got, runtimeMetricKeyGenerationTPS))
}

// The sampler has NO honest lifetime source (engine counters reset on
// restart), so it must answer "not known" - the payload then omits the key
// and the UI dashes it.
func TestSamplerHasNoLifetimeSource(t *testing.T) {
	s := newEndpointRuntimeMetricsSampler(&mockStatusEndpointRepository{}, nil)
	gen, ok := s.LifetimeGeneration("anything")
	assert.False(t, ok)
	assert.Equal(t, int64(0), gen)
	pre, ok := s.LifetimePrefill("anything")
	assert.False(t, ok, "the sampler holds raw counters that reset on restart")
	assert.Equal(t, int64(0), pre)
	pct, ok := s.KVCacheReusePercent("anything")
	assert.False(t, ok, "the sampler has no integrals and no gauge history")
	assert.Equal(t, float64(0), pct)
}

// THE SPLIT CONTRACT. A fixture with DISJOINT generation / prefill / cached
// series proves the three sums are computed independently - prefill is never
// folded into generation - and that a field an endpoint does not publish stays
// "not known" (key absent, UI dash) instead of a fabricated 0.
func TestInfluxProviderTokenFieldsAreSplitAndIndependent(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	radiance := dayWindowEndpoint("radiance-9700-64")
	halogen := &domain.Endpoint{Name: "halogen-395", URLString: "http://h", Type: "llamacpp", RuntimeMetricsKind: "llamacpp", Status: domain.StatusHealthy, RuntimeMetricsEnabled: true}
	stub := &stubFluxQuerier{tokenRowsByDay: map[string][]map[string]any{
		"start:2026-10-20T00:00:00Z": {
			tokenFieldRow("radiance-9700-64", tokenFieldGeneration, 1000),
			tokenFieldRow("radiance-9700-64", tokenFieldPrefill, 9000),
			tokenFieldRow("halogen-395", tokenFieldGeneration, 10),
			tokenFieldRow("halogen-395", tokenFieldPrefill, 40),
			tokenFieldRow("halogen-395", tokenFieldCached, 8),
		},
		"start:2026-10-19T00:00:00Z": {
			tokenFieldRow("radiance-9700-64", tokenFieldGeneration, 500),
			tokenFieldRow("radiance-9700-64", tokenFieldPrefill, 4500),
		},
	}}
	clock := now
	p := newProviderForTest(stub, []*domain.Endpoint{radiance, halogen}, func() time.Time { return clock })
	p.tokenLookback = 48 * time.Hour
	p.refresh(context.Background())

	gen, ok := p.LifetimeGeneration("radiance-9700-64")
	require.True(t, ok)
	assert.Equal(t, int64(1500), gen, "generation = the GEN day parts only")
	pre, ok := p.LifetimePrefill("radiance-9700-64")
	require.True(t, ok)
	assert.Equal(t, int64(13500), pre, "prefill is its own sum, never folded into generation")
	assert.NotEqual(t, gen, pre, "the two columns are disjoint by construction")
	_, ok = p.cachedTokens["radiance-9700-64"]
	assert.False(t, ok, "an engine publishing no cached counter has NO cached sum (unset, not 0)")

	hgen, ok := p.LifetimeGeneration("halogen-395")
	require.True(t, ok)
	assert.Equal(t, int64(10), hgen, "a day with no rows for this endpoint adds nothing")
	hpre, ok := p.LifetimePrefill("halogen-395")
	require.True(t, ok)
	assert.Equal(t, int64(40), hpre)
	pct, ok := p.KVCacheReusePercent("halogen-395")
	require.True(t, ok, "llama.cpp family: no gauge, so the counter ratio answers")
	// prompt_tokens_total counts COMPUTED tokens only, so the denominator
	// is cached + computed (mirrors the sampler's recordRatio): 8/(8+40).
	assert.InDelta(t, 100*8.0/48.0, pct, 1e-9, "cached/(cached+computed) over the window = 8/48")
}

// T1 (measured halogen-395): the OLD denominator cached/prefill produced
// 1,490,274,227/131,638,184 = 1132%, served clamped 100.0 where the truth
// is 91.9%. With cached/(cached+computed) the numerator is a term of the
// denominator, so the ratio CANNOT exceed 100 by construction - this test
// fails against the old formula without any clamp being involved.
func TestInfluxProviderKVReuseRatioCannotExceedHundredByConstruction(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	halogen := &domain.Endpoint{Name: "halogen-395", URLString: "http://h", Type: "llamacpp", RuntimeMetricsKind: "llamacpp", Status: domain.StatusHealthy, RuntimeMetricsEnabled: true}
	const cached, computed = int64(1_490_274_227), int64(131_638_184)
	stub := &stubFluxQuerier{tokenRowsByDay: map[string][]map[string]any{
		"start:2026-10-20T00:00:00Z": {
			tokenFieldRow("halogen-395", tokenFieldPrefill, computed),
			tokenFieldRow("halogen-395", tokenFieldCached, cached),
		},
	}}
	p := newProviderForTest(stub, []*domain.Endpoint{halogen}, func() time.Time { return now })
	p.refresh(context.Background())

	pct, ok := p.KVCacheReusePercent("halogen-395")
	require.True(t, ok)
	assert.InDelta(t, 100*float64(cached)/float64(cached+computed), pct, 1e-9, "the true ratio: 91.9%, not the clamped arithmetic lie")
	assert.Less(t, pct, kvReusePercentBound, "below 100 by CONSTRUCTION, not by clamping")
	assert.Contains(t, p.KVCacheReuseBasis("halogen-395"), "cached/(cached+computed)")
}

// S3 + S4: the Running headline is the window MAX (1m-max leg) and the
// Waiting headline is queue_depth sampled AT the running peak's timestamp,
// both gauges written per scrape at identical timestamps.
func TestInfluxProviderRunningWindowMaxAndPairedWaiting(t *testing.T) {
	newest := time.Date(2026, 10, 13, 10, 0, 0, 0, time.UTC)
	t1 := newest.Add(-3 * time.Minute)
	t2 := newest.Add(-2 * time.Minute)
	t3 := newest.Add(-time.Minute)
	radiance := &domain.Endpoint{Name: "rad", URLString: "http://r", Type: "openai-compatible", RuntimeMetricsKind: "radiance", Status: domain.StatusHealthy, RuntimeMetricsEnabled: true}
	stub := &stubFluxQuerier{
		lastRows: []map[string]any{
			influxRow("rad", "requests_running", 2.0, t3),
			influxRow("rad", influxFieldQueueDepth, 9.0, t3),
		},
		gaugeMaxRows: []map[string]any{
			influxRow("rad", "requests_running", 5.0, newest),
		},
		pairedRows: []map[string]any{
			influxRow("rad", "requests_running", 1.0, t1),
			influxRow("rad", influxFieldQueueDepth, 9.0, t1),
			influxRow("rad", "requests_running", 3.0, t2),
			influxRow("rad", influxFieldQueueDepth, 3.0, t2),
			influxRow("rad", "requests_running", 2.0, t3),
			influxRow("rad", influxFieldQueueDepth, 7.0, t3),
		},
	}
	p := newProviderForTest(stub, []*domain.Endpoint{radiance}, func() time.Time { return newest })
	p.refresh(context.Background())

	got := p.Snapshot(radiance)
	require.NotNil(t, got)
	running := envMetric(t, got, runtimeMetricKeyRequestsRunning)
	require.NotNil(t, running)
	require.NotNil(t, running.Max, "the 1m-max leg flows the window maximum")
	assert.InDelta(t, 5.0, *running.Max, .001)
	assert.InDelta(t, 2.0, *running.Current, .001, "current stays the newest point")

	queue := envMetric(t, got, runtimeMetricKeyQueueDepth)
	require.NotNil(t, queue)
	require.NotNil(t, queue.PairedValue, "the queue value at the running peak's timestamp")
	assert.InDelta(t, 3.0, *queue.PairedValue, .001, "argmax running=3 at t2: the queue THERE, not the current 9")
	assert.Equal(t, runtimeMetricKeyRequestsRunning, queue.PairedOf)
	assert.Equal(t, t2.UTC().Format(time.RFC3339Nano), queue.PairedAt)
	assert.InDelta(t, 9.0, *queue.Current, .001, "current stays for the Prometheus re-export")
}

// The pair is ALL-OR-NOTHING: no queue point at the peak's exact timestamp
// (or a dead raw leg) leaves the paired keys ABSENT - the UI dashes, it
// never falls back onto the current queue.
func TestInfluxProviderPairedWaitingAbsentNeverFallsBack(t *testing.T) {
	newest := time.Date(2026, 10, 13, 10, 0, 0, 0, time.UTC)
	t1 := newest.Add(-2 * time.Minute)
	t2 := newest.Add(-time.Minute)
	radiance := &domain.Endpoint{Name: "rad", URLString: "http://r", Type: "openai-compatible", RuntimeMetricsKind: "radiance", Status: domain.StatusHealthy, RuntimeMetricsEnabled: true}
	stub := &stubFluxQuerier{
		lastRows: []map[string]any{
			influxRow("rad", "requests_running", 2.0, t2),
			influxRow("rad", influxFieldQueueDepth, 9.0, t2),
		},
		pairedRows: []map[string]any{
			influxRow("rad", "requests_running", 1.0, t1),
			influxRow("rad", influxFieldQueueDepth, 9.0, t1),
			influxRow("rad", "requests_running", 4.0, t2),
			// NO queue_depth row at t2 (the peak): unpaired.
		},
	}
	p := newProviderForTest(stub, []*domain.Endpoint{radiance}, func() time.Time { return newest })
	p.refresh(context.Background())
	got := p.Snapshot(radiance)
	require.NotNil(t, got)
	queue := envMetric(t, got, runtimeMetricKeyQueueDepth)
	require.NotNil(t, queue)
	assert.Nil(t, queue.PairedValue, "no queue point at the peak timestamp: absent, not the current 9")
	assert.Empty(t, queue.PairedOf)
	assert.Empty(t, queue.PairedAt)
}

// T3: a skipped day window marks the served token sums as a disclosed LOWER
// BOUND in the payload (the disclosure used to live only in gateway logs).
func TestInfluxProviderLifetimeTokensPartialFlag(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	radiance := dayWindowEndpoint("ep")
	stub := &stubFluxQuerier{tokenErr: errors.New("day window query failed")}
	p := newProviderForTest(stub, []*domain.Endpoint{radiance}, func() time.Time { return now })
	p.tokenLookback = 24 * time.Hour
	p.refresh(context.Background())
	assert.True(t, p.LifetimeTokensPartial(), "a day window missing from the sum is a disclosed lower bound")

	complete := &stubFluxQuerier{tokenRows: []map[string]any{tokenFieldRow("ep", tokenFieldGeneration, 10)}}
	p2 := newProviderForTest(complete, []*domain.Endpoint{radiance}, func() time.Time { return now })
	p2.tokenLookback = 24 * time.Hour
	p2.refresh(context.Background())
	assert.False(t, p2.LifetimeTokensPartial(), "every window day integrated: the sums are exact")
}

// KV-reuse basis 1: the engine's own prefix_cache_hit_ratio gauge (radiance).
// The gauge x100 wins outright and the cached-counter ratio is NEVER consulted
// - in this fleet the cached counter does not reproduce the gauge (measured:
// 15.1M/187M = 8% against a 0.93 gauge), so a fixture pins both numbers and
// demands the gauge. A gauge older than stale_after stops answering rather
// than falling back onto a basis the engine does not report.
func TestInfluxProviderKVCacheReuseGaugeBasis(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	newest := now.Add(-3 * time.Second)
	radiance := dayWindowEndpoint("radiance-9700-64")
	stub := &stubFluxQuerier{
		lastRows: []map[string]any{
			influxRow("radiance-9700-64", "decode_tokens_per_second", 40.0, newest),
			influxRow("radiance-9700-64", "prefix_cache_hit_ratio", 0.93, newest),
		},
		tokenRowsByDay: map[string][]map[string]any{
			"start:2026-10-20T00:00:00Z": {
				tokenFieldRow("radiance-9700-64", tokenFieldGeneration, 1000),
				tokenFieldRow("radiance-9700-64", tokenFieldPrefill, 9000),
				tokenFieldRow("radiance-9700-64", tokenFieldCached, 720),
			},
		},
	}
	clock := now
	p := newProviderForTest(stub, []*domain.Endpoint{radiance}, func() time.Time { return clock })
	p.refresh(context.Background())

	pct, ok := p.KVCacheReusePercent("radiance-9700-64")
	require.True(t, ok)
	assert.InDelta(t, 93.0, pct, 1e-9, "the gauge x100, not the counter ratio")
	ratio := 100 * float64(p.cachedTokens["radiance-9700-64"]) / float64(p.prefillTokens["radiance-9700-64"])
	assert.InDelta(t, 8.0, ratio, 1e-9, "the counter ratio IS computable here and is a DIFFERENT number")

	clock = newest.Add(90 * time.Second)
	_, ok = p.KVCacheReusePercent("radiance-9700-64")
	assert.False(t, ok, "a gauge older than stale_after stops answering")
}

// KV-reuse honesty: a ratio needs BOTH sides over the same window. Cached rows
// with no prefill sum, a prefill sum of zero, and an endpoint with neither
// signal are all "not known" - 0% would claim the cache never hits, and a
// division by an invented denominator is a number the data cannot support.
// An out-of-range gauge (a documented 0..1 ratio reading above 1) clamps.
func TestInfluxProviderKVCacheReuseNeedsBothSidesAndClamps(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	cachedOnly := &domain.Endpoint{Name: "cached-only", URLString: "http://c", Type: "llamacpp", RuntimeMetricsKind: "llamacpp", Status: domain.StatusHealthy, RuntimeMetricsEnabled: true}
	prefillOnly := &domain.Endpoint{Name: "prefill-only", URLString: "http://p", Type: "llamacpp", RuntimeMetricsKind: "llamacpp", Status: domain.StatusHealthy, RuntimeMetricsEnabled: true}
	neither := &domain.Endpoint{Name: "voice-395", URLString: "http://v", Type: "openai-compatible", RuntimeMetricsKind: "radiance", Status: domain.StatusHealthy, RuntimeMetricsEnabled: true}
	broken := dayWindowEndpoint("broken-gauge")
	stub := &stubFluxQuerier{
		lastRows: []map[string]any{influxRow("broken-gauge", "prefix_cache_hit_ratio", 1.4, now)},
		tokenRowsByDay: map[string][]map[string]any{
			"start:2026-10-20T00:00:00Z": {
				tokenFieldRow("cached-only", tokenFieldCached, 8),
				tokenFieldRow("prefill-only", tokenFieldPrefill, 40),
			},
		},
	}
	p := newProviderForTest(stub, []*domain.Endpoint{cachedOnly, prefillOnly, neither, broken}, func() time.Time { return now })
	p.refresh(context.Background())

	_, ok := p.KVCacheReusePercent("cached-only")
	assert.False(t, ok, "no prefill denominator => not known")
	_, ok = p.KVCacheReusePercent("prefill-only")
	assert.False(t, ok, "no cached numerator => not known")
	_, ok = p.KVCacheReusePercent("voice-395")
	assert.False(t, ok, "no token counters and no gauge => not known")
	pct, ok := p.KVCacheReusePercent("broken-gauge")
	require.True(t, ok)
	assert.Equal(t, 100.0, pct, "a ratio gauge above 1 is a backend bug and clamps to 100")
}

// One field's leg failing fails the WHOLE day: the day part is dropped from
// every sum, each field's served number stays a disclosed lower bound
// (tokenGap), and the day is retried - the same honesty machinery the single
// sum used, now applied with three fields in play.
func TestInfluxProviderTokenDayFieldFailureDropsEverySum(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	stub := &stubFluxQuerier{
		tokenRowsByDay: map[string][]map[string]any{
			"start:2026-10-20T00:00:00Z": {
				tokenFieldRow("ep", tokenFieldGeneration, 500),
				tokenFieldRow("ep", tokenFieldPrefill, 4000),
			},
		},
		tokenErrField: tokenFieldPrefill,
	}
	clock := now
	p := newProviderForTest(stub, []*domain.Endpoint{dayWindowEndpoint("ep")}, func() time.Time { return clock })
	p.tokenLookback = 24 * time.Hour
	p.refresh(context.Background())

	_, ok := p.LifetimeGeneration("ep")
	assert.False(t, ok, "a day whose PREFILL leg failed is dropped from the GEN sum too")
	assert.True(t, p.tokenGap, "the sums are disclosed lower bounds while the day is unaccounted")
	assert.Equal(t, 1, countFieldDayQueries(stub, "2026-10-20T00:00:00Z", tokenFieldPrefill))

	// Recovery: the retry integrates the day and both sums serve again.
	stub.tokenErrField = ""
	clock = clock.Add(tokenDayRetryBackoff + lifetimeTokensTTL + time.Second)
	p.refresh(context.Background())
	gen, ok := p.LifetimeGeneration("ep")
	require.True(t, ok)
	assert.Equal(t, int64(500), gen)
	pre, ok := p.LifetimePrefill("ep")
	require.True(t, ok)
	assert.Equal(t, int64(4000), pre)
	// Day 19 has no rows in the fixture at all, so it stays a disclosed
	// gap; the recovery step is about the sums, not the gap flag.
}
