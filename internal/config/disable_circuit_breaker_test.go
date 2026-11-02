package config

import (
	"testing"

	"gopkg.in/yaml.v3"
)

// TestEndpointConfig_DisableCircuitBreaker_Parses pins YAML -> struct mapping
// for the per-endpoint circuit-breaker opt-out flag.
func TestEndpointConfig_DisableCircuitBreaker_Parses(t *testing.T) {
	t.Parallel()

	yamlSrc := []byte(`
discovery:
  static:
    endpoints:
      - name: "opted-out"
        url: "http://localhost:8080"
        type: "llamacpp"
        disable_circuit_breaker: true
      - name: "normal"
        url: "http://localhost:8081"
        type: "llamacpp"
`)

	cfg := DefaultConfig()
	if err := yaml.Unmarshal(yamlSrc, cfg); err != nil {
		t.Fatalf("yaml parse: %v", err)
	}

	eps := cfg.Discovery.Static.Endpoints
	if len(eps) != 2 {
		t.Fatalf("want 2 endpoints, got %d", len(eps))
	}
	if !eps[0].DisableCircuitBreaker {
		t.Error("endpoint opted-out: DisableCircuitBreaker = false, want true")
	}
	if eps[1].DisableCircuitBreaker {
		t.Error("endpoint normal: DisableCircuitBreaker = true, want false default")
	}
}
