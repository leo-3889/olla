package discovery

import (
	"context"
	"testing"

	"github.com/thushan/olla/internal/config"
)

// TestLoadFromConfig_DisableCircuitBreaker_MapsToDomain verifies the
// config.EndpointConfig.DisableCircuitBreaker field is mapped to
// domain.Endpoint.DisableCircuitBreaker during static endpoint loading.
func TestLoadFromConfig_DisableCircuitBreaker_MapsToDomain(t *testing.T) {
	t.Parallel()

	repo := NewStaticEndpointRepository()
	configs := []config.EndpointConfig{
		{
			Name:                  "opted-out",
			URL:                   "http://localhost:8080",
			Type:                  "llamacpp",
			DisableCircuitBreaker: true,
		},
		{
			Name: "normal",
			URL:  "http://localhost:8081",
			Type: "llamacpp",
		},
	}

	if err := repo.LoadFromConfig(context.Background(), configs); err != nil {
		t.Fatalf("LoadFromConfig: %v", err)
	}

	endpoints, err := repo.GetAll(context.Background())
	if err != nil {
		t.Fatalf("GetAll: %v", err)
	}
	if len(endpoints) != 2 {
		t.Fatalf("want 2 endpoints, got %d", len(endpoints))
	}

	var optedOut, normal bool
	for _, ep := range endpoints {
		switch ep.Name {
		case "opted-out":
			optedOut = ep.DisableCircuitBreaker
		case "normal":
			normal = ep.DisableCircuitBreaker
		}
	}
	if !optedOut {
		t.Error("endpoint opted-out: DisableCircuitBreaker = false, want true")
	}
	if normal {
		t.Error("endpoint normal: DisableCircuitBreaker = true, want false")
	}
}
