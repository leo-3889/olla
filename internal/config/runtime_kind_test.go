package config

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCanonicalRuntimeKind(t *testing.T) {
	for in, want := range map[string]string{
		"vllm": "vllm", "VLLM": "vllm", " vllm ": "vllm",
		"halogen": "llamacpp", "llama-cpp": "llamacpp", "llama.cpp": "llamacpp", "llamacppserver": "llamacpp",
		"radiance": "radiance", "Radiance": "radiance",
		"sglang": "", "ollama": "", "": "",
	} {
		assert.Equal(t, want, CanonicalRuntimeKind(in), "input %q", in)
	}
}

func enabled(kind string) *RuntimeMetricsConfig {
	return &RuntimeMetricsConfig{Enabled: true, Kind: kind}
}

func TestRuntimeKindWarnings(t *testing.T) {
	c := &Config{Discovery: DiscoveryConfig{Static: StaticDiscoveryConfig{Endpoints: []EndpointConfig{
		{Name: "good-typed", Type: "vllm", RuntimeMetrics: enabled("")},
		{Name: "good-pinned", Type: "openai-compatible", RuntimeMetrics: enabled("llama.cpp")},
		{Name: "generic-nokind", Type: "openai-compatible", RuntimeMetrics: enabled("")},
		{Name: "unknown-pin", Type: "vllm", RuntimeMetrics: enabled("sglang")},
		{Name: "not-opted", Type: "openai-compatible"},
		{Name: "disabled", Type: "openai-compatible", RuntimeMetrics: &RuntimeMetricsConfig{Enabled: false}},
	}}}}
	warnings := c.RuntimeKindWarnings()
	require.Len(t, warnings, 2, "got %v", warnings)
	assert.Contains(t, strings.Join(warnings, "\n"), `"generic-nokind"`)
	assert.Contains(t, strings.Join(warnings, "\n"), `"unknown-pin"`)
	assert.NotContains(t, strings.Join(warnings, "\n"), "good-")
	assert.NotContains(t, strings.Join(warnings, "\n"), "not-opted")
	assert.Nil(t, (*Config)(nil).RuntimeKindWarnings())
}
