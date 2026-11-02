// API contracts for the three status surfaces, hand-mirrored from the Go
// handler response structs. Types only, no runtime code. Keep this in
// lockstep with:
//   - internal/app/handlers/handler_status.go
//   - internal/app/handlers/handler_status_endpoints.go
//   - internal/app/handlers/handler_status_models.go
//
// Discipline: time.Time serialises via encoding/json as an ISO 8601 string,
// so every time.Time becomes string here. Pointer fields and omitempty
// fields become optional; plain fields with no omitempty stay required.

// Poll lifecycle state shared by every store. Lifted here so the stores can
// pin PollStore<StatusResponse> etc. against a single source of truth.
export type PollStatus = 'loading' | 'ok' | 'error' | 'stale';

// --- handler_status.go -------------------------------------------------

export interface SecurityViolation {
  rate_limits: number;
  size_limits: number;
}

export interface SecuritySummary {
  status: string;
  blocked_ips: number;
  violations: SecurityViolation;
}

export interface ProxySummary {
  engine: string;
  profile: string;
  balancer: string;
}

export interface SystemSummary {
  start_time: string;
  status: string;
  endpoints_up: string;
  success_rate: string;
  avg_latency: string;
  total_traffic: string;
  uptime: string;
  version: string;
  commit: string;
  active_connections: number;
  security_violations: number;
  total_requests: number;
  total_failures: number;
  // Lets the dashboard branch on the no-traffic state without guessing from
  // a "0%" success rate. Always emitted by the backend (no omitempty); false
  // on a fresh boot, flips true once any proxy request has been counted.
  // When false, success_rate arrives as "N/A" rather than "0%".
  has_traffic: boolean;
  // The computation that produced status: "host" (the endpoint fleet, the
  // default) or "model" (a configured list of models/aliases). Always emitted.
  health_mode: string;
  // Sticky-session statistics, mirrored from GET /internal/stats/sticky
  // (same shape). Branch on .enabled, never on presence.
  sticky?: StickyStats;
  // Per-model breakdown, present only in model mode.
  models?: ModelHealthEntry[];
}

export interface ModelHealthEntry {
  name: string;
  // healthy | unhealthy | unknown
  status: string;
  total_requests: number;
  successful_requests: number;
  failed_requests: number;
  // "N/A" when the model has no traffic.
  success_rate: string;
}

export interface EndpointModelsResponse {
  last_updated: string;
  count: number;
}

export interface EndpointResponse {
  avg_latency_ms?: number;
  runtime_metrics?: EndpointRuntimeMetrics;
  next_check_at?: string;
  health_check_at?: string;
  models: EndpointModelsResponse;
  name: string;
  status: string;
  success_rate: string;
  avg_latency: string;
  traffic: string;
  last_check: string;
  next_check: string;
  issues: string;
  url: string;
  id: string;
  priority: number;
  connections: number;
  requests: number;
  min_latency_ms: number;
  max_latency_ms: number;
}

export interface StatusResponse {
  timestamp: string;
  proxy: ProxySummary;
  endpoints: EndpointResponse[];
  security: SecuritySummary;
  system: SystemSummary;
}

// --- handler_status_endpoints.go --------------------------------------

export interface EndpointSummary {
  avg_latency_ms?: number;
  next_check_at?: string;
  health_check_at?: string;
  last_model_sync_at?: string;
  name: string;
  type: string;
  status: string;
  last_model_sync?: string;
  health_check: string;
  response_time?: string;
  success_rate: string;
  issues?: string;
  url: string;
  id: string;
  priority: number;
  model_count: number;
  request_count: number;
  // Gateway request counter DELTA over the 10-minute reporting window
  // (the ~10s status poll is the sampling cycle) and the span it covers.
  // ABSENT on the first poll after boot and across a gateway counter
  // reset - render the lifetime counter then, never a fake 0.
  requests_window?: number | null;
  requests_window_ms?: number | null;
  // Disclose the token sums as best-effort LOWER BOUNDS (a window day is
  // missing: cold-boot backfill in flight, or a day query failed). The
  // cell tooltip says so; present only where the sum is served.
  generation_tokens_partial?: boolean;
  prefill_tokens_partial?: boolean;
  // Proxy bytes through this endpoint (in + out), pre-formatted. Emitted by
  // the backend since the traffic column landed; empty string for endpoints
  // with no stats (the UI renders a dash via ||, not ??). Optional so
  // fixtures built against older payloads stay valid.
  traffic?: string;
  // Lifetime token production, split by phase: each field is the integral
  // of the engine's per-day token flux over the lookback window
  // (metrics_source.influx.token_lookback, 180d) - NOT the live engine
  // counter, which resets on every engine restart. ABSENT when
  // metrics_source is sampler, the day queries are failing, or the engine
  // exports no such counter - the panel renders a dash via fmtTokens,
  // never a fabricated 0. Pre-rename rows under ghost endpoint ids are NOT
  // credited to the current id.
  generation_tokens?: number | null;
  prefill_tokens?: number | null;
  // KV-cache reuse as a 0-100 percentage. Basis is per-engine: radiance
  // reports its engine prefix-cache gauge (since-boot, resets on restart),
  // halogen the 180-day ratio of cached to prompt tokens. ABSENT when the
  // engine exposes neither signal - render a dash, never a fake 0.
  kv_cache_reuse_percent?: number | null;
  // Names the basis of kv_cache_reuse_percent in operator words. The two
  // per-engine bases are not comparable, and for prefix-cache-gauge engines
  // the cell repeats the prompt-cache-hit tile - the tooltip says which
  // basis a row uses. Present exactly when the percent is present.
  kv_cache_reuse_basis?: string;
  min_latency_ms: number;
  max_latency_ms: number;
  active_connections: number;
  // Optional backend-native runtime telemetry. Older Olla versions and
  // backends without a supported metrics adapter omit this object entirely;
  // individual measurements may also be absent when a backend cannot supply
  // them. `current` is the latest sample; `avg`/`max` cover the 10-minute
  // reporting window. `stale` means Olla retained a previous scrape after the
  // collector stopped receiving fresh data, so the UI must not present its
  // values as live measurements.
  runtime_metrics?: EndpointRuntimeMetrics;
}

