package handlers

import "github.com/thushan/olla/internal/app/handlers/runtime_dialect"

// Units, keys, sources, scope and note literals of the self-describing
// runtime_metrics envelope. The embedded dashboard renders these verbatim and
// ollametrics publishes the same key vocabulary in InfluxDB, so these strings
// are a wire contract - rename them only together with the dashboard.
// Units come from the dialect package: the dashboard manifest columns and
// the envelope must always speak the same vocabulary.
const (
	runtimeMetricUnitTokS    = runtime_dialect.UnitTokS
	runtimeMetricUnitPercent = runtime_dialect.UnitPercent
	runtimeMetricUnitCount   = runtime_dialect.UnitCount
	runtimeMetricUnitMs      = runtime_dialect.UnitMs
)

const (
	runtimeMetricKeyGenerationTPS       = runtime_dialect.KeyGenerationTPS
	runtimeMetricKeyPrefillTPS          = runtime_dialect.KeyPrefillTPS
	runtimeMetricKeyCacheHitPercent     = runtime_dialect.KeyCacheHitPercent
	runtimeMetricKeyKvCacheUsagePercent = runtime_dialect.KeyKVCacheUsagePercent
	runtimeMetricKeyQueueDepth          = runtime_dialect.KeyQueueDepth
	runtimeMetricKeyRequestsRunning     = "requests_running"
	runtimeMetricKeySpecAcceptPercent   = "spec_accept_percent"
)

const (
	runtimeMetricScopeEngine = "engine"

	runtimeMetricNoteEngineThroughput = "Engine throughput (batch-aggregate across concurrent requests), not per-stream speed"
	runtimeMetricNoteSamplerWindow    = "avg/max over last 10m"
	runtimeMetricNoteInfluxGauge      = "engine rate gauge, 1m windows over 10m (ollametrics)"
	runtimeMetricNoteInfluxCounter    = "counter derivative, 1m windows over 10m (ollametrics)"
)

const (
	runtimeMetricsSourceSampler = "sampler"
	runtimeMetricsSourceInflux  = "ollametrics"
)

const (
	runtimeMetricPartCurrent = "current"
	runtimeMetricPartAvg     = "avg"
	runtimeMetricPartMax     = "max"
)

// RuntimeMetric is one observation in the runtime_metrics envelope. A nil
// Current means the backend does not publish the signal; such metrics are
// omitted from the payload entirely (absent = unsupported). Avg/Max appear
// only for windowed signals; Scope/Note/AttachTo are presentation semantics
// decided by the provider, never inferred by the UI.
type RuntimeMetric struct {
	Key      string   `json:"key"`
	Unit     string   `json:"unit"`
	Current  *float64 `json:"current,omitempty"`
	Avg      *float64 `json:"avg,omitempty"`
	Max      *float64 `json:"max,omitempty"`
	Scope    string   `json:"scope,omitempty"`
	AttachTo string   `json:"attach_to,omitempty"`
	Note     string   `json:"note,omitempty"`
}

// value returns one part of the metric, used by the Prometheus re-export.
func (m RuntimeMetric) value(part string) *float64 {
	switch part {
	case runtimeMetricPartCurrent:
		return m.Current
	case runtimeMetricPartAvg:
		return m.Avg
	case runtimeMetricPartMax:
		return m.Max
	}
	return nil
}

// envelopeMetricValue looks one key/part pair up in a snapshot.
func envelopeMetricValue(m *EndpointRuntimeMetrics, key, part string) *float64 {
	if m == nil {
		return nil
	}
	for _, metric := range m.Metrics {
		if metric.Key == key {
			return metric.value(part)
		}
	}
	return nil
}

// joinNote composes the full caveat for one metric without duplicating parts.
func joinNote(parts ...string) string {
	out := ""
	for _, p := range parts {
		if p == "" {
			continue
		}
		if out == "" {
			out = p
			continue
		}
		out += "; " + p
	}
	return out
}

