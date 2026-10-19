package health

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pterm/pterm"
	"github.com/thushan/olla/internal/core/domain"
	"github.com/thushan/olla/internal/logger"
)

const (
	// Structured-log keys repeated across transition branches (goconst).
	logKeyStatus      = "status"
	logKeyNextCheckIn = "next_check_in"

	DefaultConcurrentChecks        = 5
	DefaultHealthCheckInterval     = 30 * time.Second
	LogThrottleInterval            = 2 * time.Minute
	DefaultRecoveryCallbackTimeout = 10 * time.Second
)

type HTTPHealthChecker struct {
	repository        domain.EndpointRepository
	logger            logger.StyledLogger
	recoveryCallback  RecoveryCallback
	unhealthyCallback UnhealthyCallback

	// Load-aware Busy grading (proxy.busy_queue_depth). Zero threshold (the
	// default) keeps the probe verdict ungraded: only the operator's opt-in
	// steers on engine queue depth.
	loadReader   LoadReader
	healthClient *HealthClient

	ticker         *time.Ticker
	stopCh         chan struct{}
	busyQueueDepth float64
	isRunning      atomic.Bool
}

// LoadReader reports the freshest engine queue depth for an endpoint
// (load-aware Busy grading). Implemented by the runtime-metrics sampler;
// "not known" is an honest answer, not a zero.
type LoadReader interface {
	QueueDepthFor(endpoint *domain.Endpoint) (float64, bool)
}

// SetLoadAwareness arms Busy grading: a probe-healthy endpoint whose engine
// queue depth is >= threshold is graded Busy (routable at reduced SWRR
// weight) instead of Healthy, so new conversations steer away from a
// saturating node BEFORE it starts refusing connections. threshold <= 0
// disables grading.
func (c *HTTPHealthChecker) SetLoadAwareness(threshold float64, reader LoadReader) {
	c.loadReader = reader
	c.busyQueueDepth = threshold
}

// LoadAwarenessArmed reports whether Busy grading is active (threshold > 0
// and a reader wired). Exposed for startup logging and lifecycle tests.
func (c *HTTPHealthChecker) LoadAwarenessArmed() bool {
	return c.busyQueueDepth > 0 && c.loadReader != nil
}

// gradeProbeStatus maps a transport-successful probe to a serving status,
// grading Healthy -> Busy when the engine reports a queue at or above the
// threshold. Kept as a pure function: the graded Busy is NOT a probe failure
// (the engine answered; the old scheduler counting Busy as failure and
// breaker-striking a loaded-but-alive node must not be reintroduced), so
// isSuccess is computed from the raw probe result, never from the grade.
func gradeProbeStatus(status domain.EndpointStatus, threshold float64, reader LoadReader, endpoint *domain.Endpoint) domain.EndpointStatus {
	if threshold <= 0 || reader == nil || status != domain.StatusHealthy {
		return status
	}
	if q, ok := reader.QueueDepthFor(endpoint); ok && q >= threshold {
		return domain.StatusBusy
	}
	return status
}

// deadlineClient adapts http.Client's per-client hard Timeout to each
// request's context deadline. The probe budget is endpoint.CheckTimeout
// (enforced as a context deadline in HealthClient.performSingleCheck), but a
// single shared http.Client with a fixed Timeout (historically
// DefaultHealthCheckerTimeout = 5s) caps every probe at that constant: a
// check_timeout above it was a placebo, and a healthy-but-slow engine (probe
// latency 5-10s under queue pressure, measured on the live fleet 2026-10-19)
// was declared unhealthy for a budget it was never given. The context
// deadline stays the authority - the per-client Timeout sits one second
// above it so real timeouts surface as context.DeadlineExceeded (classified
// Timeout, retried/backed off correctly) rather than the transport's opaque
// "timeout awaiting response headers". One client per 1s budget bucket, all
// sharing the template Transport, so connection pooling is preserved.
type deadlineClient struct {
	templ    *http.Client
	byBudget map[time.Duration]*http.Client
	mu       sync.Mutex
}

