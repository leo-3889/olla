package handlers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/thushan/olla/internal/config"
	"github.com/thushan/olla/internal/logger"
)

// uiObserveRoute lives inside the dashboard CIDR zone but outside the static
// asset subtree handling, so it gets its own gated registration.
const uiObserveRoute = "/internal/ui/observe.json"

// observeTTL bounds how often the observe payload hits Influx regardless of
// client polling: power and cost change on much slower cadences than the
// 10s runtime refresh, and several dashboard tabs must not multiply the
// query load.
const observeTTL = 6 * time.Second

// observePowerRow is one meter measurement attributed to an olla endpoint.
// Pointer fields mean "the device did not report this field" - the UI draws
// a dash, never a zero.
type observePowerRow struct {
	Target     string   `json:"target"`
	Endpoint   string   `json:"endpoint"`
	DeviceName *string  `json:"device_name"`
	Node       *string  `json:"node"`
	Watts      *float64 `json:"watts"`
	TempC      *float64 `json:"temp_c"`
	MeterValid *float64 `json:"meter_valid"`
	Overpower  *float64 `json:"overpower"`
	OutputOn   *float64 `json:"output_on"`
	AgeS       *float64 `json:"age_s"`
}

type observeGpuRow struct {
	GPU                string   `json:"gpu"`
	Node               *string  `json:"node"`
	BusyPercent        *float64 `json:"busy_percent"`
	MemActivityPercent *float64 `json:"mem_activity_percent"`
	SocketPowerWatts   *float64 `json:"socket_power_watts"`
	EdgeTempC          *float64 `json:"edge_temp_c"`
	VRamUsedBytes      *float64 `json:"vram_used_bytes"`
	VRamTotalBytes     *float64 `json:"vram_total_bytes"`
	Present            *float64 `json:"present"`
	AgeS               *float64 `json:"age_s"`
}

type observeCostEntry struct {
	Cost         *float64 `json:"cost"`
	EnergyKWh    *float64 `json:"energy_kwh"`
	Tokens       *float64 `json:"tokens"`
	PeriodStatus string   `json:"period_status,omitempty"`
	PeriodStart  string   `json:"period_start,omitempty"`
}

type observeCost struct {
	Currency string                           `json:"currency"`
	Hour     map[string]*observeCostEntry     `json:"hour"`
	Day      map[string]*observeCostEntry     `json:"day"`
}

// ObservePayload is the contract of GET /internal/ui/observe.json. In
// sampler mode it is the empty shape with source "sampler" (200, never an
// error: the sections are config-disabled then anyway).
type ObservePayload struct {
	Schema      int               `json:"schema"`
	GeneratedAt time.Time         `json:"generated_at"`
	Source      string            `json:"source"`
	Power       []observePowerRow `json:"power"`
	Cost        observeCost       `json:"cost"`
	GPUs        []observeGpuRow   `json:"gpus"`
}

func newEmptyObservePayload(source string, now time.Time) *ObservePayload {
	return &ObservePayload{
		Schema:      1,
		GeneratedAt: now.UTC(),
		Source:      source,
		Power:       []observePowerRow{},
		Cost:        observeCost{Currency: "EUR", Hour: map[string]*observeCostEntry{}, Day: map[string]*observeCostEntry{}},
		GPUs:        []observeGpuRow{},
	}
}

type observeCollector struct {
	cfg    config.InfluxSourceConfig
	client fluxQuerier
	logger logger.StyledLogger
	now    func() time.Time
	ttl    time.Duration

	mu     sync.Mutex
	memo   *ObservePayload
	memoAt time.Time
}

func newObserveCollector(influx config.InfluxSourceConfig, token string, log logger.StyledLogger) *observeCollector {
	return &observeCollector{
		cfg:    influx,
		client: newFluxClient(influx.URL, influx.Org, token),
		logger: log,
		now:    time.Now,
		ttl:    observeTTL,
	}
}

// payload returns the memoized payload or collects a fresh one; the mutex
// doubles as the singleflight (one Influx fan-out at a time).
func (o *observeCollector) payload(ctx context.Context) (*ObservePayload, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.memo != nil && o.now().Sub(o.memoAt) < o.ttl {
		return o.memo, nil
	}
	payload, err := o.collect(ctx)
	if err != nil {
		return nil, err
	}
	o.memo, o.memoAt = payload, o.now()
	return payload, nil
}

