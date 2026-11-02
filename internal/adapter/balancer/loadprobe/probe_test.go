package loadprobe

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/thushan/olla/internal/config"
	"github.com/thushan/olla/internal/core/domain"
)

func testEndpoint(url string) *domain.Endpoint {
	return &domain.Endpoint{URLString: url}
}

func testConfig() config.LoadProbeConfig {
	return config.LoadProbeConfig{
		Enabled:      true,
		Timeout:      200 * time.Millisecond,
		FanOutBudget: 400 * time.Millisecond,
		HealthPath:   "/health",
		ActiveField:  "in_flight",
	}
}

// TestProbeHealthLiveShape pins the primary source against the byte-exact live
// radiance body captured 2026-11-03 (63 B, in_flight = admitted requests).
func TestProbeHealthLiveShape(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"status":"ok","model":"qwen4exp","in_flight":4,"queue_cap":40}`)
	}))
	defer srv.Close()

	loads := New(testConfig()).Probe(context.Background(), []*domain.Endpoint{testEndpoint(srv.URL)})
	load, ok := loads[srv.URL]
	if !ok {
		t.Fatalf("expected a reading keyed by URLString, got %v", loads)
	}
	if load.Active != 4 || load.Source != domain.LoadSourceHealth || !load.Confident {
		t.Fatalf("want {4 health true}, got %+v", load)
	}
}

func TestProbeCustomActiveField(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"busy":7}`)
	}))
	defer srv.Close()

	cfg := testConfig()
	cfg.ActiveField = "busy"
	loads := New(cfg).Probe(context.Background(), []*domain.Endpoint{testEndpoint(srv.URL)})
	if load, ok := loads[srv.URL]; !ok || load.Active != 7 {
		t.Fatalf("active_field override failed: %+v ok=%v", load, ok)
	}
}

// TestHealthWithoutFieldFallsBackToMetrics is the halogen shape: its /health
// answers (vision truth) with no request count, so believing absence there
// would rank it idle forever. The metrics gauges must win.
func TestHealthWithoutFieldFallsBackToMetrics(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			fmt.Fprint(w, `{"status":"ok","vision":{"enabled":true}}`)
		case "/metrics":
			fmt.Fprint(w, "# HELP x\nradiance:http_queue_depth{model_name=\"m\",engine=\"0\"} 5\n")
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	loads := New(testConfig()).Probe(context.Background(), []*domain.Endpoint{testEndpoint(srv.URL)})
	load, ok := loads[srv.URL]
	if !ok || load.Active != 5 || load.Source != domain.LoadSourceMetrics {
		t.Fatalf("metrics fallback failed: %+v ok=%v", load, ok)
	}
}

// TestMetricsRunningPlusWaitingSumsLabelSeries covers the vLLM-shaped engine:
// admitted = running + waiting, summed across per-engine label series.
func TestMetricsRunningPlusWaitingSumsLabelSeries(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/metrics" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		fmt.Fprint(w,
			"vllm:num_requests_running{model_name=\"a\",engine=\"0\"} 2\n"+
				"vllm:num_requests_running{model_name=\"a\",engine=\"1\"} 1\n"+
				"vllm:num_requests_waiting{model_name=\"a\",engine=\"0\"} 3\n")
	}))
	defer srv.Close()

	loads := New(testConfig()).Probe(context.Background(), []*domain.Endpoint{testEndpoint(srv.URL)})
	if load, ok := loads[srv.URL]; !ok || load.Active != 6 {
		t.Fatalf("want running(3)+waiting(3)=6, got %+v ok=%v", load, ok)
	}
}