func newDeadlineClient(templ *http.Client) *deadlineClient {
	return &deadlineClient{templ: templ, byBudget: make(map[time.Duration]*http.Client)}
}

func (d *deadlineClient) Do(req *http.Request) (*http.Response, error) {
	budget := DefaultHealthCheckerTimeout
	if dl, ok := req.Context().Deadline(); ok {
		if left := time.Until(dl).Round(time.Second); left > 0 {
			budget = left
		}
	}
	d.mu.Lock()
	client, ok := d.byBudget[budget]
	if !ok {
		c := *d.templ // shallow copy: same Transport, per-budget Timeout
		c.Timeout = budget + time.Second
		client = &c
		d.byBudget[budget] = client
	}
	d.mu.Unlock()
	return client.Do(req)
}

func NewHTTPHealthChecker(repository domain.EndpointRepository, logger logger.StyledLogger, client HTTPClient) *HTTPHealthChecker {
	circuitBreaker := NewCircuitBreaker()
	healthClient := NewHealthClient(client, circuitBreaker)

	return &HTTPHealthChecker{
		healthClient:     healthClient,
		repository:       repository,
		logger:           logger,
		stopCh:           make(chan struct{}),
		recoveryCallback: NoOpRecoveryCallback{},
	}
}

// SetRecoveryCallback sets the callback to be invoked when an endpoint recovers
func (c *HTTPHealthChecker) SetRecoveryCallback(callback RecoveryCallback) {
	if callback != nil {
		c.recoveryCallback = callback
	}
}

// SetUnhealthyCallback sets the callback to be invoked when an endpoint transitions to an unhealthy state.
func (c *HTTPHealthChecker) SetUnhealthyCallback(callback UnhealthyCallback) {
	if callback != nil {
		c.unhealthyCallback = callback
	}
}

func NewHTTPHealthCheckerWithDefaults(repository domain.EndpointRepository, logger logger.StyledLogger) *HTTPHealthChecker {
	// We want to enable connection pooling and reuse with some sane defaults
	client := &http.Client{
		Timeout: DefaultHealthCheckerTimeout,
		Transport: &http.Transport{
			MaxIdleConns:        10,
			MaxIdleConnsPerHost: 2,
			IdleConnTimeout:     30 * time.Second,
			DisableKeepAlives:   false,
			// Per-probe timeouts are already enforced by the CheckTimeout context in
			// performSingleCheck; this header timeout is a backstop against backends
			// that accept the TCP connection but then never send a response header.
			ResponseHeaderTimeout: DefaultHealthCheckerResponseHeaderTimeout,
			// No proxy: health probes now carry auth credentials (Authorization / API key
			// headers injected by the endpoint auth config). Routing them through an
			// environment proxy risks leaking those credentials to a third party.
		},
	}
	return NewHTTPHealthChecker(repository, logger, newDeadlineClient(client))
}

func (c *HTTPHealthChecker) Check(ctx context.Context, endpoint *domain.Endpoint) (domain.HealthCheckResult, error) {
	// Add timeout protection for individual checks
	checkCtx, cancel := context.WithTimeout(ctx, endpoint.CheckTimeout*2) // Give extra time for retries
	defer cancel()

	return c.healthClient.Check(checkCtx, endpoint)
}

func (c *HTTPHealthChecker) StartChecking(ctx context.Context) error {
	if c.isRunning.Load() {
		return nil
	}

	endpoints, err := c.repository.GetAll(ctx)
	if err != nil {
		return domain.NewEndpointError("start_health_checking", "repository",
			fmt.Errorf("failed to get endpoints for health checking: %w", err))
	}

	c.logger.Info("Starting Health Checker Service", "check_interval", DefaultHealthCheckInterval, "endpoints", len(endpoints))

	c.isRunning.Store(true)

	c.ticker = time.NewTicker(DefaultHealthCheckInterval)
	go c.healthCheckLoop(ctx)

	return nil
}

