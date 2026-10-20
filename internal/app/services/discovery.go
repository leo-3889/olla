package services

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync/atomic"

	"github.com/thushan/olla/internal/adapter/discovery"
	"github.com/thushan/olla/internal/adapter/health"
	"github.com/thushan/olla/internal/adapter/registry"
	"github.com/thushan/olla/internal/adapter/registry/profile"
	"github.com/thushan/olla/internal/config"
	"github.com/thushan/olla/internal/core/domain"
	"github.com/thushan/olla/internal/core/ports"
	"github.com/thushan/olla/internal/logger"
)

// DiscoveryService orchestrates endpoint discovery, health monitoring, and model
// cataloguing. It runs initial health checks during startup to ensure endpoints
// are validated before the proxy accepts traffic, preventing "no healthy endpoints"
// errors during the critical startup phase.
type DiscoveryService struct {
	statsCollector ports.StatsCollector
	logger         logger.StyledLogger
	registry       domain.ModelRegistry
	endpointRepo   domain.EndpointRepository
	loadReader     health.LoadReader
	config         *config.DiscoveryConfig
	// modelCapabilities mirrors the top-level config model_capabilities map
	// (operator per-model declarations); installed once at wiring time.
	modelCapabilities map[string][]string
	registryConfig    *config.ModelRegistryConfig
	healthChecker     *health.HTTPHealthChecker
	statsService      *StatsService
	modelDiscovery    *discovery.ModelDiscoveryService
	// purgeDeadFn, when set, is called with the current routable endpoint list
	// whenever a backend transitions to unhealthy. Wired by the proxy layer so sticky
	// session entries for dead backends are evicted promptly rather than waiting for TTL.
	// Stored atomically because SetPurgeDeadEndpointsFn is called from the main goroutine
	// while the health-check goroutine may concurrently invoke the callback.
	purgeDeadFn atomic.Pointer[func([]*domain.Endpoint)]
	// Load-aware Busy grading params (proxy.busy_queue_depth). The health
	// checker is created in Start(), and the app wiring calls SetLoadAwareness
	// before any service starts, so the params are stashed here and applied
	// when the checker comes into existence.
	busyQueueDepth float64
}

// NewDiscoveryService creates a new discovery service
func NewDiscoveryService(
	config *config.DiscoveryConfig,
	registryConfig *config.ModelRegistryConfig,
	statsCollector ports.StatsCollector,
	logger logger.StyledLogger,
) *DiscoveryService {
	return &DiscoveryService{
		config:         config,
		registryConfig: registryConfig,
		statsCollector: statsCollector,
		logger:         logger,
	}
}

// Name returns the service name
func (s *DiscoveryService) Name() string {
	return "discovery"
}

// Start initialises discovery components
// SetModelCapabilities installs the operator per-model capability
// declarations (top-level config model_capabilities) for discovery-time
// augmentation. Call at wiring time, before Start.
func (s *DiscoveryService) SetModelCapabilities(caps map[string][]string) {
	s.modelCapabilities = caps
}

