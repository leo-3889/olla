package handlers

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/thushan/olla/internal/config"
	"github.com/thushan/olla/internal/core/domain"
	"github.com/thushan/olla/internal/logger"
)

// influxMetricsInterval matches the sampler cadence: the dashboard's poll
// budget and the ETag semantics are tuned to it.
const influxMetricsInterval = 10 * time.Second

// influxLastRange bounds the last-value probe; ollametrics writes on a
// 10s scrape, so anything older than this is a gap, not a slow cycle.
const influxLastRange = "-5m"

// influxWindowRange bounds the avg/max window queries (1m windows over the
// same reporting window the sampler uses, plus headroom for the window).
const influxWindowRange = "-11m"

// fluxQuerier is the seam the provider queries through (httptest stubs in
// unit tests replace the HTTP client).
type fluxQuerier interface {
	query(ctx context.Context, flux string) ([]map[string]any, error)
}

type influxEndpointState struct {
	metrics        []RuntimeMetric
	newest         time.Time
	availableSince time.Time
}

// influxMetricsProvider reads engine runtime metrics from the ollametrics
// InfluxDB (measurement llm_runtime) instead of scraping each engine. It is
// a runtimeMetricsProvider, so every Snapshot gate (opt-in, routable,
// dialect, stale bound) applies identically for both sources.
type influxMetricsProvider struct {
	repository domain.EndpointRepository
	logger     logger.StyledLogger
	client     fluxQuerier
	bucket     string
	staleAfter time.Duration
	now        func() time.Time

	mu       sync.RWMutex
	states   map[string]*influxEndpointState
	firstSee map[string]time.Time
}

func newInfluxMetricsProvider(influx config.InfluxSourceConfig, token string, repository domain.EndpointRepository, log logger.StyledLogger) *influxMetricsProvider {
	return &influxMetricsProvider{
		repository: repository,
		logger:     log,
		client:     newFluxClient(influx.URL, influx.Org, token),
		bucket:     influx.Bucket,
		staleAfter: influx.StaleAfter,
		now:        time.Now,
		states:     map[string]*influxEndpointState{},
		firstSee:   map[string]time.Time{},
	}
}

func (p *influxMetricsProvider) Start(ctx context.Context) {
	if p == nil {
		return
	}
	go func() {
		p.refresh(ctx)
		ticker := time.NewTicker(influxMetricsInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				p.refresh(ctx)
			}
		}
	}()
}

// trackedEndpoints returns the names of opted-in, routable endpoints with a
// resolvable dialect, plus the set of all repository names for pruning. The
// Influx endpoint tag equals the olla endpoint Name.
func (p *influxMetricsProvider) trackedEndpoints(ctx context.Context) ([]string, map[string]struct{}, bool) {
	endpoints, err := p.repository.GetAll(ctx)
	if err != nil {
		return nil, nil, false
	}
	var names []string
	live := make(map[string]struct{}, len(endpoints))
	for _, endpoint := range endpoints {
		if endpoint == nil {
			continue
		}
		live[endpoint.Name] = struct{}{}
		if !endpoint.RuntimeMetricsEnabled || runtimeMetricsKind(endpoint) == "" || !endpoint.Status.IsRoutable() {
			p.markUnavailable(endpoint.Name)
			continue
		}
		names = append(names, endpoint.Name)
	}
	return names, live, true
}

func (p *influxMetricsProvider) markUnavailable(name string) {
	p.mu.Lock()
	delete(p.states, name)
	p.mu.Unlock()
}

func (p *influxMetricsProvider) pruneLocked(live map[string]struct{}) {
	for name := range p.states {
		if _, ok := live[name]; !ok {
			delete(p.states, name)
			delete(p.firstSee, name)
		}
	}
}

