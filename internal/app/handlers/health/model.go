package health

import (
	"context"
	"sync"

	"github.com/thushan/olla/pkg/format"
)

// Per-model entry statuses. These are distinct from the system-level verdicts
// in evaluator.go: a single model is healthy, unhealthy (it has endpoints but
// none are healthy), or unknown (no endpoint currently hosts it, so its health
// cannot be determined).
const (
	modelStatusHealthy   = "healthy"
	modelStatusUnhealthy = "unhealthy"
	modelStatusUnknown   = "unknown"
)

// ModelEvaluator derives the system status from a configured list of
// models/aliases. A model is healthy when at least one endpoint hosting it is
// currently healthy. The verdict is: every model healthy -> healthy; some ->
// degraded; none -> critical. A success-rate leg scoped to the configured
// models' own traffic is combined with that, taking the worse of the two.
//
// It is independent of HostEvaluator: it never reads the endpoint-fleet ratio,
// only the healthy-endpoint set (to test model health) and the per-model stats.
type ModelEvaluator struct {
	models []string

	// lastStatus remembers each model's previous entry status so a change is
	// logged only on transition, not on every poll. Guarded by mu.
	mu         sync.Mutex
	lastStatus map[string]string
}

// NewModelEvaluator returns a model-mode evaluator for the given configured
// model/alias names.
func NewModelEvaluator(models []string) *ModelEvaluator {
	return &ModelEvaluator{
		models:     models,
		lastStatus: make(map[string]string, len(models)),
	}
}

// Mode reports the computation kind for the response's health_mode field.
func (e *ModelEvaluator) Mode() string { return "model" }

