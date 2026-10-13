package handlers

import (
	"bufio"
	"context"
	"fmt"
	"hash"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/thushan/olla/internal/core/domain"
	"github.com/thushan/olla/internal/logger"
	"github.com/thushan/olla/internal/util"
)

const runtimeMetricsInterval = 10 * time.Second
const runtimeMetricsTimeout = 3 * time.Second

// runtimeMetricsWindow is the reporting window over which the per-signal
// average and maximum are computed (the dashboard graph window). `current`
// is always the latest sample; avg and max cover samples inside this window.
const runtimeMetricsWindow = 10 * time.Minute

// runtimeMetricsIdleWake caps how long the sampler waits when no endpoint is
// due sooner: opt-ins and endpoints added by a config reload are picked up by
// the next wake at the latest.
const runtimeMetricsIdleWake = 30 * time.Second

// runtimeMetricsMinWake floors the wake delay so an endpoint configured with
// a scrape interval below the scheduling granularity cannot spin the loop.
const runtimeMetricsMinWake = 100 * time.Millisecond

// runtimeMetricsMaxBackoff caps the retry delay after consecutive scrape
// failures: a permanently dead /metrics must not be polled forever at full
// frequency.
const runtimeMetricsMaxBackoff = time.Minute

// runtimeMetricsMaxStaleAge bounds stale retention: once the last good sample
// is older than this and scrapes keep failing, the endpoint reports no runtime
// metrics at all again instead of a hopelessly stale snapshot.
const runtimeMetricsMaxStaleAge = 15 * time.Minute

// runtimeMetricsBodyLimit bounds one scraped exposition.
const runtimeMetricsBodyLimit = 4 << 20

// EndpointRuntimeMetrics is the compact, capability-aware runtime view shown
// beside an endpoint. Nil fields mean the engine did not publish that signal.
type EndpointRuntimeMetrics struct {
	SampledAt      time.Time       `json:"sampled_at"`
	AvailableSince time.Time       `json:"available_since"`
	Stale          bool            `json:"stale"`
	Kind           string          `json:"kind,omitempty"`
	// Source names who produced the numbers: "sampler" (this process
	// scraping the engine) or "ollametrics" (canonical InfluxDB series).
	Source string `json:"source"`
	// SourceAgeS is seconds since the newest underlying sample.
	SourceAgeS *float64 `json:"source_age_s,omitempty"`
	Metrics    []RuntimeMetric `json:"metrics,omitempty"`
}

type endpointRuntimeMetricsSampler struct {
	repository  domain.EndpointRepository
	logger      logger.StyledLogger
	client      *http.Client
	now         func() time.Time
	mu          sync.RWMutex
	states      map[string]*runtimeMetricsState
	warnedKinds map[string]struct{}
}

type runtimeMetricsState struct {
	available       bool
	availableSince  time.Time
	sampledAt       time.Time
	stale           bool
	last            runtimeCounters
	hasLast         bool
	generation      throughputAggregate
	prefill         throughputAggregate
	cache           ratioAggregate
	specAccept      ratioAggregate
	kvUsage         *float64
	queueDepth      *float64
	requestsRunning *float64
	failures        int
	nextAttempt     time.Time
}

type runtimeSample struct {
	at    time.Time
	value float64
}

type throughputAggregate struct {
	current *float64
	avg     *float64
	max     *float64
	samples []runtimeSample
}
type ratioAggregate struct {
	current *float64
	avg     *float64
	samples []runtimeSample
}

// appendWindowed appends a sample and drops anything older than the
// reporting window.
func appendWindowed(samples []runtimeSample, at time.Time, value float64) []runtimeSample {
	return trimWindowed(append(samples, runtimeSample{at: at, value: value}), at)
}

// trimWindowed drops samples older than the reporting window.
func trimWindowed(samples []runtimeSample, at time.Time) []runtimeSample {
	cutoff := at.Add(-runtimeMetricsWindow)
	i := 0
	for i < len(samples) && samples[i].at.Before(cutoff) {
		i++
	}
	return samples[i:]
}

// windowStats derives the reporting-window average and maximum from the
// retained samples.
func windowStats(samples []runtimeSample) (avg, max *float64) {
	if len(samples) == 0 {
		return nil, nil
	}
	sum := 0.0
	m := samples[0].value
	for _, s := range samples {
		sum += s.value
		if s.value > m {
			m = s.value
		}
	}
	return floatPtr(sum / float64(len(samples))), floatPtr(m)
}

type runtimeCounters struct {
	generationTokens, generationSeconds      *float64
	prefillTokens, prefillSeconds            *float64
	cacheHits, cacheQueries                  *float64
	cachedPromptTokens, computedPromptTokens *float64
	kvUsage                                  *float64
	// Backend-computed instantaneous gauges, used for a signal whose
	// counter pair is absent from the exposition (radiance publishes native
	// rate gauges alongside - or instead of - its counters).
	genRate, prefillRate, cacheRatePercent *float64
	// Saturation and speculative-decode gauges (radiance for now).
	queueDepth, requestsRunning, specAcceptPercent *float64
}

