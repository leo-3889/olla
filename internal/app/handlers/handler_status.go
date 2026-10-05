package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"hash"
	"hash/fnv"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/thushan/olla/internal/app/handlers/health"
	"github.com/thushan/olla/internal/core/constants"
	"github.com/thushan/olla/internal/version"

	"github.com/thushan/olla/internal/config"

	"github.com/thushan/olla/internal/util"

	"github.com/thushan/olla/internal/core/domain"
	"github.com/thushan/olla/internal/core/ports"
	"github.com/thushan/olla/pkg/format"
)

var (
	statusHealthy  = "healthy"
	statusDegraded = "degraded"
	statusCritical = "critical"
	statusNormal   = "normal"
	statusElevated = "elevated"
	zeroTraffic    = "0 B"
	emptyString    = ""
)

type SystemSummary struct {
	// Additive field, keep JSON contract backward-compatible: absolute process
	// start so the dashboard can compute a live uptime between polls without
	// refetching. Not omitempty: always known once the process is up. Sits
	// alongside the existing relative uptime string.
	StartTime          time.Time `json:"start_time"`
	Status             string    `json:"status"`
	EndpointsUp        string    `json:"endpoints_up"`
	SuccessRate        string    `json:"success_rate"`
	AvgLatency         string    `json:"avg_latency"`
	TotalTraffic       string    `json:"total_traffic"`
	UptimeHuman        string    `json:"uptime"`
	Version            string    `json:"version"`
	Commit             string    `json:"commit"`
	ActiveConnections  int64     `json:"active_connections"`
	SecurityViolations int64     `json:"security_violations"`
	TotalRequests      int64     `json:"total_requests"`
	TotalFailures      int64     `json:"total_failures"`
	// HasTraffic lets the dashboard branch on the no-traffic state without
	// parsing SuccessRate. Always present (not omitempty): it is derivable from
	// TotalRequests, which is itself always emitted, so every payload carries
	// an explicit true/false. SuccessRate becomes "N/A" when this is false,
	// mirroring the endpoint-level convention in handler_status_endpoints.go;
	// previously it reported "0%" on a healthy fresh boot, which coupled with
	// the legacy status threshold produced a misleading "critical" verdict.
	HasTraffic bool `json:"has_traffic"`
	// HealthMode is the computation that produced Status: "host" (the endpoint
	// fleet, the historical behaviour) or "model" (a configured list of
	// models/aliases). Always present so clients can tell which verdict they are
	// looking at.
	HealthMode string `json:"health_mode"`
	// Models is the per-model breakdown in model mode. Omitted (nil) in host
	// mode so the payload is unchanged for the default configuration.
	Models []health.ModelHealthEntry `json:"models,omitempty"`
}

type ProxySummary struct {
	Engine   string `json:"engine"`
	Profile  string `json:"profile"`
	Balancer string `json:"balancer"`
}

type EndpointResponse struct {
	RuntimeMetrics *EndpointRuntimeMetrics `json:"runtime_metrics,omitempty"`
	AvgLatencyMs   *int64                  `json:"avg_latency_ms,omitempty"`
	NextCheckAt    *time.Time              `json:"next_check_at,omitempty"`
	HealthCheckAt  *time.Time              `json:"health_check_at,omitempty"`
	Models         EndpointModelsResponse  `json:"models"`
	Name           string                  `json:"name"`
	Status         string                  `json:"status"`
	SuccessRate    string                  `json:"success_rate"`
	AvgLatency     string                  `json:"avg_latency"`
	Traffic        string                  `json:"traffic"`
	LastCheck      string                  `json:"last_check"`
	NextCheck      string                  `json:"next_check"`
	Issues         string                  `json:"issues"`
	URL            string                  `json:"url"`
	// ID is a stable, opaque identifier derived from the SANITISED endpoint
	// URL via buildEndpointIDs; see handler_status_endpoints.go.
	ID          string `json:"id"`
	Priority    int    `json:"priority"`
	Connections int64  `json:"connections"`
	Requests    int64  `json:"requests"`
	// Additive dashboard fields, existing fields above are unchanged.
	// active_connections is intentionally NOT duplicated here: Connections
	// already carries the same value. last_model_sync_at is intentionally NOT
	// duplicated: Models.LastUpdated already serialises as an RFC3339 absolute.
	MinLatencyMs int64 `json:"min_latency_ms"`
	MaxLatencyMs int64 `json:"max_latency_ms"`
}

