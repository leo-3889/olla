package balancer

import (
	"context"
	"testing"
	"time"

	"github.com/thushan/olla/internal/config"
	"github.com/thushan/olla/internal/core/domain"
)

// stubProber returns canned readings and counts the rounds it fired. The
// round count matters: one Select = at most one probe round, and a
// single-endpoint tier must fire none (no choice, no cost).
type stubProber struct {
	calls int
	loads map[string]domain.Load

	// rounds records the endpoint set of every round, so a test can assert
	// WHAT was probed (the top tier only), not just how often.
	rounds [][]string
}

func (p *stubProber) Probe(_ context.Context, endpoints []*domain.Endpoint) map[string]domain.Load {
	p.calls++
	urls := make([]string, 0, len(endpoints))
	for _, ep := range endpoints {
		urls = append(urls, ep.URLString)
	}
	p.rounds = append(p.rounds, urls)
	return p.loads
}

func knownLoad(active int) domain.Load {
	return domain.Load{Active: active, Source: domain.LoadSourceHealth, Confident: true, SampledAt: time.Now()}
}

func TestLeastLoadSelector_RegisteredInFactory(t *testing.T) {
	factory := NewFactory(NewTestStatsCollector())

	selector, err := factory.Create(DefaultBalancerLeastLoad)
	if err != nil {
		t.Fatalf("factory must register %q: %v", DefaultBalancerLeastLoad, err)
	}
	if selector.Name() != DefaultBalancerLeastLoad {
		t.Errorf("Name() = %q, want %q", selector.Name(), DefaultBalancerLeastLoad)
	}

	found := false
	for _, s := range factory.GetAvailableStrategies() {
		if s == DefaultBalancerLeastLoad {
			found = true
		}
	}
	if !found {
		t.Error("GetAvailableStrategies must list least-load alongside the existing balancers")
	}

	for _, name := range []string{DefaultBalancerPriority, DefaultBalancerRoundRobin, DefaultBalancerLeastConnections} {
		if _, err := factory.Create(name); err != nil {
			t.Errorf("existing balancer %q broke: %v", name, err)
		}
	}
}

// TestLeastLoadSelector_ZeroSignalMatchesPriority is the byte-identical gate:
// with no prober wired, least-load must reproduce the exact smooth weighted
// round-robin of the priority balancer.
func TestLeastLoadSelector_ZeroSignalMatchesPriority(t *testing.T) {
	ctx := context.Background()
	endpoints := []*domain.Endpoint{
		createPriorityEndpoint("a", 11434, domain.StatusHealthy, 100),
		createPriorityEndpoint("b", 11435, domain.StatusHealthy, 100),
	}

	priority := NewPrioritySelector(NewTestStatsCollector())
	leastLoad := NewLeastLoadSelector(NewTestStatsCollector(), nil)

	for i := range 6 {
		want, err := priority.Select(ctx, endpoints)
		if err != nil {
			t.Fatalf("priority select #%d: %v", i, err)
		}
		got, err := leastLoad.Select(ctx, endpoints)
		if err != nil {
			t.Fatalf("least-load select #%d: %v", i, err)
		}
		if got.URLString != want.URLString {
			t.Fatalf("select #%d: least-load chose %s, priority chose %s - the knob-off path must be identical",
				i, got.URLString, want.URLString)
		}
	}
}

// TestLeastLoadSelector_PicksLeastLoaded is the headline requirement: the
// emptiest node in the tier wins, regardless of rotation state.
func TestLeastLoadSelector_PicksLeastLoaded(t *testing.T) {
	a := createPriorityEndpoint("a", 11434, domain.StatusHealthy, 100)
	b := createPriorityEndpoint("b", 11435, domain.StatusHealthy, 100)
	prober := &stubProber{loads: map[string]domain.Load{
		a.URLString: knownLoad(4),
		b.URLString: knownLoad(1),
	}}
	selector := NewLeastLoadSelector(NewTestStatsCollector(), prober)

	for i := range 3 {
		got, err := selector.Select(context.Background(), []*domain.Endpoint{a, b})
		if err != nil {
			t.Fatalf("select #%d: %v", i, err)
		}
		if got.URLString != b.URLString {
			t.Fatalf("select #%d: node with 1 active must beat the node with 4, picked %s", i, got.URLString)
		}
		// b's reservation grows; 1+2 stays below 4 for these three rounds.
	}
	if prober.calls != 3 {
		t.Fatalf("exactly one probe round per Select expected, got %d", prober.calls)
	}
}

