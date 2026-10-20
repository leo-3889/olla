package core

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"syscall"
	"time"

	"github.com/thushan/olla/internal/core/constants"
	"github.com/thushan/olla/internal/core/domain"
	"github.com/thushan/olla/internal/core/ports"
	"github.com/thushan/olla/internal/logger"
)

// RetryHandler manages connection failure recovery and endpoint failover
type RetryHandler struct {
	logger           logger.StyledLogger
	discoveryService ports.DiscoveryService
	// retryOnStatus is the proxy.retry_on_status set (backend refusal codes
	// that rotate the request to the next endpoint before the first byte
	// reaches the client). Empty/nil = feature off: status responses are
	// written through untouched, byte-identical to a handler without the
	// option. See WithRetryOnStatus.
	retryOnStatus map[int]struct{}
}

// RetryOption configures optional RetryHandler behaviour at construction
// time. Options keep NewRetryHandler's signature stable for every existing
// caller (both proxy engines and the package test harness) while new knobs
// ride the same constructor.
type RetryOption func(*RetryHandler)

// WithRetryOnStatus arms backend-error failover: when a backend answers with
// one of these status codes (4xx/5xx - boot-validated in
// config.ProxyConfig.Validate) and no response byte has reached the client,
// the request rotates to the next endpoint instead of forwarding the refusal.
// An empty list (the default) leaves behaviour byte-identical to a handler
// built without the option.
func WithRetryOnStatus(statuses []int) RetryOption {
	return func(h *RetryHandler) {
		if len(statuses) == 0 {
			return
		}
		set := make(map[int]struct{}, len(statuses))
		for _, code := range statuses {
			set[code] = struct{}{}
		}
		h.retryOnStatus = set
	}
}

// NewRetryHandler creates a new retry handler
func NewRetryHandler(discoveryService ports.DiscoveryService, logger logger.StyledLogger, opts ...RetryOption) *RetryHandler {
	h := &RetryHandler{
		discoveryService: discoveryService,
		logger:           logger,
	}
	for _, opt := range opts {
		opt(h)
	}
	return h
}

// ProxyFunc defines the signature for endpoint proxy implementations
type ProxyFunc func(ctx context.Context, w http.ResponseWriter, r *http.Request, endpoint *domain.Endpoint, stats *ports.RequestStats) error

// ResponseStartedWriter wraps http.ResponseWriter and records whether any response
// bytes or status codes have been sent to the client. Retry logic uses this to gate
// retry decisions: once the client has received data, retrying a non-idempotent
// request would send duplicate content or charge twice on metered APIs. It is also
// the correct "has this response been committed" signal for callers upstream of
// retry (e.g. handler_proxy.go's handleProxyError) - Header().Set does not commit
// anything, only WriteHeader/Write do, so a Content-Type-presence check is not a
// reliable substitute.
//
// A flush is also a commit: streaming code can in principle reach a bare Flush()
// (via http.NewResponseController) without a prior Write on this wrapper - bytes
// already buffered by the underlying writer would then reach the client while
// started stays false. Flush/FlushError guard against that by marking started
// before delegating, so the signal holds regardless of call order.
type ResponseStartedWriter struct {
	http.ResponseWriter
	started bool
}

// NewResponseStartedWriter wraps w so callers can observe whether a response has
// been committed via Started().
func NewResponseStartedWriter(w http.ResponseWriter) *ResponseStartedWriter {
	return &ResponseStartedWriter{ResponseWriter: w}
}

func (rw *ResponseStartedWriter) WriteHeader(code int) {
	rw.started = true
	rw.ResponseWriter.WriteHeader(code)
}

func (rw *ResponseStartedWriter) Write(b []byte) (int, error) {
	rw.started = true
	return rw.ResponseWriter.Write(b)
}

// Started reports whether a status code or body bytes have been committed to the
// client yet.
func (rw *ResponseStartedWriter) Started() bool {
	return rw.started
}

// Unwrap exposes the underlying ResponseWriter so http.NewResponseController can
// discover optional interfaces (Flush, Hijack, SetDeadline, etc.) via the chain.
// Without this, the wrapper hides the underlying flusher and SSE streams stall.
func (rw *ResponseStartedWriter) Unwrap() http.ResponseWriter {
	return rw.ResponseWriter
}