// refresh runs the bounded Flux queries for one cycle and rebuilds all
// endpoint states. A fully failed cycle keeps the previous states; their
// age grows, which is exactly how the sampler treats a dead engine.
func (p *influxMetricsProvider) refresh(ctx context.Context) {
	if p == nil || p.repository == nil {
		return
	}
	names, live, ok := p.trackedEndpoints(ctx)
	if !ok {
		return
	}
	p.mu.Lock()
	p.pruneLocked(live)
	p.mu.Unlock()
	if len(names) == 0 {
		return
	}
	// One budget for the whole cycle (5 sequential queries) plus a hard
	// per-query cap: a slow influx can no longer starve the later queries
	// (they used to share a 2x budget, so Q4/Q5 died every cycle under a
	// slow query and the avg/max columns stayed permanently empty).
	cycleCtx, cancel := context.WithTimeout(ctx, 6*fluxClientTimeout)
	defer cancel()
	query := func(flux string) ([]map[string]any, error) {
		qctx, qcancel := context.WithTimeout(cycleCtx, fluxClientTimeout)
		defer qcancel()
		return p.client.query(qctx, flux)
	}
	set := fluxStringSet(names)

	lastRows, err := query(influxBase(p.bucket, influxLastRange, set) + "|> last()")
	gaugeMean, errMean := query(influxBase(p.bucket, influxWindowRange, set) + influxFieldFilter("decode_tokens_per_second", "prefill_tokens_per_second", "prefix_cache_hit_ratio", "kv_cache_usage_ratio", "draft_acceptance_ratio") + "|> aggregateWindow(every:1m, fn:mean, createEmpty:false)")
	counterMean, errCounter := query(influxBase(p.bucket, influxWindowRange, set) + influxFieldFilter("generation_tokens_total", "prompt_tokens_total") + "|> derivative(unit:1s, nonNegative:true) |> aggregateWindow(every:1m, fn:mean, createEmpty:false)")
	counterMax, errCounterMax := query(influxBase(p.bucket, influxWindowRange, set) + influxFieldFilter("generation_tokens_total", "prompt_tokens_total") + "|> derivative(unit:1s, nonNegative:true) |> aggregateWindow(every:1m, fn:max, createEmpty:false)")
	if err != nil && errMean != nil && errCounter != nil && errCounterMax != nil {
		if p.logger != nil {
			p.logger.Warn("metrics_source influx: all runtime queries failed; keeping previous samples", "error", err.Error())
		}
		return
	}
	gaugeMax, _ := query(influxBase(p.bucket, influxWindowRange, set) + influxFieldFilter("decode_tokens_per_second", "prefill_tokens_per_second", "prefix_cache_hit_ratio", "kv_cache_usage_ratio", "draft_acceptance_ratio") + "|> aggregateWindow(every:1m, fn:max, createEmpty:false)")

	series := map[string]*influxSeries{}
	absorbAll := func(rows []map[string]any, bucket string) {
		for _, row := range rows {
			name, _ := row["endpoint"].(string)
			if name == "" {
				continue
			}
			s := series[name]
			if s == nil {
				s = newInfluxSeries()
				series[name] = s
			}
			s.absorb(row, bucket)
		}
	}
	absorbAll(lastRows, "last")
	absorbAll(gaugeMean, "gaugeMean")
	absorbAll(gaugeMax, "gaugeMax")
	absorbAll(counterMean, "counterMean")
	absorbAll(counterMax, "counterMax")

	next := make(map[string]*influxEndpointState, len(series))
	for _, name := range names {
		s := series[name]
		if s == nil || s.newest.IsZero() {
			// No points inside the bounded ranges: report nothing rather
			// than resurrecting a sample that stopped arriving.
			continue
		}
		metrics := s.envelope()
		if len(metrics) == 0 {
			continue
		}
		p.mu.Lock()
		since, seen := p.firstSee[name]
		if !seen {
			since = s.newest
			p.firstSee[name] = since
		}
		p.mu.Unlock()
		next[name] = &influxEndpointState{metrics: metrics, newest: s.newest, availableSince: since}
	}
	p.mu.Lock()
	for name, state := range next {
		p.states[name] = state
	}
	p.mu.Unlock()
}

// Snapshot applies the same gates as the sampler: opted-in, routable, known
// dialect; stale after staleAfter; gone entirely past the max stale age.
func (p *influxMetricsProvider) Snapshot(endpoint *domain.Endpoint) *EndpointRuntimeMetrics {
	if p == nil || endpoint == nil || !endpoint.RuntimeMetricsEnabled || runtimeMetricsKind(endpoint) == "" || !endpoint.Status.IsRoutable() {
		return nil
	}
	p.mu.RLock()
	state := p.states[endpoint.Name]
	p.mu.RUnlock()
	if state == nil || state.newest.IsZero() {
		return nil
	}
	age := p.now().Sub(state.newest)
	stale := age > p.staleAfter
	if stale && age > runtimeMetricsMaxStaleAge {
		return nil
	}
	ageSeconds := age.Seconds()
	return &EndpointRuntimeMetrics{
		SampledAt:      state.newest,
		AvailableSince: state.availableSince,
		Stale:          stale,
		Kind:           runtimeMetricsKind(endpoint),
		Source:         runtimeMetricsSourceInflux,
		SourceAgeS:     floatPtr(ageSeconds),
		Metrics:        state.metrics,
	}
}