// TestLeastLoadSelector_AntiClumpReservations is the burst gate: with a FROZEN
// probe answer (the engine gauges cannot move inside one burst), 8 selections
// must still spread because each selection reserves the node it handed out.
func TestLeastLoadSelector_AntiClumpReservations(t *testing.T) {
	a := createPriorityEndpoint("a", 11434, domain.StatusHealthy, 100)
	b := createPriorityEndpoint("b", 11435, domain.StatusHealthy, 100)
	prober := &stubProber{loads: map[string]domain.Load{
		a.URLString: knownLoad(0),
		b.URLString: knownLoad(0),
	}}
	selector := NewLeastLoadSelector(NewTestStatsCollector(), prober)

	counts := map[string]int{}
	for range 8 {
		got, err := selector.Select(context.Background(), []*domain.Endpoint{a, b})
		if err != nil {
			t.Fatal(err)
		}
		counts[got.URLString]++
	}
	if counts[a.URLString] != 4 || counts[b.URLString] != 4 {
		t.Fatalf("8 selects on identical load must alternate 4/4, got %v", counts)
	}

	// Reservations expire on their own; the attempt-paired decrements the proxy
	// fires around EVERY attempt (sticky hits and solo-tier skips included)
	// must not erode them, or the burst protection dies exactly during the
	// fan-out it exists for.
	for range 8 {
		selector.DecrementConnections(a)
		selector.DecrementConnections(b)
	}
	selector.resMu.Lock()
	liveA := selector.effectiveReservedLocked(a.URLString, time.Now())
	liveB := selector.effectiveReservedLocked(b.URLString, time.Now())
	selector.resMu.Unlock()
	if liveA != 4 || liveB != 4 {
		t.Fatalf("8 decrements (4 per node) must leave the 4/4 live reservations intact, got %s=%d %s=%d",
			a.URLString, liveA, b.URLString, liveB)
	}
}

// TestLeastLoadSelector_ReservationsExpireByThemselves pins the release
// mechanism: nobody has to tell a reservation it is over. A reservation only
// covers the window before the engine's own admitted gauge counts the request,
// so it must vanish on its own - and the entry must be pruned, not left behind
// as dead weight for a URL that never comes back.
func TestLeastLoadSelector_ReservationsExpireByThemselves(t *testing.T) {
	defer func(ttl time.Duration) { reservationTTL = ttl }(reservationTTL)
	reservationTTL = 10 * time.Millisecond

	a := createPriorityEndpoint("a", 11434, domain.StatusHealthy, 100)
	b := createPriorityEndpoint("b", 11435, domain.StatusHealthy, 100)
	prober := &stubProber{loads: map[string]domain.Load{
		a.URLString: knownLoad(0),
		b.URLString: knownLoad(0),
	}}
	selector := NewLeastLoadSelector(NewTestStatsCollector(), prober)

	// One selection books a reservation, which is enough to make the busy node
	// the worse choice despite identical probe answers.
	if got, err := selector.Select(context.Background(), []*domain.Endpoint{a, b}); err != nil || got.URLString != a.URLString {
		t.Fatalf("first select: %v %v", got, err)
	}
	if got, err := selector.Select(context.Background(), []*domain.Endpoint{a, b}); err != nil || got.URLString != b.URLString {
		t.Fatalf("second select must avoid the reserved node: %v %v", got, err)
	}
	selector.resMu.Lock()
	live := selector.effectiveReservedLocked(a.URLString, time.Now())
	selector.resMu.Unlock()
	if live != 1 {
		t.Fatalf("live reservation after one select = %d, want 1", live)
	}

	time.Sleep(30 * time.Millisecond)

	selector.resMu.Lock()
	live = selector.effectiveReservedLocked(a.URLString, time.Now())
	selector.resMu.Unlock()
	if live != 0 {
		t.Fatalf("reservations must expire on their own, still %d live", live)
	}

	// The previously busy node is a plain candidate again: with identical probe
	// answers both nodes now tie at 0 and the SWRR pool holds the pair, so the
	// next two selections spread. Had the reservation survived, node a would
	// still score 1 and both picks would land on b.
	picked := map[string]bool{}
	for range 2 {
		got, err := selector.Select(context.Background(), []*domain.Endpoint{a, b})
		if err != nil {
			t.Fatal(err)
		}
		picked[got.URLString] = true
	}
	if !picked[a.URLString] || !picked[b.URLString] {
		t.Fatalf("expired node must be eligible again, two selects picked %v", picked)
	}

}

