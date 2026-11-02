package health

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/thushan/olla/internal/core/domain"
)

// TestCheck_HalfOpenTokenNotShadowedByLoopCounter pins the contract that
// Check passes the IsOpen probe token (not the retry loop counter) to
// RecordSuccess/RecordFailure.
//
// Scenario: half-open probe admitted (token T, non-zero). A concurrent
// stale-handover changes lastAttempt. The probe completes. With the correct
// code, RecordSuccess(url, T) sees lastAttempt != T and is a no-op (the
// handover's slot is protected). If the loop var shadowed the outer attempt
// and RecordSuccess received 0, the guard is bypassed and the breaker
// closes — clobbering the handover's state.
func TestCheck_HalfOpenTokenNotShadowedByLoopCounter(t *testing.T) {
	probeStarted := make(chan struct{})
	releaseProbe := make(chan struct{})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(probeStarted)
		<-releaseProbe
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cb := NewCircuitBreaker()
	cb.loadOrCreateState(srv.URL)

	// Trip the breaker to open state.
	for range DefaultCircuitBreakerThreshold {
		cb.RecordFailure(srv.URL, 0)
	}
	// Backdate lastFailure so recovery timeout has elapsed → next IsOpen
	// transitions to half-open and admits one probe with a non-zero token.
	state, _ := cb.endpoints.Load(srv.URL)
	state.SetLastFailureNanos(time.Now().Add(-DefaultCircuitBreakerTimeout - time.Second).UnixNano())

	ep := &domain.Endpoint{
		Name:                 "test",
		URL:                  mustParse(srv.URL),
		HealthCheckURL:       mustParse(srv.URL),
		URLString:            srv.URL,
		HealthCheckURLString: srv.URL,
		CheckTimeout:         5 * time.Second,
	}

	hc := NewHealthClient(srv.Client(), cb)

	done := make(chan struct{})
	go func() {
		_, _ = hc.Check(context.Background(), ep)
		close(done)
	}()

	<-probeStarted // the probe is in-flight; IsOpen has returned (false, T)

	// Simulate a concurrent stale-handover: a second caller's IsOpen CAS'd
	// lastAttempt from T to a new token. The original probe is now superseded.
	fakeHandoverToken := time.Now().Add(time.Hour).UnixNano()
	state.SetLastAttemptNanos(fakeHandoverToken)

	releaseProbe <- struct{}{} // let the original probe complete
	<-done

	// With the fix (outer attempt token forwarded): RecordSuccess(url, T) →
	// T != lastAttempt (fakeHandoverToken) → guard rejects → breaker stays
	// in half-open (Tripped). The handover's slot is protected.
	//
	// With the bug (loop counter 0 forwarded): RecordSuccess(url, 0) →
	// guard skipped (attempt==0) → breaker closes, lastAttempt reset to 0.
	if !state.Tripped() {
		t.Fatal("RecordSuccess bypassed the ABA guard: the superseded probe closed the breaker. " +
			"Root cause: the for-loop var 'attempt' (int, 0-based) shadows the outer attempt " +
			"(int64, IsOpen token); RecordSuccess receives 0 which skips the guard.")
	}
}

func mustParse(raw string) *url.URL {
	u, err := url.Parse(raw)
	if err != nil {
		panic(err)
	}
	return u
}