func (c *HTTPHealthChecker) StopChecking(ctx context.Context) error {
	// CAS from true→false is the single guard; concurrent callers that lose the
	// race see false and return early without touching stopCh.
	if !c.isRunning.CompareAndSwap(true, false) {
		return nil
	}

	if c.ticker != nil {
		c.ticker.Stop()
	}

	close(c.stopCh)

	return nil
}

func (c *HTTPHealthChecker) healthCheckLoop(ctx context.Context) {
	// Function-level safety net: if something truly unrecoverable escapes the
	// per-tick inner recover, mark isRunning false so StartChecking can restart.
	defer func() {
		if c.ticker != nil {
			c.ticker.Stop()
		}
		if r := recover(); r != nil {
			c.logger.Error("Health check loop exited unexpectedly", "panic", r)
			c.isRunning.Store(false)
		}
	}()

	for {
		select {
		case <-ctx.Done():
			c.logger.Debug("Health check loop stopping due to context cancellation")
			return
		case <-c.stopCh:
			c.logger.Debug("Health check loop stopping due to stop signal")
			return
		case <-c.ticker.C:
			// Wrap per-tick work in its own recover so a panic in
			// performHealthChecks does not kill the goroutine - the loop
			// continues and the next tick fires normally.
			func() {
				defer func() {
					if r := recover(); r != nil {
						c.logger.Error("Health check tick panic recovered, loop continues",
							"panic", r)
					}
				}()
				// Derive from the loop's own context so in-flight checks are
				// cancelled when the checker shuts down, rather than running
				// to their full timeout against an already-stopped service.
				checkCtx, cancel := context.WithTimeout(ctx, DefaultHealthCheckInterval/2)
				defer cancel()
				c.performHealthChecks(checkCtx)
			}()
		}
	}
}

func (c *HTTPHealthChecker) performHealthChecks(ctx context.Context) {
	endpoints, err := c.repository.GetAll(ctx)
	if err != nil {
		c.logger.Error("Failed to get endpoints for health checking",
			"error", domain.NewEndpointError("get_endpoints", "repository", err))
		return
	}

	now := time.Now()
	var wg sync.WaitGroup

	endpointsToCheck := make([]*domain.Endpoint, 0, len(endpoints))

	// Filter endpoints that are due for checking.
	// Skip endpoints still inside a rate-limit window; hammering a throttled backend
	// will not make it respond faster and wastes quota.
	for _, endpoint := range endpoints {
		if now.Before(endpoint.NextCheckTime) {
			continue
		}
		if !endpoint.RateLimitedUntil.IsZero() && now.Before(endpoint.RateLimitedUntil) {
			continue
		}
		endpointsToCheck = append(endpointsToCheck, endpoint)
	}

	if len(endpointsToCheck) == 0 {
		return
	}

	c.logger.Debug("Performing health checks", "endpoints_to_check", len(endpointsToCheck))

	// Limit concurrency to avoid overwhelming the health client
	semaphore := make(chan struct{}, DefaultConcurrentChecks)

	for _, endpoint := range endpointsToCheck {
		wg.Add(1)
		go func(ep *domain.Endpoint) {
			defer wg.Done()

			select {
			case semaphore <- struct{}{}:
				defer func() { <-semaphore }()
			case <-ctx.Done():
				return
			}

			c.checkEndpointSafely(ctx, ep)
		}(endpoint)
	}

	wg.Wait()
}

func (c *HTTPHealthChecker) checkEndpointSafely(ctx context.Context, endpoint *domain.Endpoint) {
	// Panic recovery for individual endpoint checks
	defer func() {
		if r := recover(); r != nil {
			c.logger.Error("Health check panic recovered for endpoint",
				"endpoint", endpoint.Name,
				"url", endpoint.GetURLString(),
				"panic", r)
		}
	}()

	c.checkEndpoint(ctx, endpoint)
}