// TestLeastLoadSelector_ReservationsPrunedForRetiredEndpoints: the sweep a
// Select runs also drops entries whose URL is no longer a candidate. Without
// it, every endpoint rename/retirement of a long-lived gateway leaves a
// reservation behind forever - the drift the SWRR weight map accepts, which
// self-expiring reservations have no reason to.
func TestLeastLoadSelector_ReservationsPrunedForRetiredEndpoints(t *testing.T) {
	a := createPriorityEndpoint("a", 11434, domain.StatusHealthy, 100)
	b := createPriorityEndpoint("b", 11435, domain.StatusHealthy, 100)
	prober := &stubProber{loads: map[string]domain.Load{
		a.URLString: knownLoad(0),
		b.URLString: knownLoad(0),
	}}
	selector := NewLeastLoadSelector(NewTestStatsCollector(), prober)

	// Full-TTL reservations: nothing here may expire, only retirement prunes.
	if _, err := selector.Select(context.Background(), []*domain.Endpoint{a, b}); err != nil {
		t.Fatal(err)
	}
	selector.resMu.Lock()
	booked := len(selector.reserved)
	selector.resMu.Unlock()
	if booked == 0 {
		t.Fatal("test needs a live reservation to prune")
	}

	if _, err := selector.Select(context.Background(), []*domain.Endpoint{b}); err != nil {
		t.Fatal(err)
	}
	selector.resMu.Lock()
	entries := len(selector.reserved)
	_, stale := selector.reserved[a.URLString]
	selector.resMu.Unlock()
	if stale || entries != 0 {
		t.Fatalf("reservation of the retired node must be pruned: map holds %d URLs, stale entry=%v", entries, stale)
	}
}

// TestLeastLoadSelector_StickyHitsDoNotErodeReservations is the audit
// regression: the proxy pairs Increment/deferred DecrementConnections around
// EVERY attempt, so steady-state traffic on pinned conversations fires
// decrements that never had a matching reservation. Those must not eat the
// live reservations of concurrent NEW conversations.
func TestLeastLoadSelector_StickyHitsDoNotErodeReservations(t *testing.T) {
	a := createPriorityEndpoint("a", 11434, domain.StatusHealthy, 100)
	b := createPriorityEndpoint("b", 11435, domain.StatusHealthy, 100)
	prober := &stubProber{loads: map[string]domain.Load{
		a.URLString: knownLoad(0),
		b.URLString: knownLoad(0),
	}}
	selector := NewLeastLoadSelector(NewTestStatsCollector(), prober)
	w := NewStickySessionWrapper(selector, config.StickySessionConfig{
		Enabled: true, IdleTTLSeconds: 60, MaxSessions: 100,
	})
	w.Start()
	defer w.Stop()

	ctx, _ := injectKey(context.Background(), "sess-erosion:qwen", "session_header")
	chosen, err := w.Select(ctx, []*domain.Endpoint{a, b}) // miss: pins one node, books 1 reservation
	if err != nil {
		t.Fatal(err)
	}

	// The sticky-hit attempt's deferred decrement, five turns deep.
	for range 5 {
		w.IncrementConnections(chosen)
		w.DecrementConnections(chosen)
	}

	selector.resMu.Lock()
	live := selector.effectiveReservedLocked(chosen.URLString, time.Now())
	selector.resMu.Unlock()
	if live != 1 {
		t.Fatalf("5 hit-decrements must leave the 1 live reservation intact, got %d", live)
	}

	// And that reservation still steers a new conversation to the other node.
	got, err := selector.Select(context.Background(), []*domain.Endpoint{a, b})
	if err != nil {
		t.Fatal(err)
	}
	if got.URLString == chosen.URLString {
		t.Fatalf("the live reservation must still count (chosen %s)", got.URLString)
	}
}