// influxWindowAgg accumulates the per-1m-window rows one Flux
// aggregateWindow query returns per field series into the three numbers the
// envelope shows: average of window values, maximum window value, and the
// value of the newest window (the "current" for counter-derived rates).
type influxWindowAgg struct {
	sum, count, max float64
	latest          time.Time
	latestValue     float64
	has             bool
}

func (a *influxWindowAgg) add(at time.Time, v float64) {
	if !a.has {
		a.has, a.max = true, v
	}
	a.sum += v
	a.count++
	if v > a.max {
		a.max = v
	}
	if at.After(a.latest) {
		a.latest, a.latestValue = at, v
	}
}

func (a *influxWindowAgg) avg() (float64, bool) {
	if !a.has || a.count == 0 {
		return 0, false
	}
	return a.sum / a.count, true
}

// influxSeries collects one endpoint's rows across the five queries.
type influxSeries struct {
	last        map[string]float64
	lastAt      map[string]time.Time
	gaugeMean   map[string]*influxWindowAgg
	gaugeMax    map[string]*influxWindowAgg
	counterMean map[string]*influxWindowAgg
	counterMax  map[string]*influxWindowAgg
	newest      time.Time
}

func newInfluxSeries() *influxSeries {
	return &influxSeries{
		last:        map[string]float64{},
		lastAt:      map[string]time.Time{},
		gaugeMean:   map[string]*influxWindowAgg{},
		gaugeMax:    map[string]*influxWindowAgg{},
		counterMean: map[string]*influxWindowAgg{},
		counterMax:  map[string]*influxWindowAgg{},
	}
}

// absorb folds one result row into the series. The bucket names the query
// that produced it because last() rows and aggregateWindow rows are
// structurally identical column-wise.
func (s *influxSeries) absorb(row map[string]any, bucket string) {
	field, _ := row["_field"].(string)
	value, ok := influxFloat(row["_value"])
	if field == "" || !ok {
		return
	}
	at := influxTime(row["_time"])
	if at.After(s.newest) {
		s.newest = at
	}
	if bucket == "last" {
		if prevAt, seen := s.lastAt[field]; !seen || !at.Before(prevAt) {
			s.last[field], s.lastAt[field] = value, at
		}
		return
	}
	target := s.gaugeMean
	switch bucket {
	case "gaugeMax":
		target = s.gaugeMax
	case "counterMean":
		target = s.counterMean
	case "counterMax":
		target = s.counterMax
	}
	agg := target[field]
	if agg == nil {
		agg = &influxWindowAgg{}
		target[field] = agg
	}
	agg.add(at, value)
}

// envelope builds the self-describing metrics list, preferring the engine's
// own rate gauges (what ollametrics canonicalises) and falling back to a
// counter derivative only when the gauge family is absent. Ratios are
// stored 0..1 in Influx and percentised here, matching the sampler.
func (s *influxSeries) envelope() []RuntimeMetric {
	out := make([]RuntimeMetric, 0, 7)
	for _, pair := range []struct{ gaugeField, counterField, key string }{
		{"decode_tokens_per_second", "generation_tokens_total", runtimeMetricKeyGenerationTPS},
		{"prefill_tokens_per_second", "prompt_tokens_total", runtimeMetricKeyPrefillTPS},
	} {
		metric := RuntimeMetric{Key: pair.key, Unit: runtimeMetricUnitTokS}
		if current, ok := s.throughputCurrent(pair.gaugeField, pair.counterField); ok {
			metric.Current = floatPtr(current)
		}
		if avg, ok := s.throughputAvg(pair.gaugeField, pair.counterField); ok {
			metric.Avg = floatPtr(avg)
		}
		if max, ok := s.throughputMax(pair.gaugeField, pair.counterField); ok {
			metric.Max = floatPtr(max)
		}
		if metric.Current == nil && metric.Avg == nil && metric.Max == nil {
			continue
		}
		metric.Scope, metric.Note = s.throughputSemantics(pair.gaugeField)
		out = append(out, metric)
	}
	if v, ok := s.last["prefix_cache_hit_ratio"]; ok {
		metric := RuntimeMetric{Key: runtimeMetricKeyCacheHitPercent, Unit: runtimeMetricUnitPercent, Current: floatPtr(100 * v), Note: runtimeMetricNoteInfluxGauge}
		if agg := s.gaugeMean["prefix_cache_hit_ratio"]; agg != nil {
			if avg, ok := agg.avg(); ok {
				metric.Avg = floatPtr(100 * avg)
			}
		}
		out = append(out, metric)
	}
	if v, ok := s.last["kv_cache_usage_ratio"]; ok {
		out = append(out, RuntimeMetric{Key: runtimeMetricKeyKvCacheUsagePercent, Unit: runtimeMetricUnitPercent, Current: floatPtr(100 * v)})
	}
	if v, ok := s.last["queue_depth"]; ok {
		out = append(out, RuntimeMetric{Key: runtimeMetricKeyQueueDepth, Unit: runtimeMetricUnitCount, Current: floatPtr(v)})
	}
	if v, ok := s.last["requests_running"]; ok {
		out = append(out, RuntimeMetric{Key: runtimeMetricKeyRequestsRunning, Unit: runtimeMetricUnitCount, Current: floatPtr(v)})
	}
	if v, ok := s.last["draft_acceptance_ratio"]; ok {
		metric := RuntimeMetric{Key: runtimeMetricKeySpecAcceptPercent, Unit: runtimeMetricUnitPercent, Current: floatPtr(100 * v), AttachTo: runtimeMetricKeyGenerationTPS, Note: runtimeMetricNoteInfluxGauge}
		if agg := s.gaugeMean["draft_acceptance_ratio"]; agg != nil {
			if avg, ok := agg.avg(); ok {
				metric.Avg = floatPtr(100 * avg)
			}
		}
		out = append(out, metric)
	}
	return out
}

