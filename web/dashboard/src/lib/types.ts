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

export interface EndpointRuntimeRate {
  current?: number;
  avg?: number;
  max?: number;
}

export interface EndpointRuntimePercentage {
  current?: number;
  avg?: number;
}

export interface EndpointRuntimeMetrics {
  sampled_at?: string;
  available_since?: string;
  stale?: boolean;
  generation_tps?: EndpointRuntimeRate;
  prefill_tps?: EndpointRuntimeRate;
  cache_hit_percent?: EndpointRuntimePercentage;
  kv_cache_usage_percent?: number;
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