// buildEnvelopeMetrics converts one sampler aggregate state into the
// envelope, emitted in dashboard column order: generation, prefill, cache
// hit, KV usage, queue, running, then speculative acceptance last because it
// renders attached to generation. The radiance dialect publishes engine-wide
// rate gauges (batch-aggregate across concurrent requests), which is a
// property of the data source, so the provider states it here instead of
// letting the UI guess it from the backend kind (the dialect states it).
func buildEnvelopeMetrics(state *runtimeMetricsState, kind string) []RuntimeMetric {
	if state == nil {
		return nil
	}
	engineScoped := runtime_dialect.EngineScoped(kind)
	out := make([]RuntimeMetric, 0, 7)
	if aggregateHasValue(state.generation.current, state.generation.avg, state.generation.max) {
		out = append(out, RuntimeMetric{Key: runtimeMetricKeyGenerationTPS, Unit: runtimeMetricUnitTokS,
			Current: copyFloat(state.generation.current), Avg: copyFloat(state.generation.avg), Max: copyFloat(state.generation.max),
			Scope: engineScopeOf(engineScoped), Note: joinNote(engineNoteOf(engineScoped), runtimeMetricNoteSamplerWindow)})
	}
	if aggregateHasValue(state.prefill.current, state.prefill.avg, state.prefill.max) {
		out = append(out, RuntimeMetric{Key: runtimeMetricKeyPrefillTPS, Unit: runtimeMetricUnitTokS,
			Current: copyFloat(state.prefill.current), Avg: copyFloat(state.prefill.avg), Max: copyFloat(state.prefill.max),
			Scope: engineScopeOf(engineScoped), Note: joinNote(engineNoteOf(engineScoped), runtimeMetricNoteSamplerWindow)})
	}
	if state.cache.current != nil || state.cache.avg != nil {
		out = append(out, RuntimeMetric{Key: runtimeMetricKeyCacheHitPercent, Unit: runtimeMetricUnitPercent,
			Current: copyFloat(state.cache.current), Avg: copyFloat(state.cache.avg), Note: runtimeMetricNoteSamplerWindow})
	}
	if state.KVUsage != nil {
		out = append(out, RuntimeMetric{Key: runtimeMetricKeyKvCacheUsagePercent, Unit: runtimeMetricUnitPercent, Current: copyFloat(state.KVUsage)})
	}
	if state.QueueDepth != nil {
		out = append(out, RuntimeMetric{Key: runtimeMetricKeyQueueDepth, Unit: runtimeMetricUnitCount, Current: copyFloat(state.QueueDepth)})
	}
	if state.RequestsRunning != nil {
		out = append(out, RuntimeMetric{Key: runtimeMetricKeyRequestsRunning, Unit: runtimeMetricUnitCount, Current: copyFloat(state.RequestsRunning)})
	}
	if state.specAccept.current != nil || state.specAccept.avg != nil {
		out = append(out, RuntimeMetric{Key: runtimeMetricKeySpecAcceptPercent, Unit: runtimeMetricUnitPercent,
			Current: copyFloat(state.specAccept.current), Avg: copyFloat(state.specAccept.avg),
			AttachTo: runtimeMetricKeyGenerationTPS, Note: runtimeMetricNoteSamplerWindow})
	}
	return out
}

// aggregateHasValue reports whether a windowed aggregate carries anything at
// all. An idle endpoint legitimately keeps avg/max from its window while
// current is cleared (the last burst rate is not "current"), so presence of
// any part must keep the metric in the envelope.
func aggregateHasValue(parts ...*float64) bool {
	for _, p := range parts {
		if p != nil {
			return true
		}
	}
	return false
}

func engineScopeOf(engineScoped bool) string {
	if engineScoped {
		return runtimeMetricScopeEngine
	}
	return ""
}

func engineNoteOf(engineScoped bool) string {
	if engineScoped {
		return runtimeMetricNoteEngineThroughput
	}
	return ""
}
