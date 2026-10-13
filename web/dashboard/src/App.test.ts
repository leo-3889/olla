import { flushSync, mount, unmount } from 'svelte';
import { describe, it, expect, afterEach, vi } from 'vitest';
import App from './App.svelte';
import { navigation } from './lib/stores/navigation.svelte';
import { bootstrap } from './lib/stores/bootstrap.svelte';

// Regression coverage for finding 11: App.svelte used to hold its own
// `current` state, separate from the `navigation` store NavTabs renders
// aria-selected/tabindex/arrow-key position from. Clicking an endpoint row
// on Overview (a programmatic jump) rendered the Endpoints panel while the
// tab bar kept announcing "Overview, selected" and held keyboard focus
// position there - it only self-healed on the next explicit tab click.

// App.svelte starts the poll scheduler and fetches on mount; stub fetch so
// that doesn't error out mid-test (the panels aren't what's under test here).
// A well-formed /internal/status body: the scheduler keeps ticking while
// async tests await, and overview's history sampler reads sys.* - an empty
// object here leaked "cannot read properties of undefined (system)" errors
// out of background ticks into the runner.
function jsonResponse(body: unknown) {
  return { status: 200, ok: true, headers: { get: () => null }, json: async () => body };
}

const STATUS_BODY = {
  timestamp: '2026-01-01T00:00:00Z',
  proxy: { engine: 'olla', profile: 'ollama', balancer: 'recommended' },
  endpoints: [],
  security: { status: 'ok', blocked_ips: 0, violations: { rate_limits: 0, size_limits: 0 } },
  system: {
    start_time: '2026-01-01T00:00:00Z',
    status: 'healthy',
    endpoints_up: '0/0',
    success_rate: '100%',
    avg_latency: '0ms',
    total_traffic: '0 B',
    uptime: '0s',
    version: 'v0.0.0',
    commit: 'deadbeef',
    active_connections: 0,
    security_violations: 0,
  },
};

