package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/thushan/olla/internal/config"
)

type stubObserveQuerier struct {
	calls     int
	queries   []string
	powerRows []map[string]any
	gpuRows   []map[string]any
	hourRows  []map[string]any
	dayRows   []map[string]any
	svcRows   []map[string]any
	err       error
	gpuErr    error // fail ONLY the gpu (llm_runtime) query
	hourErr   error // fail ONLY the hour cost query
}

func (s *stubObserveQuerier) query(_ context.Context, flux string) ([]map[string]any, error) {
	s.calls++
	s.queries = append(s.queries, flux)
	if s.err != nil {
		return nil, s.err
	}
	switch {
	case strings.Contains(flux, "llm_runtime") && s.gpuErr != nil:
		return nil, s.gpuErr
	case strings.Contains(flux, "period==\"hour\"") && s.hourErr != nil:
		return nil, s.hourErr
	case strings.Contains(flux, "llm_host_power"):
		return s.powerRows, nil
	case strings.Contains(flux, "period==\"hour\""):
		return s.hourRows, nil
	case strings.Contains(flux, "period==\"day\""):
		return s.dayRows, nil
	case strings.Contains(flux, "llm_service_status"):
		return s.svcRows, nil
	default:
		return s.gpuRows, nil
	}
}

func observeConfig() config.InfluxSourceConfig {
	return config.InfluxSourceConfig{
		URL: "http://influxdb:8086", Org: "org", TokenEnv: "T", Bucket: "llm/metrics", CostsBucket: "llm/costs",
		StaleAfter:       60 * time.Second,
		PowerAttribution: map[string]string{"halogen-395": "shelly-ai-395", "radiance-qfn": "shelly-ai-box"},
		GPUEndpoints:     []string{"radiance-gpus"},
	}
}