type EndpointModelsResponse struct {
	LastUpdated time.Time `json:"last_updated"`
	Count       int64     `json:"count"`
}

type SecuritySummary struct {
	Status     string            `json:"status"`
	BlockedIPs int               `json:"blocked_ips"`
	Violations SecurityViolation `json:"violations"`
}
type SecurityViolation struct {
	RateLimits int64 `json:"rate_limits"`
	SizeLimits int64 `json:"size_limits"`
}
type StatusResponse struct {
	Timestamp time.Time          `json:"timestamp"`
	Proxy     ProxySummary       `json:"proxy"`
	Endpoints []EndpointResponse `json:"endpoints"`
	Security  SecuritySummary    `json:"security"`
	System    SystemSummary      `json:"system"`
}

type statusSnapshot struct {
	endpointStats   map[string]ports.EndpointStats
	connectionStats map[string]int64
	endpointModels  map[string]*domain.EndpointModels
	all             []*domain.Endpoint
	healthy         []*domain.Endpoint
	proxyStats      ports.ProxyStats
	securityStats   ports.SecurityStats
	// modelStats is the per-model request stats, gathered here so the model
	// health evaluator can aggregate the configured models' own traffic without
	// a second collector call.
	modelStats map[string]ports.ModelStats
}

func (a *Application) gatherStatusSnapshot(ctx context.Context) (*statusSnapshot, error) {
	all, healthy, _, err := a.getEndpointCounts(ctx)
	if err != nil {
		return nil, err
	}

	endpointModelsMap, emErr := a.modelRegistry.GetEndpointModelMap(ctx)
	if emErr != nil {
		a.logger.Warn("Failed to get model map", "error", emErr)
		endpointModelsMap = make(map[string]*domain.EndpointModels)
	}

	var endpointStats map[string]ports.EndpointStats
	var proxyStats ports.ProxyStats
	var securityStats ports.SecurityStats
	var connectionStats map[string]int64
	var modelStats map[string]ports.ModelStats

	if a.statsCollector != nil {
		endpointStats = a.statsCollector.GetEndpointStats()
		proxyStats = a.statsCollector.GetProxyStats()
		securityStats = a.statsCollector.GetSecurityStats()
		connectionStats = a.statsCollector.GetConnectionStats()
		modelStats = a.statsCollector.GetModelStats()
	}

	return &statusSnapshot{
		all:             all,
		healthy:         healthy,
		endpointStats:   endpointStats,
		proxyStats:      proxyStats,
		securityStats:   securityStats,
		connectionStats: connectionStats,
		endpointModels:  endpointModelsMap,
		modelStats:      modelStats,
	}, nil
}

func (a *Application) buildStatusResponse(ctx context.Context, snapshot *statusSnapshot) StatusResponse {
	response := StatusResponse{
		Timestamp: time.Now(),
		Endpoints: make([]EndpointResponse, len(snapshot.all)),
	}

	response.Proxy = a.buildProxySummary(a.Config.Proxy)
	response.System = a.buildSystemSummary(ctx, snapshot.all, snapshot.healthy, snapshot.proxyStats, snapshot.securityStats, snapshot.connectionStats, snapshot.endpointStats, snapshot.modelStats)
	// IDs derive once from the same snapshot so every payload agrees and
	// colliding siblings stay distinct via positional disambiguators.
	endpointIDs := buildEndpointIDs(snapshot.all)
	a.buildUnifiedEndpoints(snapshot.all, snapshot.endpointStats, snapshot.connectionStats, response.Endpoints, snapshot.endpointModels, endpointIDs)
	response.Security = a.buildSecuritySummary(snapshot.securityStats)

	return response
}

func (a *Application) statusHandler(w http.ResponseWriter, r *http.Request) {
	snapshot, err := a.gatherStatusSnapshot(r.Context())
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to get endpoint data: %v", err), http.StatusInternalServerError)
		return
	}

	response := a.buildStatusResponse(r.Context(), snapshot)

	body, err := json.Marshal(response)
	if err != nil {
		http.Error(w, "Failed to encode response", http.StatusInternalServerError)
		return
	}
	writeJSONWithETag(w, r, body, hashStatusResponse(&response))
}