func (c *HTTPHealthChecker) checkEndpoint(ctx context.Context, endpoint *domain.Endpoint) {
	now := time.Now()
	result, err := c.healthClient.Check(ctx, endpoint)

	oldStatus := endpoint.Status
	newStatus := gradeProbeStatus(result.Status, c.busyQueueDepth, c.loadReader, endpoint)
	statusChanged := oldStatus != newStatus

	endpointCopy := *endpoint
	endpointCopy.Status = newStatus
	endpointCopy.LastChecked = now
	endpointCopy.LastLatency = result.Latency

	// Persist the rate-limit window so the scheduler can skip this endpoint until
	// the backend is ready to serve probes again.
	if !result.RateLimitedUntil.IsZero() {
		endpointCopy.RateLimitedUntil = result.RateLimitedUntil
	} else {
		endpointCopy.RateLimitedUntil = time.Time{} // clear on non-429 responses
	}

	isSuccess := result.Status == domain.StatusHealthy
	nextInterval, newMultiplier := calculateBackoff(&endpointCopy, isSuccess)

	if !isSuccess {
		endpointCopy.ConsecutiveFailures++
		endpointCopy.BackoffMultiplier = newMultiplier
	} else {
		endpointCopy.ConsecutiveFailures = 0
		endpointCopy.BackoffMultiplier = 1
	}

	endpointCopy.NextCheckTime = now.Add(nextInterval)

	// Check if endpoint still exists before updating
	if !c.repository.Exists(ctx, endpoint.URL) {
		c.logger.Debug("Endpoint removed from configuration, stopping health checks",
			"endpoint", endpoint.GetURLString())
		return
	}

	if repoErr := c.repository.UpdateEndpoint(ctx, &endpointCopy); repoErr != nil {
		enhancedErr := domain.NewEndpointError("update_endpoint", endpoint.GetURLString(), repoErr)
		c.logger.Error("Failed to update endpoint", "error", enhancedErr)
		return
	}

	// Trigger recovery callback if endpoint recovered (transitioned to healthy from unhealthy)
	if statusChanged && newStatus == domain.StatusHealthy && oldStatus != domain.StatusUnknown {
		c.logger.Info("Endpoint recovered, triggering model discovery refresh",
			"endpoint", endpoint.Name,
			"was", oldStatus.String())

		if c.recoveryCallback != nil {
			// we exec recovery callback asynchronously to avoid blocking health checks
			go func(ep domain.Endpoint) {
				callbackCtx, cancel := context.WithTimeout(context.Background(), DefaultRecoveryCallbackTimeout)
				defer cancel()

				callbackErr := c.recoveryCallback.OnEndpointRecovered(callbackCtx, &ep)

				if callbackErr != nil {
					if errors.Is(callbackErr, context.DeadlineExceeded) {
						c.logger.Warn("Recovery callback timed out",
							"endpoint", ep.Name,
							"timeout", DefaultRecoveryCallbackTimeout)
					} else {
						c.logger.Warn("Failed to execute recovery callback",
							"endpoint", ep.Name,
							"error", callbackErr)
					}
				} else {
					c.logger.Debug("Recovery callback completed successfully",
						"endpoint", ep.Name)
				}
			}(endpointCopy)
		}
	}

	// Trigger unhealthy callback only when an endpoint becomes non-routable from a
	// routable state. Busy and Warming are still routable, so a Healthy→Busy transition
	// must not evict sticky sessions - that would defeat KV-cache affinity entirely.
	// Unknown→Unhealthy is intentionally excluded: nothing could have been pinned to an
	// endpoint that was never routable, so there is nothing to purge.
	if statusChanged && !newStatus.IsRoutable() && oldStatus.IsRoutable() {
		if c.unhealthyCallback != nil {
			go func(ep domain.Endpoint) {
				callbackCtx, cancel := context.WithTimeout(context.Background(), DefaultRecoveryCallbackTimeout)
				defer cancel()
				c.unhealthyCallback.OnEndpointUnhealthy(callbackCtx, &ep)
			}(endpointCopy)
		}
	}

	c.logHealthCheckResult(endpoint, oldStatus, newStatus, statusChanged, result, nextInterval, err)
}

