package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/thushan/olla/internal/config"
	"github.com/thushan/olla/internal/core/domain"
	"github.com/thushan/olla/internal/logger"
)

// influxMetricsInterval matches the sampler cadence: the dashboard's poll
// budget and the ETag semantics are tuned to it.
const influxMetricsInterval = 10 * time.Second

// influxQueryRange bounds EVERY runtime query - the last-value probe and the
// 1m-window aggregates share one window on purpose. The bound MUST stay >=
// the load reader's freshness gate (loadReaderMaxSampleAge = 90s) with
// margin: a last() window narrower than the gate dropped whole endpoint
// states (no rows in range) while the window queries still had data, so an
// exporter gap deleted the runtime keys between 5 and 11 minutes instead of
// serving them with the stale flag. The envelope's own staleness gate
// (stale_after, 60s in production) decides what is "fresh"; this bound only
// decides what still EXISTS.
const influxQueryRange = "-11m"

// lifetimeTokensTTL gates the CURRENT-day recompute of the lifetime-tokens
// day parts. One day-window integral over llm_runtime costs seconds at
// influxd, so the 10s refresh cycle must NOT re-fan it (let alone per 5s
// dashboard poll): the current day is recomputed at most once per TTL, and
// CLOSED days - whose data can no longer change - are computed exactly once
// and cached for the provider's life.
const lifetimeTokensTTL = 60 * time.Second

// tokenBackfillPerCycle bounds how many missing CLOSED day parts one refresh
// computes. A cold boot with a 180d lookback has up to 180 day windows to
// integrate and each costs seconds at influxd, so they are filled LAZILY,
// newest first, one per cycle; until they arrive the served sum is a
// best-effort lower bound (disclosed via noteTokenGap, the column renders).
const tokenBackfillPerCycle = 1

// tokenDayTimeout bounds ONE lifetime-tokens day-window integral. It must
// NOT reuse fluxClientTimeout (5s): that budget exists to bound the
// interactive poll, not a background integral. Reusing it livelocked the
// leg (measured): an 18s day query was ALWAYS cancelled, influxd recorded
// status=499, the day part dropped, and the next cycle retried it -
// forever, with the served number a permanently-empty lower bound. 60s is
// a generous headroom over the ~0.6s a pushdown-restored day costs.
const tokenDayTimeout = 60 * time.Second

// Negative cache for FAILED day windows: one slow/broken day must not
// consume the cycle budget on every 10s refresh (the retry-storm half of
// the livelock). A failed day is retried only after tokenDayRetryBackoff
// and abandoned at tokenDayMaxAttempts. The honesty contract is unchanged
// either way: the day stays excluded from the sum and the served number
// stays a disclosed lower bound (noteTokenGap).
const (
	tokenDayRetryBackoff = time.Hour
	tokenDayMaxAttempts  = 5
)

// influxDefaultTokenLookback mirrors config.defaultInfluxTokenLookback for
// providers constructed without a validated config (tests, direct wiring):
// the llm/metrics retention, so the window is "every counter sample we keep".
const influxDefaultTokenLookback = 180 * 24 * time.Hour

// tokenDayFailure is one day's negative-cache record.
type tokenDayFailure struct {
	nextTry  time.Time
	attempts int
}

// tokenDayPart is one endpoint's cached integrals for one UTC calendar day,
// ONE number per counter field the engine actually published that day.
// Presence is tracked per field: an engine that does not publish a counter
// leaves that part UNSET (radiance publishes no cached_prompt_tokens_total),
// which is what keeps "the engine publishes no such counter" distinguishable
// from "the counter read zero", and what keeps a KV-reuse ratio from ever
// dividing by a denominator the provider invented.
type tokenDayPart struct {
	gen, prefill, cached          int64
	hasGen, hasPrefill, hasCached bool
}

// set folds one counter field's day integral into the part.
func (t *tokenDayPart) set(field string, value int64) {
	switch field {
	case tokenFieldGeneration:
		t.gen, t.hasGen = value, true
	case tokenFieldPrefill:
		t.prefill, t.hasPrefill = value, true
	case tokenFieldCached:
		t.cached, t.hasCached = value, true
	}
}

// tokenDayLayout keys the day-part cache by UTC calendar day.
const tokenDayLayout = "2006-01-02" // nosec G101: date layout, not a credential

// influxFieldQueueDepth is the llm_runtime field carrying the engine queue
// depth - the signal load-aware Busy grading steers on, so its freshness is
// tracked per field, not per endpoint (see QueueDepthFor).
const influxFieldQueueDepth = "queue_depth"

// The token counters the endpoints table integrates per UTC day. Gen and
// prefill are the two token productions; cached_prompt_tokens_total is the
// prefix-cached share of the prompt counters. llama.cpp-family engines publish
// that counter and NO prefix_cache_hit_ratio gauge, so their KV-reuse basis is
// the 180d ratio of the two day-integrals; radiance/vLLM-family engines publish
// the gauge and (in this fleet) no counter that reproduces it, so their basis
// is the gauge itself. A field an engine does not publish yields NO rows and
// stays UNSET in the day part - never 0 - so a ratio can never divide a real
// numerator by a fabricated denominator, and "no data" stays distinguishable
// from "zero tokens".
const (
	tokenFieldGeneration = "generation_tokens_total"
	tokenFieldPrefill    = "prompt_tokens_total"
	tokenFieldCached     = "cached_prompt_tokens_total"
	// influxFieldPrefixCacheHitRatio is the engine-computed prefix-cache hit
	// gauge: a rate measured SINCE ENGINE BOOT, so it RESETS on every engine
	// restart and answers a fresh window's rate, not a lifetime ratio.
	influxFieldPrefixCacheHitRatio = "prefix_cache_hit_ratio"
)

// tokenDayFields is the per-endpoint fan-out of one day window (order = render
// order): one tag-equality query per endpoint per field, because the day part
// must say which counter each number came from.
var tokenDayFields = []string{tokenFieldGeneration, tokenFieldPrefill, tokenFieldCached}

// fluxQuerier is the seam the provider queries through (httptest stubs in
// unit tests replace the HTTP client).
type fluxQuerier interface {
	query(ctx context.Context, flux string) ([]map[string]any, error)
}

type influxEndpointState struct {
	newest         time.Time
	availableSince time.Time
	// queueAt is lastAt["queue_depth"] captured at assembly: the queue
	// gauge's OWN sample time. newest is the freshest point of ANY field, and
	// on a live engine the throughput fields update every scrape while
	// queue_depth moves only when the queue moves - gating on newest judged a
	// days-old queue reading as fresh.
	queueAt time.Time
	// cacheAt / cacheRatio are last()["prefix_cache_hit_ratio"] with the GAUGE'S
	// OWN sample time: the KV-reuse basis on engines that publish it. A zero
	// cacheAt means the engine publishes no such gauge (llama.cpp family), which
	// is what routes KVCacheReusePercent onto the counter-ratio basis.
	cacheAt    time.Time
	metrics    []RuntimeMetric
	cacheRatio float64
}