func (a *Application) buildProxySummary(proxyConfig config.ProxyConfig) ProxySummary {
	return ProxySummary{
		Engine:   proxyConfig.Engine,
		Profile:  proxyConfig.Profile,
		Balancer: proxyConfig.LoadBalancer,
	}
}

// buildSystemSummary assembles the system summary from the evaluator's verdict
// plus the proxy-wide display values. The verdict itself (healthy/degraded/
// critical) is computed by the health.Evaluator selected from the health config
// (host or model mode); this function only renders it and the surrounding
// display fields, so the two health modes stay independent of the response
// assembly.
func (a *Application) buildSystemSummary(ctx context.Context, all, healthy []*domain.Endpoint, proxy ports.ProxyStats, security ports.SecurityStats, connections map[string]int64, endpointStats map[string]ports.EndpointStats, modelStats map[string]ports.ModelStats) SystemSummary {
	var totalConnections, totalTraffic int64

	for url, conn := range connections {
		totalConnections += conn
		if stats, exists := endpointStats[url]; exists {
			totalTraffic += stats.TotalBytes
		}
	}

	// The evaluator is built in NewApplication; the nil-guard keeps hand-
	// constructed test Applications (which bypass the constructor) working by
	// falling back to the host evaluator with the standard thresholds.
	evaluator := a.evaluator
	if evaluator == nil {
		evaluator = health.NewHostEvaluator()
	}
	// A config that did not populate the success-rate thresholds (e.g. a bare
	// test config) would make the evaluator treat any traffic as critical, so
	// fall back to the standard thresholds in that case. Production configs are
	// validated to be non-zero.
	thresholds := a.Config.Health.SuccessRate
	if thresholds.CriticalBelow <= 0 || thresholds.DegradedBelow <= 0 {
		thresholds = health.DefaultThresholds()
	}

	in := &health.Input{
		All:           all,
		Healthy:       healthy,
		ProxyStats:    proxy,
		ModelStats:    modelStats,
		ModelRegistry: a.modelRegistry,
		AliasResolver: a.aliasResolver,
		Thresholds:    thresholds,
		Logger:        a.logger,
	}
	result := evaluator.Evaluate(ctx, in)

	// health_mode reports the mode that actually produced the verdict. In model
	// mode with a real signal that is "model". When the model evaluator had no
	// signal (e.g. a fresh boot before model discovery has run) a model-based
	// verdict would be a spurious "critical", so re-run the host evaluator and
	// report the host verdict instead - the honest label is then "host".
	healthMode := "host"
	if evaluator.Mode() == "model" {
		if result.HasModelSignal {
			healthMode = "model"
		} else {
			result = health.NewHostEvaluator().Evaluate(ctx, in)
		}
	}

	// Display values: in model mode with a real signal the success_rate and
	// has_traffic reflect the configured models' own traffic (result.Scoped);
	// otherwise (host mode, or the model-mode fallback) they reflect the
	// proxy-wide totals, preserving the historical display.
	hasTraffic := proxy.TotalRequests > 0
	var successRate *float64
	if proxy.TotalRequests > 0 {
		rate := float64(proxy.SuccessfulRequests) / float64(proxy.TotalRequests) * 100.0
		successRate = &rate
	}
	if result.Scoped {
		hasTraffic = result.HasTraffic
		successRate = result.SuccessRate
	}

	// No traffic means the success rate is undefined; noTrafficSuccessRate
	// mirrors the endpoint-level convention in handler_status_endpoints.go and
	// stops the dashboard rendering a misleading "0%" on a fresh boot.
	successRateStr := noTrafficSuccessRate
	if hasTraffic && successRate != nil {
		successRateStr = format.Percentage(*successRate)
	}

	summary := SystemSummary{
		Version:            version.Version,
		Commit:             version.Commit,
		Status:             result.Status,
		EndpointsUp:        format.EndpointsUp(len(healthy), len(all)),
		SuccessRate:        successRateStr,
		AvgLatency:         format.Latency(proxy.AverageLatency),
		ActiveConnections:  totalConnections,
		SecurityViolations: security.RateLimitViolations + security.SizeLimitViolations,
		TotalTraffic:       format.Bytes(util.SafeUint64(totalTraffic)),
		TotalRequests:      proxy.TotalRequests,
		TotalFailures:      proxy.FailedRequests,
		UptimeHuman:        format.Duration2(time.Since(a.StartTime)),
		StartTime:          a.StartTime,
		HasTraffic:         hasTraffic,
		HealthMode:         healthMode,
	}
	// The per-model breakdown is only meaningful when a model verdict was
	// actually used; in host mode (and the model-mode fallback) it stays
	// omitted so the default payload is unchanged.
	if healthMode == "model" && len(result.Models) > 0 {
		summary.Models = result.Models
	}
	return summary
}