func (c *HTTPHealthChecker) logHealthCheckResult(
	endpoint *domain.Endpoint,
	oldStatus, newStatus domain.EndpointStatus,
	statusChanged bool,
	result domain.HealthCheckResult,
	nextInterval time.Duration,
	checkErr error,
) {
	switch {
	case statusChanged:
		switch {
		case oldStatus == domain.StatusUnknown && newStatus == domain.StatusHealthy:
			c.logger.InfoHealthStatus("Endpoint initial status:",
				endpoint.Name,
				newStatus,
				"latency", result.Latency,
				logKeyNextCheckIn, nextInterval)

		case oldStatus == domain.StatusUnknown:
			// Initial discovery of unhealthy endpoint
			detailedArgs := make([]interface{}, 0, 8)
			detailedArgs = append(detailedArgs,
				"endpoint_url", endpoint.GetURLString(),
				"status_code", result.StatusCode,
				"error_type", result.ErrorType,
			)
			if checkErr != nil {
				var healthCheckErr *domain.HealthCheckError
				if errors.As(checkErr, &healthCheckErr) {
					detailedArgs = append(detailedArgs, "check_error", healthCheckErr.Error())
				} else {
					detailedArgs = append(detailedArgs, "check_error", checkErr.Error())
				}
			}
			c.logger.WarnWithContext("Endpoint discovered offline:", endpoint.Name, logger.LogContext{
				UserArgs: []interface{}{
					logKeyStatus, newStatus.String(),
					"latency", result.Latency,
					logKeyNextCheckIn, nextInterval,
				},
				DetailedArgs: detailedArgs,
			})

		case newStatus == domain.StatusHealthy:
			c.logger.InfoHealthStatus("Endpoint recovered:",
				endpoint.Name,
				newStatus,
				"was", oldStatus.String(),
				"latency", result.Latency,
				logKeyNextCheckIn, nextInterval)

		default:
			// Status changed to unhealthy
			detailedArgs := make([]interface{}, 0, 8)
			detailedArgs = append(detailedArgs,
				"endpoint_url", endpoint.GetURLString(),
				"status_code", result.StatusCode,
				"error_type", result.ErrorType,
			)
			if checkErr != nil {
				var healthCheckErr *domain.HealthCheckError
				if errors.As(checkErr, &healthCheckErr) {
					detailedArgs = append(detailedArgs, "check_error", healthCheckErr.Error())
				} else {
					detailedArgs = append(detailedArgs, "check_error", checkErr.Error())
				}
			}
			c.logger.WarnWithContext("Endpoint status changed:", endpoint.Name, logger.LogContext{
				UserArgs: []interface{}{
					logKeyStatus, newStatus.String(),
					"was", oldStatus.String(),
					"consecutive_failures", endpoint.ConsecutiveFailures + 1,
					"latency", result.Latency,
					logKeyNextCheckIn, nextInterval,
				},
				DetailedArgs: detailedArgs,
			})
		}

	case checkErr != nil && endpoint.ConsecutiveFailures > 0 && endpoint.ConsecutiveFailures%5 == 0:
		// Log ongoing issues every 5th consecutive failure instead of time-based throttling
		// Pre-allocate with capacity for all fields including error
		detailedArgs := make([]interface{}, 0, 8)
		detailedArgs = append(detailedArgs,
			"endpoint_url", endpoint.GetURLString(),
			"status_code", result.StatusCode,
			"error_type", result.ErrorType,
		)
		var healthCheckErr *domain.HealthCheckError
		if errors.As(checkErr, &healthCheckErr) {
			detailedArgs = append(detailedArgs, "check_error", healthCheckErr.Error())
		} else {
			detailedArgs = append(detailedArgs, "check_error", checkErr.Error())
		}

		c.logger.WarnWithContext("Endpoint still having issues:", endpoint.Name, logger.LogContext{
			UserArgs: []interface{}{
				logKeyStatus, newStatus.String(),
				"consecutive_failures", endpoint.ConsecutiveFailures + 1,
				logKeyNextCheckIn, nextInterval,
			},
			DetailedArgs: detailedArgs,
		})
	}
}