func newEndpointRuntimeMetricsSampler(repository domain.EndpointRepository, l logger.StyledLogger) *endpointRuntimeMetricsSampler {
	// The client carries no timeout of its own: each scrape is bounded by its
	// per-endpoint context (see refreshEndpoint), which honours the configured
	// runtime_metrics.timeout and is cancelled on shutdown. A client-level
	// Timeout would cap every scrape at a fixed value and silently ignore any
	// configured per-endpoint timeout larger than it.
	return &endpointRuntimeMetricsSampler{repository: repository, logger: l, client: &http.Client{}, now: time.Now, states: make(map[string]*runtimeMetricsState), warnedKinds: make(map[string]struct{})}
}

// Start runs the sampling loop until ctx is cancelled: refresh, then sleep
// until the earliest endpoint deadline (bounded by the idle cap). Every
// scrape goroutine is joined before the loop re-sleeps or exits, so shutdown
// cannot leak one.
func (s *endpointRuntimeMetricsSampler) Start(ctx context.Context) {
	if s == nil || s.repository == nil {
		return
	}
	go func() {
		for {
			delay := s.refresh(ctx)
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		}
	}()
}

// Refresh samples every due opted-in endpoint. Exported for manual kicks and
// tests; Start drives the loop through refresh.
func (s *endpointRuntimeMetricsSampler) Refresh(ctx context.Context) { s.refresh(ctx) }

// refresh scrapes all due endpoints concurrently (N serial scrapes would
// serialize N timeouts into the wake budget) and returns how long the caller
// may wait before the next one is due.
func (s *endpointRuntimeMetricsSampler) refresh(ctx context.Context) time.Duration {
	if s == nil || s.repository == nil {
		return runtimeMetricsIdleWake
	}
	endpoints, err := s.repository.GetAll(ctx)
	if err != nil {
		return runtimeMetricsIdleWake
	}
	s.pruneStates(endpoints)
	var wg sync.WaitGroup
	for _, endpoint := range endpoints {
		if endpoint == nil {
			continue
		}
		if !endpoint.RuntimeMetricsEnabled || runtimeMetricsKind(endpoint) == "" {
			if endpoint.RuntimeMetricsEnabled {
				// An enabled opt-in whose dialect cannot be detected would
				// otherwise die silently at this very skip line.
				s.warnUnknownKindOnce("", endpoint.Name)
			}
			// Opting out (or losing dialect detection) must also invalidate
			// whatever state the endpoint left behind, or the last
			// pre-opt-out snapshot keeps serving as "fresh" forever.
			s.markUnavailable(endpoint.URLString)
			continue
		}
		if !endpoint.Status.IsRoutable() {
			s.markUnavailable(endpoint.URLString)
			continue
		}
		if !s.shouldRefresh(endpoint) {
			continue
		}
		wg.Add(1)
		go func(endpoint *domain.Endpoint) {
			defer wg.Done()
			s.refreshEndpoint(ctx, endpoint)
		}(endpoint)
	}
	wg.Wait()
	return s.wakeDelay(endpoints)
}

