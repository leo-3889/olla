package balancer

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/thushan/olla/internal/core/domain"
)

// The tier tie-break is the only thing that spreads traffic across equal-priority
// twins. Weighted-random (the previous behaviour) clumps: with two healthy nodes
// at prio 100 a burst sits wherever the coin flips land, and sticky sessions
// then lock the clump in for the affinity TTL. These tests pin the rotation
// contract that replaced it.

func TestPrioritySelector_EqualPriorityRotationIsExact(t *testing.T) {
	selector := NewPrioritySelector(NewTestStatsCollector())
	ctx := context.Background()

	endpoints := []*domain.Endpoint{
		createPriorityEndpoint("a", 11434, domain.StatusHealthy, 100),
		createPriorityEndpoint("b", 11435, domain.StatusHealthy, 100),
	}

	sequence := make([]string, 6)
	for i := range sequence {
		endpoint, err := selector.Select(ctx, endpoints)
		require.NoError(t, err)
		sequence[i] = endpoint.Name
	}

	assert.Equal(t, []string{"a", "b", "a", "b", "a", "b"}, sequence,
		"two equal-weight twins must alternate, not coin-flip")

	counts := map[string]int{}
	for range 1000 {
		endpoint, err := selector.Select(ctx, endpoints)
		require.NoError(t, err)
		counts[endpoint.Name]++
	}

	assert.Equal(t, 500, counts["a"], "1000 selects over two twins must split 500/500")
	assert.Equal(t, 500, counts["b"], "1000 selects over two twins must split 500/500")
}

func TestPrioritySelector_RotationOrderIndependentOfSliceOrder(t *testing.T) {
	selector := NewPrioritySelector(NewTestStatsCollector())
	ctx := context.Background()

	a := createPriorityEndpoint("a", 11434, domain.StatusHealthy, 100)
	b := createPriorityEndpoint("b", 11435, domain.StatusHealthy, 100)

	// The caller's slice order is registration order and is not stable across
	// discovery refreshes; the rotation must not depend on it.
	for range 50 {
		one, err := selector.Select(ctx, []*domain.Endpoint{a, b})
		require.NoError(t, err)
		two, err := selector.Select(ctx, []*domain.Endpoint{b, a})
		require.NoError(t, err)
		assert.NotEqual(t, one.Name, two.Name, "consecutive selects must land on different twins")
	}
}

func TestPrioritySelector_RecoveredEndpointPaysBackItsDeficit(t *testing.T) {
	selector := NewPrioritySelector(NewTestStatsCollector())
	ctx := context.Background()

	a := createPriorityEndpoint("a", 11434, domain.StatusHealthy, 100)
	b := createPriorityEndpoint("b", 11435, domain.StatusHealthy, 100)

	// a serves the first four requests, then goes offline: b absorbs everything.
	for range 4 {
		_, err := selector.Select(ctx, []*domain.Endpoint{a, b})
		require.NoError(t, err)
	}
	a.Status = domain.StatusOffline
	for range 6 {
		endpoint, err := selector.Select(ctx, []*domain.Endpoint{a, b})
		require.NoError(t, err)
		require.Equal(t, "b", endpoint.Name, "only the healthy endpoint is routable")
	}

	// Back healthy: rotation resumes and evens out, it does not shadow a
	// permanently-favouring credit imbalance.
	a.Status = domain.StatusHealthy
	counts := map[string]int{}
	for range 1000 {
		endpoint, err := selector.Select(ctx, []*domain.Endpoint{a, b})
		require.NoError(t, err)
		counts[endpoint.Name]++
	}

	assert.InDelta(t, 500, counts["a"], 2, "recovered endpoint must return to an even share")
	assert.InDelta(t, 500, counts["b"], 2, "surviving endpoint must give the share back")
}

func TestPrioritySelector_WeightedRotationIsProportional(t *testing.T) {
	selector := NewPrioritySelector(NewTestStatsCollector())
	ctx := context.Background()

	endpoints := []*domain.Endpoint{
		createPriorityEndpoint("healthy", 11434, domain.StatusHealthy, 100), // 1.0
		createPriorityEndpoint("busy", 11435, domain.StatusBusy, 100),       // 0.3
		createPriorityEndpoint("warming", 11436, domain.StatusWarming, 100), // 0.1
	}

	counts := map[string]int{}
	const total = 1400
	for range total {
		endpoint, err := selector.Select(ctx, endpoints)
		require.NoError(t, err)
		counts[endpoint.Name]++
	}

	// SWRR over weights 10/3/1 has a 14-request period, so 1400 requests
	// distribute 1000/300/100 exactly. Float drift is tolerated by 1.
	assert.InDelta(t, 1000, counts["healthy"], 1)
	assert.InDelta(t, 300, counts["busy"], 1)
	assert.InDelta(t, 100, counts["warming"], 1)

	// Smoothness: the light node must never be served twice in a row while a
	// heavier candidate is waiting (a random draw produces runs, a rotation
	// does not).
	sequence := make([]string, 200)
	for i := range sequence {
		endpoint, err := selector.Select(ctx, endpoints)
		require.NoError(t, err)
		sequence[i] = endpoint.Name
	}
	for i := 1; i < len(sequence); i++ {
		if sequence[i] == "warming" {
			assert.NotEqual(t, "warming", sequence[i-1], "warming endpoint must not be queued back to back")
		}
	}
}

