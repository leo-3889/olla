package handlers

// Regression coverage for mountDashboard's enabled gate: dashboard.Handler()
// (walking and SHA-256-hashing the whole embedded SPA bundle to build its
// per-asset cache) must only run when the dashboard is actually enabled. It
// used to be evaluated unconditionally as a call argument before
// dashboard.RegisterRoutes checked cfg.Enabled internally, so a disabled
// dashboard paid that cost for nothing.

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/thushan/olla/internal/app/handlers/dashboard"
	"github.com/thushan/olla/internal/config"
	"github.com/thushan/olla/internal/router"
)

// TestMountDashboard_Disabled_DoesNotConstructHandler substitutes
// dashboardHandlerFactory with a spy to prove it is never invoked when the
// dashboard is disabled, closing the gap a purely route-table-based
// assertion could not: the old code produced the same empty route table
// whether or not it had wastefully built the handler first.
func TestMountDashboard_Disabled_DoesNotConstructHandler(t *testing.T) {
	orig := dashboardHandlerFactory
	t.Cleanup(func() { dashboardHandlerFactory = orig })

	called := false
	dashboardHandlerFactory = func() http.Handler {
		called = true
		return http.NotFoundHandler()
	}

	reg := router.NewRouteRegistry(&mockStyledLogger{})
	app := &Application{
		Config:        &config.Config{Dashboard: config.DashboardConfig{Enabled: false}},
		logger:        &mockStyledLogger{},
		routeRegistry: reg,
	}
	app.mountDashboard()

	if called {
		t.Fatal("dashboard handler factory must not be invoked when the dashboard is disabled")
	}
	if _, exists := reg.GetRoutes()[dashboard.DashboardRoute]; exists {
		t.Fatal("disabled dashboard must not register /internal/ui/")
	}
}

// TestMountDashboard_Enabled_ConstructsHandler is the positive counterpart:
// an enabled dashboard does invoke the factory exactly once and mounts the
// route, so the guard above isn't trivially satisfied by mountDashboard's
// enabled branch never running at all.
func TestMountDashboard_Enabled_ConstructsHandler(t *testing.T) {
	orig := dashboardHandlerFactory
	t.Cleanup(func() { dashboardHandlerFactory = orig })

	calls := 0
	dashboardHandlerFactory = func() http.Handler {
		calls++
		return http.NotFoundHandler()
	}

	cfg := config.DashboardConfig{
		Enabled: true,
		AccessPolicy: config.AccessPolicyConfig{
			AllowedCIDRs: []string{"127.0.0.0/8", "::1/128"},
			AllowedHosts: []string{"localhost", "127.0.0.1", "[::1]"},
		},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("validate dashboard cfg: %v", err)
	}

	reg := router.NewRouteRegistry(&mockStyledLogger{})
	app := &Application{
		Config:        &config.Config{Dashboard: cfg},
		logger:        &mockStyledLogger{},
		routeRegistry: reg,
	}
	app.mountDashboard()

	if calls != 1 {
		t.Fatalf("dashboard handler factory calls = %d, want 1", calls)
	}
	if _, exists := reg.GetRoutes()[dashboard.DashboardRoute]; !exists {
		t.Fatal("enabled dashboard must register /internal/ui/")
	}
}

// TestSlashlessDashboardRoute_CollisionSkipsMount mirrors
// TestDashboardRoute_CollisionSkipsMount for the exact slashless path
// (/internal/ui, no trailing slash). RegisterRoutes claims this path too (the
// redirect-to-canonical registration), and registerWithMethod stores routes
// in a plain map keyed by pattern, so a pre-existing registration here would
// otherwise be silently overwritten - last write wins, no panic - rather than
// caught by the collision guard.
func TestSlashlessDashboardRoute_CollisionSkipsMount(t *testing.T) {
	t.Parallel()

	reg := router.NewRouteRegistry(&mockStyledLogger{})
	const collisionBody = "PRE-EXISTING-SLASHLESS"
	reg.RegisterWithMethod(dashboard.SlashlessDashboardRoute, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(collisionBody))
	}, "collision sentinel", http.MethodGet)

	app := &Application{
		Config:        &config.Config{Dashboard: enabledDashboardCfg(t)},
		logger:        &mockStyledLogger{},
		routeRegistry: reg,
	}
	app.registerRoutes()

	mux := http.NewServeMux()
	reg.WireUp(mux)

	req := httptest.NewRequest(http.MethodGet, dashboard.SlashlessDashboardRoute, nil)
	req.RemoteAddr = "127.0.0.1:54321"
	req.Host = "localhost"
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("collision: status=%d, want 200 from sentinel handler", rec.Code)
	}
	if rec.Body.String() != collisionBody {
		t.Errorf("collision: body=%q, want sentinel %q (dashboard overwrote it)", rec.Body.String(), collisionBody)
	}

	// The trailing-slash subtree must also stay unmounted: the collision
	// guard skips the whole RegisterRoutes call, not just the colliding path.
	if _, exists := reg.GetRoutes()[dashboard.DashboardRoute]; exists {
		t.Errorf("collision on slashless route must also prevent %s from mounting", dashboard.DashboardRoute)
	}
}

