package dashboard

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/thushan/olla/internal/router"
)

// Extras (config.json, observe.json) live INSIDE the /internal/ui/ subtree
// as exact patterns. ServeMux must prefer them over the static handler, and
// they must carry the identical CIDR policy - widening the dashboard zone
// widens them, forgetting them is impossible.
func TestRegisterRoutes_ExtrasGatedBySamePolicy(t *testing.T) {
	t.Parallel()

	reg := router.NewRouteRegistry(stubLogger{})
	reached := false
	extras := map[string]http.Handler{"/internal/ui/config.json": reachedHandler(&reached)}
	require.True(t, RegisterRoutes(reg, loopbackConfig(), nil, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}), extras))

	mux := http.NewServeMux()
	reg.WireUp(mux)

	allowed := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/internal/ui/config.json", nil)
	req.RemoteAddr = "127.0.0.1:54321"
	req.Host = "localhost"
	mux.ServeHTTP(allowed, req)
	assert.Equal(t, http.StatusOK, allowed.Code, "exact extra pattern must beat the subtree handler")
	assert.True(t, reached)

	denied := httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/internal/ui/config.json", nil)
	req.RemoteAddr = "203.0.113.9:54321"
	req.Host = "evil.example"
	mux.ServeHTTP(denied, req)
	assert.Equal(t, http.StatusForbidden, denied.Code, "extras ride the dashboard CIDR policy")
}

func TestRegisterRoutes_ExtrasCollisionSkipped(t *testing.T) {
	t.Parallel()

	reg := router.NewRouteRegistry(stubLogger{})
	reg.RegisterWithMethod("/internal/ui/observe.json", http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), "collision sentinel", "GET")
	require.True(t, RegisterRoutes(reg, loopbackConfig(), nil, http.NotFoundHandler(), map[string]http.Handler{
		"/internal/ui/observe.json": http.NotFoundHandler(),
	}))
	require.Equal(t, "collision sentinel", reg.GetRoutes()["/internal/ui/observe.json"].Description, "a colliding extra must not overwrite the earlier registration")
}