// TestLeastLoadSelector_StickyHitFiresNoProbeRound is the cost half of the same
// truth: a pinned conversation never pays for a probe round, so steady-state
// traffic costs zero probes however many turns it takes.
func TestLeastLoadSelector_StickyHitFiresNoProbeRound(t *testing.T) {
	a := createPriorityEndpoint("a", 11434, domain.StatusHealthy, 100)
	b := createPriorityEndpoint("b", 11435, domain.StatusHealthy, 100)
	prober := &stubProber{loads: map[string]domain.Load{
		a.URLString: knownLoad(0),
		b.URLString: knownLoad(0),
	}}
	w := NewStickySessionWrapper(NewLeastLoadSelector(NewTestStatsCollector(), prober),
		config.StickySessionConfig{Enabled: true, IdleTTLSeconds: 60, MaxSessions: 100})
	w.Start()
	defer w.Stop()

	ctx, _ := injectKey(context.Background(), "sess-cost:qwen", "session_header")
	first, err := w.Select(ctx, []*domain.Endpoint{a, b}) // miss = the one round
	if err != nil {
		t.Fatal(err)
	}
	if prober.calls != 1 {
		t.Fatalf("pinning a conversation must cost exactly one probe round, got %d", prober.calls)
	}
	for range 5 {
		got, err := w.Select(ctx, []*domain.Endpoint{a, b})
		if err != nil || got.URLString != first.URLString {
			t.Fatalf("sticky hit: %v %v", got, err)
		}
	}
	if prober.calls != 1 {
		t.Fatalf("5 sticky hits must fire zero probe rounds, total calls %d", prober.calls)
	}
}

// TestLeastLoadSelector_UnknownRanksBehindKnown pins the degrade rule: an
// endpoint that did not answer the probe is steered past, not believed idle -
// absence is never zero. Among unknowns, gateway connection count orders them.
func TestLeastLoadSelector_UnknownRanksBehindKnown(t *testing.T) {
	answered := createPriorityEndpoint("answered", 11434, domain.StatusHealthy, 100)
	quiet := createPriorityEndpoint("quiet", 11435, domain.StatusHealthy, 100)
	prober := &stubProber{loads: map[string]domain.Load{
		answered.URLString: knownLoad(3), // busier than whatever quiet secretly is
	}}
	selector := NewLeastLoadSelector(NewTestStatsCollector(), prober)

	got, err := selector.Select(context.Background(), []*domain.Endpoint{quiet, answered})
	if err != nil {
		t.Fatal(err)
	}
	if got.URLString != answered.URLString {
		t.Fatalf("answered node with 3 active must beat the unanswered node (absence != idle), picked %s", got.URLString)
	}
}

