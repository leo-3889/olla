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
	// LifetimeGeneration / LifetimePrefill answer the endpoints-table token
	// columns: the token_lookback (180d) sum of the per-UTC-day integrals of
	// generation_tokens_total / prompt_tokens_total. Only the influx provider
	// can answer them: the sampler holds the RAW engine counters, which reset on
	// every restart and would answer "ever" with a lie, so it truthfully answers
	// "not known" (payload key absent, UI dash) in sampler mode.
	LifetimeGeneration(name string) (int64, bool)
	LifetimePrefill(name string) (int64, bool)
	// KVCacheReusePercent answers the endpoints-table "KV cache reuse" column.
	// Its BASIS is per-engine and the number alone cannot tell the two apart:
	// an engine that publishes the prefix_cache_hit_ratio gauge is answered with
	// that gauge (measured since engine boot - it resets on every restart); an
	// engine that publishes only the cached/computed prompt counters is answered
	// with the cached/(cached+computed) ratio of their day integrals over the
	// effective (retention-capped) token window. The sampler has neither integrals nor
	// gauges, so it answers "not known". See the influx provider implementation.
	KVCacheReusePercent(name string) (float64, bool)
	// KVCacheReuseBasis names, in operator words, the basis the percent above
	// was computed on ("engine prefix-cache gauge ..." vs "180d cached/prompt
	// counter ratio"). The two bases are not comparable and the number alone
	// cannot tell them apart, so the payload discloses which one a cell shows.
	// "" when the reuse percent is not known (same gate as above).
	KVCacheReuseBasis(name string) string
	// LifetimeTokensPartial discloses that the token sums above are a
	// best-effort LOWER BOUND: at least one window day is missing from the
	// sum (cold-boot backfill in flight, or a day query that failed and was
	// skipped). The payload carries it (generation_tokens_partial /
	// prefill_tokens_partial) and the cell tooltip renders "lower bound:
	// <reason>" - the disclosure used to live only in the gateway log.
	// True only in influx mode; the sampler serves no sums, so never.
	LifetimeTokensPartial() bool
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