// Evaluate computes the model-mode verdict.
func (e *ModelEvaluator) Evaluate(ctx context.Context, in *Input) *Result {
	// healthyURL is the set of endpoint URLs currently healthy. A configured
	// model is healthy iff at least one of its hosting endpoints is in this set.
	healthyURL := make(map[string]struct{}, len(in.Healthy))
	for _, ep := range in.Healthy {
		healthyURL[ep.GetURLString()] = struct{}{}
	}

	entries := make([]ModelHealthEntry, 0, len(e.models))
	healthyCount := 0
	resolvedCount := 0
	var totalReq, totalOK int64

	for _, name := range e.models {
		names := e.resolveNames(name, in)

		// Union of the endpoints that host any of the resolved names. A nil
		// endpointSet means a lookup error (or a missing registry), which is
		// indistinguishable from "no endpoints" for health purposes: the model
		// is unknown.
		var endpointSet map[string]struct{}
		if in.ModelRegistry == nil {
			endpointSet = nil
		} else {
			for _, n := range names {
				endpoints, err := in.ModelRegistry.GetEndpointsForModel(ctx, n)
				if err != nil {
					endpointSet = nil
					break
				}
				if endpointSet == nil {
					endpointSet = make(map[string]struct{})
				}
				for _, url := range endpoints {
					endpointSet[url] = struct{}{}
				}
			}
		}

		entry := ModelHealthEntry{Name: name}

		switch {
		case endpointSet == nil || len(endpointSet) == 0:
			// No endpoint currently hosts this model: its health is unknown.
			// It still counts against the fleet (it is not healthy), so it
			// drags the ratio down, but it is not a confirmed failure.
			entry.Status = modelStatusUnknown
		default:
			resolvedCount++
			healthy := false
			for url := range endpointSet {
				if _, ok := healthyURL[url]; ok {
					healthy = true
					break
				}
			}
			if healthy {
				entry.Status = modelStatusHealthy
				healthyCount++
			} else {
				entry.Status = modelStatusUnhealthy
			}
		}

		// Aggregate the model's own traffic over every resolved name. Stats are
		// keyed by the resolved backend name, so an alias must be summed over
		// its actual models, not the alias string.
		var req, ok int64
		for _, n := range names {
			if ms, exists := in.ModelStats[n]; exists {
				req += ms.TotalRequests
				ok += ms.SuccessfulRequests
			}
		}
		entry.TotalRequests = req
		entry.SuccessfulRequests = ok
		entry.FailedRequests = req - ok
		if req > 0 {
			entry.SuccessRate = format.Percentage(float64(ok) / float64(req) * 100.0)
		} else {
			entry.SuccessRate = "N/A"
		}
		totalReq += req
		totalOK += ok

		entries = append(entries, entry)
		e.logChange(in, name, entry.Status)
	}

	// No configured model resolved to an endpoint: there is no model-based
	// signal (e.g. a fresh boot before model discovery has run). The dispatcher
	// falls back to the host verdict in that case rather than reporting a
	// spurious critical.
	hasSignal := resolvedCount > 0

	// The verdict counts every configured model in the denominator, so a model
	// that is not currently hosted anywhere (unknown) is a non-healthy entry and
	// drags the ratio toward degraded/critical - matching the "all listed healthy
	// -> healthy, partial -> degraded" rule. The all-unknown case is the fresh
	// boot and is handled by the host fallback above, not by this switch.
	total := len(e.models)
	var modelVerdict string
	switch {
	case !hasSignal:
		modelVerdict = StatusCritical
	case healthyCount == 0:
		modelVerdict = StatusCritical
	case healthyCount < total:
		modelVerdict = StatusDegraded
	default:
		modelVerdict = StatusHealthy
	}

	// The success-rate leg is scoped to the configured models' own traffic, not
	// the proxy-wide rate: a healthy fleet can still be failing on the specific
	// models the operator cares about, and vice versa. With no traffic on those
	// models the success rate is undefined and the model-health verdict decides
	// on its own.
	var sr *float64
	if totalReq > 0 {
		rate := float64(totalOK) / float64(totalReq) * 100.0
		sr = &rate
	}

	result := &Result{
		Status:         worst(modelVerdict, successRateVerdict(sr, in.Thresholds)),
		Models:         entries,
		HasModelSignal: hasSignal,
	}
	if hasSignal {
		// A real model-based verdict: the success_rate and has_traffic the
		// handler renders are scoped to the configured models' own traffic.
		result.Scoped = true
		result.SuccessRate = sr
		result.HasTraffic = totalReq > 0
	} else {
		// No model resolved to an endpoint (e.g. a fresh boot before model
		// discovery has run): the handler falls back to the host verdict and
		// keeps the proxy-wide display values, so leave Scoped false and leave
		// SuccessRate/HasTraffic for the handler to fill from the proxy stats.
	}
	return result
}

// resolveNames expands a configured name into the concrete model names to
// query. If the name is a configured model alias it expands to its actual
// models; otherwise the name is used as-is. (With the unifier enabled the
// registry also resolves unified IDs/aliases internally, so a name that is a
// unified alias still finds its endpoints even though it is not a model_aliases
// key.)
func (e *ModelEvaluator) resolveNames(name string, in *Input) []string {
	if in.AliasResolver != nil && in.AliasResolver.IsAlias(name) {
		if actual := in.AliasResolver.GetActualModels(name); len(actual) > 0 {
			return actual
		}
	}
	return []string{name}
}

// logChange emits a log line only when a model's status transitions, so a
// persistently-down model logs once when it goes down and once when it recovers
// rather than on every poll. A nil logger is a no-op (test convenience).
func (e *ModelEvaluator) logChange(in *Input, name, status string) {
	e.mu.Lock()
	prev, seen := e.lastStatus[name]
	e.lastStatus[name] = status
	e.mu.Unlock()

	if seen && prev == status {
		return
	}
	if in.Logger == nil {
		return
	}
	switch {
	case status == modelStatusHealthy:
		if prev != "" {
			in.Logger.Info("Configured model recovered", "model", name, "from", prev)
		}
	default:
		in.Logger.Warn("Configured model not healthy", "model", name, "status", status, "from", prev)
	}
}

// Ensure ModelEvaluator satisfies Evaluator at compile time.
var _ Evaluator = (*ModelEvaluator)(nil)
