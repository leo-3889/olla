package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/thushan/olla/internal/app/handlers/runtime_dialect"
	"github.com/thushan/olla/internal/config"
	"github.com/thushan/olla/internal/core/domain"
)

// End-to-end proof of the one-file-dialect claim: this test registers a
// brand-new backend family ("fakedialect") with its own series names, its own
// column declarations and the engine-scoped caveat, and shows the untouched
// sampler, envelope, status payload and dashboard manifest all carry it with
// zero further edits.
func init() {
	runtime_dialect.Register(runtime_dialect.Dialect{
		Kind:         "fakedialect",
		EngineScoped: true,
		Select: func(values map[string]float64) runtime_dialect.Counters {
			pick := func(name string) *float64 {
				if v, ok := values[name]; ok {
					return &v
				}
				return nil
			}
			return runtime_dialect.Counters{
				GenerationTokens:  pick("fake:out_tokens_total"),
				GenerationSeconds: pick("fake:out_seconds_total"),
				QueueDepth:        pick("fake:waiting_requests"),
			}
		},
		Columns: []runtime_dialect.Column{
			{Key: runtime_dialect.KeyGenerationTPS, Label: "Fake throughput", Unit: runtime_dialect.UnitTokS, Windowed: true},
			{Key: runtime_dialect.KeyQueueDepth, Label: "Fake queue", Unit: runtime_dialect.UnitCount},
		},
	})
}

func TestFakeDialectFlowsThroughSamplerEnvelopeManifestAndPayload(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		if calls == 1 {
			_, _ = w.Write([]byte("fake:out_tokens_total 0\nfake:out_seconds_total 0\nfake:waiting_requests 4\n"))
			return
		}
		_, _ = w.Write([]byte("fake:out_tokens_total 30\nfake:out_seconds_total 1\nfake:waiting_requests 4\n"))
	}))
	defer server.Close()

	ep := &domain.Endpoint{Name: "fake-1", Type: "openai-compatible", RuntimeMetricsKind: "fakedialect",
		URLString: server.URL, Status: domain.StatusHealthy, RuntimeMetricsEnabled: true, RuntimeMetricsInterval: time.Nanosecond}
	s := newEndpointRuntimeMetricsSampler(&mockStatusEndpointRepository{endpoints: []*domain.Endpoint{ep}}, nil)
	s.Refresh(t.Context())
	s.Refresh(t.Context())

	got := s.Snapshot(ep)
	require.NotNil(t, got)
	assert.Equal(t, "fakedialect", got.Kind)

	// The dialect's series names reached the shared windowing machinery...
	require.NotNil(t, envNum(t, got, runtimeMetricKeyGenerationTPS, runtimeMetricPartCurrent))
	assert.InDelta(t, 30, *envNum(t, got, runtimeMetricKeyGenerationTPS, runtimeMetricPartCurrent), 0.001)
	// ...and its metadata drove the honesty annotations without any handler edit.
	gen := envMetric(t, got, runtimeMetricKeyGenerationTPS)
	assert.Equal(t, runtimeMetricScopeEngine, gen.Scope)
	assert.Contains(t, gen.Note, runtimeMetricNoteEngineThroughput)
	assert.Contains(t, gen.Note, runtimeMetricNoteSamplerWindow)
	require.NotNil(t, envNum(t, got, runtimeMetricKeyQueueDepth, runtimeMetricPartCurrent))
	assert.InDelta(t, 4, *envNum(t, got, runtimeMetricKeyQueueDepth, runtimeMetricPartCurrent), 0.001)
	// Signals the fake backend does not publish stay absent, not zero.
	assert.Nil(t, envMetric(t, got, runtimeMetricKeyPrefillTPS))

	// Status payload: the marshalled envelope speaks the dialect's numbers.
	body, err := json.Marshal(got)
	require.NoError(t, err)
	var decoded struct {
		Kind    string          `json:"kind"`
		Metrics []RuntimeMetric `json:"metrics"`
	}
	require.NoError(t, json.Unmarshal(body, &decoded))
	assert.Equal(t, "fakedialect", decoded.Kind)
	require.Len(t, decoded.Metrics, 2)

	// Manifest: the same endpoint in the config advertises exactly the
	// dialect's declared columns, including the dialect-owned label.
	cfg := &config.Config{}
	cfg.Discovery.Static.Endpoints = []config.EndpointConfig{
		{Name: "fake-1", Type: "openai-compatible", RuntimeMetrics: &config.RuntimeMetricsConfig{Enabled: true, Kind: "fakedialect"}},
	}
	cols := uiRuntimeColumnsFrom(cfg)
	require.Len(t, cols, 2)
	assert.Equal(t, "Fake throughput", cols[0].Label)
	assert.Equal(t, "Fake queue", cols[1].Label)
	assert.Equal(t, "count", cols[1].Unit)
}