// pruneStates drops state for endpoints no longer in the repository (removed
// or URL-changed by a config reload) so the map cannot accumulate stale keys
// forever.
func (s *endpointRuntimeMetricsSampler) pruneStates(endpoints []*domain.Endpoint) {
	live := make(map[string]struct{}, len(endpoints))
	for _, endpoint := range endpoints {
		if endpoint != nil {
			live[endpoint.URLString] = struct{}{}
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for key := range s.states {
		if _, ok := live[key]; !ok {
			delete(s.states, key)
		}
	}
}

func endpointScrapeInterval(endpoint *domain.Endpoint) time.Duration {
	if endpoint.RuntimeMetricsInterval > 0 {
		return endpoint.RuntimeMetricsInterval
	}
	return runtimeMetricsInterval
}

func (s *endpointRuntimeMetricsSampler) shouldRefresh(endpoint *domain.Endpoint) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return !s.now().Before(s.nextDueLocked(endpoint))
}

// nextDueLocked returns when the endpoint next wants a scrape: one interval
// after the last successful sample, pushed out to the retry deadline while a
// scrape-failure streak is backing off. Zero means never sampled: due now.
// Callers hold s.mu.
func (s *endpointRuntimeMetricsSampler) nextDueLocked(endpoint *domain.Endpoint) time.Time {
	state := s.states[endpoint.URLString]
	if state == nil {
		return time.Time{}
	}
	var due time.Time
	if !state.sampledAt.IsZero() {
		due = state.sampledAt.Add(endpointScrapeInterval(endpoint))
	}
	if state.nextAttempt.After(due) {
		due = state.nextAttempt
	}
	return due
}

// wakeDelay reports how long the sampler may sleep before the earliest
// endpoint is due. Bounded by the idle cap so config changes are still picked
// up, floored so a due-now or sub-granularity interval cannot spin the loop.
func (s *endpointRuntimeMetricsSampler) wakeDelay(endpoints []*domain.Endpoint) time.Duration {
	now := s.now()
	delay := runtimeMetricsIdleWake
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, endpoint := range endpoints {
		if endpoint == nil || !endpoint.RuntimeMetricsEnabled || runtimeMetricsKind(endpoint) == "" || !endpoint.Status.IsRoutable() {
			continue
		}
		due := s.nextDueLocked(endpoint)
		if due.IsZero() {
			return runtimeMetricsMinWake
		}
		if d := due.Sub(now); d < delay {
			delay = d
		}
	}
	if delay < runtimeMetricsMinWake {
		delay = runtimeMetricsMinWake
	}
	return delay
}

func (s *endpointRuntimeMetricsSampler) refreshEndpoint(parent context.Context, endpoint *domain.Endpoint) {
	timeout := runtimeMetricsTimeout
	if endpoint.RuntimeMetricsTimeout > 0 {
		timeout = endpoint.RuntimeMetricsTimeout
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	path := endpoint.RuntimeMetricsPath
	if path == "" {
		path = "/metrics"
	}
	url := util.ResolveURLPath(endpoint.URLString, path)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		s.noteFailure(endpoint, err.Error())
		return
	}
	for name, value := range endpoint.Headers {
		req.Header.Set(name, value)
	}
	if endpoint.AuthHeaderName != "" && endpoint.AuthHeaderValue != "" {
		req.Header.Set(endpoint.AuthHeaderName, endpoint.AuthHeaderValue)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		s.noteFailure(endpoint, err.Error())
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		s.noteFailure(endpoint, "unexpected status "+resp.Status)
		return
	}
	kind := runtimeMetricsKind(endpoint)
	s.warnUnknownKindOnce(kind, endpoint.Name)
	// Stream the exposition: a whole-body io.ReadAll turns every scrape into
	// a multi-MB string copy on top of the parse pass itself.
	counters, err := parseRuntimeCountersFrom(kind, io.LimitReader(resp.Body, runtimeMetricsBodyLimit))
	if err == nil {
		// Probe past the body limit: a surviving byte means the exposition
		// was cut mid-stream, and half a counter family would parse as a
		// decreased (reset) counter and wipe the window epoch.
		var probe [1]byte
		if n, _ := io.ReadFull(resp.Body, probe[:]); n > 0 {
			err = fmt.Errorf("exposition exceeds %d byte scrape limit", runtimeMetricsBodyLimit)
		}
	}
	if err != nil {
		s.noteFailure(endpoint, err.Error())
		return
	}
	s.record(endpoint.URLString, s.now(), counters)
}

func (s *endpointRuntimeMetricsSampler) markUnavailable(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if state := s.states[key]; state != nil {
		state.available = false
		state.hasLast = false
	}
}

// noteFailure records a scrape failure: the last snapshot (when one exists)
// is marked stale and the retry backoff deadline is armed - the scrape
// interval doubled per consecutive failure, capped at runtimeMetricsMaxBackoff.
// The reason is debug-logged once per streak, not once per retried attempt.
// State is created on first failure so a never-sampled endpoint backs off too
// instead of being retried at every wake.
func (s *endpointRuntimeMetricsSampler) noteFailure(endpoint *domain.Endpoint, reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	state := s.states[endpoint.URLString]
	if state == nil {
		state = &runtimeMetricsState{}
		s.states[endpoint.URLString] = state
	}
	state.failures++
	delay := endpointScrapeInterval(endpoint)
	for i := 0; i < state.failures && delay < runtimeMetricsMaxBackoff; i++ {
		delay *= 2
	}
	if delay > runtimeMetricsMaxBackoff {
		delay = runtimeMetricsMaxBackoff
	}
	state.nextAttempt = s.now().Add(delay)
	if state.available {
		state.stale = true
	}
	if s.logger != nil && state.failures == 1 {
		s.logger.Debug("endpoint runtime metrics scrape failed; backing off", "endpoint", endpoint.Name, "reason", reason, "retry_in", delay.String())
	}
}

// warnUnknownKindOnce logs one debug line per unrecognised kind value (e.g. a
// typo in runtime_metrics.kind). parseRuntimeCounters then samples nothing for
// it rather than silently guessing a flavour.
func (s *endpointRuntimeMetricsSampler) warnUnknownKindOnce(kind, endpointName string) {
	if s.logger == nil || isKnownRuntimeKind(kind) {
		return
	}
	// Undetected dialects (empty kind) are keyed per endpoint so one
	// misconfigured endpoint cannot mask the warning for the others;
	// non-empty unknown values keep their warn-once-per-value semantics.
	key := kind
	if key == "" {
		key = "\x00" + endpointName
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, done := s.warnedKinds[key]; done {
		return
	}
	s.warnedKinds[key] = struct{}{}
	if kind == "" {
		s.logger.Debug("runtime metrics enabled but no backend dialect detected; sampling nothing", "endpoint", endpointName)
		return
	}
	s.logger.Debug("unknown runtime metrics kind; sampling nothing", "endpoint", endpointName, "kind", kind)
}

func (s *endpointRuntimeMetricsSampler) record(key string, at time.Time, next runtimeCounters) {
	s.mu.Lock()
	defer s.mu.Unlock()
	state := s.states[key]
	if state == nil {
		state = &runtimeMetricsState{}
		s.states[key] = state
	}
	if !state.available || (state.hasLast && countersReset(state.last, next)) {
		*state = runtimeMetricsState{available: true, availableSince: at}
	}
	state.sampledAt, state.stale = at, false
	state.failures, state.nextAttempt = 0, time.Time{}
	if state.hasLast {
		recordThroughputSignal(&state.generation, at, state.last.generationTokens, next.generationTokens, state.last.generationSeconds, next.generationSeconds, next.genRate)
		recordThroughputSignal(&state.prefill, at, state.last.prefillTokens, next.prefillTokens, state.last.prefillSeconds, next.prefillSeconds, next.prefillRate)
		recordRatioSignal(&state.cache, at, state.last, next)
	} else {
		// First sample: counter deltas have nothing to diff against. Record
		// the gauge only when the exposition lacks the counter pair at all -
		// otherwise the first delta lands next scrape, and mixing one gauge
		// value into an otherwise pure counter window would skew avg/max.
		if next.generationTokens == nil || next.generationSeconds == nil {
			recordGaugeThroughput(&state.generation, at, next.genRate)
		}
		if next.prefillTokens == nil || next.prefillSeconds == nil {
			recordGaugeThroughput(&state.prefill, at, next.prefillRate)
		}
		if (next.cacheHits == nil || next.cacheQueries == nil) && (next.cachedPromptTokens == nil || next.computedPromptTokens == nil) {
			recordGaugeRatio(&state.cache, at, next.cacheRatePercent)
		}
	}
	recordGaugeRatio(&state.specAccept, at, next.specAcceptPercent)
	state.last, state.hasLast = next, true
	state.kvUsage, state.queueDepth, state.requestsRunning = next.kvUsage, next.queueDepth, next.requestsRunning
}

// recordGaugeThroughput folds a backend-computed rate gauge into the same
// aggregate as counter deltas. The gauge value is appended to the window -
// not just set as current - so avg/max and the Prometheus re-exports behave
// uniformly for both sources; windowing the engine's own instantaneous rate
// client-side is the honest way to graph it. recordThroughputSignal keeps
// the sources apart per signal: while a backend publishes the counter pair,
// only counter deltas enter the window - the gauge is never folded in
// alongside.
func recordGaugeThroughput(a *throughputAggregate, at time.Time, rate *float64) {
	if rate == nil {
		return
	}
	a.current = copyFloat(rate)
	a.samples = appendWindowed(a.samples, at, *rate)
	a.avg, a.max = windowStats(a.samples)
}

// recordGaugeRatio is the ratio counterpart of recordGaugeThroughput (same windowing rationale).
func recordGaugeRatio(a *ratioAggregate, at time.Time, percent *float64) {
	if percent == nil {
		return
	}
	a.current = copyFloat(percent)
	a.samples = appendWindowed(a.samples, at, *percent)
	a.avg, _ = windowStats(a.samples)
}

func (s *endpointRuntimeMetricsSampler) Snapshot(endpoint *domain.Endpoint) *EndpointRuntimeMetrics {
	if s == nil || endpoint == nil || !endpoint.RuntimeMetricsEnabled || runtimeMetricsKind(endpoint) == "" || !endpoint.Status.IsRoutable() {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	state := s.states[endpoint.URLString]
	if state == nil || !state.available || state.sampledAt.IsZero() {
		return nil
	}
	// Bounded stale retention: a snapshot kept alive only by the stale flag
	// stops being renderable past the max stale age.
	if state.stale && s.now().Sub(state.sampledAt) > runtimeMetricsMaxStaleAge {
		return nil
	}
	age := s.now().Sub(state.sampledAt).Seconds()
	return &EndpointRuntimeMetrics{SampledAt: state.sampledAt, AvailableSince: state.availableSince, Stale: state.stale, Kind: runtimeMetricsKind(endpoint),
		Source: runtimeMetricsSourceSampler, SourceAgeS: floatPtr(age), Metrics: buildEnvelopeMetrics(state, runtimeMetricsKind(endpoint))}
}

// recordThroughput reports whether a delta sample was recorded.
func recordThroughput(a *throughputAggregate, at time.Time, oldTokens, newTokens, oldSeconds, newSeconds *float64) bool {
	if oldTokens == nil || newTokens == nil || oldSeconds == nil || newSeconds == nil {
		return false
	}
	tokens, seconds := *newTokens-*oldTokens, *newSeconds-*oldSeconds
	if tokens < 0 || seconds <= 0 {
		return false
	}
	a.current = floatPtr(tokens / seconds)
	a.samples = appendWindowed(a.samples, at, *a.current)
	a.avg, a.max = windowStats(a.samples)
	return true
}

// recordRatio reports whether a delta ratio was recorded.
func recordRatio(a *ratioAggregate, at time.Time, old, next runtimeCounters) bool {
	var oldN, newN, oldD, newD *float64
	if old.cacheHits != nil && next.cacheHits != nil && old.cacheQueries != nil && next.cacheQueries != nil {
		oldN, newN, oldD, newD = old.cacheHits, next.cacheHits, old.cacheQueries, next.cacheQueries
	} else if old.cachedPromptTokens != nil && next.cachedPromptTokens != nil && old.computedPromptTokens != nil && next.computedPromptTokens != nil {
		// llama.cpp's prompt_tokens_total counts only COMPUTED (non-cached)
		// prompt tokens, while prompt_tokens_cached_total counts cached ones,
		// so the hit rate is cached / (cached + computed).
		oldN, newN = old.cachedPromptTokens, next.cachedPromptTokens
		oldD = floatPtr(*old.cachedPromptTokens + *old.computedPromptTokens)
		newD = floatPtr(*next.cachedPromptTokens + *next.computedPromptTokens)
	} else {
		return false
	}
	n, d := *newN-*oldN, *newD-*oldD
	if n < 0 || d <= 0 {
		return false
	}
	a.current = floatPtr(100 * n / d)
	a.samples = appendWindowed(a.samples, at, *a.current)
	a.avg, _ = windowStats(a.samples)
	return true
}

// ratioCountersPresent reports whether either cache-hit counter pair
// (hits/queries, or llama.cpp's cached/computed) is available on both
// samples, i.e. a delta ratio is computable in principle.
func ratioCountersPresent(old, next runtimeCounters) bool {
	return (old.cacheHits != nil && next.cacheHits != nil && old.cacheQueries != nil && next.cacheQueries != nil) ||
		(old.cachedPromptTokens != nil && next.cachedPromptTokens != nil && old.computedPromptTokens != nil && next.computedPromptTokens != nil)
}

// recordThroughputSignal records one throughput signal from a scrape pair.
// While the backend publishes the counter pair on both samples, counters are
// authoritative: the delta is recorded, and when the delta is not computable
// (idle engine: no elapsed decode seconds) the stale current is DROPPED
// instead of substituting the engine's rate gauge - that gauge would fold an
// instantaneous engine-wide value into a counter-delta window and present the
// last burst (or the gauge) as a live measurement. The gauge is used only
// when the pair is absent, mirroring the first-sample branch.
func recordThroughputSignal(a *throughputAggregate, at time.Time, oldTokens, newTokens, oldSeconds, newSeconds, gauge *float64) {
	if oldTokens != nil && newTokens != nil && oldSeconds != nil && newSeconds != nil {
		if !recordThroughput(a, at, oldTokens, newTokens, oldSeconds, newSeconds) {
			clearThroughputCurrent(a, at)
		}
		return
	}
	recordGaugeThroughput(a, at, gauge)
}

// recordRatioSignal is the ratio counterpart of recordThroughputSignal.
func recordRatioSignal(a *ratioAggregate, at time.Time, old, next runtimeCounters) {
	if ratioCountersPresent(old, next) {
		if !recordRatio(a, at, old, next) {
			clearRatioCurrent(a, at)
		}
		return
	}
	recordGaugeRatio(a, at, next.cacheRatePercent)
}

// clearThroughputCurrent drops the current rate after a scrape that produced
// no new measurement despite live counters, re-deriving window stats from
// samples still inside the reporting window so avg/max decay honestly during
// a prolonged idle instead of being presented as fresh indefinitely.
func clearThroughputCurrent(a *throughputAggregate, at time.Time) {
	a.current = nil
	a.samples = trimWindowed(a.samples, at)
	a.avg, a.max = windowStats(a.samples)
}

// clearRatioCurrent is the ratio counterpart of clearThroughputCurrent.
func clearRatioCurrent(a *ratioAggregate, at time.Time) {
	a.current = nil
	a.samples = trimWindowed(a.samples, at)
	a.avg, _ = windowStats(a.samples)
}
func countersReset(old, next runtimeCounters) bool {
	for _, p := range [][2]*float64{{old.generationTokens, next.generationTokens}, {old.generationSeconds, next.generationSeconds}, {old.prefillTokens, next.prefillTokens}, {old.prefillSeconds, next.prefillSeconds}, {old.cacheHits, next.cacheHits}, {old.cacheQueries, next.cacheQueries}, {old.cachedPromptTokens, next.cachedPromptTokens}, {old.computedPromptTokens, next.computedPromptTokens}} {
		if p[0] != nil && p[1] != nil && *p[1] < *p[0] {
			return true
		}
	}
	return false
}
func copyFloat(v *float64) *float64 {
	if v == nil {
		return nil
	}
	return floatPtr(*v)
}
func floatPtr(v float64) *float64 { return &v }

// canonicalRuntimeKind folds the accepted spellings of the sampled kinds onto
// their canonical ids; "" means unrecognised.
func canonicalRuntimeKind(s string) string {
	switch strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(strings.TrimSpace(s), "-", ""), ".", "")) {
	case "vllm":
		return "vllm"
	case "halogen", "llamacpp", "llamacppserver":
		return "llamacpp"
	case "radiance":
		return "radiance"
	}
	return ""
}

func isKnownRuntimeKind(kind string) bool {
	return canonicalRuntimeKind(kind) == kind && kind != ""
}

// runtimeKindNameFallbacks recognise backends reached through a generic
// endpoint type (e.g. openai-compatible) by an endpoint name marker.
var runtimeKindNameFallbacks = []struct {
	kind, marker string
}{
	{"llamacpp", "halogen"},
	{"radiance", "radiance"},
}

func runtimeMetricsKind(endpoint *domain.Endpoint) string {
	if endpoint == nil {
		return ""
	}
	// An explicit runtime_metrics.kind wins. An unrecognised value is passed
	// through (lower-cased) so the sampler logs it once instead of silently
	// folding it onto a flavour the operator never asked for.
	if k := strings.ToLower(strings.TrimSpace(endpoint.RuntimeMetricsKind)); k != "" {
		if c := canonicalRuntimeKind(k); c != "" {
			return c
		}
		return k
	}
	if c := canonicalRuntimeKind(endpoint.Type); c != "" {
		return c
	}
	lower := strings.ToLower(endpoint.Name)
	for _, fb := range runtimeKindNameFallbacks {
		if strings.Contains(lower, fb.marker) {
			return fb.kind
		}
	}
	return ""
}

// vllmKVUsageNames chains the spellings vLLM builds have used for the KV
// cache usage gauge across releases (radiance emits the same compat names).
var vllmKVUsageNames = []string{"vllm:kv_cache_usage_ratio", "vllm_kv_cache_usage_ratio", "vllm:kv_cache_usage_perc", "vllm_kv_cache_usage_perc", "vllm:gpu_cache_usage_perc", "vllm_gpu_cache_usage_perc"}

func parseRuntimeCounters(kind, exposition string) runtimeCounters {
	counters, _ := parseRuntimeCountersFrom(kind, strings.NewReader(exposition))
	return counters
}

func parseRuntimeCountersFrom(kind string, r io.Reader) (runtimeCounters, error) {
	values, err := parsePrometheusExposition(r)
	return selectRuntimeCounters(kind, values), err
}

func selectRuntimeCounters(kind string, values map[string]float64) runtimeCounters {
	get := func(names ...string) *float64 {
		for _, name := range names {
			if v, ok := values[name]; ok {
				return floatPtr(v)
			}
		}
		return nil
	}
	switch kind {
	case "vllm":
		// Older vLLM builds lack the V1-only kv_computed counter;
		// prompt_tokens_total paired with request_prefill_time_seconds_sum
		// measures whole-prompt prefill over engine prefill time (not wall
		// time), so the rate stays comparable across builds.
		return runtimeCounters{
			generationTokens: get("vllm:generation_tokens_total", "vllm_generation_tokens_total"), generationSeconds: get("vllm:request_decode_time_seconds_sum", "vllm_request_decode_time_seconds_sum"),
			prefillTokens: get("vllm:request_prefill_kv_computed_tokens_sum", "vllm_request_prefill_kv_computed_tokens_sum", "vllm:prompt_tokens_total", "vllm_prompt_tokens_total"), prefillSeconds: get("vllm:request_prefill_time_seconds_sum", "vllm_request_prefill_time_seconds_sum"),
			cacheHits: get("vllm:prefix_cache_hits_total", "vllm_prefix_cache_hits_total"), cacheQueries: get("vllm:prefix_cache_queries_total", "vllm_prefix_cache_queries_total"),
			kvUsage: normalisePercent(get(vllmKVUsageNames...)),
		}
	case "radiance":
		c := runtimeCounters{
			generationTokens: get("radiance:decode_tokens_total"), generationSeconds: get("radiance:engine_step_seconds_decode_total"),
			prefillTokens: get("radiance:prefill_tokens_total"), prefillSeconds: get("radiance:engine_step_seconds_prefill_total"),
			genRate: get("radiance:decode_tokens_per_second"), prefillRate: get("radiance:prefill_tokens_per_second"),
			kvUsage:           normalisePercent(get(vllmKVUsageNames...)),
			queueDepth:        get("radiance:http_queue_depth"),
			requestsRunning:   get("vllm:num_requests_running"),
			specAcceptPercent: scaledRatioPercent(get("radiance:draft_acceptance_ratio")),
		}
		// No counter pair forms a hit rate here: attn_cached_tokens_total is
		// tokens while linear_checkpoints_written_total counts checkpoints
		// (their ratio is tokens-per-checkpoint, not a percentage), and the
		// paged-attention cache exposes no queries counter. The GPU-prefix
		// cache hit-rate gauge comes first because that is the series
		// ollametrics (and therefore Grafana) canonicalises - reading the
		// linear-cache gauge instead made the dashboard and Grafana report
		// different hit rates for the same engine. The linear-cache gauge
		// stays as fallback for builds without the GPU-prefix series.
		c.cacheRatePercent = scaledRatioPercent(get("vllm:gpu_prefix_cache_hit_rate", "radiance:linear_prefix_cache_hit_rate"))
		return c
	case "llamacpp":
		return runtimeCounters{
			generationTokens: get("llamacpp:tokens_predicted_total", "llamacpp_tokens_predicted_total"), generationSeconds: get("llamacpp:tokens_predicted_seconds_total", "llamacpp_tokens_predicted_seconds_total"),
			prefillTokens: get("llamacpp:prompt_tokens_total", "llamacpp_prompt_tokens_total"), prefillSeconds: get("llamacpp:prompt_seconds_total", "llamacpp_prompt_seconds_total"),
			cachedPromptTokens: get("halogen:prompt_tokens_cached_total", "halogen_prompt_tokens_cached_total"), computedPromptTokens: get("llamacpp:prompt_tokens_total", "llamacpp_prompt_tokens_total"),
			kvUsage: normalisePercent(get("llamacpp:kv_cache_usage_ratio", "llamacpp_kv_cache_usage_ratio")),
		}
	}
	// Unknown kind (e.g. a typo in runtime_metrics.kind): parse nothing
	// rather than silently sampling the endpoint as llama.cpp.
	return runtimeCounters{}
}

// scaledRatioPercent converts a 0..1 backend ratio gauge to percent. Unlike
// normalisePercent it never passes a >1 value through unscaled: these gauges
// are documented ratios, so anything else is a bug in the backend, not a
// percent already.
func scaledRatioPercent(v *float64) *float64 {
	if v == nil {
		return nil
	}
	return floatPtr(100 * *v)
}
func normalisePercent(v *float64) *float64 {
	if v == nil {
		return nil
	}
	n := *v
	if n >= 0 && n <= 1 {
		n *= 100
	}
	return floatPtr(n)
}

// parsePrometheusSums is the string-input entry point kept for tests and
// callers holding the exposition in memory; scrapes stream through
// parsePrometheusExposition directly.
func parsePrometheusSums(exposition string) map[string]float64 {
	sums, _ := parsePrometheusExposition(strings.NewReader(exposition))
	return sums
}

// parsePrometheusExposition aggregates an exposition in one streaming pass.
// Aggregation is per declared type: counters sum across label series (the
// delta maths wants the total), gauges take the maximum instead - summing a
// usage ratio or rate gauge across a multi-GPU/multi-engine exposition yields
// nonsense above 100%, while the worst series is the operator-relevant value.
// Names with no `# TYPE` line keep the historic summing behaviour
// (single-series payloads aggregate identically either way).
func parsePrometheusExposition(r io.Reader) (map[string]float64, error) {
	out := make(map[string]float64)
	gauges := make(map[string]bool)
	s := bufio.NewScanner(r)
	s.Buffer(make([]byte, 1024), 1024*1024)
	for s.Scan() {
		line := strings.TrimSpace(s.Text())
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "#") {
			// `# TYPE <name> counter|gauge|histogram|summary|untyped`
			fields := strings.Fields(line)
			if len(fields) >= 4 && fields[1] == "TYPE" && fields[3] == "gauge" {
				gauges[fields[2]] = true
			}
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		name := fields[0]
		if i := strings.IndexByte(name, '{'); i >= 0 {
			name = name[:i]
		}
		value, err := strconv.ParseFloat(fields[1], 64)
		if err != nil || math.IsNaN(value) || math.IsInf(value, 0) {
			continue
		}
		if gauges[name] {
			if cur, ok := out[name]; !ok || value > cur {
				out[name] = value
			}
			continue
		}
		out[name] += value
	}
	return out, s.Err()
}

// hashEndpointRuntimeMetrics feeds the value-bearing fields of a runtime
// snapshot to the status ETag hashers. SampledAt and AvailableSince are
// deliberately excluded: both advance on every scrape even when every value
// is identical, and hashing them (as the earlier json.Marshal of the whole
// struct did) changed the ETag every interval and defeated If-None-Match
// caching for opted-in endpoints. The stale flag and the rendered numerics
// are what the dashboard columns show, so they stay hashed.
func hashEndpointRuntimeMetrics(h hash.Hash, m *EndpointRuntimeMetrics) {
	if m == nil {
		return
	}
	hashEtagString(h, strconv.FormatBool(m.Stale))
	hashEtagString(h, m.Kind)
	hashEtagString(h, m.Source)
	for _, metric := range m.Metrics {
		hashEtagString(h, metric.Key)
		hashEtagString(h, metric.Unit)
		hashRuntimeFloat(h, metric.Current)
		hashRuntimeFloat(h, metric.Avg)
		hashRuntimeFloat(h, metric.Max)
		hashEtagString(h, metric.Scope)
		hashEtagString(h, metric.AttachTo)
		hashEtagString(h, metric.Note)
	}
}

// hashRuntimeFloat encodes presence (nil is a bare separator, mirroring
// omitempty on the wire) plus the exact value via shortest round-trip float
// rendering.
func hashRuntimeFloat(h hash.Hash, v *float64) {
	if v == nil {
		_, _ = h.Write(etagSep)
		return
	}
	hashEtagString(h, strconv.FormatFloat(*v, 'g', -1, 64))
}

func writeEndpointRuntimePrometheusMetrics(b *strings.Builder, endpoints []*domain.Endpoint, provider runtimeMetricsProvider) {
	// Kept separate from proxy metrics so all samples in each added family stay contiguous.
	for _, spec := range []struct {
		name, help, key, part string
	}{
		{"olla_endpoint_generation_tokens_per_second", "Current backend generation throughput", runtimeMetricKeyGenerationTPS, runtimeMetricPartCurrent},
		{"olla_endpoint_generation_tokens_per_second_avg", "Average generation throughput over the last 10 minutes", runtimeMetricKeyGenerationTPS, runtimeMetricPartAvg},
		{"olla_endpoint_generation_tokens_per_second_max", "Maximum sampled generation throughput over the last 10 minutes", runtimeMetricKeyGenerationTPS, runtimeMetricPartMax},
		{"olla_endpoint_prefill_tokens_per_second", "Current backend incremental prefill throughput", runtimeMetricKeyPrefillTPS, runtimeMetricPartCurrent},
		{"olla_endpoint_prefill_tokens_per_second_avg", "Average incremental prefill throughput over the last 10 minutes", runtimeMetricKeyPrefillTPS, runtimeMetricPartAvg},
		{"olla_endpoint_prefill_tokens_per_second_max", "Maximum sampled incremental prefill throughput over the last 10 minutes", runtimeMetricKeyPrefillTPS, runtimeMetricPartMax},
		{"olla_endpoint_cache_hit_percent", "Current backend prompt cache hit percentage", runtimeMetricKeyCacheHitPercent, runtimeMetricPartCurrent},
		{"olla_endpoint_cache_hit_percent_avg", "Average prompt cache hit percentage over the last 10 minutes", runtimeMetricKeyCacheHitPercent, runtimeMetricPartAvg},
		{"olla_endpoint_kv_cache_usage_percent", "Current backend KV cache utilisation percentage", runtimeMetricKeyKvCacheUsagePercent, runtimeMetricPartCurrent},
		{"olla_endpoint_queue_depth", "Current backend request queue depth", runtimeMetricKeyQueueDepth, runtimeMetricPartCurrent},
		{"olla_endpoint_requests_running", "Requests currently running on the backend", runtimeMetricKeyRequestsRunning, runtimeMetricPartCurrent},
		{"olla_endpoint_spec_accept_percent", "Current speculative-decode draft acceptance percentage", runtimeMetricKeySpecAcceptPercent, runtimeMetricPartCurrent},
		{"olla_endpoint_spec_accept_percent_avg", "Average speculative-decode draft acceptance percentage over the last 10 minutes", runtimeMetricKeySpecAcceptPercent, runtimeMetricPartAvg},
	} {
		writePrometheusHelpType(b, spec.name, "gauge", spec.help)
		for _, endpoint := range endpoints {
			// Snapshot is nil for endpoints the provider does not cover (not
			// opted in, unreachable, or no sample yet); skip those.
			snapshot := provider.Snapshot(endpoint)
			if snapshot == nil {
				continue
			}
			if value := envelopeMetricValue(snapshot, spec.key, spec.part); value != nil {
				writePrometheusLabeledGauge(b, spec.name, *value, "endpoint", endpoint.Name)
			}
		}
	}
}