export interface EnvelopeMetric {
  // Wire keys: generation_tps | prefill_tps | cache_hit_percent |
  // kv_cache_usage_percent | queue_depth | requests_running | spec_accept_percent.
  // Unknown future keys MUST render (raw) rather than throw.
  key: string;
  // tok_s | percent | count today; unknown units render the number as-is.
  unit: string;
  current?: number;
  avg?: number;
  max?: number;
  // "engine" marks an engine-wide batch aggregate; the note then carries the
  // full caveat text. The provider states semantics - the UI never infers.
  scope?: string;
  // e.g. spec_accept_percent attaches to the generation_tps cell.
  attach_to?: string;
  note?: string;
}

export interface EndpointRuntimeMetrics {
  sampled_at?: string;
  available_since?: string;
  stale?: boolean;
  // Backend dialect the values were parsed for (vllm | llamacpp | radiance).
  kind?: string;
  // Who produced the numbers: "sampler" (olla scraping the engine) or
  // "ollametrics" (the canonical InfluxDB series). Optional so payloads from
  // older backends still parse.
  source?: 'sampler' | 'ollametrics';
  // Seconds between the newest underlying sample and the response.
  source_age_s?: number;
  // Absent metric = the backend does not publish that signal.
  metrics?: EnvelopeMetric[];
}

// --- /internal/ui/config.json (dashboard manifest) ---------------------

export interface UIConfigSection {
  id: string;
  enabled: boolean;
  reason?: string;
}

export interface UIConfigMetricColumn {
  key: string;
  label: string;
  unit: string;
  // Header tooltip (window, source, aggregate, known ceiling), published
  // verbatim by the backend. The built-in defaults mirror these strings.
  help?: string;
  window: boolean;
}

export interface UIConfig {
  schema: number;
  metrics_source: 'sampler' | 'influx' | string;
  poll_ms: { overview: number; endpoints: number; models: number; observe: number };
  sections: UIConfigSection[];
  runtime_metric_columns: UIConfigMetricColumn[];
  // Columns outside the dialect union (not engine-reported): the lifetime-
  // tokens column (cost rollups). Optional so manifests from older backends
  // stay valid.
  static_metric_columns?: UIConfigMetricColumn[];
}

// --- /internal/ui/observe.json (power / cost / gpu) --------------------

export interface StickyStats {
  enabled: boolean;
  active_sessions?: number;
  insertions?: number;
  hits?: number;
  misses?: number;
  evictions?: number;
  max_sessions?: number;
  idle_ttl_seconds?: number;
  // Select outcomes by label (hit/miss/bypass/no_key) and pin-producing key
  // sources, both since boot; sessions_by_endpoint is the point-in-time
  // pinned-backend distribution.
  results?: Record<string, number>;
  key_sources?: Record<string, number>;
  sessions_by_endpoint?: Record<string, number>;
}

export interface ObservePowerRow {
  target: string;
  endpoint: string;
  device_name?: string | null;
  node?: string | null;
  watts?: number | null;
  temp_c?: number | null;
  meter_valid?: number | null;
  overpower?: number | null;
  output_on?: number | null;
  age_s?: number | null;
}

