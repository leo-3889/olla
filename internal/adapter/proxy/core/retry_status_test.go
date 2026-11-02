package core

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/thushan/olla/internal/core/domain"
	"github.com/thushan/olla/internal/core/ports"
	"github.com/thushan/olla/internal/logger"
)

// Backend-error failover (proxy.retry_on_status) tests.
//
// Contract under test: a backend refusal status (429/503, configured set) that
// has NOT yet written a single byte to the client rotates the request to the
// next endpoint, exactly like a connection error does - tried endpoints are
// excluded, the request body is restored, the refusal is NEVER forwarded.
// The existing suppressions stay in force verbatim: once the response has
// started (any byte/flush committed) nothing rotates, and the
// responseStarted+IsIdempotent gate that protects the connection-error path
// gates this path identically (an uncommitted response has sent no side
// effect, so an idempotent-method request - and, per the existing gate, a
// non-idempotent one whose response never started - may rotate; a started
// non-idempotent response must not).

// newStatusRetryHandler builds a handler with the retry_on_status set armed.
// Mirrors newTestRetryHandler's harness exactly (same fake discovery
// service, same logger config), adding only the new option.
func newStatusRetryHandler(t *testing.T, statuses ...int) *RetryHandler {
	t.Helper()
	logCfg := &logger.Config{Level: "error"}
	log, _, _ := logger.New(logCfg)
	return NewRetryHandler(&testDiscoveryService{}, logger.NewPlainStyledLogger(log), WithRetryOnStatus(statuses))
}

// trackingSelector records the endpoints it hands out, in order. The rotation
// path shrinks the candidate slice; recording the sequence proves both the
// rotation itself and the tried-endpoint exclusion.
type trackingSelector struct {
	mu      sync.Mutex
	tried   []string
	cursor  int
	counter int
}

func (s *trackingSelector) Select(_ context.Context, eps []*domain.Endpoint) (*domain.Endpoint, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(eps) == 0 {
		return nil, io.EOF
	}
	ep := eps[s.cursor%len(eps)]
	s.cursor++
	s.tried = append(s.tried, ep.Name)
	return ep, nil
}
func (s *trackingSelector) Name() string                            { return "tracking" }
func (s *trackingSelector) IncrementConnections(_ *domain.Endpoint) {}
func (s *trackingSelector) DecrementConnections(_ *domain.Endpoint) {}

func (s *trackingSelector) Tried() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.tried))
	copy(out, s.tried)
	return out
}

// refusalThenSuccess serves `status`+refusalBody from the endpoint named
// first (twin-a) and 200+okBody from every later endpoint, streaming through
// the writer it is handed - the same proxyFunc shape the engines use
// (WriteHeader then copy the body, return nil).
func refusalThenSuccess(sel *trackingSelector, refusalBody, okBody string, refusalStatus int) ProxyFunc {
	return func(_ context.Context, w http.ResponseWriter, r *http.Request, ep *domain.Endpoint, _ *ports.RequestStats) error {
		n := len(sel.Tried())
		if n == 1 {
			w.WriteHeader(refusalStatus)
			_, _ = w.Write([]byte(refusalBody))
			return nil
		}
		// The rotation path must have restored the request body for us.
		// Only a POST carries one (GET arrives with http.NoBody).
		body, err := io.ReadAll(r.Body)
		if err != nil {
			return err
		}
		if r.Method == http.MethodPost && len(body) == 0 {
			return io.ErrUnexpectedEOF
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(okBody))
		return nil
	}
}