func (a *Application) buildUnifiedEndpoints(all []*domain.Endpoint, statsMap map[string]ports.EndpointStats,
	connectionStats map[string]int64, endpoints []EndpointResponse, modelMap map[string]*domain.EndpointModels, endpointIDs map[string]string) {
	for i, endpoint := range all {
		url := endpoint.GetURLString()
		stats, hasStats := statsMap[url]
		connections := connectionStats[url]
		endpointModels := modelMap[url]

		var successRate float64
		if hasStats && stats.TotalRequests > 0 {
			successRate = float64(stats.SuccessfulRequests) / float64(stats.TotalRequests) * 100.0
		}

		traffic := zeroTraffic
		requests := int64(0)
		avgLatency := int64(0)
		if hasStats {
			traffic = format.Bytes(util.SafeUint64(stats.TotalBytes))
			requests = stats.TotalRequests
			avgLatency = stats.AverageLatency
		}

		var modelDisco EndpointModelsResponse
		if endpointModels != nil {
			modelDisco = EndpointModelsResponse{
				LastUpdated: endpointModels.LastUpdated,
				Count:       int64(len(endpointModels.Models)),
			}
		}

		endpoints[i] = EndpointResponse{
			RuntimeMetrics: func() *EndpointRuntimeMetrics {
				if a.runtimeMetrics == nil {
					return nil
				}
				return a.runtimeMetrics.Snapshot(endpoint)
			}(),
			Name:        endpoint.Name,
			Status:      endpoint.Status.String(),
			Priority:    endpoint.Priority,
			Connections: connections,
			Requests:    requests,
			SuccessRate: format.Percentage(successRate),
			AvgLatency:  format.Latency(avgLatency),
			Traffic:     traffic,
			LastCheck:   format.TimeAgo(endpoint.LastChecked),
			NextCheck:   format.TimeUntil(endpoint.NextCheckTime),
			Models:      modelDisco,
			Issues:      a.getEndpointIssues(endpoint, stats, hasStats, successRate),
			ID:          endpointIDs[url],
			URL:         sanitiseDisplayURL(url),
		}

		// Additive absolute timestamps alongside the existing relative strings.
		if !endpoint.LastChecked.IsZero() {
			lc := endpoint.LastChecked
			endpoints[i].HealthCheckAt = &lc
		}
		if !endpoint.NextCheckTime.IsZero() {
			nc := endpoint.NextCheckTime
			endpoints[i].NextCheckAt = &nc
		}

		// Raw latency in ms alongside the formatted AvgLatency string. Only
		// meaningful under traffic; min/max match the plain-zero convention of
		// Requests, avg is a pointer so a no-traffic endpoint omits the field.
		if hasStats && stats.TotalRequests > 0 {
			endpoints[i].MinLatencyMs = stats.MinLatency
			endpoints[i].MaxLatencyMs = stats.MaxLatency
			avgMs := stats.AverageLatency
			endpoints[i].AvgLatencyMs = &avgMs
		}
	}

	sort.Slice(endpoints, func(i, j int) bool {
		if endpoints[i].Priority != endpoints[j].Priority {
			return endpoints[i].Priority > endpoints[j].Priority
		}
		// Within a priority band, healthy comes before anything else.
		iHealthy := endpoints[i].Status == statusHealthy
		jHealthy := endpoints[j].Status == statusHealthy
		if iHealthy != jHealthy {
			return iHealthy
		}
		// Tie-breaker for deterministic ordering across polls: the input
		// slice comes from map iteration, so without a final comparison
		// equal-priority same-health endpoints reorder between polls purely
		// from map-iteration randomisation. Name first, then ID for the
		// pathological case of two endpoints sharing a name. ID (not the
		// sanitised URL) because sanitisation strips query/fragment and two
		// distinct endpoints can share a display URL, which would leave the
		// comparison tied and ordering unstable again.
		if endpoints[i].Name != endpoints[j].Name {
			return endpoints[i].Name < endpoints[j].Name
		}
		return endpoints[i].ID < endpoints[j].ID
	})
}