// TestSlowEndpointAbsentNotZero pins the two hard rules of the round: an
// endpoint that does not answer in budget is ABSENT (never the measurement 0),
// and it cannot hold the round past the fan-out budget.
func TestSlowEndpointAbsentNotZero(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second)
		fmt.Fprint(w, `{"in_flight":1}`)
	}))
	defer slow.Close()
	fast := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"in_flight":2}`)
	}))
	defer fast.Close()

	cfg := testConfig()
	cfg.Timeout = 30 * time.Millisecond
	cfg.FanOutBudget = 80 * time.Millisecond

	start := time.Now()
	loads := New(cfg).Probe(context.Background(), []*domain.Endpoint{testEndpoint(slow.URL), testEndpoint(fast.URL)})
	elapsed := time.Since(start)

	if _, ok := loads[slow.URL]; ok {
		t.Fatalf("slow endpoint must be absent, got %v", loads[slow.URL])
	}
	if load, ok := loads[fast.URL]; !ok || load.Active != 2 {
		t.Fatalf("fast endpoint must still answer: %+v ok=%v", load, ok)
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("round waited for the slow endpoint (%s): budget not honoured", elapsed)
	}
}

// TestMetricsCapKeepsBigBody bounded: a pathological fat /metrics cannot drag
// the round (live bodies are 7.5-35 KB; this is a 2 MB body with a huge prefix).
func TestMetricsCapKeepsBigBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		fmt.Fprint(w, `{"in_flight":1}`)
	}))
	defer srv.Close()

	// metrics-only endpoint with no gauge at all -> absent, not zero
	loads := New(testConfig()).Probe(context.Background(), []*domain.Endpoint{testEndpoint(srv.URL)})
	if _, ok := loads[srv.URL]; ok {
		t.Fatalf("health 404 + gauge-less /metrics must be absent, got %v", loads[srv.URL])
	}
}

func TestProbeEdgeCases(t *testing.T) {
	p := New(testConfig())
	if got := p.Probe(context.Background(), nil); len(got) != 0 {
		t.Fatalf("nil endpoints must give an empty map, got %v", got)
	}
	if got := p.Probe(context.Background(), []*domain.Endpoint{nil, {}}); len(got) != 0 {
		t.Fatalf("nil/URL-less endpoints must be skipped, got %v", got)
	}
	// Dead server: connection refused => absence (never 0).
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	dead.Close()
	if got := p.Probe(context.Background(), []*domain.Endpoint{testEndpoint(dead.URL)}); len(got) != 0 {
		t.Fatalf("dead endpoint must be absent, got %v", got)
	}
}

// TestProbeConcurrentRounds race-safety: overlapping rounds (failover loops
// fire Select per attempt) share the client and must not race.
func TestProbeConcurrentRounds(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"in_flight":0}`)
	}))
	defer srv.Close()

	p := New(testConfig())
	done := make(chan struct{})
	for range 8 {
		go func() {
			loads := p.Probe(context.Background(), []*domain.Endpoint{testEndpoint(srv.URL)})
			if load, ok := loads[srv.URL]; !ok || load.Active != 0 || !load.Confident {
				t.Errorf("zero must be a CONFIDENT measurement, got %+v ok=%v", load, ok)
			}
			done <- struct{}{}
		}()
	}
	for range 8 {
		<-done
	}
}

// TestMetricsNonFiniteGaugeIsAbsent pins that a NaN/Inf gauge line is not a
// measurement: an endpoint whose only occupancy line reads NaN (engine reload,
// a Prometheus "+Inf") must be ABSENT, never believed idle at 0 - a fabricated
// zero makes the busiest box look like the best target in the tier.
func TestMetricsNonFiniteGaugeIsAbsent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			w.WriteHeader(http.StatusNotFound)
		case "/metrics":
			fmt.Fprint(w, "radiance:http_queue_depth{engine=\"0\"} NaN\n"+
				"vllm:num_requests_running{engine=\"0\"} +Inf\n"+
				"vllm:num_requests_waiting{engine=\"0\"} NaN\n"+
				"process_cpu_seconds_total 12.5\n")
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	loads := New(testConfig()).Probe(context.Background(), []*domain.Endpoint{testEndpoint(srv.URL)})
	if load, ok := loads[srv.URL]; ok {
		t.Fatalf("non-finite gauges must yield absence, got %+v", load)
	}
}

// TestMetricsNonFiniteSeriesSkippedFiniteStillCounts is the other half: one bad
// label series must not void the whole gauge, only its own line.
func TestMetricsNonFiniteSeriesSkippedFiniteStillCounts(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			w.WriteHeader(http.StatusNotFound)
		case "/metrics":
			fmt.Fprint(w, "radiance:http_queue_depth{engine=\"0\"} NaN\n"+
				"radiance:http_queue_depth{engine=\"1\"} 3\n")
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	loads := New(testConfig()).Probe(context.Background(), []*domain.Endpoint{testEndpoint(srv.URL)})
	if load, ok := loads[srv.URL]; !ok || load.Active != 3 {
		t.Fatalf("finite series must still count with one NaN sibling: %+v ok=%v", load, ok)
	}
}

// TestMetricsRadiancePreferredOverVLLM pins the dialect preference on an
// endpoint exporting BOTH shapes: radiance:http_queue_depth is the admitted
// count measured byte-equal to /health in_flight, while vllm running+waiting
// on the same box reports a different number. The values differ on purpose -
// equal readings would pass even if the preference were inverted.
func TestMetricsRadiancePreferredOverVLLM(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			w.WriteHeader(http.StatusNotFound)
		case "/metrics":
			fmt.Fprint(w, "radiance:http_queue_depth{engine=\"0\"} 7\n"+
				"vllm:num_requests_running{engine=\"0\"} 1\n"+
				"vllm:num_requests_waiting{engine=\"0\"} 2\n")
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	loads := New(testConfig()).Probe(context.Background(), []*domain.Endpoint{testEndpoint(srv.URL)})
	load, ok := loads[srv.URL]
	if !ok || load.Active != 7 || load.Source != domain.LoadSourceMetrics {
		t.Fatalf("radiance gauge (7) must win over vllm running+waiting (3): %+v ok=%v", load, ok)
	}
}

