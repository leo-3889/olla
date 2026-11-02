package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/thushan/olla/internal/adapter/proxy/olla"
	"github.com/thushan/olla/internal/core/domain"
)

// TestRetryOnStatus_AutoProfile_RotatesRefusal is the engine-level integration
// test for proxy.retry_on_status on the auto profile: twin-a serves a 503
// refusal with ndjson content-type (auto profile => isStreaming=true =>
// flush per Write), twin-b serves a 200. The gate must hold the refusal
// through the streaming flushes, rotate to twin-b, and deliver twin-b's
// response.
//
// RED on code where the gate's Flush() commits a held refusal (MAJOR-1)
// because auto profile flushes after every Write.
func TestRetryOnStatus_AutoProfile_RotatesRefusal(t *testing.T) {
	// twin-a: 503 refusal with ndjson content-type (auto profile streams this)
	twinA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.Header().Set("X-Backend-Refused", "true")
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte(`{"error":"overloaded"}`))
	}))
	defer twinA.Close()

	// twin-b: healthy 200
	twinB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Backend-Served", "true")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"from":"twin-b"}`))
	}))
	defer twinB.Close()

	endpointA := createTestEndpoint("twin-a", twinA.URL, domain.StatusHealthy)
	endpointB := createTestEndpoint("twin-b", twinB.URL, domain.StatusHealthy)

	// Auto profile (empty Profile = auto) with retry_on_status=[503]
	proxyCfg := &olla.Configuration{}
	proxyCfg.RetryOnStatus = []int{http.StatusServiceUnavailable}
	// Profile left empty => auto (default)

	collector := createTestStatsCollector()
	discovery := &mockDiscoveryService{endpoints: []*domain.Endpoint{endpointA, endpointB}}
	selector := &rotatingSelector{endpoints: []*domain.Endpoint{endpointA, endpointB}}

	proxy, err := olla.NewService(discovery, selector, proxyCfg, collector, nil, createTestLogger())
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	req, stats, rlog := createTestRequestWithBody("POST", "/api/generate", `{"model":"test"}`)
	w := httptest.NewRecorder()

	err = proxy.ProxyRequestToEndpoints(req.Context(), w, req, []*domain.Endpoint{endpointA, endpointB}, stats, rlog)
	if err != nil {
		t.Fatalf("ProxyRequestToEndpoints: %v", err)
	}

	// PRIMARY: client must see twin-b's 200, not twin-a's 503
	if w.Code != http.StatusOK {
		t.Fatalf("MAJOR-1: expected 200 from twin-b, got %d — gate flushed refusal on auto profile", w.Code)
	}
	if !strings.Contains(w.Body.String(), "twin-b") {
		t.Fatalf("MAJOR-1: expected twin-b body, got %q", w.Body.String())
	}

	// MAJOR-2: rotated-away 503 response headers must not leak
	if w.Header().Get("X-Backend-Refused") != "" {
		t.Errorf("MAJOR-2: rotated-away header X-Backend-Refused leaked into response: %q",
			w.Header().Get("X-Backend-Refused"))
	}

	// MAJOR-3: success accounting must count ONLY twin-b
	proxyStats := proxy.GetProxyStats()
	if proxyStats.SuccessfulRequests != 1 {
		t.Errorf("MAJOR-3: expected 1 SuccessfulRequests (twin-b only), got %d (rotated-away twin-a 503 booked as success)",
			proxyStats.SuccessfulRequests)
	}
}

// TestRetryOnStatus_StandardProfile_IsolatesMajor3 verifies MAJOR-3 in
// isolation (standard profile, no flush on gate): the rotated-away twin-a
// must not book success stats.
func TestRetryOnStatus_StandardProfile_IsolatesMajor3(t *testing.T) {
	twinA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte(`{"error":"overloaded"}`))
	}))
	defer twinA.Close()

	twinB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"from":"twin-b"}`))
	}))
	defer twinB.Close()

	endpointA := createTestEndpoint("twin-a", twinA.URL, domain.StatusHealthy)
	endpointB := createTestEndpoint("twin-b", twinB.URL, domain.StatusHealthy)

	proxyCfg := &olla.Configuration{}
	proxyCfg.RetryOnStatus = []int{http.StatusServiceUnavailable}
	proxyCfg.Profile = "standard" // no flush => isolates MAJOR-3 from MAJOR-1

	collector := createTestStatsCollector()
	discovery := &mockDiscoveryService{endpoints: []*domain.Endpoint{endpointA, endpointB}}
	selector := &rotatingSelector{endpoints: []*domain.Endpoint{endpointA, endpointB}}

	proxy, err := olla.NewService(discovery, selector, proxyCfg, collector, nil, createTestLogger())
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	req, stats, rlog := createTestRequestWithBody("GET", "/api/generate", "")
	w := httptest.NewRecorder()

	err = proxy.ProxyRequestToEndpoints(req.Context(), w, req, []*domain.Endpoint{endpointA, endpointB}, stats, rlog)
	if err != nil {
		t.Fatalf("ProxyRequestToEndpoints: %v", err)
	}

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	proxyStats := proxy.GetProxyStats()
	if proxyStats.SuccessfulRequests != 1 {
		t.Errorf("MAJOR-3: expected 1 SuccessfulRequests (twin-b only), got %d (twin-a 503 counted as success)",
			proxyStats.SuccessfulRequests)
	}
}

// rotatingSelector hands out endpoints in order from the slice it's given.
type rotatingSelector struct {
	endpoints []*domain.Endpoint
	cursor    int
}

func (s *rotatingSelector) Select(_ context.Context, eps []*domain.Endpoint) (*domain.Endpoint, error) {
	if len(eps) == 0 {
		return nil, context.DeadlineExceeded
	}
	ep := eps[s.cursor%len(eps)]
	s.cursor++
	return ep, nil
}
func (s *rotatingSelector) Name() string                            { return "rotating" }
func (s *rotatingSelector) IncrementConnections(_ *domain.Endpoint) {}
func (s *rotatingSelector) DecrementConnections(_ *domain.Endpoint) {}
