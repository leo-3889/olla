package handlers

import (
	"bufio"
	"context"
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
const runtimeMetricsWakeInterval = time.Second
const runtimeMetricsTimeout = 3 * time.Second

// runtimeMetricsWindow is the reporting window over which the per-signal
// average and maximum are computed (the dashboard graph window). `current`
// is always the latest sample; avg and max cover samples inside this window.
const runtimeMetricsWindow = 10 * time.Minute

// EndpointRuntimeMetrics is the compact, capability-aware runtime view shown
// beside an endpoint. Nil fields mean the engine did not publish that signal.
type EndpointRuntimeMetrics struct {
	SampledAt           time.Time          `json:"sampled_at"`
	AvailableSince      time.Time          `json:"available_since"`
	Stale               bool               `json:"stale"`
	GenerationTPS       *RuntimeThroughput `json:"generation_tps,omitempty"`
	PrefillTPS          *RuntimeThroughput `json:"prefill_tps,omitempty"`
	CacheHitPercent     *RuntimeAverage    `json:"cache_hit_percent,omitempty"`
	KVCacheUsagePercent *float64           `json:"kv_cache_usage_percent,omitempty"`
}

type RuntimeThroughput struct {
	Current *float64 `json:"current,omitempty"`
	Avg     *float64 `json:"avg,omitempty"`
	Max     *float64 `json:"max,omitempty"`
}

type RuntimeAverage struct {
	Current *float64 `json:"current,omitempty"`
	Avg     *float64 `json:"avg,omitempty"`
}

type endpointRuntimeMetricsSampler struct {
	repository domain.EndpointRepository
	logger     logger.StyledLogger
	client     *http.Client
	now        func() time.Time
	mu         sync.RWMutex
	states     map[string]*runtimeMetricsState
}

type runtimeMetricsState struct {
	available      bool
	availableSince time.Time
	sampledAt      time.Time
	stale          bool
	last           runtimeCounters
	hasLast        bool
	generation     throughputAggregate
	prefill        throughputAggregate
	cache          ratioAggregate
	kvUsage        *float64
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
	samples = append(samples, runtimeSample{at: at, value: value})
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
}

func newEndpointRuntimeMetricsSampler(repository domain.EndpointRepository, l logger.StyledLogger) *endpointRuntimeMetricsSampler {
	// The client carries no timeout of its own: each scrape is bounded by its
	// per-endpoint context (see refreshEndpoint), which honours the configured
	// runtime_metrics.timeout and is cancelled on shutdown. A client-level
	// Timeout would cap every scrape at a fixed value and silently ignore any
	// configured per-endpoint timeout larger than it.
	return &endpointRuntimeMetricsSampler{repository: repository, logger: l, client: &http.Client{}, now: time.Now, states: make(map[string]*runtimeMetricsState)}
}

func (s *endpointRuntimeMetricsSampler) Start(ctx context.Context) {
	if s == nil || s.repository == nil {
		return
	}
	go func() {
		s.Refresh(ctx)
		ticker := time.NewTicker(runtimeMetricsWakeInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.Refresh(ctx)
			}
		}
	}()
}

func (s *endpointRuntimeMetricsSampler) Refresh(ctx context.Context) {
	if s == nil || s.repository == nil {
		return
	}
	endpoints, err := s.repository.GetAll(ctx)
	if err != nil {
		return
	}
	for _, endpoint := range endpoints {
		if endpoint == nil || !endpoint.RuntimeMetricsEnabled || runtimeMetricsKind(endpoint) == "" {
			continue
		}
		if endpoint.Status != domain.StatusHealthy {
			s.markUnavailable(endpoint.URLString)
			continue
		}
		if s.shouldRefresh(endpoint) {
			s.refreshEndpoint(ctx, endpoint)
		}
	}
}

func (s *endpointRuntimeMetricsSampler) shouldRefresh(endpoint *domain.Endpoint) bool {
	interval := runtimeMetricsInterval
	if endpoint.RuntimeMetricsInterval > 0 {
		interval = endpoint.RuntimeMetricsInterval
	}
	s.mu.RLock()
	state := s.states[endpoint.URLString]
	s.mu.RUnlock()
	return state == nil || state.sampledAt.IsZero() || s.now().Sub(state.sampledAt) >= interval
}