// TestRetry_429_RotatesToTwinBeforeFirstByte is the #193 class: twin A refuses
// with 429 and the client must receive twin B's 200 - the refusal is never
// written. POST (the real chat-completions shape) with a body, so the test
// also proves request-body restoration on the rotation path.
func TestRetry_429_RotatesToTwinBeforeFirstByte(t *testing.T) {
	t.Parallel()

	h := newStatusRetryHandler(t, http.StatusTooManyRequests)
	sel := &trackingSelector{}
	w := httptest.NewRecorder()

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"alias-fast"}`))
	proxyFunc := refusalThenSuccess(sel, `{"error":{"type":"rate_limit_error"}}`, `{"from":"twin-b"}`, http.StatusTooManyRequests)

	err := h.ExecuteWithRetry(context.Background(), w, req,
		[]*domain.Endpoint{namedEndpoint("twin-a"), namedEndpoint("twin-b")},
		sel, &ports.RequestStats{}, proxyFunc)

	require.NoError(t, err)
	assert.Equal(t, []string{"twin-a", "twin-b"}, sel.Tried(), "429 must rotate to the twin, excluding the tried endpoint")
	assert.Equal(t, http.StatusOK, w.Code, "client must see twin B's 200, not the 429")
	assert.Equal(t, `{"from":"twin-b"}`, w.Body.String(), "the rotated-away refusal body must never reach the client")
}

// TestRetry_503_RotatesToTwin is the same contract for 503, on GET (the
// idempotent side of the method gate).
func TestRetry_503_RotatesToTwin(t *testing.T) {
	t.Parallel()

	h := newStatusRetryHandler(t, http.StatusServiceUnavailable)
	sel := &trackingSelector{}
	w := httptest.NewRecorder()

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	proxyFunc := refusalThenSuccess(sel, `{"error":"unavailable"}`, `{"from":"twin-b"}`, http.StatusServiceUnavailable)

	err := h.ExecuteWithRetry(context.Background(), w, req,
		[]*domain.Endpoint{namedEndpoint("twin-a"), namedEndpoint("twin-b")},
		sel, &ports.RequestStats{}, proxyFunc)

	require.NoError(t, err)
	assert.Equal(t, []string{"twin-a", "twin-b"}, sel.Tried())
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, `{"from":"twin-b"}`, w.Body.String())
}

// TestRetry_StatusSetEmpty_PassThroughByteIdentical is the default-off
// guarantee: with no statuses configured, a 429 reaches the client exactly as
// the backend answered it - same status, same bytes, single attempt, twin B
// never touched.
func TestRetry_StatusSetEmpty_PassThroughByteIdentical(t *testing.T) {
	t.Parallel()

	h := newStatusRetryHandler(t) // empty set = off
	sel := &trackingSelector{}
	w := httptest.NewRecorder()

	body := `{"error":{"type":"rate_limit_error","message":"slow down"}}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m"}`))
	proxyFunc := refusalThenSuccess(sel, body, `{"from":"twin-b"}`, http.StatusTooManyRequests)

	err := h.ExecuteWithRetry(context.Background(), w, req,
		[]*domain.Endpoint{namedEndpoint("twin-a"), namedEndpoint("twin-b")},
		sel, &ports.RequestStats{}, proxyFunc)

	require.NoError(t, err)
	assert.Equal(t, []string{"twin-a"}, sel.Tried(), "empty set must never rotate")
	assert.Equal(t, http.StatusTooManyRequests, w.Code, "the 429 must pass through byte-identically")
	assert.Equal(t, body, w.Body.String())
}

// TestRetry_StatusNotInSet_PassThrough confirms a status outside the
// configured set passes through untouched even while the feature is armed.
func TestRetry_StatusNotInSet_PassThrough(t *testing.T) {
	t.Parallel()

	h := newStatusRetryHandler(t, http.StatusTooManyRequests, http.StatusServiceUnavailable)
	sel := &trackingSelector{}
	w := httptest.NewRecorder()

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	proxyFunc := refusalThenSuccess(sel, `{"error":"model not found"}`, `{"from":"twin-b"}`, http.StatusNotFound)

	err := h.ExecuteWithRetry(context.Background(), w, req,
		[]*domain.Endpoint{namedEndpoint("twin-a"), namedEndpoint("twin-b")},
		sel, &ports.RequestStats{}, proxyFunc)

	require.NoError(t, err)
	assert.Equal(t, []string{"twin-a"}, sel.Tried(), "404 is not in the set; no rotation")
	assert.Equal(t, http.StatusNotFound, w.Code)
	assert.Equal(t, `{"error":"model not found"}`, w.Body.String())
}

// TestRetry_ResponseStartedSuppression covers "status arrives after flush =>
// no retry": the endpoint flushed bytes (stream start) before committing the
// refusal status, so the response is already visible to the client and the
// existing responseStarted contract forbids rotation - for non-idempotent
// methods via the responseStarted+IsIdempotent gate, for every method via the
// !responseStarted condition of the status gate.
func TestRetry_ResponseStartedSuppression(t *testing.T) {
	t.Parallel()

	for _, method := range []string{http.MethodPost, http.MethodGet} {
		t.Run(method, func(t *testing.T) {
			t.Parallel()

			h := newStatusRetryHandler(t, http.StatusTooManyRequests)
			sel := &trackingSelector{}
			w := httptest.NewRecorder()

			proxyFunc := func(_ context.Context, w http.ResponseWriter, _ *http.Request, _ *domain.Endpoint, _ *ports.RequestStats) error {
				n := len(sel.Tried())
				if n == 1 {
					// Stream starts: bytes are flushed to the client first.
					_, _ = w.Write([]byte("stream-start-chunk"))
					require.NoError(t, http.NewResponseController(w).Flush())
					// ...and the refusal status arrives only afterwards.
					w.WriteHeader(http.StatusTooManyRequests)
					_, _ = w.Write([]byte("late refusal"))
					return nil
				}
				t.Error("must not rotate after the response started")
				return nil
			}

			req := httptest.NewRequest(method, "/v1/chat/completions", nil)
			err := h.ExecuteWithRetry(context.Background(), w, req,
				[]*domain.Endpoint{namedEndpoint("twin-a"), namedEndpoint("twin-b")},
				sel, &ports.RequestStats{}, proxyFunc)

			require.NoError(t, err)
			assert.Equal(t, []string{"twin-a"}, sel.Tried(), "a flushed response must never rotate")
			assert.Contains(t, w.Body.String(), "stream-start-chunk", "the already-visible stream bytes must stay as delivered")
		})
	}
}

