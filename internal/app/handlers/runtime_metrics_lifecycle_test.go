package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/thushan/olla/internal/core/domain"
)

// testRuntimeMetricsInterval is a sub-millisecond per-endpoint interval so a
// test can issue back-to-back Refresh calls without tripping the sampler's
// "refresh at most once per interval" gate (default 10s).
const testRuntimeMetricsInterval = time.Nanosecond

// TestRuntimeMetricsSamplerStopsOnContextCancel verifies that cancelling the
// sampler's context stops the sampler: the in-flight (blocked) HTTP scrape is
// aborted and the sampler goroutine exits, so nothing survives shutdown.
func TestRuntimeMetricsSamplerStopsOnContextCancel(t *testing.T) {
	// The backend blocks until its request context is done, so a scrape only
	// finishes when the sampler's context is cancelled. This lets us prove the
	// blocked scrape is aborted on shutdown rather than left hanging.
	requestAborted := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
		close(requestAborted)
	}))
	defer server.Close()

	ep := &domain.Endpoint{Name: "vllm", URLString: server.URL, Type: "vllm", Status: domain.StatusHealthy, RuntimeMetricsEnabled: true, RuntimeMetricsInterval: testRuntimeMetricsInterval}
	s := newEndpointRuntimeMetricsSampler(&mockStatusEndpointRepository{endpoints: []*domain.Endpoint{ep}}, nil)

	ctx, cancel := context.WithCancel(context.Background())
	baseline := runtime.NumGoroutine()
	s.Start(ctx)

	// Wait until the sampler has an in-flight scrape against the blocking server.
	select {
	case <-requestAborted:
		t.Fatal("scrape aborted before we cancelled; test setup race")
	case <-time.After(300 * time.Millisecond):
	}

	cancel()

	// The blocked HTTP scrape must be aborted by the cancellation.
	select {
	case <-requestAborted:
	case <-time.After(2 * time.Second):
		t.Fatal("blocked HTTP scrape did not abort on context cancel")
	}

	// The sampler goroutine must exit; no goroutine may leak past baseline.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= baseline {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := runtime.NumGoroutine(); got > baseline {
		t.Fatalf("goroutine leak after cancel: baseline=%d got=%d", baseline, got)
	}
}

// TestRuntimeMetricsSamplerConcurrentRefreshDoesNotRace verifies that many
// concurrent Refresh calls on the same endpoint do not race. Run under -race.
func TestRuntimeMetricsSamplerConcurrentRefreshDoesNotRace(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("llamacpp:tokens_predicted_total 10\nllamacpp:tokens_predicted_seconds_total 1\n"))
	}))
	defer server.Close()

	ep := &domain.Endpoint{Name: "halogen", URLString: server.URL, Type: "halogen", Status: domain.StatusHealthy, RuntimeMetricsEnabled: true, RuntimeMetricsInterval: testRuntimeMetricsInterval}
	s := newEndpointRuntimeMetricsSampler(&mockStatusEndpointRepository{endpoints: []*domain.Endpoint{ep}}, nil)

	ctx := t.Context()
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.Refresh(ctx)
		}()
	}
	wg.Wait()

	// A snapshot read after concurrent refreshes must not race either.
	_ = s.Snapshot(ep)
}

// TestRuntimeMetricsRefreshFailedScrapeMarksLastSnapshotStale verifies that a
// failed scrape (transport-level failure) marks the last good snapshot stale.
func TestRuntimeMetricsRefreshFailedScrapeMarksLastSnapshotStale(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("llamacpp:tokens_predicted_total 5\nllamacpp:tokens_predicted_seconds_total 1\n"))
	}))

	ep := &domain.Endpoint{Name: "halogen", URLString: server.URL, Type: "halogen", Status: domain.StatusHealthy, RuntimeMetricsEnabled: true, RuntimeMetricsInterval: testRuntimeMetricsInterval}
	s := newEndpointRuntimeMetricsSampler(&mockStatusEndpointRepository{endpoints: []*domain.Endpoint{ep}}, nil)

	ctx := t.Context()
	s.Refresh(ctx)
	got := s.Snapshot(ep)
	require.NotNil(t, got)
	assert.False(t, got.Stale)

	// Backend goes away -> the next scrape fails at the transport level.
	server.Close()
	s.Refresh(ctx)
	got = s.Snapshot(ep)
	require.NotNil(t, got)
	assert.True(t, got.Stale)
}