// FlushError marks the response as started and flushes the underlying writer,
// mirroring http.ResponseController's FlushError semantics. It returns
// http.ErrNotSupported if nothing in the underlying chain supports flushing,
// rather than panicking or swallowing the failure.
//
// started is set before the flush is attempted: a flush pushes any buffered
// bytes to the client immediately, so the commit has already happened by the
// time this call is made, even if the underlying flush itself then fails.
func (rw *ResponseStartedWriter) FlushError() error {
	wasStarted := rw.started
	rw.started = true

	rc := http.NewResponseController(rw.ResponseWriter)
	if err := rc.Flush(); err != nil {
		// ErrNotSupported means nothing in the chain could flush at all, so
		// nothing was committed - restore the prior state. Any other error
		// (dead connection, etc.) may have partially written bytes before
		// failing, so started stays true.
		if errors.Is(err, http.ErrNotSupported) {
			rw.started = wasStarted
		}
		return err
	}
	return nil
}

// Flush satisfies the plain http.Flusher interface for callers that still
// type-assert for it directly. Like http.Flusher.Flush, it is best-effort and
// discards any error - use FlushError to observe failures.
func (rw *ResponseStartedWriter) Flush() {
	_ = rw.FlushError()
}

// isIdempotent reports whether the HTTP method is safe to retry after a partial
// response. GET, HEAD and OPTIONS are defined as idempotent by RFC 9110; POST,
// PATCH and DELETE are not. Retrying them risks double-billing or duplicate side
// effects if the upstream already processed the first attempt.
func isIdempotent(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	default:
		return false
	}
}

// statusGateMaxBuffer bounds the buffered refusal body. Legitimate 429/503
// bodies are small JSON error objects; a response larger than this is not
// behaving like a refusal, so it is committed to the client unbuffered and
// the rotation opportunity is forfeited. The cap bounds per-request memory.
const statusGateMaxBuffer = 64 << 10

// statusGateWriter sits above the ResponseStartedWriter for the duration of
// ExecuteWithRetry and holds back responses whose status code is in the
// configured retry_on_status set. While a response is held, NOTHING has
// reached the wrapped writer, so the retry loop can discard it and rotate the
// request to a different endpoint - the client never sees the refusal.
//
// Every other response passes through untouched: with an empty set the writer
// is behaviourally invisible (no copying, no buffering, byte-identical to
// handing the proxy func the tracker directly).
//
// A flush commits the held response immediately. Flushing is the stream-start
// signal (SSE code flushes before/while streaming tokens), and the existing
// responseStarted contract forbids retry once bytes are out - so holding
// bytes back past a flush would only lie about the commit point.
type statusGateWriter struct {
	next    http.ResponseWriter
	set     map[int]struct{}
	pending bool
	code    int
	buf     bytes.Buffer
}

func newStatusGate(next http.ResponseWriter, set map[int]struct{}) *statusGateWriter {
	return &statusGateWriter{next: next, set: set}
}

func (g *statusGateWriter) Header() http.Header { return g.next.Header() }

func (g *statusGateWriter) WriteHeader(code int) {
	if len(g.set) > 0 && !g.pending {
		if _, ok := g.set[code]; ok {
			g.pending = true
			g.code = code
			g.buf.Reset()
			return
		}
	}
	g.next.WriteHeader(code)
}

func (g *statusGateWriter) Write(b []byte) (int, error) {
	if g.pending {
		if g.buf.Len()+len(b) <= statusGateMaxBuffer {
			return g.buf.Write(b)
		}
		// Oversized body: commit what was held and stop gating. A response
		// streaming megabytes is not the refusal this gate exists to rotate.
		g.commit()
	}
	return g.next.Write(b)
}

// deferred reports the status code of a response currently held by the gate.
func (g *statusGateWriter) deferred() (int, bool) {
	return g.code, g.pending
}

// commit releases a held response to the wrapped writer (status + buffered
// body). A no-op when nothing is held, so success paths can commit
// unconditionally.
func (g *statusGateWriter) commit() {
	if !g.pending {
		return
	}
	g.pending = false
	code := g.code
	g.code = 0
	g.next.WriteHeader(code)
	if g.buf.Len() > 0 {
		_, _ = g.next.Write(g.buf.Bytes())
	}
	g.buf.Reset()
}

// discard drops a held response without ever writing it: the rotation path
// uses this so the rotated-away refusal never reaches the client.
func (g *statusGateWriter) discard() {
	g.pending = false
	g.code = 0
	g.buf.Reset()
}

