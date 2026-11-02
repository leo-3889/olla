package health

import (
	"github.com/thushan/olla/internal/config"
)

// DefaultThresholds are the standard success-rate thresholds (90/95) that
// reproduce the historical hard-coded behaviour. They are the fallback used
// when a config does not specify thresholds (e.g. a hand-constructed test
// config); production configs are validated to be non-zero so this only ever
// fires for unpopulated configs. The values mirror config.DefaultConfig's
// Health defaults; they are repeated here rather than imported because the
// config package sits below this one and cannot depend on it.
func DefaultThresholds() config.SuccessRateThresholds {
	return config.SuccessRateThresholds{CriticalBelow: 90, DegradedBelow: 95}
}

// NewEvaluator builds the Evaluator for the configured health mode. An empty
// or unrecognised mode falls back to host mode: the mode is validated at config
// load (HealthConfig.Validate) so a bad value is a startup error in practice,
// but the factory stays total so a mis-wired config can never leave the
// dispatcher with a nil evaluator.
func NewEvaluator(cfg *config.HealthConfig) Evaluator {
	if cfg != nil && cfg.Mode == "model" {
		return NewModelEvaluator(cfg.Models)
	}
	return NewHostEvaluator()
}
