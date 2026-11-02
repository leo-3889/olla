package services

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/thushan/olla/internal/adapter/balancer"
	"github.com/thushan/olla/internal/adapter/balancer/loadprobe"
	"github.com/thushan/olla/internal/adapter/metrics"
	"github.com/thushan/olla/internal/adapter/proxy"
	proxyconfig "github.com/thushan/olla/internal/adapter/proxy/config"
	"github.com/thushan/olla/internal/adapter/registry/profile"
	"github.com/thushan/olla/internal/config"
	"github.com/thushan/olla/internal/core/domain"
	"github.com/thushan/olla/internal/core/ports"
	"github.com/thushan/olla/internal/logger"
)

const (
	// defaultConnectionKeepAlive is the fallback when the proxy config omits
	// connection_keep_alive. Matches the app-layer default to keep behaviour
	// identical for operators who haven't set the field.
	defaultConnectionKeepAlive = 30 * time.Second
)

// ProxyServiceWrapper adapts the core proxy implementation to the service lifecycle
// model. It manages the creation of load balancers and proxy engines, ensuring they
// receive validated endpoints from the discovery service.
type ProxyServiceWrapper struct {
	config       *config.ProxyConfig
	proxyService ports.ProxyService
	loadBalancer domain.EndpointSelector
	// stickyWrapper is non-nil when sticky sessions are enabled; held separately
	// so Stop() can shut down its background goroutine.
	stickyWrapper    *balancer.StickySessionWrapper
	endpointRepo     domain.EndpointRepository
	discoveryService ports.DiscoveryService
	statsCollector   ports.StatsCollector
	metricsExtractor ports.MetricsExtractor
	statsService     *StatsService
	discoverySvc     *DiscoveryService
	securityService  *SecurityService
	logger           logger.StyledLogger
}

// NewProxyServiceWrapper creates a new proxy service wrapper
func NewProxyServiceWrapper(
	config *config.ProxyConfig,
	logger logger.StyledLogger,
) *ProxyServiceWrapper {
	return &ProxyServiceWrapper{
		config: config,
		logger: logger,
	}
}

// Name returns the service name
func (s *ProxyServiceWrapper) Name() string {
	return "proxy"
}

// Start initialises the proxy service and load balancer
func (s *ProxyServiceWrapper) Start(ctx context.Context) error {
	s.logger.Info("Initialising proxy service")

	// Get dependencies from services
	if s.statsService != nil {
		collector, err := s.statsService.GetCollector()
		if err != nil {
			return fmt.Errorf("failed to get stats collector: %w", err)
		}
		s.statsCollector = collector
	}
	if s.discoverySvc != nil {
		repo, err := s.discoverySvc.GetEndpointRepository()
		if err != nil {
			return fmt.Errorf("failed to get endpoint repository: %w", err)
		}
		s.endpointRepo = repo

		discService, err := s.discoverySvc.GetDiscoveryService()
		if err != nil {
			return fmt.Errorf("failed to get discovery service: %w", err)
		}
		s.discoveryService = discService
	}

	// Create load balancer (load-probe wiring inside: Start sits at the
	// gocognit ceiling, and the wiring is a factory-construction concern).
	balancerFactory := s.newBalancerFactory()

	var err error
	s.loadBalancer, err = balancerFactory.Create(s.config.LoadBalancer)
	if err != nil {
		return fmt.Errorf("failed to create load balancer: %w", err)
	}
	s.logger.Info("Load balancer created", "type", s.config.LoadBalancer)

	s.applyStickySessions()

	// Create proxy configuration
	proxyConfig := s.createProxyConfiguration()

	// Create a discovery service adapter
	s.discoveryService = &endpointRepositoryAdapter{repo: s.endpointRepo}

	// Create metrics extractor with profile factory
	s.logger.Info("Creating metrics extractor...")
	profileFactory, err := profile.NewFactoryWithDefaults()
	if err != nil {
		s.logger.Warn("Failed to create profile factory for metrics extraction", "error", err)
		// Continue without metrics extraction
		profileFactory = nil
	} else {
		s.logger.Info("Profile factory created successfully",
			"profiles", profileFactory.GetAvailableProfiles())
	}

	var metricsExtractor ports.MetricsExtractor
	if profileFactory != nil {
		metricsExtractor, err = metrics.NewExtractor(profileFactory, s.logger)
		if err != nil {
			s.logger.Warn("Failed to create metrics extractor", "error", err)
			// Continue without metrics extraction
			metricsExtractor = nil
		} else {
			s.logger.Info("Metrics extractor initialized successfully")
			// Pre-validate profiles for metrics extraction
			for _, profileName := range profileFactory.GetAvailableProfiles() {
				if inferenceProfile, profileErr := profileFactory.GetProfile(profileName); profileErr == nil {
					if extractor, ok := metricsExtractor.(*metrics.Extractor); ok {
						if validationErr := extractor.ValidateProfile(inferenceProfile); validationErr != nil {
							s.logger.Debug("Profile metrics validation failed",
								"profile", profileName, "error", validationErr)
						}
					}
				}
			}
		}
	}
	s.metricsExtractor = metricsExtractor

	// Create proxy service
	proxyFactory := proxy.NewFactory(s.statsCollector, metricsExtractor, s.logger)
	s.proxyService, err = proxyFactory.Create(s.config.Engine, s.discoveryService, s.loadBalancer, proxyConfig)
	if err != nil {
		return fmt.Errorf("failed to create proxy service: %w", err)
	}

	s.logger.Info("Proxy service initialised",
		"engine", s.config.Engine,
		"profile", s.config.Profile,
		"balancer", s.config.LoadBalancer)

	return nil
}

