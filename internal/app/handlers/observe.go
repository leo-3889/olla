package handlers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
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
// query load. It sits ABOVE the dashboard's 10s observe poll. The memo age
// runs from the last COLLECT, so the honest steady state at 10s poll / 12s
// TTL is 304/200 ALTERNATING: the poll whose memo age crosses 12s pays a
// re-collect (new generated_at -> new ETag -> 200), the next one replays
// against the fresh memo -> 304. Net effect: one Influx fan-out per ~20s
// per client set, halving poll traffic; a TTL below the poll interval would
// re-collect (and re-stamp generated_at) on every poll, defeating the ETag.
const observeTTL = 12 * time.Second

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
	Endpoint           string   `json:"endpoint"`
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
	Currency     string   `json:"currency,omitempty"`
	// AgeS is how old the cost SERVICE's newest heartbeat is (not the
	// rollup: rollup _time is the period start, so it can never express
	// freshness). Seconds; nil when the heartbeat is unknown.
	AgeS *float64 `json:"age_s"`
}

type observeCost struct {
	Currency string                       `json:"currency"`
	Hour     map[string]*observeCostEntry `json:"hour"`
	Day      map[string]*observeCostEntry `json:"day"`
	// Dropped names endpoints whose newest rollup existed but was rejected
	// by the liveness rules (older than the newest period, or past the
	// wall-clock anchor) - typically an endpoint that left the fleet
	// mid-period. Without this the row vanishes silently and an empty
	// table reads as "no fleet" instead of "these endpoints went dark".
	Dropped []string `json:"dropped"`
	// Errors names the cost periods whose Flux query FAILED this cycle
	// ("hour", "day"). Without it a period failure renders as an empty
	// map, and the UI's "no rollups yet" note sits over an error - a
	// partial outage must read as an outage, not as an idle fleet.
	Errors []string `json:"errors"`
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
	// GPUsAbsent lists configured metrics_source.influx.gpu_endpoints that
	// returned ZERO rows in the window: a dead exporter erases its cards
	// (rows expire with the 5m window), and a section that silently shows
	// 2 of 5 GPUs reads as a healthy small fleet. The UI must render an
	// explicit "no data" card per entry.
	GPUsAbsent []string `json:"gpus_absent"`
}