func TestPrioritySelector_ConcurrentRotationIsFair(t *testing.T) {
	selector := NewPrioritySelector(NewTestStatsCollector())
	ctx := context.Background()

	a := createPriorityEndpoint("a", 11434, domain.StatusHealthy, 100)
	b := createPriorityEndpoint("b", 11435, domain.StatusHealthy, 100)

	// The SWRR credit map is shared state on the request hot path. Two
	// goroutines hammer the same selector: the -race detector owns the data
	// race question, this test owns the arithmetic - the rotation must stay
	// even when selection interleaves.
	const per = 500
	counts := map[string]int{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			local := map[string]int{}
			for range per {
				endpoint, err := selector.Select(ctx, []*domain.Endpoint{a, b})
				if err != nil {
					return
				}
				local[endpoint.Name]++
			}
			mu.Lock()
			for k, v := range local {
				counts[k] += v
			}
			mu.Unlock()
		}()
	}
	wg.Wait()

	assert.Equal(t, per*2, counts["a"]+counts["b"], "every select must land on a twin")
	delta := counts["a"] - counts["b"]
	if delta < 0 {
		delta = -delta
	}
	assert.LessOrEqual(t, delta, 2, "interleaved rotation must stay even")
}

// The candidate slice is built from a map by the callers, so its order is not
// stable. A positional tie-break made the first request after a fleet change -
// every credit equal - a coin flip between the tied endpoints.
func TestPrioritySelector_EqualCreditTieBreakIsOrderIndependent(t *testing.T) {
	t.Parallel()

	low := createPriorityEndpoint("low", 11434, domain.StatusHealthy, 100)
	high := createPriorityEndpoint("high", 11900, domain.StatusHealthy, 100)
	require.Less(t, low.URLString, high.URLString, "fixture: URL order defines the tie-break")
	ctx := context.Background()

	forward := NewPrioritySelector(NewTestStatsCollector())
	reverse := NewPrioritySelector(NewTestStatsCollector())

	firstForward, err := forward.Select(ctx, []*domain.Endpoint{low, high})
	require.NoError(t, err)
	firstReverse, err := reverse.Select(ctx, []*domain.Endpoint{high, low})
	require.NoError(t, err)

	assert.Equal(t, firstForward.URLString, firstReverse.URLString,
		"equal credits must pick the same endpoint regardless of candidate order")
	assert.Equal(t, low.URLString, firstForward.URLString, "tie-break is the lowest URLString")

	// Rotation stays exact from both orders.
	seqForward := make([]string, 4)
	seqReverse := make([]string, 4)
	for i := range seqForward {
		ep, err := forward.Select(ctx, []*domain.Endpoint{low, high})
		require.NoError(t, err)
		seqForward[i] = ep.Name
		ep, err = reverse.Select(ctx, []*domain.Endpoint{high, low})
		require.NoError(t, err)
		seqReverse[i] = ep.Name
	}
	assert.Equal(t, seqForward, seqReverse)
}

// Renames and retirements leave credit entries that are never read again; the
// drift guard bounds them without touching live endpoints' rotation state.
func TestPrioritySelector_CreditMapDriftIsBounded(t *testing.T) {
	t.Parallel()

	selector := NewPrioritySelector(NewTestStatsCollector())
	ctx := context.Background()
	live := []*domain.Endpoint{
		createPriorityEndpoint("a", 11434, domain.StatusHealthy, 100),
		createPriorityEndpoint("b", 11435, domain.StatusHealthy, 100),
	}

	for i := range 40 {
		selector.currentWeight[fmt.Sprintf("http://retired-%d:11434", i)] = 1.0
	}
	_, err := selector.Select(ctx, live)
	require.NoError(t, err)

	selector.swrrMu.Lock()
	size := len(selector.currentWeight)
	selector.swrrMu.Unlock()
	assert.LessOrEqual(t, size, 4*len(live)+8, "stale credits must be pruned once the map drifts")

	// Live endpoints keep rotating exactly (their credits survived the prune).
	_, err = selector.Select(ctx, live)
	require.NoError(t, err)
	selector.swrrMu.Lock()
	_, aOK := selector.currentWeight[live[0].URLString]
	_, bOK := selector.currentWeight[live[1].URLString]
	selector.swrrMu.Unlock()
	assert.True(t, aOK && bOK, "live endpoints keep their rotation state")
}