func TestObserveCollect(t *testing.T) {
	newest := time.Date(2026, 10, 13, 10, 0, 0, 0, time.UTC)
	powerRow := func(target, field string, value any) map[string]any {
		return map[string]any{"power_target_id": target, "device_name": "Meter " + target, "node": "pc3090", "_field": field, "_value": value, "_time": newest.UTC().Format(time.RFC3339Nano)}
	}
	stub := &stubObserveQuerier{
		powerRows: []map[string]any{
			powerRow("shelly-ai-box", "active_power_w", 120.5),
			powerRow("shelly-ai-box", "meter_temperature_c", 40.1),
			powerRow("shelly-ai-box", "meter_valid", 1.0),
			powerRow("shelly-ai-box", "overpower", 0.0),
			powerRow("shelly-ai-box", "output_on", 1.0),
			powerRow("shelly-ai-395", "active_power_w", 80.0),
			powerRow("plug-unknown", "active_power_w", 999.0), // not in attribution: must be ignored
		},
		gpuRows: []map[string]any{
			{"endpoint": "radiance-gpus", "gpu": "0", "node": "pc3090", "_field": "busy_percent", "_value": 99.0, "_time": newest.UTC().Format(time.RFC3339Nano)},
			{"endpoint": "radiance-gpus", "gpu": "0", "node": "pc3090", "_field": "vram_used_bytes", "_value": 2.5e10, "_time": newest.UTC().Format(time.RFC3339Nano)},
			{"endpoint": "radiance-gpus", "gpu": "1", "node": "pc3090", "_field": "gpu_present", "_value": 0.0, "_time": newest.UTC().Format(time.RFC3339Nano)},
		},
		hourRows: []map[string]any{
			{"endpoint": "radiance-qfn", "currency": "EUR", "period": "hour", "_field": "total_cost", "_value": 9.9, "_time": newest.Add(-time.Hour).UTC().Format(time.RFC3339Nano)},
			{"endpoint": "radiance-qfn", "currency": "EUR", "period": "hour", "_field": "total_cost", "_value": 0.04, "_time": newest.UTC().Format(time.RFC3339Nano)},
			{"endpoint": "radiance-qfn", "currency": "EUR", "period": "hour", "_field": "energy_kwh", "_value": 0.165, "_time": newest.UTC().Format(time.RFC3339Nano)},
			{"endpoint": "radiance-qfn", "currency": "EUR", "period": "hour", "_field": "total_tokens", "_value": 21285204.0, "_time": newest.UTC().Format(time.RFC3339Nano)},
			{"endpoint": "radiance-qfn", "currency": "EUR", "period": "hour", "_field": "period_status", "_value": "open", "_time": newest.UTC().Format(time.RFC3339Nano)},
		},
		dayRows: []map[string]any{
			{"endpoint": "radiance-qfn", "currency": "EUR", "period": "day", "_field": "total_cost", "_value": 0.41, "_time": newest.UTC().Format(time.RFC3339Nano)},
			{"endpoint": "radiance-qfn", "currency": "EUR", "period": "day", "_field": "period_status", "_value": "open", "_time": newest.UTC().Format(time.RFC3339Nano)},
		},
	}
	c := &observeCollector{cfg: observeConfig(), client: stub, now: func() time.Time { return newest.Add(2 * time.Second) }, ttl: observeTTL}

	payload, err := c.payload(context.Background())
	require.NoError(t, err)
	assert.Equal(t, runtimeMetricsSourceInflux, payload.Source)

	require.Len(t, payload.Power, 2, "only attributed meters, newest rollup only")
	byTarget := map[string]observePowerRow{}
	for _, row := range payload.Power {
		byTarget[row.Target] = row
	}
	box := byTarget["shelly-ai-box"]
	assert.Equal(t, "radiance-qfn", box.Endpoint)
	require.NotNil(t, box.Watts)
	assert.InDelta(t, 120.5, *box.Watts, .001)
	require.NotNil(t, box.TempC)
	assert.InDelta(t, 40.1, *box.TempC, .001)
	require.NotNil(t, box.AgeS)
	assert.InDelta(t, 2.0, *box.AgeS, .01)
	assert.NotNil(t, box.DeviceName)
	assert.Equal(t, "halogen-395", byTarget["shelly-ai-395"].Endpoint)
	assert.Nil(t, byTarget["shelly-ai-395"].TempC, "field the meter did not report stays nil")

	require.Len(t, payload.GPUs, 2)
	assert.InDelta(t, 99.0, *payload.GPUs[0].BusyPercent, .001)
	assert.Nil(t, payload.GPUs[0].Present)
	require.NotNil(t, payload.GPUs[1].Present)
	assert.InDelta(t, 0.0, *payload.GPUs[1].Present, .001)

	assert.Equal(t, "EUR", payload.Cost.Currency)
	hour := payload.Cost.Hour["radiance-qfn"]
	require.NotNil(t, hour)
	require.NotNil(t, hour.Cost)
	assert.InDelta(t, 0.04, *hour.Cost, .0001, "newest rollup must not mix in the older period's value")
	assert.Equal(t, "open", hour.PeriodStatus)
	assert.Equal(t, newest.UTC().Format(time.RFC3339), hour.PeriodStart)
	require.NotNil(t, hour.Tokens)
	assert.InDelta(t, 21285204.0, *hour.Tokens, .001)

	// Memoization: a second immediate call must not re-query.
	before := stub.calls
	_, err = c.payload(context.Background())
	require.NoError(t, err)
	assert.Equal(t, before, stub.calls)

	// Stale after the TTL window: exactly one new fan-out.
	c.now = func() time.Time { return newest.Add(30 * time.Second) }
	_, err = c.payload(context.Background())
	require.NoError(t, err)
	assert.Greater(t, stub.calls, before)
}

