package config

import (
	"testing"
)

// The health config defaults must validate clean and reproduce the historical
// 90/95 success-rate thresholds, so a no-config startup is byte-for-byte
// unchanged.
func TestHealthConfig_Defaults(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.Health.Mode != "host" {
		t.Errorf("default health.mode = %q, want \"host\"", cfg.Health.Mode)
	}
	if cfg.Health.SuccessRate.CriticalBelow != 90 || cfg.Health.SuccessRate.DegradedBelow != 95 {
		t.Errorf("default success-rate thresholds = %g/%g, want 90/95",
			cfg.Health.SuccessRate.CriticalBelow, cfg.Health.SuccessRate.DegradedBelow)
	}
	if err := cfg.Health.Validate(); err != nil {
		t.Errorf("default health config must validate clean, got: %v", err)
	}
}

func TestHealthConfig_InvalidMode(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Health.Mode = "bogus"
	if err := cfg.Health.Validate(); err == nil {
		t.Error("expected an error for an unrecognised health.mode")
	}
}

func TestHealthConfig_EmptyModeIsHost(t *testing.T) {
	// An empty mode is tolerated (a config that omits the whole health block)
	// and is treated as host by the evaluator.
	cfg := DefaultConfig()
	cfg.Health.Mode = ""
	if err := cfg.Health.Validate(); err != nil {
		t.Errorf("empty health.mode should be tolerated, got: %v", err)
	}
}

func TestHealthConfig_EmptyModelEntry(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Health.Mode = "model"
	cfg.Health.Models = []string{"llama3:8b", ""}
	if err := cfg.Health.Validate(); err == nil {
		t.Error("expected an error for an empty health.models entry")
	}
}

func TestHealthConfig_WhitespaceModelEntry(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Health.Mode = "model"
	cfg.Health.Models = []string{"  llama3:8b"}
	if err := cfg.Health.Validate(); err == nil {
		t.Error("expected an error for a health.models entry with surrounding whitespace")
	}
}

func TestHealthConfig_ModelModeEmptyListAllowed(t *testing.T) {
	// An empty model list in model mode is deliberately allowed: models appear
	// and disappear at runtime, and the evaluator falls back to host mode.
	cfg := DefaultConfig()
	cfg.Health.Mode = "model"
	cfg.Health.Models = nil
	if err := cfg.Health.Validate(); err != nil {
		t.Errorf("model mode with an empty model list should be allowed, got: %v", err)
	}
}

func TestHealthConfig_UnsetThresholdsAllowed(t *testing.T) {
	// Both thresholds zero means "use the defaults"; that must validate clean so
	// a hand-built config that never sets them is not a startup error.
	cfg := DefaultConfig()
	cfg.Health.SuccessRate = SuccessRateThresholds{}
	if err := cfg.Health.Validate(); err != nil {
		t.Errorf("unset (zero) success-rate thresholds should be allowed, got: %v", err)
	}
}

func TestHealthConfig_OneThresholdSet(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Health.SuccessRate = SuccessRateThresholds{CriticalBelow: 90}
	if err := cfg.Health.Validate(); err == nil {
		t.Error("expected an error when only one of the two thresholds is set")
	}
}

func TestHealthConfig_CriticalExceedsDegraded(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Health.SuccessRate = SuccessRateThresholds{CriticalBelow: 98, DegradedBelow: 95}
	if err := cfg.Health.Validate(); err == nil {
		t.Error("expected an error when critical_below exceeds degraded_below")
	}
}

func TestHealthConfig_ThresholdOutOfRange(t *testing.T) {
	cases := []SuccessRateThresholds{
		{CriticalBelow: 101, DegradedBelow: 95},
		{CriticalBelow: 90, DegradedBelow: 101},
	}
	for _, tc := range cases {
		cfg := DefaultConfig()
		cfg.Health.SuccessRate = tc
		if err := cfg.Health.Validate(); err == nil {
			t.Errorf("expected an error for out-of-range thresholds %v", tc)
		}
	}
}

func TestHealthConfig_ValidModelMode(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Health.Mode = "model"
	cfg.Health.Models = []string{"llama3:8b", "qwen2.5:7b"}
	cfg.Health.SuccessRate = SuccessRateThresholds{CriticalBelow: 90, DegradedBelow: 95}
	if err := cfg.Health.Validate(); err != nil {
		t.Errorf("a valid model-mode health config must validate clean, got: %v", err)
	}
}
