package proxy

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/thushan/olla/internal/adapter/proxy/core"
	"github.com/thushan/olla/internal/adapter/proxy/olla"
	"github.com/thushan/olla/internal/core/domain"
)

// TestDisableCircuitBreaker_PerEndpointIsolation proves that an endpoint with
// DisableCircuitBreaker=true never gets breaker refusals, while a sibling with
// the default (false) trips after the threshold.
//
// Failure mode: connection-refused (closed test server). Each RoundTrip
// returns a net error which triggers cb.RecordFailure. After
// circuitBreakerThreshold (5) failures the flag=false endpoint is refused with
// ErrCircuitBreakerOpen; the flag=true endpoint keeps receiving the raw
// transport error on every attempt.
func TestDisableCircuitBreaker_PerEndpointIsolation(t *testing.T) {
	// Start two servers and immediately close them so all connections are refused.
	srvOptedOut := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srvNormal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	optedOutURL := srvOptedOut.URL
	normalURL := srvNormal.URL
	srvOptedOut.Close()
	srvNormal.Close()

	epOptedOut := &domain.Endpoint{
		Name:                  "opted-out",
		URL:                   mustParseURL(optedOutURL),
		URLString:             optedOutURL,
		Status:                domain.StatusHealthy,
		Type:                  domain.ProfileOllama,
		DisableCircuitBreaker: true,
	}
	epNormal := &domain.Endpoint{
		Name:      "normal",
		URL:       mustParseURL(normalURL),
		URLString: normalURL,
		Status:    domain.StatusHealthy,
		Type:      domain.ProfileOllama,
		// DisableCircuitBreaker defaults to false
	}

	collector := createTestStatsCollector()
	discovery := &mockDiscoveryService{endpoints: []*domain.Endpoint{epOptedOut, epNormal}}
	selector := newMockEndpointSelector(collector)
	config := &olla.Configuration{}
	config.ResponseTimeout = 2 * 1000000000 // 2s
	config.ReadTimeout = 1000000000         // 1s
	config.StreamBufferSize = 1024
	config.MaxIdleConns = 2
	config.IdleConnTimeout = 1000000000 // 1s

	proxy, err := olla.NewService(discovery, selector, config, collector, nil, createTestLogger())
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	const attempts = 10 // well above circuitBreakerThreshold=5

	// --- Normal endpoint (flag=false): breaker must trip ---
	var normalCBTripped bool
	for range attempts {
		req, stats, rlog := createTestRequestWithBody("GET", "/api/test", "")
		w := httptest.NewRecorder()
		err := proxy.ProxyRequestToEndpoints(req.Context(), w, req, []*domain.Endpoint{epNormal}, stats, rlog)
		if errors.Is(err, core.ErrCircuitBreakerOpen) {
			normalCBTripped = true
			break
		}
	}
	if !normalCBTripped {
		t.Fatal("flag=false endpoint: circuit breaker never tripped after 10 connection-refused attempts")
	}

	// --- Opted-out endpoint (flag=true): breaker must NOT trip ---
	for i := range attempts {
		req, stats, rlog := createTestRequestWithBody("GET", "/api/test", "")
		w := httptest.NewRecorder()
		err := proxy.ProxyRequestToEndpoints(req.Context(), w, req, []*domain.Endpoint{epOptedOut}, stats, rlog)
		if errors.Is(err, core.ErrCircuitBreakerOpen) {
			t.Fatalf("flag=true endpoint: breaker tripped at attempt %d — opt-out not effective", i+1)
		}
		// Every attempt must produce a raw connection error, not a breaker refusal.
		if err == nil {
			t.Fatalf("flag=true endpoint: attempt %d succeeded (server should be closed)", i+1)
		}
	}
}

func mustParseURL(raw string) *url.URL {
	u, err := url.Parse(raw)
	if err != nil {
		panic(err)
	}
	return u
}