// influxMetricsProvider reads engine runtime metrics from the ollametrics
// InfluxDB (measurement llm_runtime) instead of scraping each engine. It is
// a runtimeMetricsProvider, so every Snapshot gate (opt-in, routable,
// dialect, stale bound) applies identically for both sources.
type influxMetricsProvider struct {
	repository domain.EndpointRepository
	logger     logger.StyledLogger
	client     fluxQuerier
	now        func() time.Time

	states   map[string]*influxEndpointState
	firstSee map[string]time.Time
	// legFailing tracks per-query-leg failure across cycles so a PARTIAL
	// cycle failure (one window leg dies while last() lives) is logged on
	// transition: the all-fail warn only covers the total case, and silently
	// blanked avg/max columns were indistinguishable from an idle engine.
	legFailing map[string]bool
	// tokenDays is the day-part cache: UTC calendar day ("2006-01-02") ->
	// endpoint -> that day's PER-COUNTER integrals. A CLOSED day is computed
	// exactly ONCE (its data can no longer change) and kept for the
	// provider's life; the current day is recomputed at most once per
	// tokenTTL. tokenDayAt records when each part was computed.
	tokenDays  map[string]map[string]tokenDayPart
	tokenDayAt map[string]time.Time
	// tokenDayFails is the negative cache: day key -> attempts so far +
	// the earliest retry time. Cleared on success; evicted with the day
	// when sumTokenDays drops the day out of the lookback.
	tokenDayFails map[string]tokenDayFailure
	// genTokens / prefillTokens / cachedTokens are the SERVED endpoint ->
	// lifetime sums per counter field: the sum of the cached day parts inside
	// tokenLookback that carry that field, rebuilt every refresh. nil means
	// "nothing trustworthy right now" for that field - with NO day part of it
	// available the column dashes; the provider never serves numbers from a
	// window it cannot see. The sums are independent on purpose: an endpoint can
	// have prefill rows and no generation rows, and no cached rows at all
	// (radiance); the third map is also the KV-reuse numerator.
	genTokens     map[string]int64
	prefillTokens map[string]int64
	cachedTokens  map[string]int64
	// readRetention is the boot-time retention probe seam (nil in tests:
	// no HTTP).
	readRetention func(ctx context.Context) (time.Duration, error)
	bucket        string
	// kvBasisCounter is the pre-rendered counter-basis disclosure for the
	// effective window (constants cannot carry the dynamic window label).
	kvBasisCounter string
	// tokenLookback bounds the lifetime-tokens window; day parts older than
	// it are dropped from the sum and from the cache.
	tokenLookback time.Duration
	// tokenTTL bounds the CURRENT-day recompute (closed days never recompute).
	tokenTTL   time.Duration
	staleAfter time.Duration

	// retention is the runtime bucket's measured retention (0 = not read
	// or unreadable): the EFFECTIVE token window is min(tokenLookback,
	// retention), read ONCE at boot (cached forever) from the influx
	// buckets API. When the read fails the window falls back to
	// tokenLookback and the log says so - the label then states the
	// configured intent, which is the honest best the process can do.
	retention time.Duration

	mu sync.RWMutex
	// tokenGap is the disclosed state of the last sum: at least one window
	// day had no part (cold-boot backfill in flight, or a day query that
	// failed and was SKIPPED), so the served number is a best-effort LOWER
	// bound. Logged on transition (noteTokenGap) - a silent lower bound
	// would read as exact.
	tokenGap bool
}

func newInfluxMetricsProvider(influx config.InfluxSourceConfig, token string, repository domain.EndpointRepository, log logger.StyledLogger) *influxMetricsProvider {
	lookback := influx.TokenLookback
	if lookback <= 0 {
		lookback = influxDefaultTokenLookback
	}
	return &influxMetricsProvider{
		repository: repository,
		logger:     log,
		// Single-flight + TTL cache: identical Flux texts (the shared
		// llm_runtime windows) hit influxd once per cycle, not per
		// consumer (status/endpoints, observe fan-out).
		client:        newFluxResultCache(newFluxClient(influx.URL, influx.Org, token), fluxResultCacheTTL),
		bucket:        influx.Bucket,
		tokenLookback: lookback,
		tokenTTL:      lifetimeTokensTTL,
		staleAfter:    influx.StaleAfter,
		now:           time.Now,
		readRetention: bucketRetentionReader(influx.URL, influx.Org, influx.Bucket, token),
		kvBasisCounter: fmt.Sprintf(
			"%s ratio of cached/(cached+computed) prompt counter integrals",
			tokenWindowLabel(lookback),
		),
		states:        map[string]*influxEndpointState{},
		legFailing:    map[string]bool{},
		firstSee:      map[string]time.Time{},
		tokenDays:     map[string]map[string]tokenDayPart{},
		tokenDayAt:    map[string]time.Time{},
		tokenDayFails: map[string]tokenDayFailure{},
	}
}

// bucketRetentionReader reads one bucket's retention once via the influx
// management API (GET /api/v2/buckets - the Flux query API does not expose
// retention). Returns nil when the provider has no connection info (tests).
func bucketRetentionReader(baseURL, org, bucket, token string) func(context.Context) (time.Duration, error) {
	if baseURL == "" || bucket == "" {
		return nil
	}
	client := &http.Client{Timeout: fluxClientTimeout}
	return func(ctx context.Context) (time.Duration, error) {
		q := url.Values{"org": {org}, "bucket": {bucket}}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet,
			strings.TrimRight(baseURL, "/")+"/api/v2/buckets?"+q.Encode(), nil)
		if err != nil {
			return 0, err
		}
		req.Header.Set("Authorization", "Token "+token)
		resp, err := client.Do(req)
		if err != nil {
			return 0, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			excerpt, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
			return 0, fmt.Errorf("influx buckets API: %s: %s", resp.Status, strings.TrimSpace(string(excerpt)))
		}
		var payload struct {
			Buckets []struct {
				Name           string `json:"name"`
				RetentionRules []struct {
					Type         string `json:"type"`
					EverySeconds int64  `json:"everySeconds"`
				} `json:"retentionRules"`
			} `json:"buckets"`
		}
		if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&payload); err != nil {
			return 0, err
		}
		for _, b := range payload.Buckets {
			if b.Name != bucket {
				continue
			}
			for _, r := range b.RetentionRules {
				if r.Type == "filter" && r.EverySeconds > 0 {
					return time.Duration(r.EverySeconds) * time.Second, nil
				}
			}
			return 0, nil // retention 0 = infinite: no cap
		}
		return 0, fmt.Errorf("influx buckets API: bucket %q not listed", bucket)
	}
}

// lookupRetention runs the ONE boot-time retention probe and caches it. Any
// failure leaves retention 0 (no cap): the effective window falls back to
// the configured token_lookback and the log says the label then states the
// configured intent, not a measured bound.
func (p *influxMetricsProvider) lookupRetention(ctx context.Context) {
	if p == nil || p.readRetention == nil {
		return
	}
	d, err := p.readRetention(ctx)
	if err != nil {
		if p.logger != nil {
			p.logger.Warn("metrics_source influx: bucket retention unreadable; token window labels state the configured token_lookback",
				"bucket", p.bucket, "token_lookback", p.tokenLookback.String(), "error", err.Error())
		}
		return
	}
	if d <= 0 {
		return
	}
	p.mu.Lock()
	p.retention = d
	capped := d < p.tokenLookback
	if capped {
		p.kvBasisCounter = fmt.Sprintf("%s ratio of cached/(cached+computed) prompt counter integrals", tokenWindowLabel(d))
	}
	p.mu.Unlock()
	if capped && p.logger != nil {
		p.logger.Info("metrics_source influx: token window capped by bucket retention",
			"bucket", p.bucket, "retention", d.String(), "token_lookback", p.tokenLookback.String())
	}
}

// tokenWindow reports the EFFECTIVE lifetime-token window: the configured
// token_lookback capped by the measured bucket retention - data older than
// the retention does not exist, so a label may not claim it.
func (p *influxMetricsProvider) tokenWindow() time.Duration {
	if p == nil {
		return 0
	}
	window := p.tokenLookback
	p.mu.RLock()
	retention := p.retention
	p.mu.RUnlock()
	if retention > 0 && retention < window {
		return retention
	}
	return window
}

