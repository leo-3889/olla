import { flushSync, mount, unmount } from 'svelte';
import { describe, it, expect, afterEach, vi } from 'vitest';
import { endpoints } from '../lib/stores/endpoints.svelte';
import { DEFAULT_UI_CONFIG } from '../lib/stores/bootstrap.svelte';
import EndpointsPanel from './EndpointsPanel.svelte';

// Contract of the manifest help + windowed Requests phase: header tooltips
// publish the column help verbatim (manifest wins, built-in defaults
// mirror), the Requests cell headlines the window delta with the lifetime
// counter in the tooltip, and the token cells disclose a lower bound.

let component: ReturnType<typeof mount> | undefined;
afterEach(() => {
  if (component) unmount(component);
  document.body.innerHTML = '';
});

async function refreshWithEndpoint(endpoint: Record<string, unknown>) {
  global.fetch = vi.fn(async () => ({
    status: 200,
    ok: true,
    headers: { get: () => null },
    json: async () => ({ endpoints: [endpoint], total_count: 1, healthy_count: 1, routable_count: 1 }),
  }));
  endpoints.refresh();
  await vi.waitFor(() => expect(endpoints.data?.endpoints?.[0]?.name).toBe(endpoint.name));
  flushSync();
}

function base(name: string) {
  return { name, type: 'vllm', status: 'healthy', priority: 1, model_count: 1, request_count: 14, url: 'http://x' };
}

function cell(title: string): HTMLElement | null {
  return document.body.querySelector(`td[title="${title}"]`);
}

describe('endpoints help + windowed requests', () => {
  it('defaults mirror the Go dialect union: five columns, every one with help', () => {
    expect(DEFAULT_UI_CONFIG.runtime_metric_columns.map((c) => c.key)).toEqual([
      'generation_tps', 'prefill_tps', 'cache_hit_percent', 'requests_running', 'queue_depth',
    ]);
    for (const c of [...DEFAULT_UI_CONFIG.runtime_metric_columns, ...(DEFAULT_UI_CONFIG.static_metric_columns ?? [])]) {
      expect(c.help, c.key).toBeTruthy();
    }
  });

  it('renders column help as the header tooltip', async () => {
    await refreshWithEndpoint(base('hp-1'));
    component = mount(EndpointsPanel, { target: document.body });
    flushSync();
    const th = [...document.body.querySelectorAll('th[title]')].find((t) => t.textContent?.includes('Generation'));
    expect(th?.getAttribute('title')).toContain('Window: the last 10 minutes.');
  });

  it('Requests headlines the window delta, lifetime in the tooltip', async () => {
    await refreshWithEndpoint({ ...base('hp-2'), requests_window: 4, requests_window_ms: 600000 });
    component = mount(EndpointsPanel, { target: document.body });
    flushSync();
    const td = cell('Requests in the reporting window over 600.0s: 4 · lifetime: 14');
    expect(td?.textContent).toBe('4');
  });

  it('falls back to the lifetime counter when no window sample exists', async () => {
    await refreshWithEndpoint(base('hp-3'));
    component = mount(EndpointsPanel, { target: document.body });
    flushSync();
    const td = cell('Lifetime requests served (no window sample yet)');
    expect(td?.textContent).toBe('14');
  });

  it('token cells disclose a lower bound', async () => {
    await refreshWithEndpoint({ ...base('hp-4'), generation_tokens: 1000, generation_tokens_partial: true });
    component = mount(EndpointsPanel, { target: document.body });
    flushSync();
    const tds = [...document.body.querySelectorAll('td[title]')].filter((t) => t.getAttribute('title')?.includes('LOWER BOUND: at least one day is missing'));
    expect(tds.length).toBe(1);
    expect(tds[0].textContent).toBe('1,000');
  });
});