func (a *Application) buildSecuritySummary(stats ports.SecurityStats) SecuritySummary {
	var status string
	totalViolations := stats.RateLimitViolations + stats.SizeLimitViolations

	if totalViolations > 100 || stats.UniqueRateLimitedIPs > 10 {
		status = statusElevated
	} else {
		status = statusNormal
	}

	return SecuritySummary{
		Violations: SecurityViolation{
			RateLimits: stats.RateLimitViolations,
			SizeLimits: stats.SizeLimitViolations,
		},
		BlockedIPs: stats.UniqueRateLimitedIPs,
		Status:     status,
	}
}

func (a *Application) getEndpointIssues(endpoint *domain.Endpoint, stats ports.EndpointStats, hasStats bool, successRate float64) string {
	issues := make([]string, 0, 4)

	if endpoint.ConsecutiveFailures > 3 {
		issues = append(issues, "consecutive failures")
	}

	if hasStats {
		if successRate < 90.0 && stats.TotalRequests > 10 {
			issues = append(issues, "low success rate")
		}
		if stats.AverageLatency > 5000 {
			issues = append(issues, "high latency")
		}
	}

	if endpoint.Status == domain.StatusOffline || endpoint.Status == domain.StatusUnhealthy {
		issues = append(issues, "unavailable")
	}

	if len(issues) == 0 {
		return emptyString
	}

	return strings.Join(issues, ", ")
}

func (a *Application) getEndpointCounts(ctx context.Context) (all, healthy, routable []*domain.Endpoint, err error) {
	if all, err = a.repository.GetAll(ctx); err != nil {
		return
	}
	if healthy, err = a.repository.GetHealthy(ctx); err != nil {
		return
	}
	if routable, err = a.repository.GetRoutable(ctx); err != nil {
		return
	}
	return
}

// etagSep terminates each hashed field so adjacent fields cannot collide:
// without it ("ab"+"c") and ("a"+"bc") would feed the hash identical bytes.
var etagSep = []byte{0}

// hashEtagString writes a string field to the ETag hasher. Writes never error
// for fnv's hasher, the discarded error keeps the hash.Hash contract visible.
func hashEtagString(h hash.Hash, s string) {
	_, _ = h.Write([]byte(s))
	_, _ = h.Write(etagSep)
}

// hashEtagInt64 writes an integer field in base 10. Locale-independent so the
// ETag stays stable regardless of runtime environment.
func hashEtagInt64(h hash.Hash, v int64) {
	var buf [20]byte
	_, _ = h.Write(strconv.AppendInt(buf[:0], v, 10))
	_, _ = h.Write(etagSep)
}

// hashEtagInt64Ptr encodes presence: a nil pointer emits a distinct field
// boundary from a zero, mirroring omitempty semantics in the wire payload.
func hashEtagInt64Ptr(h hash.Hash, v *int64) {
	if v == nil {
		_, _ = h.Write(etagSep)
		return
	}
	hashEtagInt64(h, *v)
}

// hashEtagTime encodes a wall-clock instant via UnixNano. Only absolute event
// times reach this path; the relative time-ago / time-until renderings used in
// the dashboard (uptime, last_check, next_check, last_seen, last_model_sync)
// are deliberately excluded from the hash because their one-second granularity
// would change on every poll and defeat the ETag.
func hashEtagTime(h hash.Hash, t time.Time) {
	if t.IsZero() {
		_, _ = h.Write(etagSep)
		return
	}
	hashEtagInt64(h, t.UnixNano())
}

