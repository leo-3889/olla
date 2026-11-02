package stats

import (
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/puzpuzpuz/xsync/v4"
)

// TestCollector_CleanupKeepsNewestSurvivors proves the TTL-sweep overflow
// eviction keeps exactly MaxTrackedEndpoints NEWEST survivors when MORE than
// MaxTrackedEndpoints endpoints survive the TTL cut.
//
// Regression: the old code computed `remove := len(ages) - MaxTrackedEndpoints
// + 100`. The +100 made `remove` exceed the survivor count, so the bounded
// loop (i < remove && i < len(ages)) deleted ALL survivors — wiping healthy
// endpoints. The correct value is `len(ages) - MaxTrackedEndpoints`.
func TestCollector_CleanupKeepsNewestSurvivors(t *testing.T) {
	c := NewCollector(createTestLogger())

	const survivors = MaxTrackedEndpoints + 10 // more than the cap survive TTL
	now := int64(1_000_000_000_000_000_000)

	for i := range survivors {
		url := fmt.Sprintf("http://host-%03d:11434", i)
		// lastUsed strictly increasing: host-009 is newest, host-000 oldest.
		// All far above the TTL cutoff (now - EndpointTTL), so all survive TTL.
		c.endpoints.Store(url, &endpointData{
			url:                url,
			name:               url,
			lastUsed:           now - int64(survivors-i)*1000,
			minLatency:         -1,
			totalRequests:      xsync.NewCounter(),
			successfulRequests: xsync.NewCounter(),
			failedRequests:     xsync.NewCounter(),
			totalBytes:         xsync.NewCounter(),
			totalLatency:       xsync.NewCounter(),
		})
	}

	c.cleanup(now)

	var remaining int
	c.endpoints.Range(func(string, *endpointData) bool { remaining++; return true })

	if remaining != MaxTrackedEndpoints {
		t.Fatalf("expected %d survivors after TTL-sweep overflow eviction, got %d (the +100 bug wipes all survivors to 0)",
			MaxTrackedEndpoints, remaining)
	}

	// The NEWEST endpoint must survive; the oldest must be evicted.
	if _, ok := c.endpoints.Load(fmt.Sprintf("http://host-%03d:11434", survivors-1)); !ok {
		t.Fatal("newest surviving endpoint was evicted")
	}
	if _, ok := c.endpoints.Load("http://host-000:11434"); ok {
		t.Fatal("oldest surviving endpoint should have been evicted (overflow)")
	}
}

// compile-time guard: endpointData.lastUsed is read atomically in cleanup.
var _ = atomic.LoadInt64