func TestObserveHandlerShapesAndRevalidation(t *testing.T) {
	// Sampler mode: 200 empty shape, never an error.
	samplerApp := &Application{}
	rec := httptest.NewRecorder()
	samplerApp.observeHandler(rec, httptest.NewRequest(http.MethodGet, uiObserveRoute, nil))
	require.Equal(t, http.StatusOK, rec.Code)
	var samplerBody ObservePayload
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &samplerBody))
	assert.Equal(t, runtimeMetricsSourceSampler, samplerBody.Source)
	assert.Empty(t, samplerBody.Power)
	assert.Empty(t, samplerBody.GPUs)
	assert.NotNil(t, samplerBody.Cost.Hour)

	// Influx mode: ETag round trip.
	newest := time.Now()
	stub := &stubObserveQuerier{powerRows: []map[string]any{{"power_target_id": "shelly-ai-box", "_field": "active_power_w", "_value": 10.0, "_time": newest.UTC().Format(time.RFC3339Nano)}}}
	app := &Application{observe: &observeCollector{cfg: observeConfig(), client: stub, now: time.Now, ttl: observeTTL}}
	rec = httptest.NewRecorder()
	app.observeHandler(rec, httptest.NewRequest(http.MethodGet, uiObserveRoute, nil))
	require.Equal(t, http.StatusOK, rec.Code)
	etag := rec.Header().Get("ETag")
	require.NotEmpty(t, etag)
	assert.True(t, strings.HasPrefix(etag, `W/"`), "observe must use the weak ETag convention of every JSON route")
	assert.Equal(t, "private, no-cache", rec.Header().Get("Cache-Control"))

	rec = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, uiObserveRoute, nil)
	req.Header.Set("If-None-Match", etag)
	app.observeHandler(rec, req)
	assert.Equal(t, http.StatusNotModified, rec.Code)

	// Total Influx failure surfaces as 503, not as an empty lie.
	failing := &Application{observe: &observeCollector{cfg: observeConfig(), client: &stubObserveQuerier{err: errors.New("boom")}, now: time.Now, ttl: observeTTL}}
	rec = httptest.NewRecorder()
	failing.observeHandler(rec, httptest.NewRequest(http.MethodGet, uiObserveRoute, nil))
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
}

// Ghost guard: a retired endpoint's rollups froze mid-period (the cost
// service stops writing when the endpoint leaves the fleet). _time is the
// period start, so its newest rollup is older than the period every live
// endpoint shares - rendering it would label a three-day-old total "this
// hour". Only newest-period rows ship.
func TestObserveCostGhostDropped(t *testing.T) {
	newest := time.Date(2026, 10, 16, 13, 0, 0, 0, time.UTC)
	costRow := func(endpoint, field string, value any, at time.Time) map[string]any {
		return map[string]any{"endpoint": endpoint, "currency": "EUR", "_field": field, "_value": value, "_time": at.UTC().Format(time.RFC3339Nano)}
	}
	stub := &stubObserveQuerier{
		hourRows: []map[string]any{
			costRow("radiance-9700-64", "total_cost", 1.5, newest),
			costRow("radiance-9700-64", "period_status", "open", newest),
			costRow("radiance-qfn", "total_cost", 0.0087, newest.Add(-72*time.Hour)),
			costRow("radiance-qfn", "period_status", "open", newest.Add(-72*time.Hour)),
		},
		dayRows: []map[string]any{
			costRow("radiance-9700-64", "total_cost", 9.0, newest.Add(-15*time.Hour)),
			costRow("radiance-qfn", "total_cost", 0.02, newest.Add(-72*time.Hour)),
		},
	}
	c := &observeCollector{cfg: observeConfig(), client: stub, now: func() time.Time { return newest }, ttl: observeTTL}
	payload, err := c.payload(context.Background())
	require.NoError(t, err)

	require.Contains(t, payload.Cost.Hour, "radiance-9700-64")
	assert.NotContains(t, payload.Cost.Hour, "radiance-qfn", "frozen rollups of a retired endpoint are ghosts")
	require.Contains(t, payload.Cost.Day, "radiance-9700-64", "current day start (22:00Z yesterday) is newer than hour start")
	assert.NotContains(t, payload.Cost.Day, "radiance-qfn")
}

