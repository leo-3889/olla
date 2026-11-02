package health

import (
	"context"
)

// HostEvaluator derives the system status from the endpoint fleet: the ratio
// of healthy endpoints, combined with the proxy-wide success rate. This is the
// historical behaviour, moved out of the status handler so the two health modes
// live in independent, symmetric evaluators.
type HostEvaluator struct{}

// NewHostEvaluator returns the host-mode evaluator. It is stateless; the
// verdict is a pure function of the input snapshot.
func NewHostEvaluator() *HostEvaluator {
	return &HostEvaluator{}
}

// Mode reports the computation kind for the response's health_mode field.
func (e *HostEvaluator) Mode() string { return "host" }

// Evaluate computes the host-mode verdict.
func (e *HostEvaluator) Evaluate(_ context.Context, in *Input) *Result {
	hasTraffic := in.ProxyStats.TotalRequests > 0

	// A proxy with nothing to route to cannot be healthy regardless of traffic,
	// so this is decided before the ratio is computed: len(healthy)/len(all)
	// would be 0/0 = NaN and every NaN comparison below would be false, letting
	// an empty fleet fall through to the default healthy case.
	if len(in.All) == 0 {
		return &Result{Status: StatusCritical, HasTraffic: hasTraffic}
	}

	healthyRatio := float64(len(in.Healthy)) / float64(len(in.All))

	var healthVerdict string
	switch {
	case healthyRatio < 0.5:
		healthVerdict = StatusCritical
	case healthyRatio < 0.8:
		healthVerdict = StatusDegraded
	default:
		healthVerdict = StatusHealthy
	}

	// The success-rate leg is shared with model mode: the proxy-wide success
	// rate is evaluated against the configured thresholds and combined with the
	// health-signal verdict, taking the worse of the two. With no traffic the
	// success rate is undefined and contributes a healthy verdict, so the
	// endpoint-ratio verdict decides on its own.
	var sr *float64
	if hasTraffic {
		rate := float64(in.ProxyStats.SuccessfulRequests) / float64(in.ProxyStats.TotalRequests) * 100.0
		sr = &rate
	}

	// Host mode reports the proxy-wide display values (Scoped stays false so
	// the handler renders the proxy-wide success_rate / has_traffic).
	return &Result{
		Status:      worst(healthVerdict, successRateVerdict(sr, in.Thresholds)),
		SuccessRate: sr,
		HasTraffic:  hasTraffic,
	}
}

// Ensure HostEvaluator satisfies Evaluator at compile time.
var _ Evaluator = (*HostEvaluator)(nil)