func newEmptyObservePayload(source string, now time.Time) *ObservePayload {
	return &ObservePayload{
		Schema:      1,
		GeneratedAt: now.UTC(),
		Source:      source,
		Power:       []observePowerRow{},
		Cost:        observeCost{Currency: "EUR", Hour: map[string]*observeCostEntry{}, Day: map[string]*observeCostEntry{}, Dropped: []string{}, Errors: []string{}},
		GPUs:        []observeGpuRow{},
		GPUsAbsent:  []string{},
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
	// Negative cache: a wedged Influx must not be re-fanned (5 queries) on
	// every client poll; failures are memoized for TTL/2 and served from
	// the memo (the 503 has no ETag, so clients revalidate cheaply).
	memoErr   error
	memoErrAt time.Time
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
	if o.memoErr != nil && o.now().Sub(o.memoErrAt) < o.ttl/2 {
		return nil, o.memoErr
	}
	payload, err := o.collect(ctx)
	if err != nil {
		// The collect context inherits the REQUEST context, so a client
		// aborting an in-flight poll (tab close) lands here with
		// context.Canceled while Influx is perfectly healthy. Memoizing
		// that would serve 503 from the negative cache to EVERY client
		// for TTL/2 - one closed tab blacking the section for everyone.
		if ctx.Err() == nil {
			o.memoErr, o.memoErrAt = err, o.now()
		}
		return nil, err
	}
	o.memo, o.memoAt = payload, o.now()
	o.memoErr = nil
	return payload, nil
}

//nolint:gocognit // the complexity IS the section matrix: power, cost and GPU
// each fan out over queries with their own error/absent disclosure rules. The
// per-section helpers are already extracted (powerRows/costRows/gpuRows);
// flattening the orchestration would hide which leg is allowed to fail.
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
		// A meter can be attributed to several endpoints (one plug, two
		// boxes). Label the row with ALL of them, in sorted order:
		// first-wins would tie the row to one endpoint and silently swap
		// the label the moment that endpoint retires.
		endpointsForTarget := map[string][]string{}
		for _, endpoint := range sortedKeys(o.cfg.PowerAttribution) {
			target := o.cfg.PowerAttribution[endpoint]
			endpointsForTarget[target] = append(endpointsForTarget[target], endpoint)
		}
		labelForTarget := make(map[string]string, len(endpointsForTarget))
		targets := make([]string, 0, len(endpointsForTarget))
		for target, endpoints := range endpointsForTarget {
			targets = append(targets, target)
			labelForTarget[target] = strings.Join(endpoints, ", ")
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
				row := observePowerRow{Target: target, Endpoint: labelForTarget[target], DeviceName: group.device, Node: group.node}
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

	if len(o.cfg.GPUEndpoints) > 0 {
		queries++
		rows, err := o.client.query(cycleCtx, fmt.Sprintf(`from(bucket:%q) |> range(start:-5m) |> filter(fn:(r)=>r._measurement=="llm_runtime") |> filter(fn:(r)=>contains(set:[%s], value:r.endpoint)) |> last()`, o.cfg.Bucket, fluxStringSet(o.cfg.GPUEndpoints)))
		if err != nil {
			fail("gpu", err)
			// The success branch computes GPUsAbsent from seen rows;
			// on a failed query NOTHING was seen, so every configured
			// endpoint is unverified. Disclose them all - a dead GPU
			// query must not render as an empty healthy grid.
			payload.GPUsAbsent = append(payload.GPUsAbsent, o.cfg.GPUEndpoints...)
			sort.Strings(payload.GPUsAbsent)
		} else {
			type gpuGroup struct {
				endpoint string
				gpu      string
				fields   map[string]float64
				node     *string
				newest   time.Time
			}
			groups := map[string]*gpuGroup{}
			for _, row := range rows {
				gpu, _ := row["gpu"].(string)
				endpoint, _ := row["endpoint"].(string)
				field, _ := row["_field"].(string)
				if gpu == "" || field == "" {
					continue
				}
				value, ok := influxFloat(row["_value"])
				if !ok {
					continue
				}
				key := endpoint + "\x00" + gpu
				group := groups[key]
				if group == nil {
					group = &gpuGroup{endpoint: endpoint, gpu: gpu, fields: map[string]float64{}}
					groups[key] = group
				}
				if node, ok := row["node"].(string); ok && node != "" && group.node == nil {
					group.node = &node
				}
				group.fields[field] = value
				if at := influxTime(row["_time"]); at.After(group.newest) {
					group.newest = at
				}
			}
			for _, key := range sortedKeysAny(groups) {
				group := groups[key]
				out := observeGpuRow{Endpoint: group.endpoint, GPU: group.gpu, Node: group.node}
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
			seen := map[string]bool{}
			for _, group := range groups {
				seen[group.endpoint] = true
			}
			for _, ep := range o.cfg.GPUEndpoints {
				if !seen[ep] {
					payload.GPUsAbsent = append(payload.GPUsAbsent, ep)
				}
			}
			sort.Strings(payload.GPUsAbsent)
		}
	}

	currencies := map[string]bool{}
	costRowsRendered := false
	dedupDrop := func() {
		seen := map[string]bool{}
		kept := payload.Cost.Dropped[:0]
		for _, ep := range payload.Cost.Dropped {
			if !seen[ep] {
				seen[ep] = true
				kept = append(kept, ep)
			}
		}
		payload.Cost.Dropped = kept
	}
	costPeriods := []struct {
		period string
		into   map[string]*observeCostEntry
		// window bounds the scan. A period start can never be older than
		// its horizon (one hour period is open at a time; the AEST day
		// starts 22:00Z-23:59Z, so a live day period started <=26h ago),
		// and the liveness anchor below rejects anything past its maxAge -
		// so each window fetches everything renderable plus slack for a late
		// writer, and nothing else. The windows used to be -8d: 2.7s of
		// measured influx wall per TTL rebuild spent scanning rows the
		// anchor always drops. Ghosts inside the slack are dropped too,
		// never rendered. A period start is an ABSOLUTE instant, so the open
		// hour period always started within the last hour (tz offsets cannot
		// shift it): -4h is the renderable hour plus 3h of writer slack, and
		// recomputed CLOSED hour rows older than maxAge are anchor-dropped
		// whether or not the window scans them - narrowing changes cost, not
		// output. Day keeps -30h (26h horizon + 4h slack, AEST 22:00Z starts).
		window string
		// maxAge anchors the newest-period rule to wall-clock: when EVERY
		// endpoint is retired, "newest" is a frozen period start and the
		// relative rule alone would label it "this hour". A period may
		// only render if it started within maxAge of now. The margins are
		// deliberately generous and tz-safe: day periods start at AEST
		// midnight (= 22:00Z), so a UTC floor(now) would drop valid day
		// rows; 2h/26h tolerate any offset a period can start with.
		maxAge time.Duration
	}{{"hour", payload.Cost.Hour, "-4h", 2 * time.Hour}, {"day", payload.Cost.Day, "-30h", 26 * time.Hour}}
	for _, cp := range costPeriods {
		queries++
		// _time of a cost rollup is the PERIOD START, so the window must
		// span longer than one period to find the newest rollup at all.
		rows, err := o.client.query(cycleCtx, fmt.Sprintf(`from(bucket:%q) |> range(start:%s) |> filter(fn:(r)=>r._measurement=="llm_endpoint_cost_v3") |> filter(fn:(r)=>r.period==%q)`, o.cfg.CostsBucket, cp.window, cp.period))
		if err != nil {
			fail("cost "+cp.period, err)
			payload.Cost.Errors = append(payload.Cost.Errors, cp.period)
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
		// Liveness guard: _time of a rollup is its PERIOD START, and the
		// cost service stops writing for an endpoint that leaves the fleet,
		// freezing its newest rollup mid-period. The current period is the
		// newest period start observed across endpoints (all live endpoints
		// share it), so anything older is a ghost of a retired/absent
		// endpoint (or an endpoint that missed the whole period, whose
		// numbers would be a previous hour labelled "this hour"). Render
		// only the newest-period rows.
		var newest time.Time
		for _, group := range groups {
			if group.haveTime && group.newest.After(newest) {
				newest = group.newest
			}
		}
		anchor := o.now().UTC().Add(-cp.maxAge)
		for endpoint, group := range groups {
			if !group.haveTime || !group.newest.Equal(newest) || group.newest.UTC().Before(anchor) {
				if endpoint != "" && group.haveTime {
					payload.Cost.Dropped = append(payload.Cost.Dropped, endpoint)
				}
				continue
			}
			entry := &observeCostEntry{PeriodStatus: group.status, Currency: group.currency}
			pickFloat(group.fields, "total_cost", &entry.Cost)
			pickFloat(group.fields, "energy_kwh", &entry.EnergyKWh)
			pickFloat(group.fields, "total_tokens", &entry.Tokens)
			if !group.newest.IsZero() {
				entry.PeriodStart = group.newest.UTC().Format(time.RFC3339)
			}
			if group.currency != "" {
				currencies[group.currency] = true
			}
			cp.into[endpoint] = entry
			costRowsRendered = true
		}
		dedupDrop()
	}
	// The section header shows one currency; claiming it for the whole
	// payload is only honest when every rendered row agrees (Go map
	// iteration order would otherwise pick a random one and churn the
	// ETag). On disagreement the per-entry currency is authoritative.
	if len(currencies) == 1 {
		for cur := range currencies {
			payload.Cost.Currency = cur
		}
	}
	if costRowsRendered {
		// Cost rollups carry no usable age (_time = period start), so
		// stamp the cost service's own heartbeat: "numbers are this
		// fresh" is the operator's question.
		queries++
		hbRows, hbErr := o.client.query(cycleCtx, fmt.Sprintf(`from(bucket:%q) |> range(start:-1h) |> filter(fn:(r)=>r._measurement=="llm_service_status") |> filter(fn:(r)=>r.service=="cost-service") |> last()`, o.cfg.CostsBucket))
		if hbErr != nil {
			fail("cost age", hbErr)
		} else {
			var hbNewest time.Time
			for _, row := range hbRows {
				if at := influxTime(row["_time"]); at.After(hbNewest) {
					hbNewest = at
				}
			}
			if !hbNewest.IsZero() {
				age := payload.GeneratedAt.Sub(hbNewest).Seconds()
				for _, entry := range payload.Cost.Hour {
					entry.AgeS = floatPtr(age)
				}
				for _, entry := range payload.Cost.Day {
					entry.AgeS = floatPtr(age)
				}
			}
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
	// Same convention as every other JSON route: weak validator + private
	// revalidation (one ETag dialect on the surface, nothing shared with
	// intermediaries).
	writeJSONWithETag(w, r, body, `W/"`+hex.EncodeToString(sum[:])+`"`)
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