// Cost rows carry no usable age (_time = period start), so the payload
// stamps the COST SERVICE heartbeat instead: "how fresh are these numbers"
// is the operator's question.
func TestObserveCostAgeFromServiceHeartbeat(t *testing.T) {
	newest := time.Date(2026, 10, 16, 13, 0, 0, 0, time.UTC)
	row := func(field string, value any) map[string]any {
		return map[string]any{"endpoint": "a", "currency": "EUR", "_field": field, "_value": value, "_time": newest.UTC().Format(time.RFC3339Nano)}
	}
	stub := &stubObserveQuerier{
		hourRows: []map[string]any{row("total_cost", 0.5)},
		svcRows:  []map[string]any{{"_time": newest.Add(-90 * time.Second).UTC().Format(time.RFC3339Nano)}},
	}
	c := &observeCollector{cfg: observeConfig(), client: stub, now: func() time.Time { return newest }, ttl: observeTTL}
	payload, err := c.payload(context.Background())
	require.NoError(t, err)
	require.NotNil(t, payload.Cost.Hour["a"])
	require.NotNil(t, payload.Cost.Hour["a"].AgeS)
	assert.InDelta(t, 90.0, *payload.Cost.Hour["a"].AgeS, 0.01)
}

// The section header claims one currency for the whole payload; that is
// only honest when every rendered row agrees. On disagreement the per-entry
// currency is authoritative and the header keeps its default (a Go map
// order pick would churn the ETag every collect).
func TestObserveCostCurrencyUnanimity(t *testing.T) {
	newest := time.Date(2026, 10, 16, 13, 0, 0, 0, time.UTC)
	row := func(endpoint, currency string) map[string]any {
		return map[string]any{"endpoint": endpoint, "currency": currency, "_field": "total_cost", "_value": 1.0, "_time": newest.UTC().Format(time.RFC3339Nano)}
	}
	mixed := &stubObserveQuerier{hourRows: []map[string]any{row("a", "EUR"), row("b", "USD")}}
	c := &observeCollector{cfg: observeConfig(), client: mixed, now: func() time.Time { return newest }, ttl: observeTTL}
	payload, err := c.payload(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "EUR", payload.Cost.Hour["a"].Currency)
	assert.Equal(t, "USD", payload.Cost.Hour["b"].Currency)
	assert.Equal(t, "EUR", payload.Cost.Currency, "disagreement: header keeps the default, entries carry their own")

	unanimous := &stubObserveQuerier{hourRows: []map[string]any{row("a", "USD"), row("b", "USD")}}
	c2 := &observeCollector{cfg: observeConfig(), client: unanimous, now: func() time.Time { return newest }, ttl: observeTTL}
	payload2, err := c2.payload(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "USD", payload2.Cost.Currency)
}

// Device tags ("0", "1") collide across GPU boxes; the endpoint is part of
// the group key so two boxes' device 0 stay two rows.
func TestObserveGpuGroupsByEndpointAndDevice(t *testing.T) {
	newest := time.Date(2026, 10, 16, 13, 0, 0, 0, time.UTC)
	gpuRow := func(endpoint, gpu, node string, busy float64) map[string]any {
		return map[string]any{"endpoint": endpoint, "gpu": gpu, "node": node, "_field": "busy_percent", "_value": busy, "_time": newest.UTC().Format(time.RFC3339Nano)}
	}
	cfg := observeConfig()
	cfg.GPUEndpoints = []string{"radiance-gpus", "radiance-gpus-128"}
	stub := &stubObserveQuerier{gpuRows: []map[string]any{
		gpuRow("radiance-gpus", "0", "pc3090", 50),
		gpuRow("radiance-gpus-128", "0", "leo9700", 60),
	}}
	c := &observeCollector{cfg: cfg, client: stub, now: func() time.Time { return newest }, ttl: observeTTL}
	payload, err := c.payload(context.Background())
	require.NoError(t, err)
	require.Len(t, payload.GPUs, 2, "same device id on two boxes = two rows")
	byEndpoint := map[string]observeGpuRow{}
	for _, row := range payload.GPUs {
		byEndpoint[row.Endpoint] = row
	}
	require.Contains(t, byEndpoint, "radiance-gpus")
	require.Contains(t, byEndpoint, "radiance-gpus-128")
	assert.Equal(t, "0", byEndpoint["radiance-gpus-128"].GPU)
	require.NotNil(t, byEndpoint["radiance-gpus-128"].Node)
	assert.Equal(t, "leo9700", *byEndpoint["radiance-gpus-128"].Node)
	assert.InDelta(t, 60.0, *byEndpoint["radiance-gpus-128"].BusyPercent, .001)
}