// TokenWindowLabel names the effective token window for the dashboard
// manifest ("90d"), so the column header never claims a window the bucket
// cannot cover.
func (p *influxMetricsProvider) TokenWindowLabel() string {
	if p == nil {
		return ""
	}
	return tokenWindowLabel(p.tokenWindow())
}

// tokenWindowLabel renders a window as whole days (the lookback unit the
// labels use), at least 1d.
func tokenWindowLabel(d time.Duration) string {
	days := int64(d / (24 * time.Hour))
	if days < 1 {
		days = 1
	}
	return fmt.Sprintf("%dd", days)
}

// LifetimeTokensPartial implements the lower-bound disclosure surface: true
// whenever the last rebuild skipped a window day (cold-boot backfill in
// flight, or a day query that failed): the served token sums are then a
// best-effort LOWER bound (the same state noteTokenGap logs on transition).
func (p *influxMetricsProvider) LifetimeTokensPartial() bool {
	if p == nil {
		return false
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.tokenGap
}

func (p *influxMetricsProvider) Start(ctx context.Context) {
	if p == nil {
		return
	}
	go func() {
		p.lookupRetention(ctx)
		p.refresh(ctx)
		ticker := time.NewTicker(influxMetricsInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				p.refresh(ctx)
			}
		}
	}()
}

// trackedEndpoints returns the names of opted-in, routable endpoints with a
// resolvable dialect, plus the set of all repository names for pruning. The
// Influx endpoint tag equals the olla endpoint Name.
func (p *influxMetricsProvider) trackedEndpoints(ctx context.Context) ([]string, map[string]struct{}, bool) {
	endpoints, err := p.repository.GetAll(ctx)
	if err != nil {
		return nil, nil, false
	}
	var names []string
	live := make(map[string]struct{}, len(endpoints))
	for _, endpoint := range endpoints {
		if endpoint == nil {
			continue
		}
		live[endpoint.Name] = struct{}{}
		if !endpoint.RuntimeMetricsEnabled || runtimeMetricsKind(endpoint) == "" || !endpoint.Status.IsRoutable() {
			p.markUnavailable(endpoint.Name)
			continue
		}
		names = append(names, endpoint.Name)
	}
	return names, live, true
}

func (p *influxMetricsProvider) markUnavailable(name string) {
	p.mu.Lock()
	delete(p.states, name)
	p.mu.Unlock()
}

func (p *influxMetricsProvider) pruneLocked(live map[string]struct{}) {
	for name := range p.states {
		if _, ok := live[name]; !ok {
			delete(p.states, name)
			delete(p.firstSee, name)
		}
	}
}

// refresh runs the bounded Flux queries for one cycle and rebuilds all
// endpoint states. A fully failed cycle keeps the previous states; their
// age grows, which is exactly how the sampler treats a dead engine.
func (p *influxMetricsProvider) refresh(ctx context.Context) {
	if p == nil || p.repository == nil {
		return
	}
	names, live, ok := p.trackedEndpoints(ctx)
	if !ok {
		return
	}
	p.mu.Lock()
	p.pruneLocked(live)
	p.mu.Unlock()
	p.refreshLifetimeTokens(ctx)
	if len(names) == 0 {
		return
	}
	// One budget for the whole cycle (5 sequential queries) plus a hard
	// per-query cap: a slow influx can no longer starve the later queries
	// (they used to share a 2x budget, so Q4/Q5 died every cycle under a
	// slow query and the avg/max columns stayed permanently empty).
	cycleCtx, cancel := context.WithTimeout(ctx, 6*fluxClientTimeout)
	defer cancel()
	query := func(flux string) ([]map[string]any, error) {
		qctx, qcancel := context.WithTimeout(cycleCtx, fluxClientTimeout)
		defer qcancel()
		return p.client.query(qctx, flux)
	}
	lastRows, err := query(influxBase(p.bucket, names) + "|> last()")
	gaugeMean, errMean := query(influxBase(p.bucket, names) + influxFieldFilter("decode_tokens_per_second", "prefill_tokens_per_second", "prefix_cache_hit_ratio", "kv_cache_usage_ratio", "draft_acceptance_ratio", "requests_running") + "|> aggregateWindow(every:1m, fn:mean, createEmpty:false)")
	counterMean, errCounter := query(influxBase(p.bucket, names) + influxFieldFilter("generation_tokens_total", "prompt_tokens_total") + "|> derivative(unit:1s, nonNegative:true) |> aggregateWindow(every:1m, fn:mean, createEmpty:false)")
	counterMax, errCounterMax := query(influxBase(p.bucket, names) + influxFieldFilter("generation_tokens_total", "prompt_tokens_total") + "|> derivative(unit:1s, nonNegative:true) |> aggregateWindow(every:1m, fn:max, createEmpty:false)")
	// The all-fail leg is logged on transition (noteLeg), not per cycle: a
	// down influx used to Warn every 10s. Previous samples are kept either way.
	if err != nil && errMean != nil && errCounter != nil && errCounterMax != nil {
		p.noteLeg("all", err)
		return
	}
	p.noteLeg("all", nil)
	gaugeMax, errGaugeMax := query(influxBase(p.bucket, names) + influxFieldFilter("decode_tokens_per_second", "prefill_tokens_per_second", "prefix_cache_hit_ratio", "kv_cache_usage_ratio", "draft_acceptance_ratio", "requests_running") + "|> aggregateWindow(every:1m, fn:max, createEmpty:false)")
	// Raw scrape points (NO aggregateWindow) for the saturation pair: the
	// engine writes requests_running and queue_depth at identical
	// timestamps, so the queue value at the running peak's timestamp is an
	// exact join, not an approximation. ~132 points per field per endpoint
	// over the window: bounded by the same -11m range every leg shares.
	pairedRaw, errPaired := query(influxBase(p.bucket, names) + influxFieldFilter("requests_running", influxFieldQueueDepth))
	p.noteLeg("last", err)
	p.noteLeg("gaugeMean", errMean)
	p.noteLeg("gaugeMax", errGaugeMax)
	p.noteLeg("counterMean", errCounter)
	p.noteLeg("counterMax", errCounterMax)
	p.noteLeg("pairedRaw", errPaired)

	series := map[string]*influxSeries{}
	absorbAll := func(rows []map[string]any, bucket string) {
		for _, row := range rows {
			name, _ := row["endpoint"].(string)
			if name == "" {
				continue
			}
			s := series[name]
			if s == nil {
				s = newInfluxSeries()
				series[name] = s
			}
			s.absorb(row, bucket)
		}
	}
	absorbAll(lastRows, "last")
	absorbAll(gaugeMean, "gaugeMean")
	absorbAll(gaugeMax, "gaugeMax")
	absorbAll(counterMean, "counterMean")
	absorbAll(counterMax, "counterMax")
	absorbAll(pairedRaw, "paired")

	next := make(map[string]*influxEndpointState, len(series))
	for _, name := range names {
		s := series[name]
		if s == nil || s.newest.IsZero() {
			// No points inside the bounded ranges: report nothing rather
			// than resurrecting a sample that stopped arriving.
			continue
		}
		metrics := s.envelope()
		if len(metrics) == 0 {
			continue
		}
		p.mu.Lock()
		since, seen := p.firstSee[name]
		if !seen {
			since = s.newest
			p.firstSee[name] = since
		}
		p.mu.Unlock()
		next[name] = &influxEndpointState{
			metrics:        metrics,
			newest:         s.newest,
			availableSince: since,
			queueAt:        s.at(influxFieldQueueDepth),
			cacheAt:        s.at(influxFieldPrefixCacheHitRatio),
			cacheRatio:     s.last[influxFieldPrefixCacheHitRatio],
		}
	}
	p.mu.Lock()
	for name, state := range next {
		p.states[name] = state
	}
	p.mu.Unlock()
}

