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
	powerRows []map[string]any
	gpuRows   []map[string]any
	hourRows  []map[string]any
	dayRows   []map[string]any
	svcRows   []map[string]any
	err       error
}

func (s *stubObserveQuerier) query(_ context.Context, flux string) ([]map[string]any, error) {
	s.calls++
	if s.err != nil {
		return nil, s.err
	}
	switch {
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
	assert.Equal(t, "no-cache", rec.Header().Get("Cache-Control"))

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