// reset clears any held response between attempts: the previous attempt
// failed (returned an error), so its partial response is dead data that must
// not bleed into the next endpoint's writes.
func (g *statusGateWriter) reset() {
	g.discard()
}

// Flush commits any held response and forwards the flush. A flush is a
// stream start: after this, rotation is off the table (responseStarted).
func (g *statusGateWriter) Flush() {
	g.commit()
	_ = http.NewResponseController(g.next).Flush()
}

// FlushError mirrors Flush with observable errors, matching
// ResponseStartedWriter's contract (http.ErrNotSupported when nothing in the
// chain can flush - which committed nothing, so no bytes left the gate).
func (g *statusGateWriter) FlushError() error {
	g.commit()
	// http.ResponseController.Flush surfaces the FlushError() error of the
	// underlying chain (http.ErrNotSupported when nothing can flush).
	return http.NewResponseController(g.next).Flush()
}

// Unwrap exposes the wrapped writer so http.NewResponseController reaches the
// tracker's (and ultimately the real ResponseWriter's) Flush/Hijack/
// SetDeadline - same reason ResponseStartedWriter needs it: hiding these
// optional interfaces stalls SSE streams.
func (g *statusGateWriter) Unwrap() http.ResponseWriter { return g.next }

// ExecuteWithRetry attempts request delivery with automatic failover on connection errors.
// For non-idempotent methods (POST, PATCH, DELETE), retry is suppressed once the
// response has started. Resending to a different endpoint risks duplicate content
// reaching the client or double-charging a metered API.
//
// Backend-error failover (proxy.retry_on_status, armed via WithRetryOnStatus):
// a refusal status (e.g. 429/503) held by the status gate - i.e. no byte has
// reached the client - rotates to the next endpoint under the SAME suppressions
// as the connection-error path (responseStarted + IsIdempotent, tried-endpoint
// exclusion). The rotated-away response is never written to the client.
func (h *RetryHandler) ExecuteWithRetry(
	ctx context.Context,
	w http.ResponseWriter,
	r *http.Request,
	endpoints []*domain.Endpoint,
	selector domain.EndpointSelector,
	stats *ports.RequestStats,
	proxyFunc ProxyFunc,
) error {
	if len(endpoints) == 0 {
		return errors.New("no endpoints available")
	}

	// Work with a copy to avoid modifying the original slice
	availableEndpoints := make([]*domain.Endpoint, len(endpoints))
	copy(availableEndpoints, endpoints)

	// Preserve request body for potential retries
	bodyBytes, err := h.preserveRequestBody(r)
	if err != nil {
		return err
	}

	// Wrap the writer so we can detect when bytes have been committed to the client.
	tracker := NewResponseStartedWriter(w)

	// The status gate holds back a retry_on_status refusal so the loop below
	// can discard it and rotate BEFORE any byte reaches the tracker. With the
	// set empty (default) it is a pure pass-through.
	gate := newStatusGate(tracker, h.retryOnStatus)

	var lastErr error
	maxRetries := len(endpoints)
	attemptCount := 0
	// Tracked separately from lastErr because lastErr only ever reflects the
	// final attempt: if earlier endpoints failed with real connection errors
	// and the final one was CB-open, lastErr alone would mislabel the whole
	// exhaustion as "circuit breaker open" and hide the network failures.
	var cbOpenCount, connErrCount int

	for attemptCount < maxRetries && len(availableEndpoints) > 0 {
		if err := h.checkContextCancellation(ctx); err != nil {
			return err
		}

		h.resetRequestBodyForRetry(r, bodyBytes, attemptCount)

		// Drop any response the previous (failed) attempt left in the gate so
		// this attempt's writes start from a clean buffer.
		gate.reset()

		endpoint, err := selector.Select(ctx, availableEndpoints)
		if err != nil {
			return fmt.Errorf("endpoint selection failed: %w", err)
		}

		attemptCount++
		lastErr = h.executeProxyAttempt(ctx, gate, r, endpoint, selector, stats, proxyFunc)

		if lastErr == nil {
			// Backend-error failover (proxy.retry_on_status): the gate is
			// holding a refusal status that has NOT reached the client.
			// Rotate to the next endpoint under the same suppressions as the
			// connection-error path; the held response is DISCARDED and never
			// forwarded.
			if code, held := gate.deferred(); held {
				// Existing responseStarted + IsIdempotent suppression,
				// verbatim: once bytes are committed, resending a
				// non-idempotent request risks duplicate content reaching the
				// client or double-charging a metered API.
				if tracker.started && !isIdempotent(r.Method) {
					h.logger.Debug("skipping status retry: response already started for non-idempotent method",
						"method", r.Method,
						"endpoint", endpoint.Name,
						"status", code)
					gate.commit()
					return nil
				}
				// responseStarted guard: a flush commits the gate, so a held
				// status implies nothing has been flushed. >=1 other
				// candidate remains: rotation excludes this endpoint via the
				// same tried-endpoint removal the connection-error path uses.
				if !tracker.started && len(availableEndpoints) > 1 {
					h.logger.Info("backend returned retryable status, failing over to next endpoint",
						"endpoint", endpoint.Name,
						"status", code,
						"attempt", attemptCount,
						"remaining_endpoints", len(availableEndpoints)-1)
					gate.discard()
					availableEndpoints = h.removeFailedEndpoint(availableEndpoints, endpoint)
					continue
				}
				// Exhausted: every candidate refused (this one is the last),
				// so the refusal passes through honestly - the client sees a
				// real backend status, not a synthesized error.
				h.logger.Debug("retryable status with no remaining candidates, passing response through",
					"endpoint", endpoint.Name,
					"status", code,
					"attempts", attemptCount)
			}
			gate.commit()
			return nil
		}

		// A CB-open rejection is failover-eligible, but unlike a connection
		// error it must NOT demote the endpoint's persisted health: the breaker
		// already reflects accumulated failure state, and the proxy engine
		// owns demotion for genuine connectivity failures. Remove the endpoint
		// from this request's candidate list only, then continue. Evaluated
		// before IsConnectionError so the sentinel never reaches the
		// connection-error/handleConnectionFailure path below.
		if errors.Is(lastErr, ErrCircuitBreakerOpen) {
			cbOpenCount++
			// For non-idempotent methods, once the response has started we
			// cannot safely retry even on CB-open: the same partial-response
			// hazard applies. The guard runs identically on every failover path.
			if tracker.started && !isIdempotent(r.Method) {
				h.logger.Debug("skipping retry: response already started for non-idempotent method",
					"method", r.Method,
					"endpoint", endpoint.Name)
				return lastErr
			}
			h.logger.Debug("circuit breaker open, failing over to next endpoint",
				"endpoint", endpoint.Name,
				"remaining_endpoints", len(availableEndpoints)-1)
			availableEndpoints = h.removeFailedEndpoint(availableEndpoints, endpoint)
			continue
		}

		if !IsConnectionError(lastErr) {
			// Non-connection error warrants immediate failure
			return lastErr
		}
		connErrCount++

		// For non-idempotent methods, once the response has started we cannot
		// safely retry. The client would receive a partial response from this
		// endpoint followed by a fresh one from the next, causing corruption or
		// double-billing. Return the error and let the caller decide.
		if tracker.started && !isIdempotent(r.Method) {
			h.logger.Debug("skipping retry: response already started for non-idempotent method",
				"method", r.Method,
				"endpoint", endpoint.Name)
			return lastErr
		}

		// Handle connection error and retry logic
		availableEndpoints = h.handleConnectionFailure(ctx, endpoint, lastErr, attemptCount, availableEndpoints, maxRetries)
	}

	return h.buildFinalError(availableEndpoints, maxRetries, lastErr, cbOpenCount, connErrCount)
}

