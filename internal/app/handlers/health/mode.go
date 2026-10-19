package health

// Verdict modes: which computation produced the health verdict. The evaluator
// implementations report these, the status payload echoes them in its
// health_mode field, and config.HealthConfig.Mode selects between them
// (config declares the matching YAML vocabulary).
const (
	// ModeHost derives the verdict from endpoint statuses (the default).
	ModeHost = "host"
	// ModeModel derives the verdict from a configured list of models/aliases.
	ModeModel = "model"
)
