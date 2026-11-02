package config

import (
	"strings"
	"testing"
)

func TestValidateModelCapabilitiesValid(t *testing.T) {
	c := &Config{}
	c.ModelCapabilities = map[string][]string{
		"halogen-qwen3.8-flash-next": {"vision"},
		"whisper-small":              {"audio-transcription"},
		"moss":                       {"audio-speech", "vision"},
	}
	if err := c.ValidateModelCapabilities(); err != nil {
		t.Fatalf("valid declarations rejected: %v", err)
	}
}

func TestValidateModelCapabilitiesRejectsTypos(t *testing.T) {
	c := &Config{}
	c.ModelCapabilities = map[string][]string{"m": {"vison"}}
	err := c.ValidateModelCapabilities()
	if err == nil || !strings.Contains(err.Error(), "unknown capability") {
		t.Fatalf("typo accepted, err=%v", err)
	}
	if !strings.Contains(err.Error(), "vision, audio-transcription, audio-speech") {
		t.Fatalf("error must name the valid vocabulary: %v", err)
	}
}

func TestValidateModelCapabilitiesRejectsEmpty(t *testing.T) {
	c := &Config{}
	c.ModelCapabilities = map[string][]string{"": {"vision"}}
	if err := c.ValidateModelCapabilities(); err == nil {
		t.Fatal("empty model id accepted")
	}
	c.ModelCapabilities = map[string][]string{"m": {}}
	if err := c.ValidateModelCapabilities(); err == nil {
		t.Fatal("empty capability list accepted")
	}
}

// Pin the config-side vocabulary against the discovery-side map
// (internal/adapter/discovery/capabilities_test.go pins the mapping).
func TestValidateModelCapabilitiesVocabulary(t *testing.T) {
	for _, name := range []string{"vision", "audio-transcription", "audio-speech"} {
		c := &Config{}
		c.ModelCapabilities = map[string][]string{"m": {name}}
		if err := c.ValidateModelCapabilities(); err != nil {
			t.Errorf("valid name %q rejected: %v", name, err)
		}
	}
	for _, name := range []string{"function-calling", "embeddings", "code-generation", "vision "} {
		c := &Config{}
		c.ModelCapabilities = map[string][]string{"m": {name}}
		if err := c.ValidateModelCapabilities(); err == nil {
			t.Errorf("out-of-scope name %q accepted (engine-flag vocab only)", name)
		}
	}
}