func (s *influxSeries) hasGaugeThroughput() bool {
	for _, field := range []string{"decode_tokens_per_second", "prefill_tokens_per_second"} {
		if _, ok := s.last[field]; ok {
			return true
		}
		if _, ok := s.gaugeMean[field]; ok {
			return true
		}
	}
	return false
}

// throughputCurrent: freshest value, gauge first; the counter derivative is
// consulted only when the gauge family is absent entirely, so a build that
// publishes both never shows a derived approximation.
func (s *influxSeries) throughputCurrent(gaugeField, counterField string) (float64, bool) {
	if v, ok := s.last[gaugeField]; ok {
		return v, true
	}
	if !s.hasGaugeThroughput() {
		if agg := s.counterMean[counterField]; agg != nil && agg.has {
			return agg.latestValue, true
		}
	}
	return 0, false
}

func (s *influxSeries) throughputAvg(gaugeField, counterField string) (float64, bool) {
	if agg := s.gaugeMean[gaugeField]; agg != nil {
		return agg.avg()
	}
	if !s.hasGaugeThroughput() {
		if agg := s.counterMean[counterField]; agg != nil {
			return agg.avg()
		}
	}
	return 0, false
}

func (s *influxSeries) throughputMax(gaugeField, counterField string) (float64, bool) {
	if agg := s.gaugeMax[gaugeField]; agg != nil && agg.has {
		return agg.max, true
	}
	if !s.hasGaugeThroughput() {
		if agg := s.counterMax[counterField]; agg != nil && agg.has {
			return agg.max, true
		}
	}
	return 0, false
}

// throughputSemantics states what the throughput number IS: an engine-wide
// batch rate when it comes from the engine's own gauges, a derived
// approximation when it comes from counter deltas.
func (s *influxSeries) throughputSemantics(gaugeField string) (string, string) {
	_, hasLast := s.last[gaugeField]
	_, hasMean := s.gaugeMean[gaugeField]
	if hasLast || hasMean || s.hasGaugeThroughput() {
		return runtimeMetricScopeEngine, joinNote(runtimeMetricNoteEngineThroughput, runtimeMetricNoteInfluxGauge)
	}
	return "", runtimeMetricNoteInfluxCounter
}

func influxBase(bucket, rng, set string) string {
	return fmt.Sprintf(`from(bucket:%q) |> range(start:%s) |> filter(fn:(r)=>r._measurement=="llm_runtime") |> filter(fn:(r)=>contains(set:[%s], value:r.endpoint))`, bucket, rng, set)
}

func influxFieldFilter(fields ...string) string {
	parts := make([]string, 0, len(fields))
	for _, f := range fields {
		parts = append(parts, fmt.Sprintf(`r._field==%q`, f))
	}
	return " |> filter(fn:(r)=>" + strings.Join(parts, " or ") + ")"
}

func fluxStringSet(names []string) string {
	quoted := make([]string, 0, len(names))
	for _, name := range names {
		quoted = append(quoted, fmt.Sprintf("%q", name))
	}
	return strings.Join(quoted, ",")
}

func influxFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	case string:
		var f float64
		if _, err := fmt.Sscanf(n, "%g", &f); err == nil {
			return f, true
		}
	}
	return 0, false
}

func influxTime(v any) time.Time {
	s, _ := v.(string)
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	return t
}