func (s *DiscoveryService) Start(ctx context.Context) error {
	s.logger.Info("Initialising discovery service")

	if s.statsService != nil {
		collector, err := s.statsService.GetCollector()
		if err != nil {
			return fmt.Errorf("failed to get stats collector: %w", err)
		}
		s.statsCollector = collector
	}

	// Create model registry using factory with configuration
	if s.registryConfig == nil {
		// Fallback to default if no config provided
		s.registry = registry.NewMemoryModelRegistry(s.logger)
	} else {
		registryConfig := registry.RegistryConfig{
			Type:            s.registryConfig.Type,
			EnableUnifier:   s.registryConfig.EnableUnifier,
			UnificationConf: &s.registryConfig.Unification,
			// Routing strategy must be forwarded so the configured type (strict,
			// optimistic, discovery) takes effect. Without this, registries fall
			// back to strict regardless of what the operator configured.
			RoutingStrategy: &s.registryConfig.RoutingStrategy,
		}
		var err error
		s.registry, err = registry.NewModelRegistry(registryConfig, s.logger)
		if err != nil {
			return fmt.Errorf("failed to create model registry: %w", err)
		}
	}

	switch s.config.Type {
	case "static":
		staticRepo := discovery.NewStaticEndpointRepository()
		if err := staticRepo.LoadFromConfig(ctx, s.config.Static.Endpoints); err != nil {
			return fmt.Errorf("failed to load endpoints from config: %w", err)
		}
		s.endpointRepo = staticRepo
	default:
		return fmt.Errorf("unsupported discovery type: %s", s.config.Type)
	}

	s.healthChecker = health.NewHTTPHealthCheckerWithDefaults(s.endpointRepo, s.logger)
	s.armLoadAwareGrading()

	// Evict sticky session entries for backends that LEFT THE FLEET. The purgeFn
	// is nil when sticky sessions are disabled, making this callback a cheap
	// no-op then. NOTE the GetAll: the wrapper's contract is registry
	// membership, not health. Passing GetRoutable here meant every unhealthy
	// transition deleted every pin to that node - a blip purged the whole
	// node's KV affinity, and the conversation never came back. Health blips
	// are now ridden out by the sticky wrapper (bypass outcome, pin kept);
	// only config removal is unambiguous enough to evict.
	s.healthChecker.SetUnhealthyCallback(health.UnhealthyCallbackFunc(func(ctx context.Context, _ *domain.Endpoint) {
		fn := s.purgeDeadFn.Load()
		if fn == nil {
			return
		}
		registered, err := s.endpointRepo.GetAll(ctx)
		if err != nil {
			s.logger.Warn("Failed to fetch registered endpoints for sticky session purge", "error", err)
			return
		}
		(*fn)(registered)
	}))

	if err := s.healthChecker.StartChecking(ctx); err != nil {
		return fmt.Errorf("failed to start health checker: %w", err)
	}

	// Critical: Run initial health check to validate endpoints immediately.
	// This prevents the proxy from starting with no healthy endpoints.
	if err := s.healthChecker.RunHealthCheck(ctx, true); err != nil {
		s.logger.Warn("Failed to force initial health check", "error", err)
	}

	endpoints, err := s.endpointRepo.GetAll(ctx)
	if err != nil {
		return fmt.Errorf("failed to get endpoints: %w", err)
	}

	for _, endpoint := range endpoints {
		s.logger.Info("Endpoint registered",
			"name", endpoint.Name,
			"url", endpoint.URLString,
			"priority", endpoint.Priority)
	}

	if s.config.ModelDiscovery.Enabled {
		httpClient := &http.Client{
			Timeout: s.config.ModelDiscovery.Timeout,
		}
		profileFactory, err := profile.NewFactoryWithDefaults()
		if err != nil {
			return fmt.Errorf("failed to create profile factory: %w", err)
		}
		client := discovery.NewHTTPModelDiscoveryClient(profileFactory, s.logger, httpClient)
		client.SetModelCapabilities(s.modelCapabilities)
		client.SetCapabilityProbe(true)
		discoveryConfig := discovery.DiscoveryConfig{
			Interval:          s.config.ModelDiscovery.Interval,
			Timeout:           s.config.ModelDiscovery.Timeout,
			ConcurrentWorkers: s.config.ModelDiscovery.ConcurrentWorkers,
			RetryAttempts:     s.config.ModelDiscovery.RetryAttempts,
			RetryBackoff:      s.config.ModelDiscovery.RetryBackoff,
		}
		s.modelDiscovery = discovery.NewModelDiscoveryService(client, s.endpointRepo, s.registry, discoveryConfig, s.logger)

		// Set up recovery callback to trigger model discovery when endpoints recover
		s.healthChecker.SetRecoveryCallback(health.RecoveryCallbackFunc(func(ctx context.Context, endpoint *domain.Endpoint) error {
			s.logger.Info("Triggering model discovery for recovered endpoint",
				"endpoint", endpoint.Name,
				"url", endpoint.GetURLString())

			// Discover models for the recovered endpoint
			if err := s.modelDiscovery.DiscoverEndpoint(ctx, endpoint); err != nil {
				s.logger.Warn("Failed to discover models for recovered endpoint",
					"endpoint", endpoint.Name,
					"error", err)
				return err
			}

			s.logger.Info("Successfully refreshed models for recovered endpoint",
				"endpoint", endpoint.Name)
			return nil
		}))

		if err := s.modelDiscovery.Start(ctx); err != nil {
			return fmt.Errorf("failed to start model discovery: %w", err)
		}

		// Run initial model discovery to populate the catalogue immediately
		if err := s.modelDiscovery.DiscoverAll(ctx); err != nil {
			s.logger.Warn("Initial model discovery failed", "error", err)
		}
	}

	s.logger.Info("Discovery service initialised",
		"type", s.config.Type,
		"endpoints", len(endpoints))

	return nil
}

// Stop gracefully shuts down discovery components
// armLoadAwareGrading wires the queue reader into the health checker and
// reports the result once. The boot line is gated on the checker's own
// accessor - the single source of truth for "armed" - so the logged
// sentence and the grading state cannot drift apart if arming ever grows a
// path that does not go through the config field. Extracted from Start so
// the pinned gocognit budget for Start stays green.
func (s *DiscoveryService) armLoadAwareGrading() {
	if s.busyQueueDepth <= 0 || s.loadReader == nil {
		return
	}
	s.healthChecker.SetLoadAwareness(s.busyQueueDepth, s.loadReader)
	if s.healthChecker.LoadAwarenessArmed() {
		s.logger.Info("Load-aware Busy grading armed", "queue_depth_threshold", s.busyQueueDepth)
	}
}