// cleanupable is an optional interface implemented by proxy engines that own
// background goroutines or connection pools that must be released on shutdown.
// Using a type-assert rather than embedding in ports.ProxyService keeps the
// interface minimal and avoids forcing all current and future implementations
// to add a no-op method.
type cleanupable interface {
	Cleanup()
}

// Stop gracefully shuts down the proxy service
func (s *ProxyServiceWrapper) Stop(ctx context.Context) error {
	s.logger.Info(" Stopping proxy service")

	if s.stickyWrapper != nil {
		s.stickyWrapper.Stop()
	}

	// Call Cleanup on the proxy engine if it supports it. This stops the
	// cleanupLoop goroutine and releases per-endpoint connection pools,
	// preventing a goroutine/resource leak if the engine is ever rebuilt
	// in-process.
	if c, ok := s.proxyService.(cleanupable); ok {
		c.Cleanup()
	}

	defer func() {
		s.logger.ResetLine()
		s.logger.InfoWithStatus("Stopping proxy service", "OK")
	}()
	return nil
}

// Dependencies returns service dependencies
func (s *ProxyServiceWrapper) Dependencies() []string {
	return []string{"discovery", "security", "stats"}
}

// createProxyConfiguration builds engine-specific configuration. The proxy factory
// will augment this with engine-specific settings (e.g., connection pool parameters
// for Olla engine).
func (s *ProxyServiceWrapper) createProxyConfiguration() *proxy.Configuration {
	keepAlive := s.config.ConnectionKeepAlive
	if keepAlive == 0 {
		keepAlive = defaultConnectionKeepAlive
	}
	return &proxy.Configuration{
		ProxyPrefix:           "",
		ConnectionTimeout:     s.config.ConnectionTimeout,
		ConnectionKeepAlive:   keepAlive,
		ResponseTimeout:       s.config.ResponseTimeout,
		ReadTimeout:           s.config.ReadTimeout,
		ResponseHeaderTimeout: s.config.ResponseHeaderTimeout,
		TLSHandshakeTimeout:   s.config.TLSHandshakeTimeout,
		StreamBufferSize:      s.config.StreamBufferSize,
		Retry: proxyconfig.RetryPolicy{
			Disabled:        !s.config.Retry.Enabled,
			MaxAttempts:     s.config.Retry.MaxAttempts,
			NoNonIdempotent: !s.config.Retry.NonIdempotent,
		},
		RetryOnStatus: s.config.RetryOnStatus,
	}
}

// GetProxyService returns the underlying proxy service
func (s *ProxyServiceWrapper) GetProxyService() (ports.ProxyService, error) {
	if s.proxyService == nil {
		return nil, errors.New("proxy service not initialised")
	}
	return s.proxyService, nil
}

// GetLoadBalancer returns the load balancer
func (s *ProxyServiceWrapper) GetLoadBalancer() (domain.EndpointSelector, error) {
	if s.loadBalancer == nil {
		return nil, errors.New("load balancer not initialised")
	}
	return s.loadBalancer, nil
}

// StickyStats returns a point-in-time snapshot of sticky session metrics,
// or nil when sticky sessions are disabled or not yet initialised.
func (s *ProxyServiceWrapper) StickyStats() *balancer.StickyStats {
	if s.stickyWrapper == nil {
		return nil
	}
	stats := s.stickyWrapper.Stats()
	return &stats
}

// PurgeDeadEndpoints removes sticky session entries that point to backends ABSENT
// from the registry - `registered` must be the GetAll list (healthy and unhealthy
// alike), never the routable subset: the wrapper evicts pins only when an endpoint
// has left the fleet, and health blips are ridden out with a bypass outcome.
// It is a no-op when sticky sessions are disabled.
func (s *ProxyServiceWrapper) PurgeDeadEndpoints(registered []*domain.Endpoint) {
	if s.stickyWrapper != nil {
		s.stickyWrapper.PurgeDeadEndpoints(registered)
	}
}

// endpointRepositoryAdapter provides interface adaptation between the domain repository
// and the discovery service interface expected by the proxy layer.
type endpointRepositoryAdapter struct {
	repo domain.EndpointRepository
}

func (a *endpointRepositoryAdapter) GetEndpoints(ctx context.Context) ([]*domain.Endpoint, error) {
	return a.repo.GetAll(ctx)
}