// TestRetry_NonIdempotentSuppressionPreserved pins the idempotency semantics
// of the existing gate on the new path: the composite suppression is
// responseStarted && !isIdempotent(method). A POST whose response STARTED with
// a refusal never rotates; a POST whose refusal never started rotates (the
// backend refused - no side effect was consumed), matching the connection-
// error path's treatment of an unstarted POST (TestRetry_POSTBeforeBytesWritten_DoesRetry).
func TestRetry_NonIdempotentSuppressionPreserved(t *testing.T) {
	t.Parallel()

	t.Run("started POST with refusal never rotates", func(t *testing.T) {
		t.Parallel()

		h := newStatusRetryHandler(t, http.StatusTooManyRequests)
		sel := &trackingSelector{}
		w := httptest.NewRecorder()

		proxyFunc := func(_ context.Context, w http.ResponseWriter, _ *http.Request, _ *domain.Endpoint, _ *ports.RequestStats) error {
			n := len(sel.Tried())
			if n == 1 {
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte("partial-generation"))
				require.NoError(t, http.NewResponseController(w).Flush())
				// Mid-stream the backend flips to a refusal status (some
				// engines 429 their own SSE stream when a quota trips).
				w.WriteHeader(http.StatusTooManyRequests)
				return nil
			}
			t.Error("must not rotate a started non-idempotent response")
			return nil
		}

		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m"}`))
		err := h.ExecuteWithRetry(context.Background(), w, req,
			[]*domain.Endpoint{namedEndpoint("twin-a"), namedEndpoint("twin-b")},
			sel, &ports.RequestStats{}, proxyFunc)

		require.NoError(t, err)
		assert.Equal(t, []string{"twin-a"}, sel.Tried())
		assert.Contains(t, w.Body.String(), "partial-generation")
	})

	t.Run("unstarted POST refusal rotates", func(t *testing.T) {
		t.Parallel()

		h := newStatusRetryHandler(t, http.StatusTooManyRequests)
		sel := &trackingSelector{}
		w := httptest.NewRecorder()

		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m"}`))
		proxyFunc := refusalThenSuccess(sel, `{"error":"rate limited"}`, `{"from":"twin-b"}`, http.StatusTooManyRequests)

		err := h.ExecuteWithRetry(context.Background(), w, req,
			[]*domain.Endpoint{namedEndpoint("twin-a"), namedEndpoint("twin-b")},
			sel, &ports.RequestStats{}, proxyFunc)

		require.NoError(t, err)
		assert.Equal(t, []string{"twin-a", "twin-b"}, sel.Tried())
		assert.Equal(t, http.StatusOK, w.Code)
	})
}

// TestRetry_ExhaustedCandidates_PassesStatusThrough: when the 429-er is the
// only candidate (single endpoint, and a fleet where every endpoint refuses),
// the refusal passes through honestly and the loop terminates - no rotation,
// no infinite loop, no synthesized error.
func TestRetry_ExhaustedCandidates_PassesStatusThrough(t *testing.T) {
	t.Parallel()

	t.Run("sole endpoint", func(t *testing.T) {
		t.Parallel()

		h := newStatusRetryHandler(t, http.StatusTooManyRequests, http.StatusServiceUnavailable)
		sel := &trackingSelector{}
		w := httptest.NewRecorder()

		req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		proxyFunc := refusalThenSuccess(sel, `{"error":"rate limited"}`, `unused`, http.StatusTooManyRequests)

		err := h.ExecuteWithRetry(context.Background(), w, req,
			[]*domain.Endpoint{namedEndpoint("only")},
			sel, &ports.RequestStats{}, proxyFunc)

		require.NoError(t, err, "a passed-through 429 is a completed request, not an error")
		assert.Equal(t, []string{"only"}, sel.Tried())
		assert.Equal(t, http.StatusTooManyRequests, w.Code)
		assert.Equal(t, `{"error":"rate limited"}`, w.Body.String())
	})

	t.Run("every endpoint refuses", func(t *testing.T) {
		t.Parallel()

		h := newStatusRetryHandler(t, http.StatusServiceUnavailable)
		sel := &trackingSelector{}
		w := httptest.NewRecorder()

		proxyFunc := func(_ context.Context, w http.ResponseWriter, _ *http.Request, ep *domain.Endpoint, _ *ports.RequestStats) error {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("no capacity at " + ep.Name))
			return nil
		}

		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m"}`))
		err := h.ExecuteWithRetry(context.Background(), w, req,
			[]*domain.Endpoint{namedEndpoint("twin-a"), namedEndpoint("twin-b")},
			sel, &ports.RequestStats{}, proxyFunc)

		require.NoError(t, err)
		// Bounded by the endpoint count: A rotates to B, B is sole remaining
		// candidate so its 503 passes through. The client sees exactly one
		// response - the last refusal - and no third attempt happens.
		assert.Equal(t, []string{"twin-a", "twin-b"}, sel.Tried())
		assert.Equal(t, http.StatusServiceUnavailable, w.Code)
		assert.Equal(t, "no capacity at twin-b", w.Body.String())
	})
}