// preserveRequestBody reads and preserves request body for potential retries
func (h *RetryHandler) preserveRequestBody(r *http.Request) ([]byte, error) {
	if r.Body == nil || r.Body == http.NoBody {
		return nil, nil
	}

	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		h.logger.Error("Failed to read request body for retry preservation", "error", err)
		return nil, fmt.Errorf("failed to read request body: %w", err)
	}

	if err := r.Body.Close(); err != nil {
		h.logger.Warn("Failed to close original request body", "error", err)
	}

	// Recreate the body for the first attempt
	r.Body = io.NopCloser(bytes.NewReader(bodyBytes))
	return bodyBytes, nil
}

// checkContextCancellation verifies if the context has been cancelled
func (h *RetryHandler) checkContextCancellation(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return fmt.Errorf("request cancelled: %w", ctx.Err())
	default:
		return nil
	}
}

// resetRequestBodyForRetry recreates request body for retry attempts
func (h *RetryHandler) resetRequestBodyForRetry(r *http.Request, bodyBytes []byte, attemptCount int) {
	if bodyBytes != nil && attemptCount > 0 {
		r.Body = io.NopCloser(bytes.NewReader(bodyBytes))
	}
}

// executeProxyAttempt executes a single proxy attempt with connection counting
func (h *RetryHandler) executeProxyAttempt(ctx context.Context, w http.ResponseWriter, r *http.Request,
	endpoint *domain.Endpoint, selector domain.EndpointSelector, stats *ports.RequestStats, proxyFunc ProxyFunc) error {

	selector.IncrementConnections(endpoint)
	defer selector.DecrementConnections(endpoint)

	return proxyFunc(ctx, w, r, endpoint, stats)
}

