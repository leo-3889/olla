package handlers

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFluxClientQueryParsesStreamedTables(t *testing.T) {
	var gotAuth, gotCT, gotQueryOrg, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotCT = r.Header.Get("Content-Type")
		gotQueryOrg = r.URL.Query().Get("org")
		buf := make([]byte, 4096)
		n, _ := r.Body.Read(buf)
		gotBody = string(buf[:n])
		w.Header().Set("Content-Type", "application/vnd.influxdb")
		// Annotated CSV exactly as the live influxd streams it: leading
		// empty annotation cell, header row per table, "_result" default.
		fmt.Fprint(w, ",result,table,_field,_value,endpoint\n")
		fmt.Fprint(w, ",_result,0,queue_depth,3,radiance-qfn\n")
		fmt.Fprint(w, ",_result,0,requests_running,2,radiance-qfn\n")
		fmt.Fprint(w, ",result,table,_field,_value,endpoint\n")
		fmt.Fprint(w, ",_result,1,queue_depth,9,halogen-395\n")
	}))
	defer srv.Close()

	c := newFluxClient(srv.URL, "org name", "tok123")
	rows, err := c.query(context.Background(), `from(bucket:"b")|>range(start:-5m)`)
	require.NoError(t, err)
	require.Len(t, rows, 3)
	assert.Equal(t, "Token tok123", gotAuth)
	assert.Equal(t, "application/json", gotCT)
	assert.Equal(t, "org name", gotQueryOrg, "org must travel as a URL query parameter")
	assert.Contains(t, gotBody, `"format":"csv"`, "the wire format the server actually speaks")
	assert.Contains(t, gotBody, `"header":true`)
	assert.Contains(t, gotBody, `"query":"from(bucket:\"b\")`)
	assert.Equal(t, "radiance-qfn", rows[0]["endpoint"])
	assert.Equal(t, "3", rows[0]["_value"], "CSV values are strings; call sites coerce")
	assert.Equal(t, "halogen-395", rows[2]["endpoint"])
}

func TestFluxClientQueryErrorTable(t *testing.T) {
	// Flux syntax errors arrive as a 200 + an "error" result table.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, ",error,reference\n")
		fmt.Fprint(w, `,"error @1:168: found unexpected argument v",42`+"\n")
	}))
	defer srv.Close()
	c := newFluxClient(srv.URL, "org", "tok")
	_, err := c.query(context.Background(), "bad")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "found unexpected argument v")
}

func TestFluxClientQueryErrorExcerpt(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "provide orgID or org", http.StatusBadRequest)
	}))
	defer srv.Close()
	c := newFluxClient(srv.URL, "org", "tok")
	_, err := c.query(context.Background(), "x")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "400")
	assert.Contains(t, err.Error(), "provide orgID or org")
}

func TestFluxClientQueryTimeout(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	}))
	defer srv.Close()
	defer close(release)
	c := newFluxClient(srv.URL, "org", "tok")
	c.client.Timeout = 50 * time.Millisecond
	_, err := c.query(context.Background(), "x")
	require.Error(t, err)
	assert.True(t, strings.Contains(err.Error(), "timeout") || strings.Contains(err.Error(), "deadline"), "got %v", err)
}

// --- fluxResultCache: single-flight + TTL, keyed by the exact Flux text.

type countingFluxQuerier struct {
	mu    sync.Mutex
	calls int
	rows  []map[string]any
	err   error
	// block: when non-nil, the querier parks inside query() until it
	// closes (or the caller context dies); entered signals each call
	// START so tests can time joiners against a provably in-flight leader.
	block   <-chan struct{}
	entered chan struct{}
}

func (q *countingFluxQuerier) query(ctx context.Context, _ string) ([]map[string]any, error) {
	q.mu.Lock()
	q.calls++
	q.mu.Unlock()
	if q.entered != nil {
		q.entered <- struct{}{}
	}
	if q.block != nil {
		select {
		case <-q.block:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return q.rows, q.err
}

func (q *countingFluxQuerier) count() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.calls
}

func cacheRows() []map[string]any {
	return []map[string]any{{"_value": "1"}}
}

// The plan's contract: identical Flux issued by two callers in the same
// cycle hits InfluxDB ONCE.
func TestFluxResultCacheDedupsConcurrentIdenticalFlux(t *testing.T) {
	release := make(chan struct{})
	inner := &countingFluxQuerier{rows: cacheRows(), block: release, entered: make(chan struct{}, 4)}
	c := newFluxResultCache(inner, time.Minute)

	results := make(chan error, 2)
	var wg sync.WaitGroup
	launch := func() {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rows, err := c.query(context.Background(), "same flux")
			if err == nil && len(rows) != 1 {
				err = fmt.Errorf("rows = %d, want 1", len(rows))
			}
			results <- err
		}()
	}
	launch()
	<-inner.entered // leader provably inside the inner querier
	launch()        // joins the in-flight call
	close(release)
	wg.Wait()
	close(results)
	for err := range results {
		require.NoError(t, err)
	}
	assert.Equal(t, 1, inner.count(), "two callers, ONE influxd round trip")
}

