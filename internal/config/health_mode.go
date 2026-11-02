package config

// health.mode vocabulary (the YAML side of health.ModeHost / health.ModeModel
// in internal/app/handlers/health; config cannot import that package, so the
// pair is mirrored here and pinned by the config docs test).
const (
	// HealthModeHost derives the verdict from endpoint statuses (default).
	HealthModeHost = "host"
	// HealthModeModel derives it from a configured list of models/aliases.
	HealthModeModel = "model"
)