// One plug attributed to two endpoints: the row label must name both
// (sorted), not the alphabetically-first one - a first-wins label silently
// swaps when that endpoint retires.
func TestObserveSharedMeterLabelsAllEndpoints(t *testing.T) {
	newest := time.Date(2026, 10, 16, 13, 0, 0, 0, time.UTC)
	cfg := observeConfig()
	cfg.PowerAttribution = map[string]string{"alpha": "shelly-ai-box", "bravo": "shelly-ai-box"}
	stub := &stubObserveQuerier{powerRows: []map[string]any{
		{"power_target_id": "shelly-ai-box", "device_name": "Meter", "node": "n1", "_field": "active_power_w", "_value": 10.0, "_time": newest.UTC().Format(time.RFC3339Nano)},
	}}
	c := &observeCollector{cfg: cfg, client: stub, now: func() time.Time { return newest }, ttl: observeTTL}
	payload, err := c.payload(context.Background())
	require.NoError(t, err)
	require.Len(t, payload.Power, 1)
	assert.Equal(t, "alpha, bravo", payload.Power[0].Endpoint)
}

func TestObserveCostWindowPerPeriod(t *testing.T) {
	stub := &stubObserveQuerier{}
	c := &observeCollector{cfg: observeConfig(), client: stub, now: time.Now, ttl: observeTTL}
	_, err := c.payload(context.Background())
	require.NoError(t, err)
	var hour, day string
	for _, q := range stub.queries {
		switch {
		case strings.Contains(q, `period=="hour"`):
			hour = q
		case strings.Contains(q, `period=="day"`):
			day = q
		}
	}
	require.NotEmpty(t, hour)
	require.NotEmpty(t, day)
	assert.Contains(t, hour, "range(start:-26h)", "hourly rollups must not scan 8d")
	assert.Contains(t, day, "range(start:-8d)", "day needs the multi-day window for the newest-period rule")
}

func TestObserveCostWallClockAnchor(t *testing.T) {
	now := time.Date(2026, 10, 16, 9, 0, 0, 0, time.UTC)
	row := func(period string, start time.Time, cost float64) map[string]any {
		return map[string]any{"endpoint": "ghost-395", "currency": "EUR", "period": period, "_field": "total_cost", "_value": cost, "_time": start.UTC().Format(time.RFC3339Nano)}
	}
	// All endpoints retired: the newest rollups are frozen and old - the
	// newest-per-period rule alone would label them "this hour"/"today".
	stub := &stubObserveQuerier{
		hourRows: []map[string]any{row("hour", now.Add(-3*time.Hour), 1.2)},
		dayRows:  []map[string]any{row("day", now.Add(-35*time.Hour), 9.9)},
	}
	c := &observeCollector{cfg: observeConfig(), client: stub, now: func() time.Time { return now }, ttl: observeTTL}
	p, err := c.payload(context.Background())
	require.NoError(t, err)
	assert.Empty(t, p.Cost.Hour, "frozen hour period is not 'this hour'")
	assert.Empty(t, p.Cost.Day, "frozen day period is not 'today'")

	// Open day period starting at AEST midnight (22:00Z yesterday = 11h
	// old at 09:00Z) must survive - a UTC floor(now) rule would drop it.
	stub2 := &stubObserveQuerier{
		hourRows: []map[string]any{row("hour", now.Truncate(time.Hour), 0.1)},
		dayRows:  []map[string]any{row("day", time.Date(2026, 10, 15, 22, 0, 0, 0, time.UTC), 0.4)},
	}
	c2 := &observeCollector{cfg: observeConfig(), client: stub2, now: func() time.Time { return now }, ttl: observeTTL}
	p2, err := c2.payload(context.Background())
	require.NoError(t, err)
	assert.Contains(t, p2.Cost.Day, "ghost-395", "AEST-midnight day period is inside the 26h anchor")
	assert.Contains(t, p2.Cost.Hour, "ghost-395")
}

