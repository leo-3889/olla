// Package loadprobe answers "how many requests is each of these engine
// endpoints carrying right now" in ONE parallel round, fired at assignment
// time - when a new conversation needs a node (a sticky miss). It is not a
// poller: the answer is only worth having at the instant a choice is made, and
// conversations already pinned to a node never pay for it.
//
// Nothing here reads InfluxDB. The numbers come from the engines themselves
// (measured 2026-11-03 on the live fleet: radiance answers /health in ~1.0 ms
// with a 63-byte body, also while 4 generations were mid-flight; /metrics is
// 34 KB in ~1.8 ms), so the routing path has no external dependency.
package loadprobe

import (
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/thushan/olla/internal/config"
	"github.com/thushan/olla/internal/core/domain"
)

const (
	// defaultTimeout is the per-probe budget used when the config carries
	// none (the boot validator requires >0 when the feature is enabled, so
	// this only guards zero-value construction in tests).
	defaultTimeout = 250 * time.Millisecond
	// defaultBudget caps the whole fan-out when FanOutBudget is unset.
	defaultBudget = 400 * time.Millisecond
	// maxHealthBytes caps the health-body read. The live shape is 63 bytes;
	// the cap only stops a pathological endpoint from stalling the round.
	maxHealthBytes = 64 << 10
	// maxMetricsBytes caps the /metrics fallback read. Live bodies measure
	// 34-35 KB (radiance) and 7.5 KB (halogen); 128 KB covers an engine with
	// room to grow without letting an unbounded body eat the budget.
	maxMetricsBytes   = 128 << 10
	defaultHealthPath = "/health"
)

// Prober implements domain.LoadProber over direct HTTP reads of the engines.
type Prober struct {
	cfg    config.LoadProbeConfig
	client *http.Client
	// now is stubbable for tests; the package deliberately does not cache,
	// every Probe result is stamped at completion.
	now func() time.Time
}

var _ domain.LoadProber = (*Prober)(nil)

// New builds a Prober from the (boot-validated) load_probe config. The client
// is shared across rounds on purpose: keep-alive to the engines is what keeps
// a round at one LAN RTT. No client Timeout is set - every request is bounded
// by its context, which is the only budget the round honours.
func New(cfg config.LoadProbeConfig) *Prober {
	return &Prober{cfg: cfg, client: &http.Client{Transport: newTransport()}, now: time.Now}
}

// newTransport clones the process transport for keep-alive reuse across
// rounds; if the global was replaced with a non-Transport (test harnesses do
// this), run with a plain client rather than asserting our way into a panic.
func newTransport() http.RoundTripper {
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return nil
	}
	transport := base.Clone()
	transport.MaxIdleConnsPerHost = 4 // one round uses one conn per endpoint; fleets are single-digit
	return transport
}

// Probe fires one parallel round and returns endpoint URLString -> reading.
// An endpoint with no usable reading is ABSENT from the map: absence means
// "not measured", and must never be confused with the measurement 0.
//
// Budgets: each endpoint probe runs under cfg.Timeout, the whole round under
// cfg.FanOutBudget. When the budget expires, late probes do not delay the
// answer - they are dropped (their contexts were cancelled with the round).
// The /metrics fallback of one endpoint gets only what the round still has
// left (capped at cfg.Timeout), never a fresh full timeout: a wedged engine
// must cost the round one timeout, not two.
func (p *Prober) Probe(ctx context.Context, endpoints []*domain.Endpoint) map[string]domain.Load {
	result := make(map[string]domain.Load, len(endpoints))
	if p == nil || len(endpoints) == 0 {
		return result
	}

	timeout := p.cfg.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	budget := p.cfg.FanOutBudget
	if budget <= 0 {
		budget = 2 * timeout
		if budget < defaultBudget {
			budget = defaultBudget
		}
	}
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()

	var (
		mu sync.Mutex
		wg sync.WaitGroup
	)
	for _, ep := range endpoints {
		if ep == nil || ep.URLString == "" {
			continue
		}
		wg.Add(1)
		go func(ep *domain.Endpoint) {
			defer wg.Done()
			load, ok := p.probeOne(ctx, ep, timeout)
			if !ok {
				return
			}
			mu.Lock()
			result[ep.URLString] = load
			mu.Unlock()
		}(ep)
	}

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
	}

	// Copy under the lock: on the budget path late goroutines can still be
	// writing into result, and the caller reads the returned map unlocked.
	mu.Lock()
	defer mu.Unlock()
	out := make(map[string]domain.Load, len(result))
	for k, v := range result {
		out[k] = v
	}
	return out
}