// TestLeastLoadSelector_SingleEndpointTierSkipsProbe: no choice, no round.
func TestLeastLoadSelector_SingleEndpointTierSkipsProbe(t *testing.T) {
	prober := &stubProber{loads: map[string]domain.Load{}}
	selector := NewLeastLoadSelector(NewTestStatsCollector(), prober)

	top := createPriorityEndpoint("top", 11434, domain.StatusHealthy, 100)
	low := createPriorityEndpoint("low", 11435, domain.StatusHealthy, 90)

	got, err := selector.Select(context.Background(), []*domain.Endpoint{top, low})
	if err != nil {
		t.Fatal(err)
	}
	if got.URLString != top.URLString || prober.calls != 0 {
		t.Fatalf("solo top-tier node must win with zero probes: picked %s calls=%d", got.URLString, prober.calls)
	}
}

// TestLeastLoadSelector_PriorityTierIsPreserved: least-load means emptiest IN
// the tier. An empty prio-90 node must not beat a busy prio-100 node.
func TestLeastLoadSelector_PriorityTierIsPreserved(t *testing.T) {
	high := createPriorityEndpoint("busy-high", 11434, domain.StatusHealthy, 100)
	low := createPriorityEndpoint("empty-low", 11435, domain.StatusHealthy, 90)
	prober := &stubProber{loads: map[string]domain.Load{
		high.URLString: knownLoad(9),
		low.URLString:  knownLoad(0),
	}}
	selector := NewLeastLoadSelector(NewTestStatsCollector(), prober)

	for range 4 {
		got, err := selector.Select(context.Background(), []*domain.Endpoint{high, low})
		if err != nil {
			t.Fatal(err)
		}
		if got.Priority != 100 {
			t.Fatalf("prio-90 (empty) must not beat prio-100 (busy): picked %s prio %d", got.URLString, got.Priority)
		}
	}

	// The probe round is scoped to the tier: the prio-90 node is never in it
	// (probing failover-only nodes would let their answer influence the round).
	for _, round := range prober.rounds {
		if len(round) != 1 || round[0] != high.URLString {
			t.Fatalf("probe round must cover the top tier only, got %v", round)
		}
	}

	// With the top tier unroutable, the lower tier is reachable again.
	high.Status = domain.StatusUnhealthy
	got, err := selector.Select(context.Background(), []*domain.Endpoint{high, low})
	if err != nil || got.URLString != low.URLString {
		t.Fatalf("lower tier must serve when the top tier is down: %v %v", got, err)
	}
}

func TestLeastLoadSelector_ErrorsMatchPriority(t *testing.T) {
	selector := NewLeastLoadSelector(NewTestStatsCollector(), &stubProber{})
	if _, err := selector.Select(context.Background(), nil); err == nil {
		t.Fatal("empty candidate list must error")
	}
	offline := createPriorityEndpoint("off", 11434, domain.StatusOffline, 100)
	if _, err := selector.Select(context.Background(), []*domain.Endpoint{offline}); err == nil {
		t.Fatal("no-routable candidate list must error")
	}
}

// TestLeastLoadSelector_ErrorTextsByteMatchPriority: switching load_balancer
// must not change the error contract a caller or an operator sees for the same
// empty fleet, so the texts are asserted byte-for-byte against priority's.
func TestLeastLoadSelector_ErrorTextsByteMatchPriority(t *testing.T) {
	offline := createPriorityEndpoint("off", 11434, domain.StatusOffline, 100)
	cases := []struct {
		name      string
		endpoints []*domain.Endpoint
	}{
		{"empty", nil},
		{"none routable", []*domain.Endpoint{offline}},
	}
	for _, tc := range cases {
		_, wantErr := NewPrioritySelector(NewTestStatsCollector()).Select(context.Background(), tc.endpoints)
		_, gotErr := NewLeastLoadSelector(NewTestStatsCollector(), &stubProber{}).Select(context.Background(), tc.endpoints)
		if wantErr == nil || gotErr == nil {
			t.Fatalf("%s: expected errors from both selectors", tc.name)
		}
		if gotErr.Error() != wantErr.Error() {
			t.Errorf("%s: least-load error %q must byte-match priority's %q", tc.name, gotErr, wantErr)
		}
	}
}