// refreshLifetimeTokens maintains the lifetime-tokens day parts and rebuilds
// the served sums. The authority is the DIRECT counter integral over the
// runtime bucket - llm_runtime's token counters, integrated per UTC calendar
// day and PER FIELD (generation / prefill / cached prompt) - NOT the cost
// rollups: llm_endpoint_cost_v3 is a lossy downstream derivation, and a
// dropped or incomplete cost interval silently shrinks a lifetime total
// (measured: 233.2M ledger vs 254.4M true counter integral over the same
// 24h). A cumulative counter cannot answer "ever" (it resets on every engine
// restart), but a per-day integral can: difference(nonNegative: true) clamps
// a restart's reset step to 0, and grouping by the FULL tag identity keeps
// one series per identity, so a restart that re-registers tags (model /
// runtime change) never interleaves two series and corrupts the deltas.
//
// Cost: one day-window query costs seconds at influxd (a full-lookback scan
// is not feasible: the 180d live window exceeds any poll budget), hence the
// DAY-PART CACHE:
//   - a CLOSED day (day end <= now) is computed exactly ONCE and cached for
//     the provider's life - its data can no longer change;
//   - the CURRENT day is recomputed at most once per tokenTTL (60s);
//   - on boot the closed days backfill LAZILY, newest first, at most
//     tokenBackfillPerCycle per refresh (bounded by token_lookback), so a
//     cold start never fans 180 queries in one cycle.
//
// Honesty rules:
//   - the endpoint set is the CURRENT configured repository names; ghost ids
//     in the data (a rename's pre-rename rows, retired endpoints) are
//     excluded server-side by the per-endpoint tag equality AND by a set
//     guard client-side.
//     Renames are deliberately NOT aliased: -64's number excludes the
//     radiance-qfn rows. Aliasing would need a metrics.yaml mapping, which
//     is ollametrics scope.
//   - a day whose query ERRORS is SKIPPED from the sum (its cached part is
//     dropped), so the served number is a best-effort LOWER bound, disclosed
//     on transition via noteTokenGap; it never crashes the poll, never fakes
//     the missing day, and the next cycles retry it.
//   - with NO day part available at all (cold boot, influx down) the map is
//     nil: LifetimeTokens reports "not known" -> the payload omits the key ->
//     the UI dashes it. Serving numbers from a window the provider cannot
//     see would be a stale lie.
//   - the leg logs warn/recover on transitions via noteLeg, and its failure
//     never touches the runtime-metrics states: one leg dying must not
//     blank the throughput tiles (partial-failure disclosure, the observe
//     pattern).
func (p *influxMetricsProvider) refreshLifetimeTokens(ctx context.Context) {
	if p.bucket == "" {
		return
	}
	names, live, ok := p.configuredEndpointNames(ctx)
	if !ok {
		p.noteLeg("lifetimeTokens", errEndpointsUnreachable)
		p.mu.Lock()
		p.genTokens, p.prefillTokens, p.cachedTokens = nil, nil, nil
		p.mu.Unlock()
		return
	}
	if len(names) == 0 {
		// Empty-set guard: an empty contains(set:[]) set is a Flux compile
		// hazard, and there is nothing to render anyway.
		p.noteLeg("lifetimeTokens", nil)
		p.mu.Lock()
		p.genTokens, p.prefillTokens, p.cachedTokens = map[string]int64{}, map[string]int64{}, map[string]int64{}
		p.mu.Unlock()
		return
	}
	targets := tokenTargets{names: names, live: live}
	now := p.now()
	today := utcDayStart(now)
	first := utcDayStart(now.Add(-p.tokenLookback))

	// The current day first: it carries the newest production.
	if p.tokenDayDue(today, now) {
		p.computeTokenDay(ctx, today, targets)
	}
	// Then lazily backfill missing CLOSED days, newest first, on a strict
	// per-cycle budget (the cold-boot case).
	for day, budget := today.AddDate(0, 0, -1), tokenBackfillPerCycle; !day.Before(first) && budget > 0; day = day.AddDate(0, 0, -1) {
		if p.tokenDayCached(day) || !p.tokenDayRetryAllowed(day, now) {
			continue
		}
		p.computeTokenDay(ctx, day, targets)
		budget--
	}
	p.sumTokenDays(first, now)
}

// errEndpointsUnreachable labels the lifetime leg's failure to read the
// endpoint repository: the ghost-id set guard needs the configured set, so
// without it the query cannot be bounded honestly.
var errEndpointsUnreachable = errors.New("endpoint repository unavailable")

// tokenTargets is the configured endpoint set for one lifetime day window:
// the names (ONE tag-equality query each - a set-membership endpoint filter
// kills tag pushdown and scans the whole measurement: measured >120s per day
// vs 2-3s) plus the Go set for the client-side ghost guard.
type tokenTargets struct {
	live  map[string]struct{}
	names []string
}

// configuredEndpointNames returns the deduplicated names of ALL configured
// endpoints. The lifetime column counts an endpoint that opted out of the
// runtime tiles too - it still served traffic, and token production is not a
// runtime-tile concern. ok=false means the repository failed.
func (p *influxMetricsProvider) configuredEndpointNames(ctx context.Context) ([]string, map[string]struct{}, bool) {
	endpoints, err := p.repository.GetAll(ctx)
	if err != nil {
		return nil, nil, false
	}
	names := make([]string, 0, len(endpoints))
	live := make(map[string]struct{}, len(endpoints))
	for _, endpoint := range endpoints {
		if endpoint == nil || endpoint.Name == "" {
			continue
		}
		if _, dup := live[endpoint.Name]; dup {
			continue
		}
		names = append(names, endpoint.Name)
		live[endpoint.Name] = struct{}{}
	}
	return names, live, true
}

// utcDayStart is the UTC midnight opening a timestamp's calendar day - the
// day-part boundary. UTC (not local) so the window is the same for every
// viewer of one gateway.
func utcDayStart(t time.Time) time.Time {
	y, m, d := t.UTC().Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

// tokenDayFlux renders the VERIFIED day-window integral for ONE endpoint and
// ONE counter field (the fan-out is endpoint x field: the day part must know
// which counter a number came from, and an engine that publishes no such
// counter yields NO rows for it):
// the one counter field of llm_runtime, filtered by TAG EQUALITY on the
// endpoint (influxd pushes tag filters down; a contains(set) endpoint filter
// scans the whole measurement - measured >120s for one day vs 2-3s), with
// the field as a single equality in ITS OWN filter - the pushdown
// planner handles only and/or/not of comparisons on _measurement/_field/
// tags, and any function call inside a filter expression disables pushdown
// for the WHOLE filter, so mixing contains(value:r._field, ...) into the
// tag predicate made the storage read scan every llm_runtime series of the
// day (measured 18.29s wall / 44.45 CPU-s vs 0.56s / 1.62 CPU-s for one
// day), grouped by the FULL tag identity (model/runtime tags split a series on
// engine restart - grouping less than that interleaves two series and corrupts
// every delta after the split), difference(nonNegative: true) clamping a
// restart's reset step to 0 (and each series' first-row delta to 0), then
// summed to one number per endpoint. This influxd rejects difference(n:), hence
// the nonNegative form; range() takes unquoted RFC3339 datetime literals.
func tokenDayFlux(bucket string, day, stop time.Time, endpoint, field string) string {
	return fmt.Sprintf(`from(bucket:%q) |> range(start:%s, stop:%s) |> filter(fn:(r)=>r._measurement=="llm_runtime" and r.endpoint==%s) |> filter(fn:(r)=>r._field==%s) |> group(columns:["endpoint","_field","model","runtime","engine_id","engine","node","role","display_name"]) |> difference(nonNegative: true) |> sum(column:"_value") |> group(columns:["endpoint"]) |> sum()`,
		bucket, day.UTC().Format(time.RFC3339Nano), stop.UTC().Format(time.RFC3339Nano), fluxQuoted(endpoint), fluxQuoted(field))
}

// tokenDayDue reports whether a day part must be (re)computed: a missing part
// is always due; a CLOSED day that has one is never due again (permanent
// cache); the current day re-arms at the TTL.
func (p *influxMetricsProvider) tokenDayDue(day time.Time, now time.Time) bool {
	key := day.Format(tokenDayLayout)
	p.mu.RLock()
	defer p.mu.RUnlock()
	at, ok := p.tokenDayAt[key]
	if !ok {
		return p.tokenDayRetryAllowedLocked(key, now)
	}
	if !day.AddDate(0, 0, 1).After(now) {
		return false
	}
	return !now.Before(at.Add(p.tokenTTL))
}

// tokenDayRetryAllowedLocked consults the negative cache: a day with
// failures is due only once the backoff has passed, and never again at
// the attempt cap. Callers hold p.mu.
func (p *influxMetricsProvider) tokenDayRetryAllowedLocked(key string, now time.Time) bool {
	f, ok := p.tokenDayFails[key]
	if !ok {
		return true
	}
	if f.attempts >= tokenDayMaxAttempts {
		return false
	}
	return !now.Before(f.nextTry)
}

// tokenDayRetryAllowed is the backfill loop's locked view of the negative
// cache (a blocked day is skipped WITHOUT spending the per-cycle budget,
// exactly like a cached one).
func (p *influxMetricsProvider) tokenDayRetryAllowed(day time.Time, now time.Time) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.tokenDayRetryAllowedLocked(day.Format(tokenDayLayout), now)
}