func TestObserveGpusAbsent(t *testing.T) {
	now := time.Date(2026, 10, 16, 9, 0, 0, 0, time.UTC)
	gpuRow := func(endpoint, gpu, field string, value any) map[string]any {
		return map[string]any{"endpoint": endpoint, "gpu": gpu, "node": "leo9700", "_field": field, "_value": value, "_time": now.Format(time.RFC3339Nano)}
	}
	cfg := observeConfig()
	cfg.GPUEndpoints = []string{"radiance-gpus", "radiance-gpus-128"}
	stub := &stubObserveQuerier{gpuRows: []map[string]any{gpuRow("radiance-gpus-128", "0", "busy_percent", 97.0)}}
	c := &observeCollector{cfg: cfg, client: stub, now: func() time.Time { return now }, ttl: observeTTL}
	p, err := c.payload(context.Background())
	require.NoError(t, err)
	assert.Equal(t, []string{"radiance-gpus"}, p.GPUsAbsent, "dead exporter must be named, not hidden")

	stub2 := &stubObserveQuerier{gpuRows: []map[string]any{
		gpuRow("radiance-gpus-128", "0", "busy_percent", 97.0),
		gpuRow("radiance-gpus", "0", "busy_percent", 12.0),
	}}
	c2 := &observeCollector{cfg: cfg, client: stub2, now: func() time.Time { return now }, ttl: observeTTL}
	p2, err := c2.payload(context.Background())
	require.NoError(t, err)
	assert.Empty(t, p2.GPUsAbsent)
	assert.Len(t, p2.GPUs, 2)
}

func TestObserveCostDroppedDisclosure(t *testing.T) {
	now := time.Date(2026, 10, 16, 9, 30, 0, 0, time.UTC)
	row := func(endpoint, period string, start time.Time) map[string]any {
		return map[string]any{"endpoint": endpoint, "currency": "EUR", "period": period, "_field": "total_cost", "_value": 1.0, "_time": start.UTC().Format(time.RFC3339Nano)}
	}
	// live-a reports the current hour; ghost-b's newest rollup is last
	// hour's period start -> dropped, and must be NAMED, not hidden.
	stub := &stubObserveQuerier{
		hourRows: []map[string]any{
			row("live-a", "hour", now.Truncate(time.Hour)),
			row("ghost-b", "hour", now.Truncate(time.Hour).Add(-time.Hour)),
		},
	}
	c := &observeCollector{cfg: observeConfig(), client: stub, now: func() time.Time { return now }, ttl: observeTTL}
	p, err := c.payload(context.Background())
	require.NoError(t, err)
	assert.Contains(t, p.Cost.Hour, "live-a")
	assert.NotContains(t, p.Cost.Hour, "ghost-b")
	assert.Equal(t, []string{"ghost-b"}, p.Cost.Dropped)
}

func TestObserveNegativeCache(t *testing.T) {
	clock := time.Date(2026, 10, 16, 9, 0, 0, 0, time.UTC)
	stub := &stubObserveQuerier{err: errors.New("influx down")}
	c := &observeCollector{cfg: observeConfig(), client: stub, now: func() time.Time { return clock }, ttl: observeTTL}
	_, err1 := c.payload(context.Background())
	require.Error(t, err1)
	callsAfterFirst := stub.calls
	_, err2 := c.payload(context.Background())
	require.Error(t, err2)
	assert.Equal(t, callsAfterFirst, stub.calls, "a failing collect must not re-fan Influx within the negative-cache window")

	// After TTL/2 the next caller retries, and success clears the memo.
	clock = clock.Add(observeTTL)
	stub.err = nil
	_, err := c.payload(context.Background())
	require.NoError(t, err)
	assert.Greater(t, stub.calls, callsAfterFirst)
}

