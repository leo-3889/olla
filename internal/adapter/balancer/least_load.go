package balancer

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/thushan/olla/internal/core/domain"
	"github.com/thushan/olla/internal/core/ports"
)

// LeastLoadSelector assigns a NEW conversation to the endpoint carrying the
// fewest admitted requests (processing + queued in front of it), inside the
// highest priority tier.
//
// Ordering rules, in the exact sequence Select applies them:
//  1. sticky runs first and short-circuits this selector: a pinned
//     conversation is answered by balancer/sticky.go before Select is reached,
//     so an existing session never migrates because the fleet got busier. This
//     selector only ever sees sticky misses (new conversations) and failover
//     attempts (retry.go calls Select again per attempt).
//  2. the priority tier rule is unchanged from PrioritySelector: a prio-90
//     endpoint stays failover-only while a prio-100 is routable. Least-load
//     means "emptiest node IN THE TIER", never "emptiest node in the fleet".
//  3. a one-endpoint tier is returned without probing: no choice, no cost.
//  4. with no prober (knob off) the existing smooth weighted round-robin runs
//     unchanged, so least-load is byte-identical to priority with no signal.
//  5. otherwise ONE probe round ranks the tier: known readings first (fewest
//     admitted wins), unknown endpoints behind them ordered by the gateway's
//     own live connection count, ties broken by SWRR.
//
// The probe measures the engine's admitted count directly from the engine
// (loadprobe package) - never from Influx, never from a cached poller.
type LeastLoadSelector struct {
	inner  *PrioritySelector
	prober domain.LoadProber

	// reserved holds the self-expiring reservations handed out per endpoint:
	// one expiry timestamp per selection. Without reservations, the parallel
	// requests of one client (a fan-out burst, or an agent firing turns in a
	// loop) probe in the same instant, all see identical load, and all pick the
	// same node - the clump the SWRR rotation exists to prevent.
	//
	// A reservation expires on its own instead of being released by a counter
	// decrement: the proxy pairs Increment/deferred DecrementConnections around
	// EVERY attempt (retry.go executeProxyAttempt), including sticky hits that
	// never reserved and solo-tier skips, so any decrement pairing eats live
	// reservations of concurrent new conversations - exactly during the fan-out
	// the counter exists to protect - and a long generation double-counts
	// against its own node (engine in_flight plus reservation) for its whole
	// duration. A reservation therefore exists only to cover the parallel-
	// arrival window before the engine's own admitted gauge starts counting the
	// request (~one RTT plus the engine's accounting delay); expiry means no
	// decrement pairing is needed anywhere, sticky hits and solo skips are
	// neutral, and expired entries are pruned on read. Entries for URLs that
	// leave the fleet are never read again - the same bounded drift SWRR
	// weights already accept.
	resMu    sync.Mutex
	reserved map[string][]time.Time
}

// reservationTTL is how long one reservation counts against its endpoint. A
// package var so tests can shrink it; deliberately not a config knob - one
// time.Time per selection is the whole cost, and no operator needs to tune it.
var reservationTTL = 2 * time.Second

// NewLeastLoadSelector builds the selector. A nil prober is the knob-off shape
// (byte-identical to PrioritySelector).
func NewLeastLoadSelector(statsCollector ports.StatsCollector, prober domain.LoadProber) *LeastLoadSelector {
	return &LeastLoadSelector{
		inner:    NewPrioritySelector(statsCollector),
		prober:   prober,
		reserved: make(map[string][]time.Time),
	}
}

// Name returns the selection strategy name.
func (l *LeastLoadSelector) Name() string { return DefaultBalancerLeastLoad }

// candidate is one tier member with its ranking inputs resolved.
type candidate struct {
	endpoint *domain.Endpoint
	known    bool
	active   int // admitted per the probe, when known
	conn     int // gateway-side live streams, when unknown
	score    int // active + live reservations, when known
}