// Within the TTL the identical text is served from the cache; past it the
// cycle re-queries (observeTTL semantics, keyed by Flux text).
func TestFluxResultCacheServesIdenticalFluxWithinTTL(t *testing.T) {
	inner := &countingFluxQuerier{rows: cacheRows()}
	clock := time.Unix(1700000000, 0)
	c := newFluxResultCache(inner, 10*time.Second)
	c.now = func() time.Time { return clock }

	_, err := c.query(context.Background(), "q")
	require.NoError(t, err)
	clock = clock.Add(5 * time.Second)
	rows, err := c.query(context.Background(), "q")
	require.NoError(t, err)
	assert.Len(t, rows, 1)
	assert.Equal(t, 1, inner.count(), "the second call is served from the cache")

	clock = clock.Add(6 * time.Second) // age 11s > TTL
	_, err = c.query(context.Background(), "q")
	require.NoError(t, err)
	assert.Equal(t, 2, inner.count(), "past the TTL the text re-queries")

	_, err = c.query(context.Background(), "other flux")
	require.NoError(t, err)
	assert.Equal(t, 3, inner.count(), "the key is the EXACT Flux text")
}

// Failures are never cached: the next caller retries immediately (backoff
// lives in the leg owners - noteLeg, the token-day negative cache - not in
// hidden cache state).
func TestFluxResultCacheNeverCachesFailures(t *testing.T) {
	inner := &countingFluxQuerier{err: fmt.Errorf("influx down")}
	clock := time.Unix(1700000000, 0)
	c := newFluxResultCache(inner, time.Minute)
	c.now = func() time.Time { return clock }

	_, err := c.query(context.Background(), "q")
	require.Error(t, err)
	_, err = c.query(context.Background(), "q")
	require.Error(t, err)
	assert.Equal(t, 2, inner.count(), "a failed query is not memoized")
}

// A cancelled INITIATOR must not poison the cache: the leader completes
// (its context is detached with the deadline preserved), the result is
// returned, and the NEXT caller re-queries - a cancelled read may be
// truncated, and truncated rows served for the whole TTL would fabricate
// samples.
func TestFluxResultCacheNoCacheOnCancelledInitiator(t *testing.T) {
	release := make(chan struct{})
	inner := &countingFluxQuerier{rows: cacheRows(), block: release, entered: make(chan struct{}, 1)}
	c := newFluxResultCache(inner, time.Minute)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = c.query(ctx, "q")
	}()
	<-inner.entered
	cancel()
	close(release)
	<-done

	rows, err := c.query(context.Background(), "q")
	require.NoError(t, err)
	assert.Len(t, rows, 1)
	assert.Equal(t, 2, inner.count(), "the cancelled-initiator result was NOT cached")
}

// A joiner that walks away (its own poll aborted) gets its own ctx error,
// the leader finishes untouched, and the successful result IS cached for
// everyone else.
func TestFluxResultCacheJoinerCancellationKeepsLeader(t *testing.T) {
	release := make(chan struct{})
	inner := &countingFluxQuerier{rows: cacheRows(), block: release, entered: make(chan struct{}, 4)}
	c := newFluxResultCache(inner, time.Minute)

	leaderDone := make(chan struct{})
	go func() {
		defer close(leaderDone)
		_, _ = c.query(context.Background(), "q")
	}()
	<-inner.entered

	jctx, jcancel := context.WithCancel(context.Background())
	joinerErr := make(chan error, 1)
	go func() {
		_, err := c.query(jctx, "q")
		joinerErr <- err
	}()
	time.Sleep(50 * time.Millisecond) // let the joiner attach
	jcancel()
	require.ErrorIs(t, <-joinerErr, context.Canceled)

	close(release)
	<-leaderDone

	rows, err := c.query(context.Background(), "q")
	require.NoError(t, err)
	assert.Len(t, rows, 1, "the leader result served the fresh caller from cache")
	assert.Equal(t, 1, inner.count(), "one influxd round trip for all three callers")
}