func (p *influxMetricsProvider) tokenDayCached(day time.Time) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	_, ok := p.tokenDayAt[day.Format(tokenDayLayout)]
	return ok
}

// computeTokenDay integrates one UTC calendar day for every configured endpoint
// and every counter field, and caches the per-endpoint parts. A FAILED leg (any
// endpoint, any field) marks the WHOLE day failed: the part is removed (SKIPPED
// from every sum, so the served numbers stay disclosed lower bounds) and the
// day is retried on a later cycle. A missing day shrinks the sums, it never
// fakes them, and one field's failure never silently degrades the other two
// into a partial day the operator cannot see.
func (p *influxMetricsProvider) computeTokenDay(ctx context.Context, day time.Time, targets tokenTargets) {
	key := day.Format(tokenDayLayout)
	stop := day.AddDate(0, 0, 1)
	parts := make(map[string]tokenDayPart, len(targets.names))
	for _, name := range targets.names {
		for _, field := range tokenDayFields {
			value, seen, err := p.queryTokenDayField(ctx, day, stop, name, field, targets)
			p.noteLeg("lifetimeTokens:"+field, err)
			if err != nil {
				p.markTokenDayFailed(key)
				return
			}
			if !seen {
				// The engine publishes no such counter: the field stays UNSET.
				continue
			}
			part := parts[name]
			part.set(field, value)
			parts[name] = part
		}
	}
	p.mu.Lock()
	p.tokenDays[key] = parts
	p.tokenDayAt[key] = p.now()
	delete(p.tokenDayFails, key)
	p.mu.Unlock()
}

// markTokenDayFailed applies the day's negative-cache record: the day part is
// dropped (so every sum loses it), the attempt count rises and the retry arms
// at +tokenDayRetryBackoff, abandoned at tokenDayMaxAttempts.
func (p *influxMetricsProvider) markTokenDayFailed(key string) {
	p.mu.Lock()
	delete(p.tokenDays, key)
	delete(p.tokenDayAt, key)
	if p.tokenDayFails == nil {
		p.tokenDayFails = map[string]tokenDayFailure{}
	}
	f := p.tokenDayFails[key]
	f.attempts++
	f.nextTry = p.now().Add(tokenDayRetryBackoff)
	p.tokenDayFails[key] = f
	p.mu.Unlock()
}

// queryTokenDayField integrates ONE counter field for ONE endpoint over one day
// window. The second return says whether the field produced any row at all:
// NO row means the engine does not publish that counter (Flux emits nothing for
// an empty group), which is "not known", not zero. Rows whose endpoint is not
// configured (a rename's pre-rename id, a retired endpoint) are dropped by the
// ghost guard; the sum over the surviving rows is the field's day integral.
func (p *influxMetricsProvider) queryTokenDayField(ctx context.Context, day, stop time.Time, name, field string, targets tokenTargets) (int64, bool, error) {
	qctx, cancel := context.WithTimeout(ctx, tokenDayTimeout)
	rows, err := p.client.query(qctx, tokenDayFlux(p.bucket, day, stop, name, field))
	cancel()
	if err != nil {
		return 0, false, err
	}
	var sum float64
	seen := false
	for _, row := range rows {
		rowName, _ := row["endpoint"].(string)
		if _, configured := targets.live[rowName]; !configured {
			// Ghost-id row (rename boundary, retired endpoint): never credited
			// to a current endpoint, never surfaced.
			continue
		}
		value, ok := influxFloat(row["_value"])
		if !ok {
			continue
		}
		sum += value
		seen = true
	}
	if !seen {
		return 0, false, nil
	}
	return int64(sum), true, nil
}

// sumTokenDays rebuilds the three served sums from the cached day parts inside
// the lookback (older parts are evicted), per counter field: a field's sum only
// exists for an endpoint that has at least one day part carrying it. The
// disclosure is window-wide, not per field: any window day without a part means
// EVERY sum is a best-effort lower bound.
func (p *influxMetricsProvider) sumTokenDays(first, now time.Time) {
	gen := map[string]int64{}
	prefill := map[string]int64{}
	cached := map[string]int64{}
	p.mu.Lock()
	for key, parts := range p.tokenDays {
		day, err := time.Parse(tokenDayLayout, key)
		if err != nil || day.Before(first) {
			delete(p.tokenDays, key)
			delete(p.tokenDayAt, key)
			delete(p.tokenDayFails, key)
			continue
		}
		for name, part := range parts {
			if part.hasGen {
				gen[name] += part.gen
			}
			if part.hasPrefill {
				prefill[name] += part.prefill
			}
			if part.hasCached {
				cached[name] += part.cached
			}
		}
	}
	p.mu.Unlock()
	today := utcDayStart(now)
	missing := !p.tokenDayCached(today)
	for day := first; !missing && day.Before(today); day = day.AddDate(0, 0, 1) {
		missing = !p.tokenDayCached(day)
	}
	p.noteTokenGap(missing)
	p.mu.Lock()
	// Nothing visible in the whole window for a field: that column dashes.
	p.genTokens = nilIfEmpty(gen)
	p.prefillTokens = nilIfEmpty(prefill)
	p.cachedTokens = nilIfEmpty(cached)
	p.mu.Unlock()
}

// nilIfEmpty turns an empty served map into nil so the payload keys are ABSENT
// (UI dash) rather than a fabricated 0.
func nilIfEmpty(m map[string]int64) map[string]int64 {
	if len(m) == 0 {
		return nil
	}
	return m
}

// noteTokenGap logs on TRANSITION when the served lifetime sum skips a day
// window (cold-boot backfill in flight, or a day query that failed): the
// number is then a best-effort LOWER bound, and a silent lower bound would
// read as exact.
func (p *influxMetricsProvider) noteTokenGap(missing bool) {
	p.mu.Lock()
	prev := p.tokenGap
	p.tokenGap = missing
	p.mu.Unlock()
	if p.logger == nil || missing == prev {
		return
	}
	if missing {
		p.logger.Warn("metrics_source influx: lifetime tokens best-effort, day windows missing from the sum", "leg", "lifetimeTokens")
	} else {
		p.logger.Info("metrics_source influx: lifetime tokens complete, every day window in the lookback is integrated", "leg", "lifetimeTokens")
	}
}