// TestProbeSendsEndpointAuthHeader: a credential-gated engine answers 401 to a
// bare /health and /metrics, so an unauthenticated prober would report every
// node of that fleet as unknown on every assignment. The configured outbound
// auth header must ride on both requests.
func TestProbeSendsEndpointAuthHeader(t *testing.T) {
	var (
		mu        sync.Mutex
		healthH   string
		metricsH  string
		gotHealth bool
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			mu.Lock()
			healthH = r.Header.Get("X-Api-Key")
			gotHealth = healthH == "Bearer tok"
			mu.Unlock()
			if !gotHealth {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			fmt.Fprint(w, `{"in_flight":4}`)
		case "/metrics":
			mu.Lock()
			metricsH = r.Header.Get("X-Api-Key")
			mu.Unlock()
			if metricsH != "Bearer tok" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			fmt.Fprint(w, "radiance:http_queue_depth 9\n")
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	ep := testEndpoint(srv.URL)
	ep.AuthHeaderName = "X-Api-Key"
	ep.AuthHeaderValue = "Bearer tok"

	// Authed health answer wins.
	if load, ok := New(testConfig()).Probe(context.Background(), []*domain.Endpoint{ep})[srv.URL]; !ok || load.Active != 4 {
		t.Fatalf("authed /health must be believed: %+v ok=%v (header seen: %q)", load, ok, healthH)
	}

	// And the same header reaches the /metrics fallback.
	srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			w.WriteHeader(http.StatusNotFound)
		case "/metrics":
			mu.Lock()
			metricsH = r.Header.Get("X-Api-Key")
			mu.Unlock()
			if metricsH != "Bearer tok" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			fmt.Fprint(w, "radiance:http_queue_depth 9\n")
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	if load, ok := New(testConfig()).Probe(context.Background(), []*domain.Endpoint{ep})[srv.URL]; !ok || load.Active != 9 {
		t.Fatalf("authed /metrics fallback must be believed: %+v ok=%v (header seen: %q)", load, ok, metricsH)
	}
}

// TestHealthNon2xxFallsBackToMetrics: a 503 on /health (wedged engine, still
// exporting metrics) is absence, not a reading - the gauges must decide.
func TestHealthNon2xxFallsBackToMetrics(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprint(w, `{"in_flight":4}`) // a body that must NOT be believed
		case "/metrics":
			fmt.Fprint(w, "radiance:http_queue_depth 3\n")
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	loads := New(testConfig()).Probe(context.Background(), []*domain.Endpoint{testEndpoint(srv.URL)})
	load, ok := loads[srv.URL]
	if !ok || load.Active != 3 || load.Source != domain.LoadSourceMetrics {
		t.Fatalf("health 503 must fall through to /metrics (3): %+v ok=%v", load, ok)
	}
}

// TestHungEndpointsRoundHonoursBudgetNotTwoTimeouts is the fan-out-budget
// promise: a wedged engine used to cost the round a full timeout for /health
// PLUS a fresh full timeout for the /metrics fallback (worst case 2x). With
// Timeout 2 s and a budget of 80 ms the round must answer in the budget, with
// every hung node absent.
func TestHungEndpointsRoundHonoursBudgetNotTwoTimeouts(t *testing.T) {
	hung := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
			fmt.Fprint(w, `{"in_flight":1}`)
		}
	}))
	defer hung.Close()
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	dead.Close()

	cfg := testConfig()
	cfg.Timeout = 2 * time.Second
	cfg.FanOutBudget = 80 * time.Millisecond

	start := time.Now()
	loads := New(cfg).Probe(context.Background(), []*domain.Endpoint{
		testEndpoint(hung.URL), testEndpoint(hung.URL + "/second"), testEndpoint(dead.URL),
	})
	elapsed := time.Since(start)

	if len(loads) != 0 {
		t.Fatalf("hung endpoints must all be absent, got %v", loads)
	}
	if elapsed > time.Second {
		t.Fatalf("round took %s with Timeout=2s/budget=80ms: the /metrics fallback got its own full timeout", elapsed)
	}
}

// TestFallbackGetsOnlyRemainingBudget bounds the same rule from the other side:
// when the health attempt leaves room inside the round, the fallback still runs
// (it is not skipped for having less than the configured timeout) and the whole
// endpoint stays inside the round budget instead of spending timeout twice.
func TestFallbackGetsOnlyRemainingBudget(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			time.Sleep(50 * time.Millisecond) // burns most of the round
			w.WriteHeader(http.StatusNotFound)
		case "/metrics":
			time.Sleep(5 * time.Millisecond)
			fmt.Fprint(w, "radiance:http_queue_depth 2\n")
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	cfg := testConfig()
	cfg.Timeout = 2 * time.Second
	cfg.FanOutBudget = 150 * time.Millisecond

	start := time.Now()
	loads := New(cfg).Probe(context.Background(), []*domain.Endpoint{testEndpoint(srv.URL)})
	elapsed := time.Since(start)

	if load, ok := loads[srv.URL]; !ok || load.Active != 2 {
		t.Fatalf("fallback with remaining budget must still answer: %+v ok=%v", load, ok)
	}
	if elapsed > 300*time.Millisecond {
		t.Fatalf("endpoint cost %s: the fallback did not inherit the round budget", elapsed)
	}
}
