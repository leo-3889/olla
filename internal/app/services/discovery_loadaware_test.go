package services

import (
	"context"
	"testing"

	"github.com/thushan/olla/internal/config"
	"github.com/thushan/olla/internal/core/domain"
)

type stubLoadReader struct{ q float64 }

func (r stubLoadReader) QueueDepthFor(*domain.Endpoint) (float64, bool) { return r.q, true }

// The app wiring phase calls SetLoadAwareness BEFORE services start, and
// DiscoveryService.Start() is where the health checker instance is created.
// Regression: the earlier implementation dropped the params when the checker
// did not exist yet, which made proxy.busy_queue_depth inert in production
// (grading silently never armed). SetLoadAwareness must stash the params and
// Start must apply them to the checker it constructs.
func TestDiscoveryService_SetLoadAwareness_BeforeStart(t *testing.T) {
	cfg := &config.DiscoveryConfig{
		Type: "static",
		Static: config.StaticDiscoveryConfig{
			Endpoints: []config.EndpointConfig{
				{
					Name:           "e1",
					URL:            "http://127.0.0.1:9/v1",
					Type:           "openai-compatible",
					HealthCheckURL: "http://127.0.0.1:9/health",
				},
			},
		},
	}
	svc := NewDiscoveryService(cfg, nil, nil, newTestLogger())
	svc.SetLoadAwareness(3, stubLoadReader{q: 3})

	ctx := context.Background()
	if err := svc.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer svc.Stop(ctx)

	if svc.healthChecker == nil {
		t.Fatal("health checker not created by Start")
	}
	if !svc.healthChecker.LoadAwarenessArmed() {
		t.Fatal("load-awareness params lost across Start: grading not armed")
	}
}