// LifetimeGeneration / LifetimePrefill implement the lifetime-token read
// surface consumed by the endpoints status payload: the summed per-day counter
// integrals of generation_tokens_total / prompt_tokens_total for the endpoint
// name (the Influx endpoint tag equals the olla endpoint Name). Each sum is a
// best-effort LOWER bound whenever a day window is missing (disclosed in the
// log): a day whose query failed is skipped, never faked. ok=false (payload:
// key absent, UI dash) in sampler mode, while NO day part carrying THAT field
// is available (cold boot with influx down, or an engine that publishes only
// one of the two counters), and for endpoints with no counter rows at all
// (comfyui/voice endpoints export no token counters) - a fabricated 0 would
// claim the endpoint produced nothing, which the data cannot support.
func (p *influxMetricsProvider) LifetimeGeneration(name string) (int64, bool) {
	if p == nil {
		return 0, false
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	v, ok := p.genTokens[name]
	return v, ok
}

func (p *influxMetricsProvider) LifetimePrefill(name string) (int64, bool) {
	if p == nil {
		return 0, false
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	v, ok := p.prefillTokens[name]
	return v, ok
}

// kvReusePercentBound is the ceiling of a percentage: a documented 0..1 ratio
// gauge that reads above 1 (or below 0) is a backend bug, and "137% cache
// reuse" / "-4% cache reuse" must never reach the dashboard.
const kvReusePercentBound = 100.0

func clampKVReusePercent(v float64) float64 {
	switch {
	case v < 0:
		return 0
	case v > kvReusePercentBound:
		return kvReusePercentBound
	}
	return v
}

// clampPercentPtr clamps a derived percentage pointer (0..100), preserving nil.
func clampPercentPtr(p *float64) *float64 {
	if p == nil {
		return nil
	}
	c := clampKVReusePercent(*p)
	return &c
}

// KVCacheReusePercent implements the KV-cache-reuse read surface. THE BASIS IS
// PER ENGINE and the number alone does not disclose it, which the payload
// comment states verbatim:
//
//   - Engines publishing prefix_cache_hit_ratio (radiance / vLLM family) are
//     answered with that gauge x100: the engine's OWN hit rate, measured since
//     ENGINE BOOT, so it resets on every engine restart. Freshness is the
//     gauge's own sample time against the same stale_after gate the runtime
//     tiles use - an engine that stopped exporting must not keep answering
//     "93%" from its last scrape.
//   - Engines publishing no such gauge (llama.cpp family) are answered with
//     the ratio of the counter day-integrals over the effective token window:
//     cached / (cached + computed) - prompt_tokens_total counts COMPUTED
//     (non-cached) prompt tokens, so it is a term of the denominator, not the
//     denominator (mirrors the sampler's recordRatio; the ratio is <= 100 by
//     construction). Both sides are integrals of the same identity, so a
//     restart cancels out; the reading is a window ratio, not a rate, and it
//     moves far more slowly.
//
// The two bases are NOT comparable with each other. ok=false (dash) when the
// endpoint has no gauge and no cached-counter rows, when its gauge is stale,
// when the cached+computed denominator is unavailable or zero (nothing to
// divide by), or in sampler mode. A 0 is never invented for a missing
// numerator: it would claim the cache never hits.
// Basis strings for the KV-cache-reuse disclosure. The gauge basis is a
// constant (no allocation on the poll path); the counter basis names the
// EFFECTIVE window and is pre-rendered on the provider (kvBasisCounter).
const kvReuseBasisGauge = "engine prefix-cache gauge, since engine boot (resets on restart)"

// kvReuse computes the reuse percent AND its basis in one gated read, so the
// two can never disagree about whether the value is known.
func (p *influxMetricsProvider) kvReuse(name string) (float64, string, bool) {
	if p == nil {
		return 0, "", false
	}
	p.mu.RLock()
	state := p.states[name]
	cached, hasCached := p.cachedTokens[name]
	prefill, hasPrefill := p.prefillTokens[name]
	p.mu.RUnlock()
	if state != nil && !state.cacheAt.IsZero() {
		if p.now().Sub(state.cacheAt) > p.staleAfter {
			return 0, "", false
		}
		return clampKVReusePercent(kvReusePercentBound * state.cacheRatio), kvReuseBasisGauge, true
	}
	// llama.cpp-family prompt_tokens_total counts COMPUTED (non-cached)
	// prompt tokens only - the same fact the sampler's recordRatio encodes
	// as cached/(cached+computed) - so the denominator of the reuse ratio
	// is cached + computed, NOT the raw prompt sum. Measured (halogen-395):
	// cached 1,490,274,227 / prompt 131,638,184 = 1132%, served clamped
	// 100.0 where the truth is 91.9%. With cached/(cached+computed) the
	// numerator is a term of the denominator, so the ratio is <= 100 by
	// CONSTRUCTION - the clamp below guards only the gauge path.
	p.mu.RLock()
	basis := p.kvBasisCounter
	p.mu.RUnlock()
	if hasCached && hasPrefill {
		denominator := cached + prefill
		if denominator > 0 {
			return clampKVReusePercent(kvReusePercentBound * float64(cached) / float64(denominator)), basis, true
		}
	}
	return 0, "", false
}

func (p *influxMetricsProvider) KVCacheReusePercent(name string) (float64, bool) {
	v, _, ok := p.kvReuse(name)
	return v, ok
}

func (p *influxMetricsProvider) KVCacheReuseBasis(name string) string {
	_, basis, ok := p.kvReuse(name)
	if !ok {
		return ""
	}
	return basis
}

// Snapshot applies the same gates as the sampler: opted-in, routable, known
// dialect; stale after staleAfter; gone entirely past the max stale age.
func (p *influxMetricsProvider) Snapshot(endpoint *domain.Endpoint) *EndpointRuntimeMetrics {
	if p == nil || endpoint == nil || !endpoint.RuntimeMetricsEnabled || runtimeMetricsKind(endpoint) == "" || !endpoint.Status.IsRoutable() {
		return nil
	}
	p.mu.RLock()
	state := p.states[endpoint.Name]
	p.mu.RUnlock()
	if state == nil || state.newest.IsZero() {
		return nil
	}
	age := p.now().Sub(state.newest)
	stale := age > p.staleAfter
	if stale && age > runtimeMetricsMaxStaleAge {
		return nil
	}
	ageSeconds := age.Seconds()
	return &EndpointRuntimeMetrics{
		SampledAt:      state.newest,
		AvailableSince: state.availableSince,
		Stale:          stale,
		Kind:           runtimeMetricsKind(endpoint),
		Source:         runtimeMetricsSourceInflux,
		SourceAgeS:     floatPtr(ageSeconds),
		Metrics:        state.metrics,
	}
}

// influxWindowAgg accumulates the per-1m-window rows one Flux
// aggregateWindow query returns per field series into the three numbers the
// envelope shows: average of window values, maximum window value, and the
// value of the newest window (the "current" for counter-derived rates).
type influxWindowAgg struct {
	latest          time.Time
	sum, count, max float64
	latestValue     float64
	has             bool
}

func (a *influxWindowAgg) add(at time.Time, v float64) {
	if !a.has {
		a.has, a.max = true, v
	}
	a.sum += v
	a.count++
	if v > a.max {
		a.max = v
	}
	if at.After(a.latest) {
		a.latest, a.latestValue = at, v
	}
}

func (a *influxWindowAgg) avg() (float64, bool) {
	if !a.has || a.count == 0 {
		return 0, false
	}
	return a.sum / a.count, true
}

// influxSeries collects one endpoint's rows across the five queries.
type influxSeries struct {
	last        map[string]float64
	lastAt      map[string]time.Time
	gaugeMean   map[string]*influxWindowAgg
	gaugeMax    map[string]*influxWindowAgg
	counterMean map[string]*influxWindowAgg
	counterMax  map[string]*influxWindowAgg
	newest      time.Time
	// paired indexes the RAW requests_running/queue_depth points by scrape
	// timestamp (the engine writes both fields at identical timestamps, so
	// the join is exact). envelope() takes the argmax over running and reads
	// the queue value at that exact timestamp.
	paired map[time.Time]*influxPairedPoint
}

// influxPairedPoint is one scrape's saturation pair; a missing field stays
// nil ("not published"), never 0.
type influxPairedPoint struct {
	running, queue *float64
}

func newInfluxSeries() *influxSeries {
	return &influxSeries{
		last:        map[string]float64{},
		lastAt:      map[string]time.Time{},
		gaugeMean:   map[string]*influxWindowAgg{},
		gaugeMax:    map[string]*influxWindowAgg{},
		counterMean: map[string]*influxWindowAgg{},
		counterMax:  map[string]*influxWindowAgg{},
		paired:      map[time.Time]*influxPairedPoint{},
	}
}

// absorb folds one result row into the series. The bucket names the query
// that produced it because last() rows and aggregateWindow rows are
// structurally identical column-wise.
// at returns the freshest sample time recorded for one field (zero when the
// field never arrived).
// noteLeg logs a query leg on FAILURE/RECOVERY transitions (never per
// cycle - the refresh runs every 10s). legs allowed to fail independently:
// the cycle keeps last() rows when only a window leg dies, which used to be
// silent and read as "engine idle, no averages".
func (p *influxMetricsProvider) noteLeg(leg string, err error) {
	failing := err != nil
	p.mu.Lock()
	prev := p.legFailing[leg]
	p.legFailing[leg] = failing
	p.mu.Unlock()
	if p.logger == nil || failing == prev {
		return
	}
	if failing {
		p.logger.Warn("metrics_source influx: query leg failing", "leg", leg, "error", err.Error())
	} else {
		p.logger.Info("metrics_source influx: query leg recovered", "leg", leg)
	}
}

func (s *influxSeries) at(field string) time.Time { return s.lastAt[field] }

func (s *influxSeries) absorb(row map[string]any, bucket string) {
	field, _ := row["_field"].(string)
	value, ok := influxFloat(row["_value"])
	if field == "" || !ok {
		return
	}
	at := influxTime(row["_time"])
	if at.After(s.newest) {
		s.newest = at
	}
	if bucket == "last" {
		if prevAt, seen := s.lastAt[field]; !seen || !at.Before(prevAt) {
			s.last[field], s.lastAt[field] = value, at
		}
		return
	}
	if bucket == "paired" {
		point := s.paired[at]
		if point == nil {
			point = &influxPairedPoint{}
			s.paired[at] = point
		}
		switch field {
		case "requests_running":
			v := value
			point.running = &v
		case influxFieldQueueDepth:
			v := value
			point.queue = &v
		}
		return
	}
	target := s.gaugeMean
	switch bucket {
	case "gaugeMax":
		target = s.gaugeMax
	case "counterMean":
		target = s.counterMean
	case "counterMax":
		target = s.counterMax
	}
	agg := target[field]
	if agg == nil {
		agg = &influxWindowAgg{}
		target[field] = agg
	}
	agg.add(at, value)
}

// envelope builds the self-describing metrics list, preferring the engine's
// own rate gauges (what ollametrics canonicalises) and falling back to a
// counter derivative only when the gauge family is absent. Ratios are
// stored 0..1 in Influx and percentised here, matching the sampler.
// gaugeMeanAvg folds the gaugeMean window average into one optional part,
// scaled (100 for ratios rendered as percent). Nil when the field has no
// mean window - the metric part stays absent, never 0.
func (s *influxSeries) gaugeMeanAvg(field string, scale float64) *float64 {
	agg := s.gaugeMean[field]
	if agg == nil {
		return nil
	}
	if avg, ok := agg.avg(); ok {
		return floatPtr(scale * avg)
	}
	return nil
}

func (s *influxSeries) envelope() []RuntimeMetric {
	out := make([]RuntimeMetric, 0, 7)
	for _, pair := range []struct{ gaugeField, counterField, key string }{
		{"decode_tokens_per_second", "generation_tokens_total", runtimeMetricKeyGenerationTPS},
		{"prefill_tokens_per_second", "prompt_tokens_total", runtimeMetricKeyPrefillTPS},
	} {
		metric := RuntimeMetric{Key: pair.key, Unit: runtimeMetricUnitTokS}
		if current, ok := s.throughputCurrent(pair.gaugeField, pair.counterField); ok {
			metric.Current = floatPtr(current)
		}
		if avg, ok := s.throughputAvg(pair.gaugeField, pair.counterField); ok {
			metric.Avg = floatPtr(avg)
		}
		if peak, ok := s.throughputMax(pair.gaugeField, pair.counterField); ok {
			metric.Max = floatPtr(peak)
		}
		if metric.Current == nil && metric.Avg == nil && metric.Max == nil {
			continue
		}
		metric.Scope, metric.Note = s.throughputSemantics(pair.gaugeField)
		out = append(out, metric)
	}
	if v, ok := s.last["prefix_cache_hit_ratio"]; ok {
		out = append(out, RuntimeMetric{Key: runtimeMetricKeyCacheHitPercent, Unit: runtimeMetricUnitPercent,
			Current: floatPtr(clampKVReusePercent(100 * v)), Avg: clampPercentPtr(s.gaugeMeanAvg("prefix_cache_hit_ratio", 100)), Note: runtimeMetricNoteInfluxGauge})
	}
	if v, ok := s.last["kv_cache_usage_ratio"]; ok {
		out = append(out, RuntimeMetric{Key: runtimeMetricKeyKvCacheUsagePercent, Unit: runtimeMetricUnitPercent, Current: floatPtr(clampKVReusePercent(100 * v))})
	}
	if v, ok := s.last["requests_running"]; ok {
		metric := RuntimeMetric{Key: runtimeMetricKeyRequestsRunning, Unit: runtimeMetricUnitCount,
			Current: floatPtr(v), Avg: s.gaugeMeanAvg("requests_running", 1)}
		if agg := s.gaugeMax["requests_running"]; agg != nil && agg.has {
			metric.Max = floatPtr(agg.max)
		}
		out = append(out, metric)
	}
	if v, ok := s.last[influxFieldQueueDepth]; ok {
		metric := RuntimeMetric{Key: runtimeMetricKeyQueueDepth, Unit: runtimeMetricUnitCount, Current: floatPtr(v)}
		if at, queue, ok := s.pairedQueueAtRunningMax(); ok {
			metric.PairedOf = runtimeMetricKeyRequestsRunning
			metric.PairedAt = at.UTC().Format(time.RFC3339Nano)
			metric.PairedValue = floatPtr(queue)
		}
		out = append(out, metric)
	}
	if v, ok := s.last["draft_acceptance_ratio"]; ok {
		out = append(out, RuntimeMetric{Key: runtimeMetricKeySpecAcceptPercent, Unit: runtimeMetricUnitPercent,
			Current: floatPtr(clampKVReusePercent(100 * v)), Avg: clampPercentPtr(s.gaugeMeanAvg("draft_acceptance_ratio", 100)),
			AttachTo: runtimeMetricKeyGenerationTPS, Note: runtimeMetricNoteInfluxGauge})
	}
	return out
}

// pairedQueueAtRunningMax takes the RAW-point argmax over requests_running
// inside the window (ties: the NEWEST maximum) and returns the queue_depth
// value recorded at that exact timestamp. No queue point at the peak's
// timestamp is "not paired" (ok=false) - the UI dashes; it never falls back
// to the current queue, never to 0.
func (s *influxSeries) pairedQueueAtRunningMax() (time.Time, float64, bool) {
	var (
		bestAt    time.Time
		bestValue float64
		found     bool
	)
	for at, point := range s.paired {
		if point.running == nil {
			continue
		}
		if !found || *point.running > bestValue || (*point.running == bestValue && at.After(bestAt)) {
			found, bestValue, bestAt = true, *point.running, at
		}
	}
	if !found {
		return time.Time{}, 0, false
	}
	point := s.paired[bestAt]
	if point == nil || point.queue == nil {
		return time.Time{}, 0, false
	}
	return bestAt, *point.queue, true
}

// loadReaderMaxSampleAge mirrors the sampler's rule: the health checker
// grades Busy from the queue signal only when it is fresher than this.
const loadReaderMaxSampleAge = 90 * time.Second

// QueueDepthFor implements health.LoadReader for load-aware Busy grading
// under metrics_source influx mode: the freshest sampled queue_depth gauge
// for the endpoint. States are keyed by endpoint Name (the Influx endpoint
// tag), deleted when an endpoint leaves the tracked set, and "not known" for
// absent, zero-time, queue-less, or >90s-old QUEUE samples (the queue field's
// own timestamp, not the endpoint's newest point) - the probe verdict stays
// ungraded rather than steering on old data.
func (p *influxMetricsProvider) QueueDepthFor(endpoint *domain.Endpoint) (float64, bool) {
	if p == nil || endpoint == nil {
		return 0, false
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	st := p.states[endpoint.Name]
	if st == nil || st.queueAt.IsZero() {
		return 0, false
	}
	// The QUEUE's own sample time, not the endpoint's newest point: a live
	// engine refreshes throughput every scrape, so gating on `newest` let a
	// queue reading from the last time the queue moved steer Busy forever -
	// the reader's whole purpose is a fresh sample.
	if p.now().Sub(st.queueAt) > loadReaderMaxSampleAge {
		return 0, false
	}
	for _, m := range st.metrics {
		if m.Key == runtimeMetricKeyQueueDepth && m.Current != nil {
			return *m.Current, true
		}
	}
	return 0, false
}

func (s *influxSeries) hasGaugeThroughput() bool {
	for _, field := range []string{"decode_tokens_per_second", "prefill_tokens_per_second"} {
		if _, ok := s.last[field]; ok {
			return true
		}
		if _, ok := s.gaugeMean[field]; ok {
			return true
		}
	}
	return false
}

// throughputCurrent: freshest value, gauge first; the counter derivative is
// consulted only when the gauge family is absent entirely, so a build that
// publishes both never shows a derived approximation.
func (s *influxSeries) throughputCurrent(gaugeField, counterField string) (float64, bool) {
	if v, ok := s.last[gaugeField]; ok {
		return v, true
	}
	if !s.hasGaugeThroughput() {
		if agg := s.counterMean[counterField]; agg != nil && agg.has {
			return agg.latestValue, true
		}
	}
	return 0, false
}

func (s *influxSeries) throughputAvg(gaugeField, counterField string) (float64, bool) {
	if agg := s.gaugeMean[gaugeField]; agg != nil {
		return agg.avg()
	}
	if !s.hasGaugeThroughput() {
		if agg := s.counterMean[counterField]; agg != nil {
			return agg.avg()
		}
	}
	return 0, false
}

func (s *influxSeries) throughputMax(gaugeField, counterField string) (float64, bool) {
	if agg := s.gaugeMax[gaugeField]; agg != nil && agg.has {
		return agg.max, true
	}
	if !s.hasGaugeThroughput() {
		if agg := s.counterMax[counterField]; agg != nil && agg.has {
			return agg.max, true
		}
	}
	return 0, false
}

// throughputSemantics states what the throughput number IS: an engine-wide
// batch rate when it comes from the engine's own gauges, a derived
// approximation when it comes from counter deltas.
func (s *influxSeries) throughputSemantics(gaugeField string) (string, string) {
	_, hasLast := s.last[gaugeField]
	_, hasMean := s.gaugeMean[gaugeField]
	if hasLast || hasMean || s.hasGaugeThroughput() {
		return runtimeMetricScopeEngine, joinNote(runtimeMetricNoteEngineThroughput, runtimeMetricNoteInfluxGauge)
	}
	return "", runtimeMetricNoteInfluxCounter
}

// influxBase opens EVERY runtime leg: the bounded llm_runtime window,
// filtered to the tracked endpoints as an OR of tag equalities, never
// contains(set:[...], value:r.endpoint) - pushdown handles only and/or/not
// of comparisons on _measurement/_field/tags, and a function call in the
// expression disables pushdown for the WHOLE filter, turning the read into
// a scan of every llm_runtime series of the window (the day-window twin of
// this bug measured 18.29s / 44.45 CPU-s vs 0.56s / 1.62 CPU-s; this window
// is ~0.7s today and the trap scales with it). An empty set renders false:
// the same matches-nothing the empty contains(set:[]) gave, so refresh()'s
// len==0 early return stays the only empty-set policy.
func influxBase(bucket string, endpoints []string) string {
	predicate := "false"
	if len(endpoints) > 0 {
		parts := make([]string, 0, len(endpoints))
		for _, name := range endpoints {
			parts = append(parts, fmt.Sprintf(`r.endpoint==%s`, fluxQuoted(name)))
		}
		predicate = "(" + strings.Join(parts, " or ") + ")"
	}
	return fmt.Sprintf(`from(bucket:%q) |> range(start:%s) |> filter(fn:(r)=>r._measurement=="llm_runtime") |> filter(fn:(r)=>%s)`, bucket, influxQueryRange, predicate)
}

func influxFieldFilter(fields ...string) string {
	parts := make([]string, 0, len(fields))
	for _, f := range fields {
		parts = append(parts, fmt.Sprintf(`r._field==%q`, f))
	}
	return " |> filter(fn:(r)=>" + strings.Join(parts, " or ") + ")"
}

var fluxControlEscape = regexp.MustCompile(`\\x([0-9a-fA-F]{2})`)

// fluxStringSet renders config-supplied strings as Flux literals. Go's %q
// escapes control chars as \xNN, which Flux does not understand (syntax
// error); Flux knows \uXXXX, so translate. Ordinary names (letters, dots,
// dashes) pass through %q untouched.
func fluxStringSet(names []string) string {
	quoted := make([]string, 0, len(names))
	for _, name := range names {
		quoted = append(quoted, fluxQuoted(name))
	}
	return strings.Join(quoted, ",")
}

// fluxQuoted renders ONE config-supplied string as a Flux string literal
// (the \xNN -> \uXXXX control-char translation of fluxStringSet, single
// value: tag-equality filters need one literal, not a joined set).
func fluxQuoted(name string) string {
	q := fmt.Sprintf("%q", name)
	return fluxControlEscape.ReplaceAllStringFunc(q, func(m string) string { return `\u00` + m[2:] })
}

func influxFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case string:
		var f float64
		if _, err := fmt.Sscanf(n, "%g", &f); err == nil {
			return f, true
		}
	}
	return 0, false
}

func influxTime(v any) time.Time {
	s, _ := v.(string)
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	return t
}
