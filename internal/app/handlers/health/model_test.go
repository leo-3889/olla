package health

import (
	"context"
	"testing"

	"github.com/thushan/olla/internal/adapter/registry"
	"github.com/thushan/olla/internal/core/domain"
	"github.com/thushan/olla/internal/core/ports"
	"github.com/thushan/olla/internal/logger"
)

// mockModelRegistry implements only GetEndpointsForModel; the rest of the
// ModelRegistry interface is embedded (nil) and never called by the evaluator.
type mockModelRegistry struct {
	domain.ModelRegistry
	models map[string][]string
}

func (m *mockModelRegistry) GetEndpointsForModel(_ context.Context, modelName string) ([]string, error) {
	return m.models[modelName], nil
}

// recordingLogger records Warn/Info calls so the warn-on-change behaviour can be
// asserted. The rest of the StyledLogger interface is embedded (nil) and unused.
type recordingLogger struct {
	logger.StyledLogger
	warns []string
	infos []string
}

func (l *recordingLogger) Warn(msg string, _ ...any) { l.warns = append(l.warns, msg) }
func (l *recordingLogger) Info(msg string, _ ...any) { l.infos = append(l.infos, msg) }

// healthyEndpoint builds a single healthy endpoint at the given URL.
func healthyEndpoint(url string) []*domain.Endpoint {
	return []*domain.Endpoint{{Name: "e", URLString: url, Status: domain.StatusHealthy}}
}

func modelInput(reg *mockModelRegistry, healthy []*domain.Endpoint, stats map[string]ports.ModelStats, alias *registry.AliasResolver) *Input {
	return &Input{
		Healthy:       healthy,
		ModelStats:    stats,
		ModelRegistry:  reg,
		AliasResolver:  alias,
		Thresholds:     testThresholds(),
	}
}

func TestModelEvaluator_AllHealthy(t *testing.T) {
	reg := &mockModelRegistry{models: map[string][]string{"a": {"http://a:11434"}, "b": {"http://b:11434"}}}
	e := NewModelEvaluator([]string{"a", "b"})
	res := e.Evaluate(context.Background(), modelInput(reg, append(healthyEndpoint("http://a:11434"), healthyEndpoint("http://b:11434")...), nil, nil))
	if res.Status != StatusHealthy {
		t.Errorf("both models healthy: got %q, want healthy", res.Status)
	}
	if !res.HasModelSignal || !res.Scoped {
		t.Error("a real model verdict must set HasModelSignal and Scoped")
	}
}

func TestModelEvaluator_PartialHealthy_Degraded(t *testing.T) {
	reg := &mockModelRegistry{models: map[string][]string{"a": {"http://a:11434"}, "b": {"http://b:11434"}}}
	e := NewModelEvaluator([]string{"a", "b"})
	// Only a is healthy; b's endpoint is down.
	res := e.Evaluate(context.Background(), modelInput(reg, healthyEndpoint("http://a:11434"), nil, nil))
	if res.Status != StatusDegraded {
		t.Errorf("1 of 2 models healthy: got %q, want degraded", res.Status)
	}
}

func TestModelEvaluator_NoneHealthy_Critical(t *testing.T) {
	reg := &mockModelRegistry{models: map[string][]string{"a": {"http://a:11434"}, "b": {"http://b:11434"}}}
	e := NewModelEvaluator([]string{"a", "b"})
	// Both endpoints down.
	res := e.Evaluate(context.Background(), modelInput(reg, nil, nil, nil))
	if res.Status != StatusCritical {
		t.Errorf("no model healthy: got %q, want critical", res.Status)
	}
}

func TestModelEvaluator_LowSuccessRate_EscalatesCritical(t *testing.T) {
	reg := &mockModelRegistry{models: map[string][]string{"a": {"http://a:11434"}}}
	e := NewModelEvaluator([]string{"a"})
	// Model a is healthy (so the health-signal verdict is healthy) but its own
	// success rate is 80%, below the 90% critical threshold.
	stats := map[string]ports.ModelStats{"a": {TotalRequests: 100, SuccessfulRequests: 80}}
	res := e.Evaluate(context.Background(), modelInput(reg, healthyEndpoint("http://a:11434"), stats, nil))
	if res.Status != StatusCritical {
		t.Errorf("healthy model with 80%% success: got %q, want critical", res.Status)
	}
}