func hashEtagTimePtr(h hash.Hash, t *time.Time) {
	if t == nil {
		_, _ = h.Write(etagSep)
		return
	}
	hashEtagTime(h, *t)
}

// hashEtagStringSlice encodes a slice in order. Callers must guarantee the
// slice is already sorted deterministically; the dashboard's model/endpoint
// slices all sort before serialisation for diffable output across polls.
func hashEtagStringSlice(h hash.Hash, s []string) {
	for _, x := range s {
		hashEtagString(h, x)
	}
	_, _ = h.Write(etagSep)
}

// formatEtag renders the FNV-1a sum as a quoted WEAK validator per RFC 7232:
// the hashed fields exclude every per-second-rendered relative string and the
// wall-clock timestamp, so two payloads a second apart under unchanged state
// hash identically - but the hash still intentionally omits some fields that
// could change without changing the operator-visible answer (e.g. field
// ordering), so it is not a true content identity. Weak is the honest claim:
// clients may use it for If-None-Match but must not collapse two distinct
// payloads based on byte-for-byte identity. Base36 mirrors the style of
// stableEndpointID so client and server identity formats read the same.
// 64-bit (not 32-bit): these ETags cover a growing, unbounded field set
// (endpoints, models) where a 32-bit sum's collision odds stop being
// negligible at realistic fleet sizes; stableEndpointID's 32-bit identity
// hash is unrelated and deliberately left alone (wire-visible IDs, not
// cache-validation hashes).
func formatEtag(h hash.Hash64) string {
	return `W/"` + strconv.FormatUint(h.Sum64(), 36) + `"`
}

// maxIfNoneMatchBytes bounds the size of an If-None-Match header we are willing
// to parse. A legitimate client lists at most a handful of validators; an
// oversized header is always pathological (or hostile), so short-circuiting it
// stops a multi-MB If-None-Match from driving an unbounded split before the
// server-level MaxHeaderBytes cap was in place. 1 KiB is far above any real
// browser/curl request.
const maxIfNoneMatchBytes = 1 << 10 // 1 KiB

// etagMatches implements the If-None-Match comparison: a bare "*" matches any
// current representation, otherwise each listed validator is compared using
// weak equivalence per RFC 7232 s3.2: the W/ prefix is stripped from BOTH the
// header value and the current ETag before comparing, so a client that echoes
// our W/"..." verbatim matches, and a hypothetical client sending a strong
// "..." form against our current weak value also matches. Weak equivalence is
// the correct comparison for the If-None-Match purpose these handlers use
// (cache freshness), and is what the spec's examples assume once the server
// emits weak validators. The bare "*" wildcard is unchanged.
func etagMatches(ifNoneMatch, etag string) bool {
	if ifNoneMatch == "" {
		return false
	}
	if len(ifNoneMatch) > maxIfNoneMatchBytes {
		// An oversized If-None-Match is never a legitimate match; treat it as
		// no match so the handler falls through to a full 200 rather than
		// spending unbounded time splitting a multi-MB header.
		return false
	}
	if ifNoneMatch == "*" {
		return true
	}
	cmpCur := stripWeakPrefix(etag)
	for _, t := range strings.Split(ifNoneMatch, ",") {
		if stripWeakPrefix(strings.TrimSpace(t)) == cmpCur {
			return true
		}
	}
	return false
}

// stripWeakPrefix removes the leading W/ weak-validator marker so two
// validators can be compared by opaque-token alone, exactly as RFC 7232's
// weak comparison rule requires. A bare * wildcard is preserved untouched so
// the wildcard path above keeps working; this helper is only reached for the
// per-token comparison branch.
func stripWeakPrefix(v string) string {
	return strings.TrimPrefix(v, "W/")
}