func (s *DiscoveryService) Stop(ctx context.Context) error {
	s.logger.Info(" Stopping discovery service")

	defer func() {
		s.logger.ResetLine()
		s.logger.InfoWithStatus("Stopping discovery service", "OK")
	}()
	if s.healthChecker != nil {
		if err := s.healthChecker.StopChecking(ctx); err != nil {
			s.logger.Warn("  Failed to stop health checker", "error", err)
		}
	}

	if s.modelDiscovery != nil {
		if err := s.modelDiscovery.Stop(ctx); err != nil {
			s.logger.Warn("  Failed to stop model discovery", "error", err)
		}
	}
	return nil
}

// Dependencies returns service dependencies
func (s *DiscoveryService) Dependencies() []string {
	return []string{"stats"}
}

// GetRegistry returns the model registry
func (s *DiscoveryService) GetRegistry() (domain.ModelRegistry, error) {
	if s.registry == nil {
		return nil, errors.New("model registry not initialised")
	}
	return s.registry, nil
}

// GetEndpointRepository returns the endpoint repository
func (s *DiscoveryService) GetEndpointRepository() (domain.EndpointRepository, error) {
	if s.endpointRepo == nil {
		return nil, errors.New("endpoint repository not initialised")
	}
	return s.endpointRepo, nil
}

// GetHealthChecker returns the health checker
func (s *DiscoveryService) GetHealthChecker() (*health.HTTPHealthChecker, error) {
	if s.healthChecker == nil {
		return nil, errors.New("health checker not initialised")
	}
	return s.healthChecker, nil
}

// GetDiscoveryService returns itself as a ports.DiscoveryService
func (s *DiscoveryService) GetDiscoveryService() (ports.DiscoveryService, error) {
	if s.registry == nil || s.endpointRepo == nil {
		return nil, errors.New("discovery service not fully initialised")
	}
	return s, nil
}

// GetHealthyEndpoints implements ports.DiscoveryService. It is a DISPATCH candidate
// source, not a status listing: it returns the ROUTABLE set (healthy/busy/warming),
// mirroring endpointRepositoryAdapter.GetHealthyEndpoints. The graded-weight ladder
// (healthy 1.0 / busy 0.3 / warming 0.1) only works while a Busy endpoint stays a
// candidate at reduced weight; with a healthy-only filter, load-aware Busy grading
// (proxy.busy_queue_depth) removes the endpoint from routing entirely, so an alias
// whose only live carrier is graded Busy answers 404 model_not_found for the whole
// probe cycle instead of degrading to reduced-weight routing.
// Status/model listing handlers keep the strict repository GetHealthy.
func (s *DiscoveryService) GetHealthyEndpoints(ctx context.Context) ([]*domain.Endpoint, error) {
	if s.endpointRepo == nil {
		return nil, errors.New("endpoint repository not initialized")
	}
	return s.endpointRepo.GetRoutable(ctx)
}

// GetEndpoints implements ports.DiscoveryService
func (s *DiscoveryService) GetEndpoints(ctx context.Context) ([]*domain.Endpoint, error) {
	if s.endpointRepo == nil {
		return nil, errors.New("endpoint repository not initialized")
	}
	return s.endpointRepo.GetAll(ctx)
}

// RefreshEndpoints implements ports.DiscoveryService
func (s *DiscoveryService) RefreshEndpoints(ctx context.Context) error {
	// For static discovery, endpoints don't change, so this is a no-op
	// In a dynamic discovery implementation, this would refresh from the discovery source
	return nil
}

// SetStatsService sets the stats service dependency
func (s *DiscoveryService) SetStatsService(statsService *StatsService) {
	s.statsService = statsService
}

// SetLoadAwareness arms load-aware Busy grading (proxy.busy_queue_depth,
// reader = runtime-metrics sampler). threshold <= 0 is inert. Safe at any
// point in the lifecycle: the checker is created in Start(), which runs AFTER
// the app wiring phase, so the params are stashed on the service and applied
// when the checker is constructed (and immediately, if it already exists).
func (s *DiscoveryService) SetLoadAwareness(threshold float64, reader health.LoadReader) {
	s.busyQueueDepth = threshold
	s.loadReader = reader
	if s.healthChecker != nil {
		s.healthChecker.SetLoadAwareness(threshold, reader)
	}
}

// SetPurgeDeadEndpointsFn registers a function the health checker calls with the
// REGISTERED endpoint list (GetAll) after an endpoint transitions to unhealthy -
// the proxy layer turns that into a sticky purge of pins whose target left the
// fleet. The argument is deliberately not the routable set: a health blip must
// not delete the affected node's KV affinity.
func (s *DiscoveryService) SetPurgeDeadEndpointsFn(fn func([]*domain.Endpoint)) {
	s.purgeDeadFn.Store(&fn)
}

// UpdateEndpointStatus updates the status of an endpoint in the repository
func (s *DiscoveryService) UpdateEndpointStatus(ctx context.Context, endpoint *domain.Endpoint) error {
	if s.endpointRepo == nil {
		return errors.New("endpoint repository not initialized")
	}
	return s.endpointRepo.UpdateEndpoint(ctx, endpoint)
}