func TestModelEvaluator_UnknownModel_CountsUnhealthy(t *testing.T) {
	// "a" resolves to an endpoint and is healthy; "b" resolves to no endpoint
	// (unknown). The unknown model must still count against the fleet, so the
	// verdict is degraded, not healthy.
	reg := &mockModelRegistry{models: map[string][]string{"a": {"http://a:11434"}}}
	e := NewModelEvaluator([]string{"a", "b"})
	res := e.Evaluate(context.Background(), modelInput(reg, healthyEndpoint("http://a:11434"), nil, nil))
	if res.Status != StatusDegraded {
		t.Errorf("1 healthy + 1 unknown: got %q, want degraded", res.Status)
	}
	// The unknown model must be reported with an "unknown" status.
	var bStatus string
	for _, m := range res.Models {
		if m.Name == "b" {
			bStatus = m.Status
		}
	}
	if bStatus != modelStatusUnknown {
		t.Errorf("model b status = %q, want unknown", bStatus)
	}
}

func TestModelEvaluator_NoSignal_FallsBack(t *testing.T) {
	// No configured model resolves to any endpoint: there is no model-based
	// signal, so HasModelSignal must be false (the handler then uses the host
	// verdict) and the result must not be scoped.
	reg := &mockModelRegistry{models: map[string][]string{}}
	e := NewModelEvaluator([]string{"a", "b"})
	res := e.Evaluate(context.Background(), modelInput(reg, nil, nil, nil))
	if res.HasModelSignal {
		t.Error("no model resolved to an endpoint: HasModelSignal must be false")
	}
	if res.Scoped {
		t.Error("no model signal: result must not be scoped")
	}
}

func TestModelEvaluator_AliasExpansion(t *testing.T) {
	// A configured alias expands to its actual models; the union of their
	// endpoints determines health, and stats are aggregated over the actual
	// names (not the alias string).
	reg := &mockModelRegistry{models: map[string][]string{
		"actual1": {"http://a:11434"},
		"actual2": {"http://b:11434"},
	}}
	alias := registry.NewAliasResolver(map[string][]string{"my-alias": {"actual1", "actual2"}}, nil)
	e := NewModelEvaluator([]string{"my-alias"})
	// Both actual models' endpoints are healthy; stats live under the actual
	// names.
	stats := map[string]ports.ModelStats{
		"actual1": {TotalRequests: 10, SuccessfulRequests: 10},
		"actual2": {TotalRequests: 10, SuccessfulRequests: 10},
	}
	res := e.Evaluate(context.Background(), modelInput(reg, append(healthyEndpoint("http://a:11434"), healthyEndpoint("http://b:11434")...), stats, alias))
	if res.Status != StatusHealthy {
		t.Errorf("alias with both actual models healthy: got %q, want healthy", res.Status)
	}
	if len(res.Models) != 1 || res.Models[0].Name != "my-alias" {
		t.Fatalf("expected a single entry named my-alias, got %+v", res.Models)
	}
	if res.Models[0].TotalRequests != 20 {
		t.Errorf("alias total requests = %d, want 20 (summed over actual models)", res.Models[0].TotalRequests)
	}
}

func TestModelEvaluator_WarnOnChange(t *testing.T) {
	reg := &mockModelRegistry{models: map[string][]string{"a": {"http://a:11434"}}}
	e := NewModelEvaluator([]string{"a"})
	log := &recordingLogger{}

	// First: model down -> one warning.
	e.Evaluate(context.Background(), withLogger(modelInput(reg, nil, nil, nil), log))
	// Second: still down -> no new warning (dedup).
	e.Evaluate(context.Background(), withLogger(modelInput(reg, nil, nil, nil), log))
	// Third: recovered -> one info, no warning.
	e.Evaluate(context.Background(), withLogger(modelInput(reg, healthyEndpoint("http://a:11434"), nil, nil), log))

	if len(log.warns) != 1 {
		t.Errorf("expected exactly 1 warning across 3 polls (down, down, up), got %d: %v", len(log.warns), log.warns)
	}
	if len(log.infos) != 1 {
		t.Errorf("expected exactly 1 recovery info, got %d: %v", len(log.infos), log.infos)
	}
}

// withLogger returns a copy of in with the given logger set.
func withLogger(in *Input, log *recordingLogger) *Input {
	in.Logger = log
	return in
}