func (c *HTTPHealthChecker) GetSchedulerStats() map[string]interface{} {
	running := c.isRunning.Load()

	if !running {
		return map[string]interface{}{
			"isRunning": false,
		}
	}

	return map[string]interface{}{
		"isRunning":      running,
		"check_interval": DefaultHealthCheckInterval.String(),
	}
}

func (c *HTTPHealthChecker) RunHealthCheck(ctx context.Context, initial bool) error {
	if !c.isRunning.Load() {
		return domain.NewEndpointError("run_health_check", "health_checker",
			errors.New("health checker is not running"))
	}

	endpoints, err := c.repository.GetAll(ctx)
	if err != nil {
		return domain.NewEndpointError("run_health_check", "repository",
			fmt.Errorf("failed to get endpoints: %w", err))
	}

	if initial {
		c.logger.Info("Initialising health checks", "endpoints", len(endpoints))
	} else {
		c.logger.Info("Running full health checks", "endpoints", len(endpoints))
	}

	c.logEndpointsTable(endpoints)

	// Use controlled concurrency for manual health checks too
	var wg sync.WaitGroup
	semaphore := make(chan struct{}, 10) // Allow more concurrency for manual checks

	for _, endpoint := range endpoints {
		wg.Add(1)
		go func(ep *domain.Endpoint) {
			defer wg.Done()

			// Acquire semaphore
			select {
			case semaphore <- struct{}{}:
				defer func() { <-semaphore }()
			case <-ctx.Done():
				return
			}

			c.checkEndpointSafely(ctx, ep)
		}(endpoint)
	}

	wg.Wait()
	return nil
}

func (c *HTTPHealthChecker) logEndpointsTable(endpoints []*domain.Endpoint) {
	if len(endpoints) == 0 {
		return
	}

	// Sort routes by registration priority
	type endpointEntry struct {
		url      string
		name     string
		kind     string
		health   string
		priority int
		interval int
		timeout  int
	}

	entries := make([]endpointEntry, 0, len(endpoints))
	for _, info := range endpoints {
		entries = append(entries, endpointEntry{
			url:      info.URL.String(),
			name:     info.Name,
			kind:     info.Type,
			health:   info.HealthCheckPathString,
			priority: info.Priority,
			interval: int(info.CheckInterval.Seconds()),
			timeout:  int(info.CheckTimeout.Seconds()),
		})
	}

	sort.Slice(entries, func(i, j int) bool {
		return entries[i].priority > entries[j].priority
	})

	// Build table
	tableData := make([][]string, 0, len(entries)+1)
	tableData = append(tableData, []string{"PRI", "NAME", "TYPE", "URL", "HEALTH", "CHECK", "TIMEOUT"})

	for _, entry := range entries {
		tableData = append(tableData, []string{
			strconv.Itoa(entry.priority),
			entry.name,
			entry.kind,
			entry.url,
			entry.health,
			fmt.Sprintf("%ds", entry.interval),
			fmt.Sprintf("%ds", entry.timeout),
		})
	}

	c.logger.InfoWithCount("Registered endpoints", len(entries))
	// .WithHeaderStyle(c.logger.Theme.Accent)
	tableString, _ := pterm.DefaultTable.WithHasHeader().WithData(tableData).Srender()
	fmt.Print(tableString)
}
