// Dashboard bootstrap: fetches /internal/ui/config.json ONCE at startup and
// derives everything config-driven from it - which nav sections exist, which
// runtime metric columns the endpoints table renders, and whether the
// observe store (power/cost/GPU, only backed by metrics_source=influx) is
// created at all. When the manifest cannot be reached the built-in sampler
// defaults keep the dashboard byte-identical to a pre-manifest build: a
// broken config endpoint must never take the UI down.
import { createPollStore, type PollStore } from './poll-store.svelte';
import { navigation, SECTIONS, type Section } from './navigation.svelte';
import type { ObserveResponse, UIConfig } from '../types';

const CONFIG_URL = '/internal/ui/config.json';

export const DEFAULT_UI_CONFIG: UIConfig = {
  schema: 1,
  metrics_source: 'sampler',
  // Must equal the stores' own fallback defaults (poll stores below): the
  // manifest is a promise about cadence, not decoration.
  poll_ms: { overview: 5000, endpoints: 5000, models: 15000, observe: 10000 },
  sections: [
    { id: 'overview', enabled: true },
    { id: 'endpoints', enabled: true },
    { id: 'models', enabled: true },
    { id: 'power', enabled: false, reason: 'requires metrics_source.mode=influx' },
    { id: 'cost', enabled: false, reason: 'requires metrics_source.mode=influx' },
    { id: 'gpu', enabled: false, reason: 'requires metrics_source.mode=influx and metrics_source.influx.gpu_endpoints' },
  ],
  // Built-in defaults mirror the Go dialect union (runtime_dialect.UnionColumns
  // over the registered dialects) INCLUDING the help strings, published
  // verbatim by /internal/ui/config.json - the manifest always wins.
  runtime_metric_columns: [
    { key: 'generation_tps', label: 'Generation', unit: 'tok_s', window: true, help: 'Window: the last 10 minutes. Headline = the window MAX; sub-row = window avg/max. Source: engine-reported generation throughput (10s sampler scrapes / 1m ollametrics Influx windows) - max of 10s samples, not a true peak. Blanks with the whole runtime envelope when the source is stale.' },
    { key: 'prefill_tps', label: 'Prefill', unit: 'tok_s', window: true, help: 'Window: the last 10 minutes. Headline = the window MAX; sub-row = window avg/max. Source: engine-reported prefill throughput (10s sampler scrapes / 1m ollametrics Influx windows) - max of 10s samples, not a true peak. Blanks with the whole runtime envelope when the source is stale.' },
    { key: 'cache_hit_percent', label: 'Prompt-cache hit rate', unit: 'percent', window: true, help: "Window: the last 10 minutes. Headline = the latest engine sample; sub-row = window avg. Source: engine prompt-cache counters (cached / (cached + computed) prompt tokens, or the engine's own hit-rate gauge) via sampler scrapes / ollametrics Influx. Blanks with the runtime envelope when the source is stale." },
    { key: 'requests_running', label: 'Running', unit: 'count', window: false, help: "Window: the last 10 minutes. Headline = the window MAX of the engine's running-requests gauge (10s sampler scrapes / 1m ollametrics Influx windows) - max of 10s samples, not a true peak. Blanks with the whole runtime envelope when the source is stale." },
    { key: 'queue_depth', label: 'Waiting', unit: 'count', window: false, help: 'Window: the last 10 minutes. Headline = queue_depth sampled AT THE TIMESTAMP of the window max of Running (both engine gauges are written per scrape at identical timestamps). Dash = no paired sample in the window - never the current value, never 0. Source: engine queue gauge via sampler / ollametrics. Blanks with the runtime envelope when the source is stale.' },
  ],
  // Must match the Go uiStaticColumns(): lifetime token production split by
  // phase plus KV-cache reuse - all outside the dialect union (per-day
  // integrals / engine gauges, not window-sampled engine rates).
  static_metric_columns: [
    { key: 'generation_tokens', label: 'Tokens 180d (Gen)', unit: 'count', window: false, help: "Window: the effective token lookback 180d = min(configured token_lookback, the bucket's measured retention). Source: per-UTC-day integrals of the engine token counter via ollametrics/Influx - influx mode only; sampler mode dashes every cell. A missing day window shrinks the sum: the cell tooltip then says LOWER BOUND. Dash = no data, never a fabricated 0." + ' Counter: generation_tokens_total.' },
    { key: 'prefill_tokens', label: 'Tokens 180d (Prefill)', unit: 'count', window: false, help: "Window: the effective token lookback 180d = min(configured token_lookback, the bucket's measured retention). Source: per-UTC-day integrals of the engine token counter via ollametrics/Influx - influx mode only; sampler mode dashes every cell. A missing day window shrinks the sum: the cell tooltip then says LOWER BOUND. Dash = no data, never a fabricated 0." + ' Counter: prompt_tokens_total (llama.cpp-family counts COMPUTED prompt tokens).' },
    { key: 'kv_cache_reuse_percent', label: 'KV cache reuse', unit: 'percent', window: false, help: "Per-engine basis, named in the cell tooltip: the engine's prefix-cache gauge (measured since engine boot, resets on every restart) or the ratio cached/(cached+computed) prompt tokens over the token window. Source: engine metrics via ollametrics/Influx; sampler mode dashes. Dash = the engine exposes no cache-reuse signal." },
  ],
};

class BootstrapStore {
  config: UIConfig = $state(DEFAULT_UI_CONFIG);
  state: 'defaults' | 'ready' | 'failed' = $state('defaults');
  observe: PollStore<ObserveResponse> | null = $state(null);
  #loaded = false;
  #warned = false;

  sectionEnabled(id: string): boolean {
    return this.config.sections.some((s) => s.id === id && s.enabled);
  }

  /** force re-fetches the manifest (test seam for flipping source modes;
   *  the singleton otherwise loads exactly once per page). */
  async load(force = false): Promise<void> {
    if (this.#loaded && !force) return;
    this.#loaded = true;
    try {
      const init: RequestInit = { headers: { Accept: 'application/json' }, cache: 'no-store' };
      if (typeof AbortSignal !== 'undefined' && typeof AbortSignal.timeout === 'function') {
        init.signal = AbortSignal.timeout(5000);
      }
      const resp = await fetch(CONFIG_URL, init);
      if (!resp.ok) throw new Error('HTTP ' + resp.status);
      const body = (await resp.json()) as UIConfig;
      if (typeof body !== 'object' || body === null || !Array.isArray(body.sections) || !Array.isArray(body.runtime_metric_columns)) {
        throw new Error('invalid config.json shape');
      }
      this.config = body;
      this.state = 'ready';
    } catch (e) {
      this.state = 'failed';
      if (!this.#warned) {
        this.#warned = true;
        console.warn('dashboard config.json unavailable, using built-in defaults:', e);
      }
    }
    navigation.configure(this.config.sections.filter((s) => s.enabled).map((s) => s.id));
    const wantsObserve = ['power', 'cost', 'gpu'].some((id) => this.sectionEnabled(id));
    if (wantsObserve && !this.observe) {
      // Created (not started): App.svelte starts it next to overview; the
      // scheduler owns its lifecycle from there.
      this.observe = createPollStore<ObserveResponse>({
        name: 'observe',
        url: '/internal/ui/observe.json',
        intervalMs: this.config.poll_ms.observe || 10000,
      });
    }
  }
}

export const bootstrap = new BootstrapStore();
export type { Section };
export { SECTIONS };