func (o *observeCollector) collect(ctx context.Context) (*ObservePayload, error) {
	payload := newEmptyObservePayload(runtimeMetricsSourceInflux, o.now())
	cycleCtx, cancel := context.WithTimeout(ctx, 2*fluxClientTimeout)
	defer cancel()
	queries, failures := 0, 0
	fail := func(query string, err error) {
		failures++
		if o.logger != nil {
			o.logger.Warn("observe: influx query failed", "query", query, "error", err.Error())
		}
	}

	if len(o.cfg.PowerAttribution) > 0 {
		queries++
		endpointForTarget := map[string]string{}
		for _, endpoint := range sortedKeys(o.cfg.PowerAttribution) {
			target := o.cfg.PowerAttribution[endpoint]
			if _, taken := endpointForTarget[target]; !taken {
				endpointForTarget[target] = endpoint
			}
		}
		targets := make([]string, 0, len(endpointForTarget))
		for target := range endpointForTarget {
			targets = append(targets, target)
		}
		sort.Strings(targets)
		rows, err := o.client.query(cycleCtx, fmt.Sprintf(`from(bucket:%q) |> range(start:-5m) |> filter(fn:(r)=>r._measurement=="llm_host_power") |> filter(fn:(r)=>contains(set:[%s], value:r.power_target_id)) |> last()`, o.cfg.Bucket, fluxStringSet(targets)))
		if err != nil {
			fail("power", err)
		} else {
			type meterGroup struct {
				fields map[string]float64
				device *string
				node   *string
				newest time.Time
			}
			groups := map[string]*meterGroup{}
			for _, row := range rows {
				target, _ := row["power_target_id"].(string)
				if target == "" {
					continue
				}
				group := groups[target]
				if group == nil {
					group = &meterGroup{fields: map[string]float64{}}
					groups[target] = group
				}
				if name, ok := row["device_name"].(string); ok && name != "" && group.device == nil {
					group.device = &name
				}
				if node, ok := row["node"].(string); ok && node != "" && group.node == nil {
					group.node = &node
				}
				field, _ := row["_field"].(string)
				value, ok := influxFloat(row["_value"])
				if field == "" || !ok {
					continue
				}
				group.fields[field] = value
				if at := influxTime(row["_time"]); at.After(group.newest) {
					group.newest = at
				}
			}
			for _, target := range targets {
				group := groups[target]
				if group == nil {
					continue
				}
				row := observePowerRow{Target: target, Endpoint: endpointForTarget[target], DeviceName: group.device, Node: group.node}
				pickFloat(group.fields, "active_power_w", &row.Watts)
				pickFloat(group.fields, "meter_temperature_c", &row.TempC)
				pickFloat(group.fields, "meter_valid", &row.MeterValid)
				pickFloat(group.fields, "overpower", &row.Overpower)
				pickFloat(group.fields, "output_on", &row.OutputOn)
				if !group.newest.IsZero() {
					age := payload.GeneratedAt.Sub(group.newest).Seconds()
					row.AgeS = floatPtr(age)
				}
				payload.Power = append(payload.Power, row)
			}
		}
	}

	if o.cfg.GPUEndpoint != "" {
		queries++
		rows, err := o.client.query(cycleCtx, fmt.Sprintf(`from(bucket:%q) |> range(start:-5m) |> filter(fn:(r)=>r._measurement=="llm_runtime") |> filter(fn:(r)=>r.endpoint==%q) |> last()`, o.cfg.Bucket, o.cfg.GPUEndpoint))
		if err != nil {
			fail("gpu", err)
		} else {
			type gpuGroup struct {
				fields map[string]float64
				node   *string
				newest time.Time
			}
			groups := map[string]*gpuGroup{}
			for _, row := range rows {
				gpu, _ := row["gpu"].(string)
				field, _ := row["_field"].(string)
				if gpu == "" || field == "" {
					continue
				}
				value, ok := influxFloat(row["_value"])
				if !ok {
					continue
				}
				group := groups[gpu]
				if group == nil {
					group = &gpuGroup{fields: map[string]float64{}}
					groups[gpu] = group
				}
				if node, ok := row["node"].(string); ok && node != "" && group.node == nil {
					group.node = &node
				}
				group.fields[field] = value
				if at := influxTime(row["_time"]); at.After(group.newest) {
					group.newest = at
				}
			}
			for _, gpu := range sortedKeysAny(groups) {
				group := groups[gpu]
				out := observeGpuRow{GPU: gpu, Node: group.node}
				pickFloat(group.fields, "busy_percent", &out.BusyPercent)
				pickFloat(group.fields, "mem_activity_percent", &out.MemActivityPercent)
				pickFloat(group.fields, "socket_power_watts", &out.SocketPowerWatts)
				pickFloat(group.fields, "edge_temp_celsius", &out.EdgeTempC)
				pickFloat(group.fields, "vram_used_bytes", &out.VRamUsedBytes)
				pickFloat(group.fields, "vram_total_bytes", &out.VRamTotalBytes)
				pickFloat(group.fields, "gpu_present", &out.Present)
				if !group.newest.IsZero() {
					age := payload.GeneratedAt.Sub(group.newest).Seconds()
					out.AgeS = floatPtr(age)
				}
				payload.GPUs = append(payload.GPUs, out)
			}
		}
	}

	costPeriods := []struct {
		period string
		into   map[string]*observeCostEntry
	}{{"hour", payload.Cost.Hour}, {"day", payload.Cost.Day}}
	for _, cp := range costPeriods {
		queries++
		// _time of a cost rollup is the PERIOD START, so the window must
		// span longer than one period to find the newest rollup at all.
		rows, err := o.client.query(cycleCtx, fmt.Sprintf(`from(bucket:%q) |> range(start:-8d) |> filter(fn:(r)=>r._measurement=="llm_endpoint_cost_v3") |> filter(fn:(r)=>r.period==%q)`, o.cfg.CostsBucket, cp.period))
		if err != nil {
			fail("cost "+cp.period, err)
			continue
		}
		type costGroup struct {
			fields   map[string]float64
			status   string
			currency string
			newest   time.Time
			haveTime bool
		}
		groups := map[string]*costGroup{}
		for _, row := range rows {
			endpoint, _ := row["endpoint"].(string)
			field, _ := row["_field"].(string)
			if endpoint == "" || field == "" {
				continue
			}
			group := groups[endpoint]
			if group == nil {
				group = &costGroup{fields: map[string]float64{}}
				groups[endpoint] = group
			}
			if currency, ok := row["currency"].(string); ok && currency != "" && group.currency == "" {
				group.currency = currency
			}
			// Rollup point _time = period start: keep ONLY the newest
			// rollup's fields per endpoint; anything from an older period
			// would silently mix two periods' numbers into one tile.
			at := influxTime(row["_time"])
			if !at.IsZero() {
				if group.haveTime && at.Before(group.newest) {
					continue
				}
				if !group.haveTime || at.After(group.newest) {
					group.fields = map[string]float64{}
					group.status = ""
					group.newest = at
					group.haveTime = true
				}
			}
			if field == "period_status" {
				if status, ok := row["_value"].(string); ok {
					group.status = status
				}
				continue
			}
			value, ok := influxFloat(row["_value"])
			if !ok {
				continue
			}
			group.fields[field] = value
		}
		for endpoint, group := range groups {
			entry := &observeCostEntry{PeriodStatus: group.status}
			pickFloat(group.fields, "total_cost", &entry.Cost)
			pickFloat(group.fields, "energy_kwh", &entry.EnergyKWh)
			pickFloat(group.fields, "total_tokens", &entry.Tokens)
			if !group.newest.IsZero() {
				entry.PeriodStart = group.newest.UTC().Format(time.RFC3339)
			}
			if group.currency != "" {
				payload.Cost.Currency = group.currency
			}
			cp.into[endpoint] = entry
		}
	}

	if queries > 0 && failures == queries {
		return nil, fmt.Errorf("all %d observe influx queries failed", queries)
	}
	return payload, nil
}

// observeHandler serves /internal/ui/observe.json with a content ETag so
// idle dashboards cost one cheap 304 per poll.
func (a *Application) observeHandler(w http.ResponseWriter, r *http.Request) {
	var payload *ObservePayload
	if a.observe != nil {
		p, err := a.observe.payload(r.Context())
		if err != nil {
			http.Error(w, "observe source unavailable: "+err.Error(), http.StatusServiceUnavailable)
			return
		}
		payload = p
	} else {
		payload = newEmptyObservePayload(runtimeMetricsSourceSampler, time.Now())
	}
	body, err := json.Marshal(payload)
	if err != nil {
		http.Error(w, "observe encode failed", http.StatusInternalServerError)
		return
	}
	sum := sha256.Sum256(body)
	etag := `"` + hex.EncodeToString(sum[:]) + `"`
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "no-cache")
	if match := r.Header.Get("If-None-Match"); match != "" && (match == etag || match == "W/"+etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(body)
}

func pickFloat(fields map[string]float64, field string, into **float64) {
	if v, ok := fields[field]; ok {
		*into = floatPtr(v)
	}
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func sortedKeysAny[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