func (s *endpointRuntimeMetricsSampler) refreshEndpoint(parent context.Context, endpoint *domain.Endpoint) {
	timeout := runtimeMetricsTimeout
	if endpoint.RuntimeMetricsTimeout > 0 {
		timeout = endpoint.RuntimeMetricsTimeout
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	url := util.ResolveURLPath(endpoint.URLString, "/metrics")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		s.markStale(endpoint.URLString)
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
		s.markStale(endpoint.URLString)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		s.markStale(endpoint.URLString)
		return
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		s.markStale(endpoint.URLString)
		return
	}
	counters := parseRuntimeCounters(runtimeMetricsKind(endpoint), string(body))
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
func (s *endpointRuntimeMetricsSampler) markStale(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if state := s.states[key]; state != nil && state.available {
		state.stale = true
	}
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
	if state.hasLast {
		recordThroughput(&state.generation, at, state.last.generationTokens, next.generationTokens, state.last.generationSeconds, next.generationSeconds)
		recordThroughput(&state.prefill, at, state.last.prefillTokens, next.prefillTokens, state.last.prefillSeconds, next.prefillSeconds)
		recordRatio(&state.cache, at, state.last, next)
	}
	state.last, state.hasLast, state.kvUsage = next, true, next.kvUsage
}

func (s *endpointRuntimeMetricsSampler) Snapshot(endpoint *domain.Endpoint) *EndpointRuntimeMetrics {
	if s == nil || endpoint == nil || endpoint.Status != domain.StatusHealthy {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	state := s.states[endpoint.URLString]
	if state == nil || !state.available || state.sampledAt.IsZero() {
		return nil
	}
	return &EndpointRuntimeMetrics{SampledAt: state.sampledAt, AvailableSince: state.availableSince, Stale: state.stale,
		GenerationTPS: copyThroughput(state.generation), PrefillTPS: copyThroughput(state.prefill), CacheHitPercent: copyRatio(state.cache), KVCacheUsagePercent: copyFloat(state.kvUsage)}
}

func recordThroughput(a *throughputAggregate, at time.Time, oldTokens, newTokens, oldSeconds, newSeconds *float64) {
	if oldTokens == nil || newTokens == nil || oldSeconds == nil || newSeconds == nil {
		return
	}
	tokens, seconds := *newTokens-*oldTokens, *newSeconds-*oldSeconds
	if tokens < 0 || seconds <= 0 {
		return
	}
	a.current = floatPtr(tokens / seconds)
	a.samples = appendWindowed(a.samples, at, *a.current)
	a.avg, a.max = windowStats(a.samples)
}
func recordRatio(a *ratioAggregate, at time.Time, old, next runtimeCounters) {
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
		return
	}
	n, d := *newN-*oldN, *newD-*oldD
	if n < 0 || d <= 0 {
		return
	}
	a.current = floatPtr(100 * n / d)
	a.samples = appendWindowed(a.samples, at, *a.current)
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
func copyThroughput(a throughputAggregate) *RuntimeThroughput {
	if a.current == nil && a.avg == nil && a.max == nil {
		return nil
	}
	return &RuntimeThroughput{copyFloat(a.current), copyFloat(a.avg), copyFloat(a.max)}
}
func copyRatio(a ratioAggregate) *RuntimeAverage {
	if a.current == nil && a.avg == nil {
		return nil
	}
	return &RuntimeAverage{copyFloat(a.current), copyFloat(a.avg)}
}
func copyFloat(v *float64) *float64 {
	if v == nil {
		return nil
	}
	return floatPtr(*v)
}
func floatPtr(v float64) *float64 { return &v }

func runtimeMetricsKind(endpoint *domain.Endpoint) string {
	if endpoint == nil {
		return ""
	}
	t := endpoint.Type
	switch strings.ToLower(strings.ReplaceAll(t, "-", "")) {
	case "vllm":
		return "vllm"
	case "halogen", "llamacpp", "llama.cpp", "llamacppserver":
		return "llamacpp"
	}
	// Fallback for backends exposed through a generic type (e.g.
	// openai-compatible): recognise Halogen/llama.cpp by endpoint name.
	if strings.Contains(strings.ToLower(endpoint.Name), "halogen") {
		return "llamacpp"
	}
	return ""
}

func parseRuntimeCounters(kind, exposition string) runtimeCounters {
	values := parsePrometheusSums(exposition)
	get := func(names ...string) *float64 {
		for _, name := range names {
			if v, ok := values[name]; ok {
				return floatPtr(v)
			}
		}
		return nil
	}
	if kind == "vllm" {
		c := runtimeCounters{
			generationTokens: get("vllm:generation_tokens_total", "vllm_generation_tokens_total"), generationSeconds: get("vllm:request_decode_time_seconds_sum", "vllm_request_decode_time_seconds_sum"),
			prefillTokens: get("vllm:request_prefill_kv_computed_tokens_sum", "vllm_request_prefill_kv_computed_tokens_sum"), prefillSeconds: get("vllm:request_prefill_time_seconds_sum", "vllm_request_prefill_time_seconds_sum"),
			cacheHits: get("vllm:prefix_cache_hits_total", "vllm_prefix_cache_hits_total"), cacheQueries: get("vllm:prefix_cache_queries_total", "vllm_prefix_cache_queries_total"),
		}
		if v := get("vllm:kv_cache_usage_ratio", "vllm_kv_cache_usage_ratio", "vllm:kv_cache_usage_perc", "vllm_kv_cache_usage_perc"); v != nil {
			c.kvUsage = normalisePercent(v)
		} else {
			// gpu_cache_usage_perc is already a percentage; do not rescale it.
			c.kvUsage = get("vllm:gpu_cache_usage_perc", "vllm_gpu_cache_usage_perc")
		}
		return c
	}
	return runtimeCounters{
		generationTokens: get("llamacpp:tokens_predicted_total", "llamacpp_tokens_predicted_total"), generationSeconds: get("llamacpp:tokens_predicted_seconds_total", "llamacpp_tokens_predicted_seconds_total"),
		prefillTokens: get("llamacpp:prompt_tokens_total", "llamacpp_prompt_tokens_total"), prefillSeconds: get("llamacpp:prompt_seconds_total", "llamacpp_prompt_seconds_total"),
		cachedPromptTokens: get("halogen:prompt_tokens_cached_total", "halogen_prompt_tokens_cached_total"), computedPromptTokens: get("llamacpp:prompt_tokens_total", "llamacpp_prompt_tokens_total"),
		kvUsage: normalisePercent(get("llamacpp:kv_cache_usage_ratio", "llamacpp_kv_cache_usage_ratio")),
	}
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
func parsePrometheusSums(exposition string) map[string]float64 {
	out := make(map[string]float64)
	s := bufio.NewScanner(strings.NewReader(exposition))
	s.Buffer(make([]byte, 1024), 1024*1024)
	for s.Scan() {
		line := strings.TrimSpace(s.Text())
		if line == "" || strings.HasPrefix(line, "#") {
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
		if err == nil && !math.IsNaN(value) && !math.IsInf(value, 0) {
			out[name] += value
		}
	}
	return out
}

func writeEndpointRuntimePrometheusMetrics(b *strings.Builder, endpoints []*domain.Endpoint, sampler *endpointRuntimeMetricsSampler) {
	// Kept separate from proxy metrics so all samples in each added family stay contiguous.
	for _, spec := range []struct {
		name, help string
		get        func(*EndpointRuntimeMetrics) *float64
	}{
		{"olla_endpoint_generation_tokens_per_second", "Current backend generation throughput", func(m *EndpointRuntimeMetrics) *float64 {
			if m.GenerationTPS == nil {
				return nil
			}
			return m.GenerationTPS.Current
		}},
		{"olla_endpoint_generation_tokens_per_second_avg", "Average generation throughput over the last 10 minutes", func(m *EndpointRuntimeMetrics) *float64 {
			if m.GenerationTPS == nil {
				return nil
			}
			return m.GenerationTPS.Avg
		}},
		{"olla_endpoint_generation_tokens_per_second_max", "Maximum sampled generation throughput over the last 10 minutes", func(m *EndpointRuntimeMetrics) *float64 {
			if m.GenerationTPS == nil {
				return nil
			}
			return m.GenerationTPS.Max
		}},
		{"olla_endpoint_prefill_tokens_per_second", "Current backend incremental prefill throughput", func(m *EndpointRuntimeMetrics) *float64 {
			if m.PrefillTPS == nil {
				return nil
			}
			return m.PrefillTPS.Current
		}},
		{"olla_endpoint_prefill_tokens_per_second_avg", "Average incremental prefill throughput over the last 10 minutes", func(m *EndpointRuntimeMetrics) *float64 {
			if m.PrefillTPS == nil {
				return nil
			}
			return m.PrefillTPS.Avg
		}},
		{"olla_endpoint_prefill_tokens_per_second_max", "Maximum sampled incremental prefill throughput over the last 10 minutes", func(m *EndpointRuntimeMetrics) *float64 {
			if m.PrefillTPS == nil {
				return nil
			}
			return m.PrefillTPS.Max
		}},
		{"olla_endpoint_cache_hit_percent", "Current backend prompt cache hit percentage", func(m *EndpointRuntimeMetrics) *float64 {
			if m.CacheHitPercent == nil {
				return nil
			}
			return m.CacheHitPercent.Current
		}},
		{"olla_endpoint_cache_hit_percent_avg", "Average prompt cache hit percentage over the last 10 minutes", func(m *EndpointRuntimeMetrics) *float64 {
			if m.CacheHitPercent == nil {
				return nil
			}
			return m.CacheHitPercent.Avg
		}},
		{"olla_endpoint_kv_cache_usage_percent", "Current backend KV cache utilisation percentage", func(m *EndpointRuntimeMetrics) *float64 { return m.KVCacheUsagePercent }},
	} {
		writePrometheusHelpType(b, spec.name, "gauge", spec.help)
		for _, endpoint := range endpoints {
			// Snapshot is nil for endpoints the sampler does not cover (not
			// opted in, unhealthy, or no sample yet); the spec getters
			// dereference the snapshot, so skip those.
			snapshot := sampler.Snapshot(endpoint)
			if snapshot == nil {
				continue
			}
			if value := spec.get(snapshot); value != nil {
				writePrometheusLabeledGauge(b, spec.name, *value, "endpoint", endpoint.Name)
			}
		}
	}
}
