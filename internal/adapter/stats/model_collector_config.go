package stats

import "time"

// ModelCollectorConfig provides configuration options for the ModelCollector
type ModelCollectorConfig struct {

	// PercentileTrackerType specifies which percentile tracker to use ("reservoir", "simple", default: "reservoir")
	PercentileTrackerType string

	// MaxTrackedModels is the maximum number of models to track (default: 50)
	MaxTrackedModels int

	// ModelStatsTTL is how long to keep model statistics (default: 6h)
	ModelStatsTTL time.Duration

	// ModelCleanupInterval is how often to clean up old data (default: 30m)
	ModelCleanupInterval time.Duration

	// PercentileSampleSize is the size of the reservoir for percentile calculation (default: 100)
	PercentileSampleSize int

	// EnableDetailedStats enables detailed model-endpoint statistics tracking (default: false)
	EnableDetailedStats bool
}

// DefaultModelCollectorConfig returns the default configuration
func DefaultModelCollectorConfig() *ModelCollectorConfig {
	return &ModelCollectorConfig{
		MaxTrackedModels:      50,               // Reduced from 100
		ModelStatsTTL:         6 * time.Hour,    // Reduced from 24h
		ModelCleanupInterval:  30 * time.Minute, // More frequent cleanup
		PercentileSampleSize:  100,
		EnableDetailedStats:   false,
		PercentileTrackerType: "reservoir",
	}
}

// Validate ensures the configuration values are reasonable
func (c *ModelCollectorConfig) Validate() *ModelCollectorConfig {
	if c.MaxTrackedModels <= 0 {
		c.MaxTrackedModels = 50
	}
	if c.ModelStatsTTL <= 0 {
		c.ModelStatsTTL = 6 * time.Hour
	}
	if c.ModelCleanupInterval <= 0 {
		c.ModelCleanupInterval = 30 * time.Minute
	}
	if c.PercentileSampleSize <= 0 {
		c.PercentileSampleSize = 100
	}
	if c.PercentileTrackerType == "" {
		c.PercentileTrackerType = "reservoir"
	}
	return c
}