// GetHealthyEndpoints is the proxy dispatch candidate source. It returns the
// ROUTABLE set (healthy/busy/warming), not healthy-only: the priority
// selector grades traffic weights by status (1.0/0.3/0.1) and load-aware
// Busy grading is meaningless while the candidate filter excludes Busy - the
// healthy-only filter made the whole graded-weight ladder dead code, and an
// all-Busy top tier produced a 503 instead of degraded routing. With the
// default threshold (grading off) Busy/Warming are unreachable statuses in
// practice, so the filter change is behavior-neutral until opted in.
// Status/model listing endpoints keep the strict GetHealthy.
func (a *endpointRepositoryAdapter) GetHealthyEndpoints(ctx context.Context) ([]*domain.Endpoint, error) {
	return a.repo.GetRoutable(ctx)
}

func (a *endpointRepositoryAdapter) RefreshEndpoints(ctx context.Context) error {
	// No-op for static endpoints
	return nil
}

func (a *endpointRepositoryAdapter) UpdateEndpointStatus(ctx context.Context, endpoint *domain.Endpoint) error {
	// Update endpoint status in repository
	return a.repo.UpdateEndpoint(ctx, endpoint)
}

// SetStatsService sets the stats service dependency
func (s *ProxyServiceWrapper) SetStatsService(statsService *StatsService) {
	s.statsService = statsService
}

// SetDiscoveryService sets the discovery service dependency
func (s *ProxyServiceWrapper) SetDiscoveryService(discoveryService *DiscoveryService) {
	s.discoverySvc = discoveryService
}

// SetSecurityService sets the security service dependency
func (s *ProxyServiceWrapper) SetSecurityService(securityService *SecurityService) {
	s.securityService = securityService
}

// newBalancerFactory builds the balancer factory and wires the
// assignment-time occupancy probe when proxy.load_probe is armed for
// "least-load". SetLoadProber must precede Create: the registration closure
// reads the prober there.
func (s *ProxyServiceWrapper) newBalancerFactory() *balancer.Factory {
	factory := balancer.NewFactory(s.statsCollector)
	s.logLoadProbeConfig()
	if s.config.LoadProbe.Enabled && s.config.LoadBalancer == balancer.DefaultBalancerLeastLoad {
		factory.SetLoadProber(loadprobe.New(s.config.LoadProbe))
	}
	return factory
}

// logLoadProbeConfig states in ONE line per combination what the assignment-time
// occupancy probe will actually do, and says so LOUDLY when a knob is inert -
// the same doctrine as retry_on_status and sticky_sessions key_sources: a
// silently inert knob reads as "the feature is broken" in the field.
//
// Both half-configured shapes warn: load_probe enabled on a balancer that never
// probes, and "least-load" selected with the probe disabled (it then runs the
// priority/SWRR path and looks like a balancer that does nothing). Only the
// armed combination logs the "armed" Info - reporting success for a probe that
// is wired to nothing is worse than silence.
//
// Off by default: with load_probe disabled and a non-least-load balancer
// nothing is logged and the gateway behaves exactly as a build without the
// feature. SetLoadProber must run BEFORE the balancer factory's Create (the
// registration closure reads the prober there).
func (s *ProxyServiceWrapper) logLoadProbeConfig() {
	leastLoad := s.config.LoadBalancer == balancer.DefaultBalancerLeastLoad
	if !s.config.LoadProbe.Enabled {
		if leastLoad {
			s.logger.Warn("proxy.load_balancer is \"least-load\" but proxy.load_probe.enabled is false: no occupancy signal exists, so it runs as \"priority\" (SWRR) until load_probe.enabled",
				"load_balancer", s.config.LoadBalancer)
		}
		return
	}
	if !leastLoad {
		s.logger.Warn("proxy.load_probe.enabled has no effect unless proxy.load_balancer is \"least-load\"",
			"load_balancer", s.config.LoadBalancer)
		return
	}
	s.logger.Info("Load probe armed",
		"timeout", s.config.LoadProbe.Timeout,
		"fan_out_budget", s.config.LoadProbe.FanOutBudget,
		"health_path", s.config.LoadProbe.HealthPath,
		"active_field", s.config.LoadProbe.ActiveField)
}

// applyStickySessions wraps the current load balancer with KV-cache affinity routing
// when sticky sessions are enabled. It also wires the purge function into the discovery
// service immediately after the wrapper is assigned, ensuring the write of stickyWrapper
// happens-before the health-checker goroutine can observe it via PurgeDeadEndpoints.
func (s *ProxyServiceWrapper) applyStickySessions() {
	if !s.config.StickySessions.Enabled {
		return
	}
	sw := balancer.NewStickySessionWrapper(s.loadBalancer, s.config.StickySessions)
	sw.Start()
	s.stickyWrapper = sw
	s.loadBalancer = sw
	s.logger.Info("Sticky session affinity enabled",
		"idle_ttl_seconds", s.config.StickySessions.IdleTTLSeconds,
		"max_sessions", s.config.StickySessions.MaxSessions,
		"key_sources", s.config.StickySessions.KeySources)

	// stickyWrapper is now initialised — register the purge hook so that sticky
	// session entries for dead backends are evicted on health-check failure rather
	// than waiting for the session TTL to expire.
	if s.discoverySvc != nil {
		s.discoverySvc.SetPurgeDeadEndpointsFn(s.PurgeDeadEndpoints)
	}
}
