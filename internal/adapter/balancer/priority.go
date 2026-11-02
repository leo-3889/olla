package balancer

import (
	"context"
	"errors"
	"math/rand"
	"sort"
	"sync"

	"github.com/thushan/olla/internal/core/domain"
	"github.com/thushan/olla/internal/core/ports"
)

// PrioritySelector implements priority-based endpoint selection with connection tracking.
//
// Inside the winning priority tier, traffic is spread by smooth weighted
// round-robin over the candidates' status weights (healthy 1.0, busy 0.3,
// warming 0.1). Equal-weight twins - two identical nodes at the same priority,
// which is what a "fast" tier built from two identical engines is - alternate
// exactly a,b,a,b, so each new conversation starts on the node that is next in
// the rotation.
//
// The tier tie-break used to be a weighted coin flip. With equal weights an
// independent draw per request clumps: a burst can sit 70/30 across two nodes
// for minutes, and sticky sessions then lock the clump in for the whole affinity
// TTL. Measured on the live fleet 2026-10-19 over one container lifetime:
// 7288 requests on radiance-9700-64 vs 1040 on radiance-9700-128, both prio
// 100, both healthy.
type PrioritySelector struct {
	statsCollector ports.StatsCollector

	// swrrMu guards the rotation state. Select is the per-request hot path;
	// the critical section is a handful of float adds over one tier (fleets
	// are single-digit endpoints), so a mutex costs less than the lock-free
	// bookkeeping needed to keep a rotation exact under concurrency.
	swrrMu sync.Mutex
	// currentWeight is the SWRR credit per endpoint, keyed by URLString (the
	// identity the sticky wrapper and PurgeDeadEndpoints already use). Each
	// pass adds the endpoint's weight and the winner pays the tier total, so
	// equal weights produce an exact rotation and unequal weights a
	// proportional one that interleaves the light nodes instead of bursting
	// them (1.0/0.3/0.1 -> healthy, healthy, busy, healthy, warming, ...).
	// Entries for endpoints that leave the fleet are never read again; the map
	// is bounded by the number of distinct endpoints the process has routed to,
	// which is config-sized, so it is not pruned.
	currentWeight map[string]float64
}

// NewPrioritySelector creates a new priority-based endpoint selector
func NewPrioritySelector(statsCollector ports.StatsCollector) *PrioritySelector {
	return &PrioritySelector{
		statsCollector: statsCollector,
		currentWeight:  make(map[string]float64),
	}
}

// Name returns the name of the selection strategy
func (p *PrioritySelector) Name() string {
	return DefaultBalancerPriority
}

// Select chooses the highest priority routable endpoint, distributing requests
// inside that tier by smooth weighted round-robin.
func (p *PrioritySelector) Select(ctx context.Context, endpoints []*domain.Endpoint) (*domain.Endpoint, error) {
	if len(endpoints) == 0 {
		return nil, errors.New("no endpoints available")
	}

	// Filter only routable endpoints
	routable := make([]*domain.Endpoint, 0, len(endpoints))
	for _, endpoint := range endpoints {
		if endpoint.Status.IsRoutable() {
			routable = append(routable, endpoint)
		}
	}

	if len(routable) == 0 {
		return nil, errors.New("no routable endpoints available")
	}

	// Sort by priority (highest first)
	sort.Slice(routable, func(i, j int) bool {
		return routable[i].Priority > routable[j].Priority
	})

	// Get the highest priority tier
	highestPriority := routable[0].Priority
	// Pre-allocate with capacity for all routable endpoints (worst case all have same priority)
	highestPriorityEndpoints := make([]*domain.Endpoint, 0, len(routable))

	for _, endpoint := range routable {
		if endpoint.Priority == highestPriority {
			highestPriorityEndpoints = append(highestPriorityEndpoints, endpoint)
		} else {
			break // Since sorted, we can break early
		}
	}

	// If only one endpoint at highest priority, return it
	if len(highestPriorityEndpoints) == 1 {
		return highestPriorityEndpoints[0], nil
	}

	// Multiple endpoints at same priority - rotate across them by status weight
	return p.weightedRoundRobin(highestPriorityEndpoints), nil
}

// weightedRoundRobin spreads traffic across endpoints that share the top
// priority using smooth weighted round-robin (nginx's algorithm): add every
// candidate's weight to its credit, hand the request to the highest credit,
// then subtract the tier's total weight from the winner. The selection is
// deterministic - equal weights give an exact alternation, unequal weights a
// proportional one that never queues a run of the same node while a lighter one
// waits. Rotation state lives on the selector, so the queue is shared by every
// alias whose top tier contains the same endpoint.
func (p *PrioritySelector) weightedRoundRobin(endpoints []*domain.Endpoint) *domain.Endpoint {
	if len(endpoints) == 1 {
		return endpoints[0]
	}

	weights := make([]float64, len(endpoints))
	total := 0.0
	for i, endpoint := range endpoints {
		weights[i] = endpoint.Status.GetTrafficWeight()
		total += weights[i]
	}

	// Unreachable through normal routing: Select forwards only routable
	// statuses and healthy/busy/warming all carry positive weight. Rotating
	// weightless endpoints is meaningless, so shuffle as a last resort.
	if total == 0 {
		//nolint:gosec // non-cryptographic shuffle is fine for endpoint shuffling
		return endpoints[rand.Intn(len(endpoints))]
	}

	p.swrrMu.Lock()
	defer p.swrrMu.Unlock()

	if p.currentWeight == nil {
		p.currentWeight = make(map[string]float64)
	}

	best := endpoints[0]
	for i, endpoint := range endpoints {
		p.currentWeight[endpoint.URLString] += weights[i]
		if p.currentWeight[endpoint.URLString] > p.currentWeight[best.URLString] {
			best = endpoint
		}
	}
	p.currentWeight[best.URLString] -= total

	return best
}

// IncrementConnections increments the connection count for an endpoint
func (p *PrioritySelector) IncrementConnections(endpoint *domain.Endpoint) {
	p.statsCollector.RecordConnection(endpoint, 1)
}

// DecrementConnections decrements the connection count for an endpoint
func (p *PrioritySelector) DecrementConnections(endpoint *domain.Endpoint) {
	p.statsCollector.RecordConnection(endpoint, -1)
}
