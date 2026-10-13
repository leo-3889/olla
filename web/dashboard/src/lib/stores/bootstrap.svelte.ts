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
    { id: 'gpu', enabled: false, reason: 'requires metrics_source.mode=influx and metrics_source.influx.gpu_endpoint' },
  ],
  runtime_metric_columns: [
    { key: 'generation_tps', label: 'Generation', unit: 'tok_s', window: true },
    { key: 'prefill_tps', label: 'Prefill', unit: 'tok_s', window: true },
    { key: 'cache_hit_percent', label: 'Prompt-cache hit rate', unit: 'percent', window: true },
    { key: 'kv_cache_usage_percent', label: 'KV-cache utilisation', unit: 'percent', window: false },
    { key: 'queue_depth', label: 'Queue depth', unit: 'count', window: false },
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