// handleConnectionFailure processes connection failures and manages endpoint removal.
//
// This is one of two layers of failure protection, and it fires first: a
// single connection-refused error demotes the endpoint to offline right here
// (see markEndpointUnhealthy), on the very first attempt, well before the
// proxy engine's own per-endpoint circuit breaker (olla/service.go's
// circuitBreaker, threshold 5) could accumulate enough failures to open.
// That's intentional layering, not redundancy: connection-refused means the
// backend process is down/unreachable, which request-path health demotion
// (backed by the health checker's periodic re-probing) is the right tool
// for - retrying it further or waiting for a 5-failure threshold would just
// burn latency on a backend that health checks will keep confirming is
// down. In practice this means the circuit breaker rarely gets to open on
// connection-refused; it mostly governs the failure classes that don't
// trip this 1-strike path - hangs and response-header timeouts, where the
// backend is reachable but not answering. See olla/service.go's
// circuitBreaker.IsOpen for the other side of this split.
func (h *RetryHandler) handleConnectionFailure(ctx context.Context, endpoint *domain.Endpoint,
	err error, attemptCount int, availableEndpoints []*domain.Endpoint, maxRetries int) []*domain.Endpoint {

	h.logger.Warn("Connection failed to endpoint, marking as unhealthy",
		"endpoint", endpoint.Name,
		"error", err,
		"attempt", attemptCount,
		"remaining_endpoints", len(availableEndpoints)-1)

	h.markEndpointUnhealthy(ctx, endpoint)

	// Remove failed endpoint from available list
	updatedEndpoints := h.removeFailedEndpoint(availableEndpoints, endpoint)

	if len(updatedEndpoints) > 0 && attemptCount < maxRetries {
		h.logger.Info("Retrying request with different endpoint",
			"available_endpoints", len(updatedEndpoints),
			"attempts_remaining", maxRetries-attemptCount)
	}

	return updatedEndpoints
}

// removeFailedEndpoint removes the failed endpoint from the available list
func (h *RetryHandler) removeFailedEndpoint(endpoints []*domain.Endpoint, failedEndpoint *domain.Endpoint) []*domain.Endpoint {
	for i := range endpoints {
		if endpoints[i].Name == failedEndpoint.Name {
			// Remove element at index i by copying subsequent elements
			copy(endpoints[i:], endpoints[i+1:])
			return endpoints[:len(endpoints)-1]
		}
	}
	return endpoints
}

// buildFinalError constructs the appropriate error message for retry failure.
// cbOpenCount and connErrCount are tallied across every attempt, not just the
// last one - lastErr alone can't distinguish "every endpoint was CB-open"
// from "some failed on the network and the last one happened to be CB-open",
// and conflating the two hides genuine connectivity failures from operators.
func (h *RetryHandler) buildFinalError(availableEndpoints []*domain.Endpoint, maxRetries int, lastErr error, cbOpenCount, connErrCount int) error {
	if len(availableEndpoints) == 0 {
		switch {
		case cbOpenCount > 0 && connErrCount > 0:
			return fmt.Errorf("all endpoints unavailable (%d connection failure(s), %d circuit breaker open): %w",
				connErrCount, cbOpenCount, lastErr)
		case cbOpenCount > 0:
			// CB-open exhaustion is reachable here now (FR-2 removes endpoints
			// without demoting them); don't mislabel it a connection failure, which
			// would imply a network outage that never happened.
			return fmt.Errorf("all endpoints unavailable (circuit breaker open): %w", lastErr)
		default:
			return fmt.Errorf("all endpoints failed with connection errors: %w", lastErr)
		}
	}
	return fmt.Errorf("max attempts (%d) reached: %w", maxRetries, lastErr)
}