func TestFluxStringSetEscaping(t *testing.T) {
	assert.Equal(t, `"radiance-gpus","a.b"`, fluxStringSet([]string{"radiance-gpus", "a.b"}))
	// Go's %q emits \x01; Flux only parses \uXXXX.
	assert.Equal(t, `"a\u0001b"`, fluxStringSet([]string{"a\x01b"}))
}

// P2a: a FAILED gpu query must disclose every configured endpoint as
// absent - the success branch's seen-based computation never runs, and an
// empty grid with an empty absent list reads as a healthy small fleet.
func TestObserveGpuQueryFailureDisclosesAll(t *testing.T) {
	now := time.Date(2026, 10, 17, 9, 0, 0, 0, time.UTC)
	cfg := observeConfig()
	cfg.GPUEndpoints = []string{"radiance-gpus", "radiance-gpus-128"}
	stub := &stubObserveQuerier{gpuErr: errors.New("influx flux 500")}
	c := &observeCollector{cfg: cfg, client: stub, now: func() time.Time { return now }, ttl: observeTTL}
	p, err := c.payload(context.Background())
	require.NoError(t, err, "a partial failure degrades the payload, it does not 503")
	assert.Empty(t, p.GPUs)
	assert.Equal(t, []string{"radiance-gpus", "radiance-gpus-128"}, p.GPUsAbsent)
}

// P2a: a failed cost period is named in cost.errors - the UI distinguishes
// "query failed" from "no rollups yet" only if the payload says which.
func TestObserveCostPeriodErrorMarker(t *testing.T) {
	now := time.Date(2026, 10, 17, 9, 30, 0, 0, time.UTC)
	dayRow := map[string]any{"endpoint": "live-a", "currency": "EUR", "period": "day", "_field": "total_cost", "_value": 3.5, "_time": now.Truncate(24 * time.Hour).UTC().Format(time.RFC3339Nano)}
	stub := &stubObserveQuerier{hourErr: errors.New("influx flux 500"), dayRows: []map[string]any{dayRow}}
	c := &observeCollector{cfg: observeConfig(), client: stub, now: func() time.Time { return now }, ttl: observeTTL}
	p, err := c.payload(context.Background())
	require.NoError(t, err)
	assert.Equal(t, []string{"hour"}, p.Cost.Errors)
	assert.Contains(t, p.Cost.Day, "live-a", "the surviving period still renders")
}

// P2b: the collect context inherits the request context, so closing a tab
// aborts in-flight polls with context.Canceled. Memoizing that as a
// negative-cache hit blacked the observe surface for EVERY client for
// TTL/2 while Influx was healthy.
func TestObserveNegativeCacheIgnoresClientAbort(t *testing.T) {
	clock := time.Date(2026, 10, 17, 9, 0, 0, 0, time.UTC)
	stub := &stubObserveQuerier{err: context.Canceled}
	c := &observeCollector{cfg: observeConfig(), client: stub, now: func() time.Time { return clock }, ttl: observeTTL}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := c.payload(ctx)
	require.Error(t, err)
	abortedCalls := stub.calls
	stub.err = nil
	_, err = c.payload(context.Background())
	require.NoError(t, err, "one closed tab must not serve 503 to healthy clients")
	assert.Greater(t, stub.calls, abortedCalls)

	// Real server-side failures DO memoize (the guard is abort-specific).
	stub2 := &stubObserveQuerier{err: errors.New("influx flux 500")}
	c2 := &observeCollector{cfg: observeConfig(), client: stub2, now: func() time.Time { return clock }, ttl: observeTTL}
	_, err = c2.payload(context.Background())
	require.Error(t, err)
	calls := stub2.calls
	_, err = c2.payload(context.Background())
	require.Error(t, err)
	assert.Equal(t, calls, stub2.calls, "genuine failures keep the negative cache")
}
