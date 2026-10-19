package services

import (
	"context"
	"net/url"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/thushan/olla/internal/core/domain"
)

// statusSplitRepo records WHICH repository filter the dispatch candidate source
// uses. GetHealthy is the strict healthy-only filter; GetRoutable admits
// healthy/busy/warming. The two sets are deliberately disjoint so a test cannot
// pass against the wrong filter.
type statusSplitRepo struct {
	endpoints []*domain.Endpoint
}

func (r *statusSplitRepo) GetAll(context.Context) ([]*domain.Endpoint, error) {
	return r.endpoints, nil
}

func (r *statusSplitRepo) GetHealthy(context.Context) ([]*domain.Endpoint, error) {
	var out []*domain.Endpoint
	for _, e := range r.endpoints {
		if e.Status == domain.StatusHealthy {
			out = append(out, e)
		}
	}
	return out, nil
}

func (r *statusSplitRepo) GetRoutable(context.Context) ([]*domain.Endpoint, error) {
	var out []*domain.Endpoint
	for _, e := range r.endpoints {
		if e.Status.IsRoutable() {
			out = append(out, e)
		}
	}
	return out, nil
}

func (r *statusSplitRepo) UpdateEndpoint(context.Context, *domain.Endpoint) error { return nil }
func (r *statusSplitRepo) Exists(_ context.Context, u *url.URL) bool              { return true }

func routedNames(t *testing.T, svc *DiscoveryService) []string {
	t.Helper()
	endpoints, err := svc.GetHealthyEndpoints(context.Background())
	require.NoError(t, err)
	names := make([]string, 0, len(endpoints))
	for _, e := range endpoints {
		names = append(names, e.Name)
	}
	return names
}

// GetHealthyEndpoints is the proxy/alias dispatch candidate source
// (handler_proxy.go -> filterEndpointsByProfile -> resolveAliasEndpoints). It must
// return the ROUTABLE set, not healthy-only.
//
// Regression: with a healthy-only filter, load-aware Busy grading
// (proxy.busy_queue_depth) deleted the graded endpoint from routing. Live
// 2026-10-19: alias "fast" carried one live node (its twin was offline); every
// time the engine queue reached the threshold the node was graded Busy,
// resolveAliasEndpoints found 0 healthy candidates and strict routing answered
// 404 model_not_found for the whole ~30s probe cycle (15 client-visible 404s in
// 25 minutes). Busy must reach the balancer, where it carries weight 0.3.
func TestDiscoveryService_DispatchCandidates_IncludeBusyAndWarming(t *testing.T) {
	svc := &DiscoveryService{endpointRepo: &statusSplitRepo{endpoints: []*domain.Endpoint{
		{Name: "healthy", Status: domain.StatusHealthy},
		{Name: "busy", Status: domain.StatusBusy},
		{Name: "warming", Status: domain.StatusWarming},
		{Name: "offline", Status: domain.StatusOffline},
		{Name: "unhealthy", Status: domain.StatusUnhealthy},
		{Name: "ratelimited", Status: domain.StatusRateLimited},
		{Name: "configerror", Status: domain.StatusConfigError},
	}}}

	require.ElementsMatch(t, []string{"healthy", "busy", "warming"}, routedNames(t, svc),
		"dispatch candidates must be exactly the routable statuses")
}

// A Busy endpoint must survive as a candidate together with a healthy twin, i.e.
// the balancer - not the candidate filter - decides how much traffic it gets.
func TestDiscoveryService_DispatchCandidates_BusySingleCarrierIsNotDropped(t *testing.T) {
	svc := &DiscoveryService{endpointRepo: &statusSplitRepo{endpoints: []*domain.Endpoint{
		{Name: "sole-carrier", Status: domain.StatusBusy},
		{Name: "other-alias-node", Status: domain.StatusOffline},
	}}}

	require.Equal(t, []string{"sole-carrier"}, routedNames(t, svc),
		"the only live carrier of an alias must stay routable when graded Busy")
}

func TestDiscoveryService_DispatchCandidates_NilRepo(t *testing.T) {
	svc := &DiscoveryService{}
	_, err := svc.GetHealthyEndpoints(context.Background())
	require.Error(t, err)
}