// IsConnectionError identifies transient network errors suitable for retry.
// Client-side context cancellations and deadline exceeded errors are NOT
// connection errors — the backend is not at fault and must not be penalised
// or failed-over with an already-expired context.
func IsConnectionError(err error) bool {
	if err == nil {
		return false
	}

	// Short-circuit before the net.Error check: context.DeadlineExceeded's
	// concrete type (*url.Error wrapping it) satisfies net.Error (Timeout()==true),
	// which would falsely mark the endpoint unhealthy for a client timeout.
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return false
	}

	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}

	var syscallErr syscall.Errno
	if errors.As(err, &syscallErr) {
		switch syscallErr {
		case syscall.ECONNREFUSED, syscall.ECONNRESET, syscall.ECONNABORTED:
			return true
		default:
			// Non-connection syscall errors
		}
	}

	return hasConnectionError(err)
}

// connectionErrors is a last-resort string fallback for errors that have lost
// their type information (e.g. plain errors.New from middleware or test stubs).
// Well-wrapped OS errors are already caught by the net.Error / syscall.Errno
// branches above, so this list covers only cases those branches cannot reach.
var connectionErrors = []string{
	"connection refused",     // syscall.ECONNREFUSED on non-unwrappable paths
	"connection reset",       // syscall.ECONNRESET on non-unwrappable paths
	"no such host",           // *net.DNSError without type chain
	"network is unreachable", // syscall.ENETUNREACH without type chain
	"no route to host",       // syscall.EHOSTUNREACH without type chain
	"i/o timeout",            // plain-string timeout errors; net.Error.Timeout() covers wrapped ones
	"connectex:",             // Windows dial error prefix; appears without net.Error wrapping on some paths
}

func hasConnectionError(err error) bool {
	if err == nil {
		return false
	}

	errStr := strings.ToLower(err.Error())
	for _, pattern := range connectionErrors {
		if strings.Contains(errStr, pattern) {
			return true
		}
	}

	return false
}

// markEndpointUnhealthy transitions endpoint to offline state with backoff calculation
func (h *RetryHandler) markEndpointUnhealthy(ctx context.Context, endpoint *domain.Endpoint) {
	if endpoint == nil {
		return
	}

	now := time.Now()

	// Work with copy to preserve original state
	endpointCopy := *endpoint
	endpointCopy.Status = domain.StatusOffline
	endpointCopy.ConsecutiveFailures++
	endpointCopy.LastChecked = now

	// Calculate proper exponential backoff multiplier
	// First failure: keep default interval from the endpoint but set multiplier to 2
	// Subsequent failures: apply exponential backoff
	var backoffInterval time.Duration

	if endpointCopy.BackoffMultiplier <= 1 {
		// First failure - use normal interval
		endpointCopy.BackoffMultiplier = 2
		backoffInterval = endpointCopy.CheckInterval
	} else {
		// Subsequent failures - apply current multiplier and calculate next
		backoffInterval = endpointCopy.CheckInterval * time.Duration(endpointCopy.BackoffMultiplier)

		// Calculate next multiplier for future failures
		endpointCopy.BackoffMultiplier *= 2
		if endpointCopy.BackoffMultiplier > constants.DefaultMaxBackoffMultiplier {
			endpointCopy.BackoffMultiplier = constants.DefaultMaxBackoffMultiplier
		}
	}

	if backoffInterval > constants.DefaultMaxBackoffSeconds {
		backoffInterval = constants.DefaultMaxBackoffSeconds
	}
	endpointCopy.NextCheckTime = now.Add(backoffInterval)

	h.logger.Warn("Marking endpoint as unhealthy due to connection failure",
		"endpoint", endpoint.Name,
		"consecutive_failures", endpointCopy.ConsecutiveFailures,
		"backoff_multiplier", endpointCopy.BackoffMultiplier,
		"next_check", endpointCopy.NextCheckTime.Format(time.RFC3339))

	// Persist status change via discovery service
	h.updateEndpointStatus(ctx, &endpointCopy)
}

// updateEndpointStatus persists endpoint state changes
func (h *RetryHandler) updateEndpointStatus(ctx context.Context, endpoint *domain.Endpoint) {
	if err := h.discoveryService.UpdateEndpointStatus(ctx, endpoint); err != nil {
		h.logger.Debug("Failed to update endpoint status in repository", "error", err)
	}
}