// probeOne tries the cheap primary (the health JSON) then the /metrics
// fallback for one endpoint. No answer within the contexts => (zero, false).
//
// The fallback inherits the round budget, not a second full timeout: the
// health attempt already spent up to timeout of the round, so the fallback
// gets min(timeout, what the round deadline still allows) and is skipped
// outright when nothing remains.
func (p *Prober) probeOne(ctx context.Context, ep *domain.Endpoint, timeout time.Duration) (domain.Load, bool) {
	base := strings.TrimRight(ep.URLString, "/")
	if load, ok := p.probeHealth(ctx, ep, base, timeout); ok {
		return load, true
	}
	if deadline, ok := ctx.Deadline(); ok {
		if remaining := time.Until(deadline); remaining < timeout {
			if remaining <= 0 {
				return domain.Load{}, false
			}
			timeout = remaining
		}
	}
	return p.probeMetrics(ctx, ep, base, timeout)
}

// probeHealth reads the configured health path and lifts the configured
// numeric field (live shape: {"status":"ok","model":"qwen4exp",
// "in_flight":4,"queue_cap":40}). A missing field, a non-JSON body or a
// non-2xx answer is "not measured", not zero: engines whose /health says
// something else (halogen reports vision truth there, not counts) must fall
// through to /metrics rather than be believed as idle.
func (p *Prober) probeHealth(ctx context.Context, ep *domain.Endpoint, base string, timeout time.Duration) (domain.Load, bool) {
	path := p.cfg.HealthPath
	if path == "" {
		path = defaultHealthPath
	}
	field := p.cfg.ActiveField
	if field == "" {
		field = "in_flight"
	}

	body, ok := p.getBytes(ctx, base+path, timeout, maxHealthBytes, ep)
	if !ok {
		return domain.Load{}, false
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return domain.Load{}, false
	}
	raw, ok := fields[field]
	if !ok {
		return domain.Load{}, false
	}
	var active float64
	if err := json.Unmarshal(raw, &active); err != nil {
		return domain.Load{}, false
	}

	return domain.Load{
		Active:    int(active),
		Source:    domain.LoadSourceHealth,
		Confident: true,
		SampledAt: p.now(),
	}, true
}

// probeMetrics reads /metrics and derives the admitted-request count from the
// engine gauges. Preference order, by measured fidelity on this fleet:
//   - radiance:http_queue_depth  - the HTTP-layer admitted count, byte-for-byte
//     equal to /health in_flight (verified 2026-11-03 under 4 concurrent
//     generations). Despite the dashboard label, it is NOT a waiting-only gauge.
//   - vllm:num_requests_running + vllm:num_requests_waiting - the vLLM-shaped
//     equivalent, summed.
//
// Values are summed across label series (multi-engine endpoints report one
// line per engine). Line parsing is deliberately naive (strip labels, parse
// "name value"): no Prometheus dependency for two gauges.
func (p *Prober) probeMetrics(ctx context.Context, ep *domain.Endpoint, base string, timeout time.Duration) (domain.Load, bool) {
	body, ok := p.getBytes(ctx, base+"/metrics", timeout, maxMetricsBytes, ep)
	if !ok {
		return domain.Load{}, false
	}

	var (
		queue   float64
		running float64
		waiting float64
	)
	seenQueue, seenVLLM := false, false
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name := line
		if i := strings.IndexAny(name, " {"); i >= 0 {
			name = name[:i]
		}
		valueField := line[strings.LastIndexByte(line, ' ')+1:]
		value, err := strconv.ParseFloat(valueField, 64)
		if err != nil || math.IsNaN(value) || math.IsInf(value, 0) {
			// A NaN/Inf gauge (scraped during an engine reload, or the
			// "+Inf" a Prometheus histogram prints) says nothing about
			// occupancy: skip the line. If no finite line of a gauge
			// remains, that source is ABSENT, never believed as 0.
			continue
		}
		switch name {
		case "radiance:http_queue_depth":
			queue += value
			seenQueue = true
		case "vllm:num_requests_running":
			running += value
			seenVLLM = true
		case "vllm:num_requests_waiting":
			waiting += value
			seenVLLM = true
		}
	}

	switch {
	case seenQueue:
		return domain.Load{Active: int(queue), Source: domain.LoadSourceMetrics, Confident: true, SampledAt: p.now()}, true
	case seenVLLM:
		return domain.Load{Active: int(running + waiting), Source: domain.LoadSourceMetrics, Confident: true, SampledAt: p.now()}, true
	default:
		return domain.Load{}, false
	}
}

// getBytes GETs url under a per-probe timeout and returns a capped body.
// Non-2xx, transport error, or deadline => not ok (absence, never 0). The
// endpoint's configured outbound auth header is sent: a credential-gated
// engine answers 401 to a bare /health, and an unauthenticated prober would
// then read every node of that fleet as unknown on every single assignment.
func (p *Prober) getBytes(ctx context.Context, url string, timeout time.Duration, limit int, ep *domain.Endpoint) ([]byte, bool) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, false
	}
	if ep.AuthHeaderName != "" && ep.AuthHeaderValue != "" {
		req.Header.Set(ep.AuthHeaderName, ep.AuthHeaderValue)
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, false
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, false
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, int64(limit)))
	if err != nil {
		return nil, false
	}
	return body, true
}
