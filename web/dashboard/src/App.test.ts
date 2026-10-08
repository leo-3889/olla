import { flushSync, mount, unmount } from 'svelte';
import { describe, it, expect, afterEach, vi } from 'vitest';
import App from './App.svelte';
import { navigation } from './lib/stores/navigation.svelte';

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
global.fetch = vi.fn(async () => ({
  status: 200,
  ok: true,
  headers: { get: () => null },
  json: async () => ({
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
  }),
}));

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
