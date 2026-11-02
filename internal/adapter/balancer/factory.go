package balancer

import (
	"fmt"
	"sync"

	"github.com/thushan/olla/internal/core/domain"
	"github.com/thushan/olla/internal/core/ports"
)

const DefaultBalancerPriority = "priority"
const DefaultBalancerRoundRobin = "round-robin"
const DefaultBalancerLeastConnections = "least-connections"

// DefaultBalancerLeastLoad dispatches NEW conversations (sticky misses) to the
// endpoint with the fewest admitted requests inside the top priority tier. It
// is opt-in: "priority" remains the default so enabling it is a config choice,
// not an upgrade side effect.
const DefaultBalancerLeastLoad = "least-load"

type Factory struct {
	creators       map[string]func(ports.StatsCollector) domain.EndpointSelector
	statsCollector ports.StatsCollector

	// loadProber is the occupancy source handed to selectors that need one
	// ("least-load"). nil = no signal, and a selector built with no signal
	// behaves exactly like "priority".
	loadProber domain.LoadProber
	mu         sync.RWMutex
}

func NewFactory(statsCollector ports.StatsCollector) *Factory {
	factory := &Factory{
		creators:       make(map[string]func(ports.StatsCollector) domain.EndpointSelector),
		statsCollector: statsCollector,
	}

	factory.Register(DefaultBalancerPriority, func(collector ports.StatsCollector) domain.EndpointSelector {
		return NewPrioritySelector(collector)
	})
	factory.Register(DefaultBalancerRoundRobin, func(collector ports.StatsCollector) domain.EndpointSelector {
		return NewRoundRobinSelector(collector)
	})
	factory.Register(DefaultBalancerLeastConnections, func(collector ports.StatsCollector) domain.EndpointSelector {
		return NewLeastConnectionsSelector(collector)
	})

	// The closure reads loadProber at Create time, so SetLoadProber must run
	// BEFORE Create for the selector to be built with a prober - that ordering
	// is enforced at the single call site (app/services/proxy.go).
	factory.Register(DefaultBalancerLeastLoad, func(collector ports.StatsCollector) domain.EndpointSelector {
		factory.mu.RLock()
		prober := factory.loadProber
		factory.mu.RUnlock()
		return NewLeastLoadSelector(collector, prober)
	})

	return factory
}

// SetLoadProber supplies the occupancy source consumed by the "least-load"
// selector. Safe to call before or after Create; nil means "no signal", and a
// selector built with no signal behaves exactly like "priority".
func (f *Factory) SetLoadProber(prober domain.LoadProber) {
	f.mu.Lock()
	f.loadProber = prober
	f.mu.Unlock()
}

func (f *Factory) Register(name string, creator func(ports.StatsCollector) domain.EndpointSelector) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.creators[name] = creator
}

func (f *Factory) Create(name string) (domain.EndpointSelector, error) {
	f.mu.RLock()
	creator, exists := f.creators[name]
	f.mu.RUnlock()

	if !exists {
		return nil, fmt.Errorf("unknown load balancer strategy: %s", name)
	}

	return creator(f.statsCollector), nil
}

func (f *Factory) GetAvailableStrategies() []string {
	f.mu.RLock()
	defer f.mu.RUnlock()

	strategies := make([]string, 0, len(f.creators))
	for name := range f.creators {
		strategies = append(strategies, name)
	}
	return strategies
}
