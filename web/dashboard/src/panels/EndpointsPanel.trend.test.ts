import { flushSync, mount, unmount } from 'svelte';
import { describe, it, expect, afterEach, vi } from 'vitest';
import { endpoints } from '../lib/stores/endpoints.svelte';
import { endpointHistory, sampleEndpoints } from '../lib/stores/endpoint-history.svelte';
import EndpointsPanel from './EndpointsPanel.svelte';

// The endpoints-page trend chart: the busiest endpoint is the default
// selection, clicking a row's name button moves the selection (row tint +
// chart title + aria-pressed), and the chart consumes the per-endpoint
// rings sampled from the SAME envelope numbers as the cells.

let component: ReturnType<typeof mount> | undefined;
afterEach(() => {
  if (component) unmount(component);
  document.body.innerHTML = '';
  endpointHistory.reset();
  vi.unstubAllGlobals();
});

function jsonResponse(body: unknown) {
  return {
    status: 200,
    ok: true,
    headers: { get: () => null },
    json: async () => body,
  };
}

function ep(id: string, name: string, requests: number, gen: number | null) {
  return {
    id,
    name,
    url: `http://${id}:1`,
    type: 'openai-compatible',
    status: 'healthy',
    priority: 100,
    success_rate: '99.0%',
    model_count: 1,
    request_count: requests,
    active_connections: 0,
    max_latency_ms: 10,
    health_check_at: new Date().toISOString(),
    runtime_metrics: {
      stale: false,
      metrics: [
        ...(gen === null ? [] : [{ key: 'generation_tps', current: gen }]),
        { key: 'prefill_tps', current: 100 },
      ],
    },
  };
}

const BUSY = ep('busy-id', 'radiance-a', 500, 40);
const QUIET = ep('quiet-id', 'radiance-b', 5, 1);

async function refresh(list: Record<string, unknown>[]) {
  const body = {
    endpoints: list,
    total_count: list.length,
    healthy_count: list.length,
    routable_count: list.length,
  };
  global.fetch = vi.fn(async () => jsonResponse(body)) as never;
  endpoints.refresh();
  // Wait for the STORE to swap payloads, not for a name to appear in the
  // DOM: on a shrinking refresh the removed-row name is still in innerHTML
  // from the previous render, so a DOM-text wait resolves against stale
  // rows and the selection assertions race the reactive update.
  const want = list.map((l) => l.name).join(',');
  await vi.waitFor(() => {
    const got = (endpoints.data?.endpoints ?? []).map((e) => e.name).join(',');
    if (got !== want) throw new Error(`store not swapped: ${got}`);
  });
  flushSync();
}

function mountPanel() {
  component = mount(EndpointsPanel, { target: document.body });
  flushSync();
}

function buttonFor(name: string): HTMLButtonElement {
  const b = [...document.querySelectorAll('button.ep-select')].find(
    (x) => x.textContent?.includes(name),
  ) as HTMLButtonElement;
  return b;
}

function selectedTrNames(): string[] {
  return [...document.querySelectorAll('tr.ep-selected td:first-child')].map((td) =>
    td.textContent?.trim() ?? '',
  );
}

describe('EndpointsPanel trend selection', () => {
  it('defaults to the busiest endpoint and charts it', async () => {
    mountPanel();
    await refresh([QUIET, BUSY]);
    expect(selectedTrNames().some((t) => t.includes('radiance-a'))).toBe(true);
    expect(buttonFor('radiance-a').getAttribute('aria-pressed')).toBe('true');
    const trend = document.querySelector('[data-testid="endpoint-trend"]');
    expect(trend?.textContent).toContain('radiance-a');
    // one successful refresh sampled -> collecting state is expected at n=1
    expect(trend?.textContent).toContain('collecting samples');
  });

  it('clicking a row button moves the selection, row tint, and chart title', async () => {
    mountPanel();
    await refresh([QUIET, BUSY]);
    buttonFor('radiance-b').click();
    flushSync();
    expect(selectedTrNames().some((t) => t.includes('radiance-b'))).toBe(true);
    expect(buttonFor('radiance-b').getAttribute('aria-pressed')).toBe('true');
    expect(buttonFor('radiance-a').getAttribute('aria-pressed')).toBe('false');
    expect(document.querySelector('[data-testid="endpoint-trend"]')?.textContent).toContain(
      'radiance-b',
    );
  });

  it('chart plots the selected endpoint ring with averages', async () => {
    mountPanel();
    await refresh([QUIET, BUSY]);
    buttonFor('radiance-b').click();
    // Seed the ring directly (deterministic times): the poll tick already
    // pushed one live sample during refresh, so start from n=0 conceptually
    // by resetting and feeding a full window.
    endpointHistory.reset();
    const base = 1_000;
    for (let i = 0; i < 3; i++) {
      sampleEndpoints(
        { endpoints: [ep('quiet-id', 'radiance-b', 5, i * 10)] } as never,
        true,
        base + i * 5000,
      );
    }
    flushSync();
    const trend = document.querySelector('[data-testid="endpoint-trend"]');
    expect(trend?.querySelector('path.line-gen')).not.toBeNull();
    expect(trend?.textContent).toContain('gen avg 10 tok/s');
    expect(trend?.textContent).toContain('prefill avg 100 tok/s');
    expect(trend?.textContent).toContain('3 samples');
  });

  it('selection falls back to the busiest when the selected endpoint leaves the fleet', async () => {
    mountPanel();
    await refresh([QUIET, BUSY]);
    buttonFor('radiance-b').click();
    flushSync();
    await refresh([BUSY]);
    expect(buttonFor('radiance-a').getAttribute('aria-pressed')).toBe('true');
    expect(document.querySelector('[data-testid="endpoint-trend"]')?.textContent).toContain(
      'radiance-a',
    );
  });
});