// TestStatusGateWriter_EmptySetPassThrough pins the writer-level guarantee
// behind "empty default => byte-identical": with an empty set the gate writes
// straight through (status, body, flush ordering) and never reports a
// deferred response.
func TestStatusGateWriter_EmptySetPassThrough(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	g := newStatusGate(rec, nil)

	g.WriteHeader(http.StatusTooManyRequests)
	_, _ = g.Write([]byte("direct"))
	assert.Equal(t, http.StatusTooManyRequests, rec.Code, "status must commit immediately with the set empty")
	assert.Equal(t, "direct", rec.Body.String())
	_, held := g.deferred()
	assert.False(t, held, "nothing is ever held with the set empty")
}

// commitTrackingWriter counts WriteHeader calls. httptest.ResponseRecorder
// pre-sets Code=200, so "nothing was committed" is only observable through
// the call count, never through the code value.
type commitTrackingWriter struct {
	*httptest.ResponseRecorder
	headerWrites int
}

func (w *commitTrackingWriter) WriteHeader(code int) {
	w.headerWrites++
	w.ResponseRecorder.WriteHeader(code)
}

// TestStatusGateWriter_HoldsAndDiscards pins the gate mechanics the rotation
// path relies on: in-set status is held invisible, discard() forwards nothing,
// commit() forwards status+body, a flush commits (stream start).
func TestStatusGateWriter_HoldsAndDiscards(t *testing.T) {
	t.Parallel()

	t.Run("held then discarded forwards nothing", func(t *testing.T) {
		t.Parallel()
		rec := &commitTrackingWriter{ResponseRecorder: httptest.NewRecorder()}
		g := newStatusGate(rec, map[int]struct{}{429: {}})
		g.WriteHeader(429)
		_, _ = g.Write([]byte("refusal"))
		code, held := g.deferred()
		assert.True(t, held)
		assert.Equal(t, 429, code)
		assert.Equal(t, 0, rec.headerWrites, "a held response must not have touched the wrapped writer")
		assert.Empty(t, rec.Body.String())
		g.discard()
		_, held = g.deferred()
		assert.False(t, held)
		assert.Equal(t, 0, rec.headerWrites, "a discarded response is never written to the client")
		assert.Empty(t, rec.Body.String())
	})

	t.Run("commit forwards status and body", func(t *testing.T) {
		t.Parallel()
		rec := &commitTrackingWriter{ResponseRecorder: httptest.NewRecorder()}
		g := newStatusGate(rec, map[int]struct{}{503: {}})
		g.WriteHeader(503)
		_, _ = g.Write([]byte("no capacity"))
		assert.Equal(t, 0, rec.headerWrites, "held: not committed yet")
		g.commit()
		assert.Equal(t, 1, rec.headerWrites)
		assert.Equal(t, 503, rec.Code)
		assert.Equal(t, "no capacity", rec.Body.String())
	})

	t.Run("flush commits the held response", func(t *testing.T) {
		t.Parallel()
		rec := &commitTrackingWriter{ResponseRecorder: httptest.NewRecorder()}
		g := newStatusGate(rec, map[int]struct{}{429: {}})
		g.WriteHeader(429)
		_, _ = g.Write([]byte("streamed refusal"))
		require.NoError(t, http.NewResponseController(g).Flush())
		_, held := g.deferred()
		assert.False(t, held, "a flush commits the response: streaming started, rotation is over")
		assert.Equal(t, 429, rec.Code)
		assert.Equal(t, "streamed refusal", rec.Body.String())
	})
}
