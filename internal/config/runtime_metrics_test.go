package config

import (
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// TestRuntimeMetricsConfig_Parsing covers parsing of the per-endpoint
// runtime_metrics block: the enabled flag and the interval/timeout durations.
func TestRuntimeMetricsConfig_Parsing(t *testing.T) {
	t.Parallel()

	t.Run("enabled with interval and timeout", func(t *testing.T) {
		t.Parallel()
		yamlIn := `
name: vllm-1
url: http://localhost:8000
type: vllm
runtime_metrics:
  enabled: true
  interval: 15s
  timeout: 4s
`
		var got EndpointConfig
		if err := yaml.Unmarshal([]byte(yamlIn), &got); err != nil {
			t.Fatalf("Unmarshal: %v", err)
		}
		if got.RuntimeMetrics == nil {
			t.Fatalf("RuntimeMetrics should be present when the block is in YAML, got nil")
		}
		if !got.RuntimeMetrics.Enabled {
			t.Errorf("Enabled should be true, got false")
		}
		if got.RuntimeMetrics.Interval != 15*time.Second {
			t.Errorf("Interval should be 15s, got %v", got.RuntimeMetrics.Interval)
		}
		if got.RuntimeMetrics.Timeout != 4*time.Second {
			t.Errorf("Timeout should be 4s, got %v", got.RuntimeMetrics.Timeout)
		}
	})

	t.Run("enabled false with omitted durations", func(t *testing.T) {
		t.Parallel()
		yamlIn := `
name: vllm-2
url: http://localhost:8000
type: vllm
runtime_metrics:
  enabled: false
`
		var got EndpointConfig
		if err := yaml.Unmarshal([]byte(yamlIn), &got); err != nil {
			t.Fatalf("Unmarshal: %v", err)
		}
		if got.RuntimeMetrics == nil {
			t.Fatalf("RuntimeMetrics should be present when the block is in YAML, got nil")
		}
		if got.RuntimeMetrics.Enabled {
			t.Errorf("Enabled should be false, got true")
		}
		if got.RuntimeMetrics.Interval != 0 {
			t.Errorf("Interval should be zero when omitted, got %v", got.RuntimeMetrics.Interval)
		}
		if got.RuntimeMetrics.Timeout != 0 {
			t.Errorf("Timeout should be zero when omitted, got %v", got.RuntimeMetrics.Timeout)
		}
	})
}

// TestRuntimeMetricsConfig_DisabledByDefault asserts the block is absent (nil)
// when an endpoint does not opt in, so the sampler leaves it alone.
func TestRuntimeMetricsConfig_DisabledByDefault(t *testing.T) {
	t.Parallel()

	yamlIn := `
name: plain
url: http://localhost:11434
type: ollama
`
	var got EndpointConfig
	if err := yaml.Unmarshal([]byte(yamlIn), &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got.RuntimeMetrics != nil {
		t.Errorf("RuntimeMetrics should be nil when not present in YAML, got %+v", got.RuntimeMetrics)
	}
}