// writeJSONWithETag emits a buffered JSON body with a weak-validator ETag,
// returning a bare 304 (no body, no Content-Encoding) when the client's
// If-None-Match matches. Headers are set before WriteHeader so they reach the
// client on both 200 and 304 paths.
func writeJSONWithETag(w http.ResponseWriter, r *http.Request, body []byte, etag string) {
	w.Header().Set(constants.HeaderContentType, constants.ContentTypeJSON)
	w.Header().Set("ETag", etag)
	// private: this payload carries operator-only endpoint/traffic data, never
	// suitable for a shared cache. no-cache (not no-store): the client may
	// still store the body but must revalidate via If-None-Match before
	// reusing it, which is exactly the 304 flow above/below - the two are not
	// in tension.
	w.Header().Set("Cache-Control", "private, no-cache")
	if etagMatches(r.Header.Get("If-None-Match"), etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// hashStatusResponse computes the FNV-1a ETag over the stable fields of a
// StatusResponse, deliberately excluding the top-level Timestamp, every
// relative time-ago string, and the per-endpoint health-check tick timestamps
// (next_check_at, health_check_at) - see hashEndpointResponse. start_time and
// models.last_updated are absolute data that only moves on a real event, so
// they stay in.
func hashStatusResponse(resp *StatusResponse) string {
	h := fnv.New64a()
	hashEtagString(h, resp.Proxy.Engine)
	hashEtagString(h, resp.Proxy.Profile)
	hashEtagString(h, resp.Proxy.Balancer)

	hashEtagTime(h, resp.System.StartTime)
	hashEtagString(h, resp.System.Status)
	hashEtagString(h, resp.System.EndpointsUp)
	hashEtagString(h, resp.System.SuccessRate)
	hashEtagString(h, resp.System.AvgLatency)
	hashEtagString(h, resp.System.TotalTraffic)
	hashEtagString(h, resp.System.Version)
	hashEtagString(h, resp.System.Commit)
	hashEtagInt64(h, resp.System.ActiveConnections)
	hashEtagInt64(h, resp.System.SecurityViolations)
	hashEtagInt64(h, resp.System.TotalRequests)
	hashEtagInt64(h, resp.System.TotalFailures)
	hashEtagString(h, resp.System.HealthMode)
	for i := range resp.System.Models {
		m := &resp.System.Models[i]
		hashEtagString(h, m.Name)
		hashEtagString(h, m.Status)
		hashEtagInt64(h, m.TotalRequests)
		hashEtagInt64(h, m.SuccessfulRequests)
		hashEtagInt64(h, m.FailedRequests)
		hashEtagString(h, m.SuccessRate)
	}

	hashEtagString(h, resp.Security.Status)
	hashEtagInt64(h, int64(resp.Security.BlockedIPs))
	hashEtagInt64(h, resp.Security.Violations.RateLimits)
	hashEtagInt64(h, resp.Security.Violations.SizeLimits)

	for i := range resp.Endpoints {
		hashEndpointResponse(h, &resp.Endpoints[i])
	}
	return formatEtag(h)
}

// hashEndpointResponse feeds only the stable/semantic fields of one
// EndpointResponse to the hasher. Relative strings (last_check, next_check)
// are skipped because they render with one-second granularity and would
// change on every poll; NextCheckAt/HealthCheckAt are skipped too - they
// advance on every health-check tick (30s, see internal/adapter/health)
// regardless of whether the endpoint's status actually changed, which
// churned this ETag under zero traffic and defeated caching. The runtime
// metrics contribution is hashed values-only for the same reason (see
// hashEndpointRuntimeMetrics).
func hashEndpointResponse(h hash.Hash, ep *EndpointResponse) {
	hashEtagString(h, ep.ID)
	hashEtagString(h, ep.Name)
	hashEtagString(h, ep.Status)
	hashEtagString(h, ep.URL)
	hashEtagString(h, ep.SuccessRate)
	hashEtagString(h, ep.AvgLatency)
	hashEtagString(h, ep.Traffic)
	hashEtagString(h, ep.Issues)
	hashEtagInt64(h, int64(ep.Priority))
	hashEtagInt64(h, ep.Connections)
	hashEtagInt64(h, ep.Requests)
	hashEtagInt64(h, ep.MinLatencyMs)
	hashEtagInt64(h, ep.MaxLatencyMs)
	hashEtagInt64Ptr(h, ep.AvgLatencyMs)
	hashEndpointRuntimeMetrics(h, ep.RuntimeMetrics)
	hashEtagTime(h, ep.Models.LastUpdated)
	hashEtagInt64(h, ep.Models.Count)
}