// Sampler-mode manifest and its (empty-by-contract) observe payload. Tests
// that need influx mode override the mock's implementation locally.
let CONFIG_BODY: Record<string, unknown> = {
  schema: 1,
  metrics_source: 'sampler',
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
let OBSERVE_BODY: Record<string, unknown> = {
  schema: 1,
  generated_at: '2026-01-01T00:00:00Z',
  source: 'sampler',
  power: [],
  cost: { currency: 'EUR', hour: {}, day: {} },
  gpus: [],
};

global.fetch = vi.fn(async (input: unknown) => {
  const url = String(input);
  if (url.includes('/internal/ui/config.json')) return jsonResponse(CONFIG_BODY);
  if (url.includes('/internal/ui/observe.json')) return jsonResponse(OBSERVE_BODY);
  return jsonResponse(STATUS_BODY);
}) as ReturnType<typeof vi.fn>;

let component: ReturnType<typeof mount> | undefined;
afterEach(() => {
  if (component) unmount(component);
  document.body.innerHTML = '';
  navigation.set('overview'); // leave the singleton store clean for other test files
});

describe('App navigation authority', () => {
  it('a programmatic jump updates both the rendered panel and the tab bar state', () => {
    component = mount(App, { target: document.body });
    flushSync();

    expect(document.getElementById('panel-overview')).not.toBeNull();
    expect(document.getElementById('tab-overview')!.getAttribute('aria-selected')).toBe('true');

    // Simulate a programmatic jump the way OverviewPanel's "jump to
    // endpoint" link does, via the same navigation authority App uses.
    navigation.set('endpoints');
    flushSync();

    // The panel actually rendered...
    expect(document.getElementById('panel-endpoints')).not.toBeNull();
    expect(document.getElementById('panel-overview')).toBeNull();
    // ...and the tab bar agrees, rather than still announcing Overview.
    expect(document.getElementById('tab-endpoints')!.getAttribute('aria-selected')).toBe('true');
    expect(document.getElementById('tab-overview')!.getAttribute('aria-selected')).toBe('false');
    expect(document.getElementById('tab-endpoints')!.getAttribute('tabindex')).toBe('0');
    expect(document.getElementById('tab-overview')!.getAttribute('tabindex')).toBe('-1');
  });

  it('arrow-key navigation continues from the correct tab after a programmatic jump', () => {
    component = mount(App, { target: document.body });
    flushSync();

    navigation.set('endpoints');
    flushSync();

    const tabs = document.querySelector('[role="tablist"]');
    tabs!.dispatchEvent(new KeyboardEvent('keydown', { key: 'ArrowRight', bubbles: true, cancelable: true }));
    flushSync();

    // From endpoints (index 1 of [overview, endpoints, models]), ArrowRight
    // must land on models, not restart from wherever App's stale `current`
    // used to think the tab bar was.
    expect(document.getElementById('panel-models')).not.toBeNull();
    expect(document.getElementById('tab-models')!.getAttribute('aria-selected')).toBe('true');
  });
});

// The axe-core devDependency existed for the whole campaign with zero call
// sites - a dependency that gates nothing. One real scan on the mounted app:
// axe-core in jsdom cannot see layout/colour, but it does catch the
// structural class of defects (landmarks, tab/aria wiring like the
// aria-controls fix, labels, duplicate ids) that the panels' unit tests miss.
describe('App accessibility (axe-core structural scan)', () => {
  it('the mounted dashboard has no axe violations', async () => {
    const axe = (await import('axe-core')).default;
    component = mount(App, { target: document.body });
    flushSync();

    const results = await axe.run(document.body, {
      // jsdom has no layout engine: colour-contrast and viewport-scale meta
      // need a real renderer (checked separately by contrast.test.ts and the
      // live browser pass). Everything else is layout-independent.
      rules: {
        'color-contrast': { enabled: false },
        'meta-viewport': { enabled: false },
      },
    });
    const pretty = results.violations.map(
      (v) => `${v.id} (${v.impact}): ${v.nodes.map((n) => n.html).join(' | ')}`
    );
    expect(pretty).toEqual([]);
  });
});

// Influx mode end-to-end render: manifest enables power/cost/gpu, the
// observe payload carries live rows. This is the only automated pass over
// the Observe* components' markup, so it also axe-scans them (the sampler
// scan above never renders them).
describe('App influx-mode observe sections', () => {
  it('renders power, cost and GPU sections from the observe payload with no axe violations', async () => {
    const axe = (await import('axe-core')).default;
    CONFIG_BODY = {
      ...CONFIG_BODY,
      metrics_source: 'influx',
      sections: [
        { id: 'overview', enabled: true },
        { id: 'endpoints', enabled: true },
        { id: 'models', enabled: true },
        { id: 'power', enabled: true },
        { id: 'cost', enabled: true },
        { id: 'gpu', enabled: true },
      ],
    };
    OBSERVE_BODY = {
      schema: 1,
      generated_at: '2026-10-13T09:00:00Z',
      source: 'ollametrics',
      power: [
        {
          target: 'shelly-ai-395',
          endpoint: 'halogen-395',
          node: 'leo395max',
          device_name: 'Shelly Plug S 395',
          watts: 118.4,
          temp_c: 41.2,
          meter_valid: 1,
          overpower: 0,
          output_on: 1,
          age_s: 4,
        },
      ],
      cost: {
        currency: 'EUR',
        hour: { 'halogen-395': { cost: 0.045, period_status: 'open', period_start: '2026-10-13T09:00:00Z', tokens: 120000, energy_kwh: 0.31 } },
        day: { 'halogen-395': { cost: 0.83, period_status: 'open', period_start: '2026-10-13T00:00:00Z', tokens: 2400000, energy_kwh: 2.9 } },
      },
      gpus: [
        {
          gpu: 0,
          node: 'bigbox',
          present: 1,
          busy_percent: 62.5,
          mem_activity_percent: 40,
          socket_power_watts: 137,
          edge_temp_c: 64.1,
          vram_used_bytes: 12 * 1024 ** 3,
          vram_total_bytes: 32 * 1024 ** 3,
          age_s: 6,
        },
      ],
    };
    await bootstrap.load(true); // re-read the manifest in influx mode
    // Earlier tests in this file navigated to models and App pushed that
    // hash; jsdom's URL persists across tests, so pin the landing panel.
    window.history.replaceState(null, '', '/');
    navigation.set('overview');
    component = mount(App, { target: document.body });
    flushSync();
    await vi.waitFor(() => expect(document.body.textContent).toContain('Shelly Plug S 395'), { timeout: 2000 });

    expect(document.body.textContent).toContain('Power');
    expect(document.body.textContent).toContain('118.4');
    expect(document.body.textContent).toContain('Cost');
    expect(document.body.textContent).toContain('GPU');
    expect(document.body.textContent).toContain('partial'); // open rollups labelled partial

    const results = await axe.run(document.body, {
      rules: { 'color-contrast': { enabled: false }, 'meta-viewport': { enabled: false } },
    });
    const pretty = results.violations.map(
      (v) => `${v.id} (${v.impact}): ${v.nodes.map((n) => n.html).join(' | ')}`
    );
    expect(pretty).toEqual([]);

    // Leave the singleton clean for anything that mounts App afterwards.
    CONFIG_BODY = { ...CONFIG_BODY, metrics_source: 'sampler' };
    await bootstrap.load(true);
  });
});