export interface ObserveCostEntry {
  cost?: number | null;
  energy_kwh?: number | null;
  tokens?: number | null;
  period_status?: string | null;
  period_start?: string | null;
  currency?: string;
  // Age of the cost SERVICE heartbeat (rollup _time is the period start
  // and can never express freshness). Null when the heartbeat is unknown.
  age_s?: number | null;
}

export interface ObserveCost {
  /** endpoints whose rollup was rejected by the liveness rules (went dark) */
  dropped?: string[];
  /** cost periods whose Flux query FAILED this cycle ("hour", "day") -
   *  a failed period must read as an outage, not as "no rollups yet" */
  errors?: string[];
  currency: string;
  hour: Record<string, ObserveCostEntry>;
  day: Record<string, ObserveCostEntry>;
}

export interface ObserveGpuRow {
  endpoint: string;
  gpu: string;
  node?: string | null;
  busy_percent?: number | null;
  mem_activity_percent?: number | null;
  socket_power_watts?: number | null;
  edge_temp_c?: number | null;
  vram_used_bytes?: number | null;
  vram_total_bytes?: number | null;
  present?: number | null;
  age_s?: number | null;
}

export interface ObserveResponse {
  schema: number;
  generated_at: string;
  source: string;
  power: ObservePowerRow[];
  cost: ObserveCost;
  gpus: ObserveGpuRow[];
  /** configured gpu_endpoints with zero rows in the window (dead exporter);
   *  optional for payloads predating the field. */
  gpus_absent?: string[];
  power_absent?: string[];
}

export interface EndpointStatusResponse {
  timestamp: string;
  endpoints: EndpointSummary[];
  total_count: number;
  healthy_count: number;
  routable_count: number;
}

// --- handler_status_models.go ----------------------------------------

export interface ModelSummary {
  last_seen_at?: string;
  name: string;
  type?: string;
  family?: string;
  size?: string;
  params?: string;
  quant?: string;
  last_seen: string;
  endpoints: string[];
  // Positional pair of `endpoints`: endpoints[i] is hosted on endpoint_ids[i].
  // The backend emits both omitempty, so either may be absent on older builds;
  // when present the ids are the keys to EndpointsPanel's ep-${stableId(id)}
  // row ids (display names are NOT unique - see EndpointsPanel.dup-names.test).
  endpoint_ids?: string[];
  aliases?: string[];
  capabilities?: string[];
}

export interface ModelGroupSummary {
  family: string;
  models: ModelSummary[];
  endpoints: string[];
  model_count: number;
}

export interface ModelStatusResponse {
  timestamp: string;
  models_by_family: Record<string, string[]>;
  recent_models: ModelSummary[];
  model_groups?: ModelGroupSummary[];
  total_models: number;
  total_families: number;
  total_endpoints: number;
}

// /internal/stats/sticky. When sticky sessions are off the handler answers
// 200 {"enabled":false} and nothing else, so every other field is optional.
// Counters are raw numbers.
export interface StickyStatsResponse {
  enabled: boolean;
  active_sessions?: number;
  insertions?: number;
  hits?: number;
  misses?: number;
  evictions?: number;
  max_sessions?: number;
  idle_ttl_seconds?: number;
}

// /internal/stats/translators. Counts are numbers; rates, success and latency
// arrive as pre-formatted strings ("42.5%", "1.2s").
export interface TranslatorStatsEntry {
  translator_name: string;
  success_rate: string;
  passthrough_rate: string;
  average_latency: string;
  total_requests: number;
  successful_requests: number;
  failed_requests: number;
  passthrough_requests: number;
  translation_requests: number;
  streaming_requests: number;
  non_streaming_requests: number;
  fallback_no_compatible_endpoints: number;
  fallback_translator_does_not_support_passthrough: number;
  fallback_cannot_passthrough: number;
}

export interface TranslatorStatsResponse {
  timestamp: string;
  translators: TranslatorStatsEntry[];
  // The dashboard derives its own totals, so the parser does not populate this.
  summary?: {
    overall_success_rate: string;
    overall_passthrough_rate: string;
    total_translators: number;
    active_translators: number;
    total_requests: number;
    total_passthrough: number;
    total_translations: number;
    total_streaming: number;
    total_non_streaming: number;
  };
}

// /internal/process. Memory sizes, durations and the pressure/health labels are
// pre-formatted strings; goroutine count and GC cycles are numbers. avg_gc_pause
// is "" until the first GC has run.
export interface ProcessStatsResponse {
  timestamp: string;
  memory: {
    heap_alloc: string;
    heap_sys: string;
    heap_inuse: string;
    memory_pressure: string;
  };
  garbage_collection: {
    avg_gc_pause: string;
    num_gc_cycles: number;
  };
  goroutines: { health_status: string; count: number };
  runtime: { uptime: string; go_version: string; num_cpu: number };
}
