package handlers

import (
	"context"
	"os"

	"github.com/thushan/olla/internal/config"
	"github.com/thushan/olla/internal/core/domain"
	"github.com/thushan/olla/internal/logger"
)

// runtimeMetricsProvider serves the runtime metrics snapshot consumed by the
// status handlers and the Prometheus re-export. Two implementations exist:
// the in-process sampler (default; scrapes each engine's /metrics) and the
// influx provider (reads the canonical llm_runtime series written by the
// ollametrics stack). Snapshot gates - opt-in, routable, known dialect,
// stale bound - hold identically for both, so nothing downstream can tell
// them apart except through the payload's source fields.
type runtimeMetricsProvider interface {
	Snapshot(endpoint *domain.Endpoint) *EndpointRuntimeMetrics
	// LifetimeTokens answers the endpoints-table "Tokens (lifetime)" column.
	// Only the influx provider can: the sampler reads engine counters that
	// reset on restart, so it truthfully answers "not known" (payload key
	// absent, UI dash) in sampler mode.
	LifetimeTokens(name string) (int64, bool)
}

// newRuntimeMetricsSource resolves the configured metrics backend once at
// startup. influx with a missing token env is a configuration mistake that
// must not take the gateway down: it logs and keeps the sampler. The sampler
// is NOT started in influx mode (its own scrapes would be invisible cost),
// and the observe collector exists only in influx mode.
func newRuntimeMetricsSource(ctx context.Context, cfg *config.Config, repository domain.EndpointRepository, log logger.StyledLogger) (runtimeMetricsProvider, *observeCollector, string) {
	if cfg.MetricsSource.InfluxEnabled() {
		token := os.Getenv(cfg.MetricsSource.Influx.TokenEnv)
		if token == "" {
			log.Error("metrics_source influx: token env not set; falling back to sampler",
				"token_env", cfg.MetricsSource.Influx.TokenEnv)
		} else {
			provider := newInfluxMetricsProvider(cfg.MetricsSource.Influx, token, repository, log)
			provider.Start(ctx)
			return provider, newObserveCollector(cfg.MetricsSource.Influx, token, log), config.MetricsSourceModeInflux
		}
	}
	sampler := newEndpointRuntimeMetricsSampler(repository, log)
	sampler.Start(ctx)
	// The EFFECTIVE mode: an influx intent that fell back (missing token)
	// reports sampler, so config.json can never advertise sections the
	// process is not serving.
	return sampler, nil, config.MetricsSourceModeSampler
}