// TestInternalAPIGate is the wiring behind dashboard.gate_internal_api
// (upstream issue #214): enabled, the operational API answers through the
// SAME AccessPolicy as /internal/ui/*; disabled (the default), handlers
// pass through untouched so existing monitoring scrapes from unlisted
// hosts keep working.
func TestInternalAPIGate(t *testing.T) {
	newApp := func(gate bool) *Application {
		d := config.DashboardConfig{
			Enabled:         true,
			GateInternalAPI: gate,
			AccessPolicy:    config.AccessPolicyConfig{AllowedCIDRs: []string{"10.0.0.0/8"}},
		}
		// Validate parses the CIDRs into the cache ParsedCIDRs reads;
		// un-parsed policy is deny-all by design (fail closed).
		if err := d.Validate(); err != nil {
			t.Fatalf("validate: %v", err)
		}
		return &Application{Config: &config.Config{Dashboard: d}}
	}
	hello := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })

	req := httptest.NewRequest(http.MethodGet, "/internal/status", nil)
	req.RemoteAddr = "10.1.2.3:1234"
	req.Host = "127.0.0.1:40114"
	blocked := httptest.NewRequest(http.MethodGet, "/internal/status", nil)
	blocked.RemoteAddr = "192.0.2.7:1234"
	blocked.Host = "127.0.0.1:40114"

	off := newApp(false).internalAPIGate()(hello)
	for _, r := range []*http.Request{req, blocked} {
		rec := httptest.NewRecorder()
		off.ServeHTTP(rec, r)
		if rec.Code != http.StatusOK {
			t.Fatalf("gate off: %s got %d, want 200 (default must not break scrapes)", r.RemoteAddr, rec.Code)
		}
	}

	on := newApp(true).internalAPIGate()(hello)
	rec := httptest.NewRecorder()
	on.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("gate on: allowed ip got %d, want 200", rec.Code)
	}
	rec = httptest.NewRecorder()
	on.ServeHTTP(rec, blocked)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("gate on: unlisted ip got %d, want 403", rec.Code)
	}
}

// TestInternalAPIGate_RouteWiring proves the gate is applied at the routing
// table, not just in isolation: with the knob on, gated operational routes
// answer 403 from a CIDR the policy rejects, /internal/health (the LB
// probe) stays exempt, an allowed source still reaches its handler, and
// with the knob off (shipped default) the same disallowed source is not
// rejected anywhere - byte-identical to the pre-gate behaviour.
//
// Only handlers that answer from a bare Application are probed here
// (sticky stats, process, version); the gate runs before any handler, so
// one probe per gated family proves the wrapper position.
func TestInternalAPIGate_RouteWiring(t *testing.T) {
	newMux := func(t *testing.T, gate bool) http.Handler {
		t.Helper()
		d := config.DashboardConfig{
			Enabled:         true,
			GateInternalAPI: gate,
			AccessPolicy:    config.AccessPolicyConfig{AllowedCIDRs: []string{"127.0.0.0/8", "::1/128"}},
		}
		if err := d.Validate(); err != nil {
			t.Fatalf("validate: %v", err)
		}
		reg := router.NewRouteRegistry(&mockStyledLogger{})
		app := &Application{
			Config:        &config.Config{Dashboard: d},
			logger:        &mockStyledLogger{},
			routeRegistry: reg,
		}
		app.registerRoutes()
		mux := http.NewServeMux()
		reg.WireUp(mux)
		return mux
	}
	get := func(h http.Handler, path, ip string) int {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.RemoteAddr = ip + ":33333"
		req.Host = "127.0.0.1:40114"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}

	on := newMux(t, true)
	for _, path := range []string{"/internal/stats/sticky", "/internal/process", "/version"} {
		if code := get(on, path, "192.0.2.7"); code != http.StatusForbidden {
			t.Errorf("gate on: %s from unlisted ip got %d, want 403", path, code)
		}
	}
	if code := get(on, "/internal/health", "192.0.2.7"); code != http.StatusOK {
		t.Errorf("gate on: /internal/health from unlisted ip got %d, want 200 (LB probes must stay exempt)", code)
	}
	if code := get(on, "/internal/stats/sticky", "127.0.0.1"); code != http.StatusOK {
		t.Errorf("gate on: /internal/stats/sticky from allowed ip got %d, want 200", code)
	}

	off := newMux(t, false)
	for _, path := range []string{"/internal/stats/sticky", "/internal/process", "/version", "/internal/health"} {
		if code := get(off, path, "192.0.2.7"); code == http.StatusForbidden {
			t.Errorf("gate off: %s from unlisted ip got 403, want ungated passthrough (shipped default)", path)
		}
	}
}
