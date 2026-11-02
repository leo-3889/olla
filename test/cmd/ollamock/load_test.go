package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// slowServer mirrors testServer but with a TTFT hold, so an in-flight
// generation is observable while it waits for its first byte.
func slowServer(t *testing.T, ttftMS int) *httptest.Server {
	t.Helper()
	srv := newServer(serverConfig{
		name:         "load-test",
		models:       []string{"test-model"},
		ttftMS:       ttftMS,
		tps:          0,
		streamChunks: 1,
	})
	ts := httptest.NewServer(srv.handler())
	t.Cleanup(ts.Close)
	return ts
}

func healthInFlight(t *testing.T, url string) (int, map[string]any) {
	t.Helper()
	resp := getURL(t, url+"/health")
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var v map[string]any
	if err := json.Unmarshal(body, &v); err != nil {
		t.Fatalf("health body not JSON: %q: %v", body, err)
	}
	n, ok := v["in_flight"].(float64)
	if !ok {
		t.Fatalf("health body lacks numeric in_flight: %v", v)
	}
	return int(n), v
}

// TestInFlightTracksAdmittedRequests is the mock's contract with the
// least-load e2e: /health in_flight rises while generations are admitted
// (TTFT hold counts - the engine is busy) and drains when they finish.
func TestInFlightTracksAdmittedRequests(t *testing.T) {
	ts := slowServer(t, 400)

	if n, v := healthInFlight(t, ts.URL); n != 0 || v["status"] != "ok" {
		t.Fatalf("idle health must read 0/ok, got %v", v)
	}
	if v := healthBodyProbe(t, ts.URL); v["model"] != "test-model" || v["queue_cap"] != float64(40) {
		t.Fatalf("health must carry the radiance-shaped fields, got %v", v)
	}

	done := make(chan struct{}, 2)
	for range 2 {
		go func() {
			resp, err := http2Post(ts.URL + "/v1/chat/completions")
			if err == nil {
				resp.Body.Close()
			}
			done <- struct{}{}
		}()
	}

	// Poll for >=1 admitted, then >=2: goroutine arrival is staggered and a
	// loaded machine can hold the counter at 1 for a moment. The deadline
	// itself is the failure condition - no post-loop equality re-check that
	// a busy CI box could lose the race against (false negative).
	deadline := time.Now().Add(5 * time.Second)
	pollInFlight(t, ts.URL, deadline, func(n int) bool { return n >= 1 }, "one admitted generation")
	pollInFlight(t, ts.URL, deadline, func(n int) bool { return n >= 2 }, "two admitted generations")

	<-done
	<-done
	pollInFlight(t, ts.URL, deadline, func(n int) bool { return n == 0 }, "in_flight to drain to 0")
}

// pollInFlight waits until /health in_flight satisfies want. Passing the
// deadline is the failure - the last reading is reported for context.
func pollInFlight(t *testing.T, url string, deadline time.Time, want func(int) bool, wantDesc string) {
	t.Helper()
	for {
		n, v := healthInFlight(t, url)
		if want(n) {
			return
		}
		if !time.Now().Before(deadline) {
			t.Fatalf("deadline passed waiting for %s; last health reading %v", wantDesc, v)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestNoHealthInFlightFlagShape pins the --no-health-inflight contract: the
// health JSON omits the in_flight key entirely (so the least-load probe finds
// no primary field and must fall back) while /metrics keeps serving
// radiance:http_queue_depth from the very same counter.
func TestNoHealthInFlightFlagShape(t *testing.T) {
	srv := newServer(serverConfig{
		name:             "flag-test",
		models:           []string{"test-model"},
		ttftMS:           400,
		streamChunks:     1,
		noHealthInflight: true,
	})
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	idle := healthBodyProbe(t, ts.URL)
	if idle["status"] != "ok" {
		t.Fatalf("flagged health must still report ok, got %v", idle)
	}
	if _, present := idle["in_flight"]; present {
		t.Fatalf("--no-health-inflight must drop the key, got %v", idle)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		resp, err := http2Post(ts.URL + "/v1/chat/completions")
		if err == nil {
			resp.Body.Close()
		}
	}()

	gauge := regexp.MustCompile(`(?m)^radiance:http_queue_depth\{[^}]*\}\s+([0-9.]+)`)
	deadline := time.Now().Add(5 * time.Second)
	busy := false
	for time.Now().Before(deadline) {
		resp := getURL(t, ts.URL+"/metrics")
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if m := gauge.FindStringSubmatch(string(b)); m != nil && m[1] != "0" {
			// Same instant: the health door must still be load-blind.
			if got := healthBodyProbe(t, ts.URL); got["in_flight"] != nil {
				t.Fatalf("health must stay key-free while busy, got %v", got)
			}
			busy = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !busy {
		t.Fatal("/metrics never served a nonzero radiance:http_queue_depth while a generation was admitted")
	}
	<-done
}

// TestMetricsGaugesMatchHealth pins the fallback door to the same truth:
// radiance:http_queue_depth (the admitted count the probe prefers) must equal
// /health in_flight, and the vLLM pair must sum to it too.
func TestMetricsGaugesMatchHealth(t *testing.T) {
	ts := slowServer(t, 500)
	go func() {
		resp, err := http2Post(ts.URL + "/v1/completions")
		if err == nil {
			resp.Body.Close()
		}
	}()

	deadline := time.Now().Add(2 * time.Second)
	var body string
	for time.Now().Before(deadline) {
		if n, _ := healthInFlight(t, ts.URL); n >= 1 {
			resp := getURL(t, ts.URL+"/metrics")
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			body = string(b)
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if body == "" {
		t.Fatal("never observed admitted load to cross-check /metrics")
	}

	gauge := func(name string) int {
		re := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(name) + `\{[^}]*\}\s+(\S+)`)
		m := re.FindStringSubmatch(body)
		if m == nil {
			t.Fatalf("gauge %s missing from:\n%s", name, body)
		}
		v, err := strconv.ParseFloat(m[1], 64)
		if err != nil {
			t.Fatalf("gauge %s unparseable %q", name, m[1])
		}
		return int(v)
	}
	queue := gauge("radiance:http_queue_depth")
	running := gauge("vllm:num_requests_running")
	waiting := gauge("vllm:num_requests_waiting")
	if queue != 1 || running+waiting != 1 {
		t.Fatalf("gauges must read the one admitted request: queue=%d running=%d waiting=%d", queue, running, waiting)
	}
	if !strings.Contains(body, "model_name=\"test-model\"") {
		t.Fatal("gauges must carry the served model's label")
	}
}

// small helpers to keep the test above import-light.
func healthBodyProbe(t *testing.T, url string) map[string]any {
	t.Helper()
	resp := getURL(t, url+"/health")
	defer resp.Body.Close()
	var v map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&v)
	return v
}

func http2Post(url string) (*http.Response, error) {
	return http.Post(url, "application/json", strings.NewReader(
		`{"model":"test-model","prompt":"x","stream":true,"messages":[{"role":"user","content":"x"}]}`))
}