// Select ranks the top priority tier by live occupancy and returns the
// emptiest endpoint. See the type comment for the full rule order.
func (l *LeastLoadSelector) Select(ctx context.Context, endpoints []*domain.Endpoint) (*domain.Endpoint, error) {
	// Sweep expired reservations (and URLs that left the fleet) once per
	// selection - before the early returns, so the map drains even when every
	// tier is a solo node. Fleets are single-digit endpoints, so this walk is
	// cheaper than the probe round it replaces bookkeeping for.
	now := time.Now()
	l.resMu.Lock()
	l.purgeExpiredLocked(topPriorityTier(endpoints), now)
	l.resMu.Unlock()

	// Error texts byte-match PrioritySelector: switching load_balancer must not
	// change the failure an operator sees for the same empty fleet.
	if len(endpoints) == 0 {
		return nil, errors.New("no endpoints available")
	}
	tier := topPriorityTier(endpoints)
	if len(tier) == 0 {
		return nil, errors.New("no routable endpoints available")
	}
	if len(tier) == 1 {
		return tier[0], nil
	}
	if l.prober == nil {
		return l.inner.weightedRoundRobin(tier), nil
	}

	loads := l.prober.Probe(ctx, tier)

	l.resMu.Lock()
	defer l.resMu.Unlock()

	cands := make([]candidate, 0, len(tier))
	for _, ep := range tier {
		c := candidate{endpoint: ep, conn: int(l.inner.statsCollector.GetConnectionCount(ep.URLString))}
		if load, ok := loads[ep.URLString]; ok {
			c.known = true
			c.active = load.Active
			c.score = load.Active + l.effectiveReservedLocked(ep.URLString, now)
		}
		cands = append(cands, c)
	}

	// Rank: known readings first; among known, fewest admitted (live
	// reservations included); unknown endpoints rank behind every answered one
	// and order among themselves by the gateway's own live connection count.
	sort.SliceStable(cands, func(i, j int) bool {
		a, b := cands[i], cands[j]
		if a.known != b.known {
			return a.known
		}
		if a.known && a.score != b.score {
			return a.score < b.score
		}
		if !a.known && a.conn != b.conn {
			return a.conn < b.conn
		}
		return false
	})

	// Collect the tied head and hand it to SWRR: equal-load nodes then
	// alternate exactly (and unequal-status weights still apply), so the
	// zero-signal case degenerates to the exact priority rotation.
	head := []*domain.Endpoint{cands[0].endpoint}
	for _, c := range cands[1:] {
		if sameRank(c, cands[0]) {
			head = append(head, c.endpoint)
		} else {
			break // sorted: everything after is strictly worse
		}
	}

	winner := l.inner.weightedRoundRobin(head)
	// The only place a reservation is booked: the multi-endpoint scored path,
	// where a choice was actually made from a fresh probe round.
	l.reserved[winner.URLString] = append(l.reserved[winner.URLString], now.Add(reservationTTL))
	return winner, nil
}

// effectiveReservedLocked counts the non-expired reservations for url, dropping
// the expired ones (and the map entry when none remain). Callers hold resMu.
func (l *LeastLoadSelector) effectiveReservedLocked(url string, now time.Time) int {
	return l.keepLiveLocked(url, now)
}

// purgeExpiredLocked drops expired reservations for every tier member, plus the
// entries of URLs that are no longer in the candidate set at all (an endpoint
// that left the fleet can never reserve again, so its entry is dead weight).
// Called once per Select under resMu; fleets are single-digit endpoints, so the
// map is bounded by tier size x arrival rate either way. Callers hold resMu.
func (l *LeastLoadSelector) purgeExpiredLocked(tier []*domain.Endpoint, now time.Time) {
	inTier := make(map[string]struct{}, len(tier))
	for _, ep := range tier {
		l.keepLiveLocked(ep.URLString, now)
		inTier[ep.URLString] = struct{}{}
	}
	for url := range l.reserved {
		if _, ok := inTier[url]; !ok {
			delete(l.reserved, url)
		}
	}
}

// keepLiveLocked compacts url's expiry list and returns how many are live.
func (l *LeastLoadSelector) keepLiveLocked(url string, now time.Time) int {
	expiries := l.reserved[url]
	if len(expiries) == 0 {
		return 0
	}
	live := expiries[:0]
	for _, expiry := range expiries {
		if expiry.After(now) {
			live = append(live, expiry)
		}
	}
	if len(live) == 0 {
		delete(l.reserved, url)
	} else {
		l.reserved[url] = live
	}
	return len(live)
}

// sameRank reports whether two tier candidates are tied under the ordering
// above and therefore belong in the same SWRR pool.
func sameRank(a, b candidate) bool {
	if a.known != b.known {
		return false
	}
	if a.known {
		return a.score == b.score
	}
	return a.conn == b.conn
}

// IncrementConnections delegates stream accounting to the stats collector.
// Reservations deliberately do NOT increase here: they are booked at selection,
// and the proxy pairs Increment with a deferred Decrement around every attempt
// (sticky hits included), so booking here as well would over-count.
func (l *LeastLoadSelector) IncrementConnections(endpoint *domain.Endpoint) {
	l.inner.IncrementConnections(endpoint)
}

// DecrementConnections releases the collector stream ONLY. It never touches
// reservations: they expire on their own (see the note on the type), which
// makes the attempt-paired decrement a sticky hit or a solo-tier skip also
// triggers neutral by construction.
func (l *LeastLoadSelector) DecrementConnections(endpoint *domain.Endpoint) {
	l.inner.DecrementConnections(endpoint)
}

// topPriorityTier is the tier rule of PrioritySelector.Select extracted:
// routable only, highest priority first, ONLY the top tier (early break by
// design - a lower tier wins solely when the top one has no routable member).
func topPriorityTier(endpoints []*domain.Endpoint) []*domain.Endpoint {
	routable := make([]*domain.Endpoint, 0, len(endpoints))
	for _, endpoint := range endpoints {
		if endpoint.Status.IsRoutable() {
			routable = append(routable, endpoint)
		}
	}
	if len(routable) == 0 {
		return nil
	}
	sort.Slice(routable, func(i, j int) bool {
		return routable[i].Priority > routable[j].Priority
	})
	highest := routable[0].Priority
	tier := routable[:0:0]
	for _, endpoint := range routable {
		if endpoint.Priority != highest {
			break
		}
		tier = append(tier, endpoint)
	}
	return tier
}
