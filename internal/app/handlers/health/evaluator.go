// Package health computes the overall system status shown on the dashboard.
//
// Two independent evaluators implement the Evaluator interface: HostEvaluator
// derives the verdict from the endpoint fleet (the historical behaviour) and
// ModelEvaluator derives it from a configured list of models/aliases. The
// status handler is a thin dispatcher that assembles the response from the
// evaluator's verdict; the verdict logic itself lives here, isolated from
// response assembly so each mode can be reasoned about and tested on its own.
//
// Shared kernel: anything that is identical in both modes (the success-rate
// threshold check, the verdict combination, and the per-model entry type) is
// defined exactly once in this file and referenced by both evaluators. Each
// evaluator owns only the part that is genuinely different: how it derives its
// health signal.
package health

import (
	"context"

	"github.com/thushan/olla/internal/adapter/registry"
	"github.com/thushan/olla/internal/config"
	"github.com/thushan/olla/internal/core/domain"
	"github.com/thushan/olla/internal/core/ports"
	"github.com/thushan/olla/internal/logger"
)

// System-level verdict strings. These are the three values the dashboard's
// system.status field can take; they are distinct from the per-model entry
// statuses (healthy/unhealthy/unknown) defined in model.go.
const (
	StatusHealthy  = "healthy"
	StatusDegraded = "degraded"
	StatusCritical = "critical"
)

// Evaluator computes an overall system status verdict from a snapshot of the
// current fleet, traffic, and (for model mode) the configured model list.
type Evaluator interface {
	// Mode reports which computation produced the verdict: ModeHost or
	// ModeModel.
	Mode() string
	// Evaluate derives the verdict for the given snapshot.
	Evaluate(ctx context.Context, in *Input) *Result
}

// Input is everything an evaluator needs to reach a verdict, gathered once per
// status poll by the handler. Both evaluators read from the same shape so the
// dispatcher stays uniform; each evaluator simply ignores the fields it does
// not use.
type Input struct {

	// ModelRegistry maps model names to the endpoints that host them.
	ModelRegistry domain.ModelRegistry

	// Logger is used by ModelEvaluator for warn-on-change logging.
	Logger logger.StyledLogger

	// ModelStats is the per-model request stats keyed by the resolved backend
	// model name. ModelEvaluator aggregates the configured models' traffic
	// from here.
	ModelStats map[string]ports.ModelStats

	// AliasResolver expands a configured name that is a model_aliases key into
	// its actual model names. Nil when no aliases are configured.
	AliasResolver *registry.AliasResolver

	// All and Healthy are the endpoint fleet as seen by the repository.
	// HostEvaluator uses both; ModelEvaluator uses Healthy to test whether a
	// configured model has at least one healthy host.
	All     []*domain.Endpoint
	Healthy []*domain.Endpoint

	// ProxyStats is the proxy-wide request stats. HostEvaluator uses it for
	// the success-rate leg; ModelEvaluator ignores it (its success-rate leg is
	// scoped to the configured models' own traffic, from ModelStats).
	ProxyStats ports.ProxyStats

	// Thresholds are the success-rate thresholds from config, applied in both
	// modes.
	Thresholds config.SuccessRateThresholds
}

// Result is a verdict plus the detail the handler surfaces on the response.
type Result struct {
	// SuccessRate is the success-rate percentage for the relevant scope, or nil
	// when there is no traffic (undefined).
	SuccessRate *float64
	// Status is the overall verdict: StatusHealthy, StatusDegraded, or
	// StatusCritical.
	Status string
	// Models is the per-model breakdown, populated only in model mode.
	Models []ModelHealthEntry
	// HasModelSignal is true when at least one configured model resolved to an
	// endpoint, i.e. a model-based verdict is meaningful. Model mode only; when
	// false the dispatcher falls back to the host verdict rather than reporting
	// a spurious critical for a fleet whose models simply have not been
	// discovered yet.
	HasModelSignal bool
	// Scoped is true when the SuccessRate and HasTraffic below describe the
	// configured models' own traffic (model mode with a real signal) rather than
	// the proxy-wide totals. The handler uses this to pick which success_rate
	// and has_traffic to render: in host mode (and in the model-mode fallback)
	// it is false and the proxy-wide values are shown.
	Scoped bool
	// HasTraffic is true when the relevant scope has seen at least one request.
	HasTraffic bool
}

// ModelHealthEntry is one configured model's contribution to the verdict. It is
// the single definition of the per-model breakdown; the handler's SystemSummary
// references this type directly rather than redefining it.
type ModelHealthEntry struct {
	Name               string `json:"name"`
	Status             string `json:"status"`       // healthy | unhealthy | unknown
	SuccessRate        string `json:"success_rate"` // "N/A" when there is no traffic
	TotalRequests      int64  `json:"total_requests"`
	SuccessfulRequests int64  `json:"successful_requests"`
	FailedRequests     int64  `json:"failed_requests"`
}

// severity ranks a system verdict for comparison: higher is worse. An
// unrecognised value (e.g. an empty string) ranks as the least severe so a
// missing verdict can never escalate the combined result.
func severity(s string) int {
	switch s {
	case StatusCritical:
		return 2
	case StatusDegraded:
		return 1
	default:
		return 0
	}
}

// worst returns the more severe of two system verdicts. Both evaluators
// combine their health-signal verdict with the shared success-rate verdict via
// this, so the "take the worse of the two" rule is defined once.
func worst(a, b string) string {
	if severity(a) >= severity(b) {
		return a
	}
	return b
}

// successRateVerdict maps a success-rate percentage (nil when there is no
// traffic) to a verdict using the configured thresholds. No traffic is not a
// failure: the success rate is undefined, so it contributes a healthy verdict
// and the health-signal verdict decides on its own.
func successRateVerdict(sr *float64, t config.SuccessRateThresholds) string {
	if sr == nil {
		return StatusHealthy
	}
	switch {
	case *sr < t.CriticalBelow:
		return StatusCritical
	case *sr < t.DegradedBelow:
		return StatusDegraded
	default:
		return StatusHealthy
	}
}
