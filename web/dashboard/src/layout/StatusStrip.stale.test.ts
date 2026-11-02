import { flushSync, mount, unmount } from 'svelte';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { endpoints } from '../lib/stores/endpoints.svelte';
import { overview } from '../lib/stores/overview.svelte';
import StatusStrip from './StatusStrip.svelte';
import { startClock } from '../lib/clock.svelte';
import { pollScheduler } from '../lib/poll-scheduler';
import type { EndpointResponse, SystemSummary } from '../lib/types';

// C6 regression: StatusStrip read overview.data without consulting
// overview.status, so during an outage it displayed confident numbers while the
// panels below reported unreachable. The fix surfaces a stale/error indicator
// on the strip when overview.status is 'stale' or 'error', reusing the
// data-state pattern the panels already use.
//
// The strip is driven through the real overview store rather than a substituted
// fake: `vi.mock` intercepts nothing under the Bun runtime the repo gates on
// (`bun run test`), so a mocked store silently left every assertion looking at
// the real one. Injecting payloads through global.fetch is how every other
// panel test feeds the same singletons, and the store's own status machine then
// produces the stale/error transitions the strip has to react to.

let component: ReturnType<typeof mount> | undefined;
afterEach(() => {
  if (component) unmount(component);
  component = undefined;
  document.body.innerHTML = '';
  vi.useRealTimers();
});

function jsonResponse(body: unknown) {
  return {
    status: 200,
    ok: true,
    headers: { get: () => null },
    json: async () => body,
  };
}

// `version` doubles as the refresh sentinel: the store is a module singleton, so
// waiting on status alone can resolve on the PREVIOUS test's payload.
function system(version: string, overrides: Partial<SystemSummary> = {}): SystemSummary {
  return {
    status: 'healthy',
    endpoints_up: '2/2',
    success_rate: '99.0%',
    avg_latency: '20ms',
    total_traffic: '0',
    uptime: '0s',
    version,
    commit: 'abc',
    start_time: '2026-10-05T08:00:00Z',
    active_connections: 0,
    security_violations: 0,
    total_requests: 0,
    total_failures: 0,
    has_traffic: false,
    health_mode: 'host',
    ...overrides,
  };
}

function endpoint(name: string, status: string): EndpointResponse {
  return {
    name,
    status,
    url: `http://${name}:11434`,
    id: `id-${name}`,
    success_rate: '0%',
    avg_latency: '0ms',
    traffic: '0',
    last_check: '',
    next_check: '',
    issues: '',
    priority: 0,
    connections: 0,
    requests: 0,
    min_latency_ms: 0,
    max_latency_ms: 0,
    models: { last_updated: '', count: 0 },
  };
}

async function pollSystem(sys: SystemSummary, endpointList: EndpointResponse[] = []): Promise<void> {
  global.fetch = vi.fn(async () =>
    jsonResponse({ timestamp: '2026-10-05T09:00:00Z', system: sys, endpoints: endpointList })
  );
  overview.refresh();
  await vi.waitFor(() => expect(overview.data?.system.version).toBe(sys.version));
  flushSync();
}

// A dropped poll: the store keeps the last known-good payload and escalates
// error -> stale once the last success ages past 3x the 5s overview interval.
async function failPoll(): Promise<void> {
  global.fetch = vi.fn(async () => {
    throw new Error('ECONNREFUSED');
  });
  overview.refresh();
  await vi.waitFor(() => expect(overview.status === 'stale' || overview.status === 'error').toBe(true));
  flushSync();
}

function mountStrip(): void {
  component = mount(StatusStrip, { target: document.body });
  flushSync();
}

describe('StatusStrip staleness indicator', () => {
  it('shows a stale indicator when overview.status is "stale" with data present', async () => {
    vi.useFakeTimers({ toFake: ['Date'] });
    await pollSystem(system('stale-probe'));
    vi.advanceTimersByTime(20_000);
    await failPoll();
    expect(overview.status).toBe('stale');
    mountStrip();

    const strip = document.querySelector('.status-strip');
    expect(strip).toBeTruthy();
    // data-state mirrors the panels' pattern so CSS/AT can distinguish a
    // confident strip from a stale one.
    expect(strip!.getAttribute('data-state')).toBe('stale');
    // A visible stale marker is surfaced, not just an attribute.
    expect(strip!.querySelector('[data-stale]')!.textContent!.trim().length).toBeGreaterThan(0);
    // The frozen figures stay on screen - greyed, not blanked.
    expect(strip!.querySelector('.system-status dd')!.textContent).toContain('healthy');
  });

  it('shows an error indicator when overview.status is "error"', async () => {
    await pollSystem(system('error-probe'));
    await failPoll();
    expect(overview.status).toBe('error');
    mountStrip();

    const strip = document.querySelector('.status-strip')!;
    expect(strip.getAttribute('data-state')).toBe('error');
    expect(strip.querySelector('[data-stale]')).toBeTruthy();
  });

  it('shows no staleness marker when overview.status is "ok"', async () => {
    await pollSystem(system('ok-probe'));
    mountStrip();

    const strip = document.querySelector('.status-strip')!;
    expect(strip.getAttribute('data-state')).toBeNull();
    expect(strip.querySelector('[data-stale]')).toBeNull();
  });
});

describe('StatusStrip degraded reason', () => {
  // The strip used to derive its offline count from the pausable endpoints
  // store, which froze mid-outage once EndpointsPanel unmounted. It now reads
  // overview.data.endpoints (always fresh because the overview store runs for
  // the lifetime of the dashboard). When that field is absent the strip must
  // stay silent rather than fabricate a count.

  it('derives the offline count from overview.data.endpoints (fresh payload)', async () => {
    // The endpoints store is deliberately loaded FIRST with a different, larger
    // offline count. A strip that still consulted it - directly or by accident -
    // would report 5, so the exact figure proves the source is the overview
    // payload. (It also replaces the old "throw if the endpoints store is read"
    // guard, which only worked because of a store mock the runner ignores.)
    global.fetch = vi.fn(async () =>
      jsonResponse({
        endpoints: [
          endpoint('down-a', 'offline'),
          endpoint('down-b', 'offline'),
          endpoint('down-c', 'critical'),
          endpoint('down-d', 'offline'),
          endpoint('down-e', 'offline'),
        ],
        total_count: 5,
        healthy_count: 0,
        routable_count: 0,
      })
    );
    endpoints.refresh();
    await vi.waitFor(() => expect(endpoints.data?.endpoints).toHaveLength(5));

    await pollSystem(
      system('degraded-probe', { status: 'degraded' }),
      [endpoint('a', 'offline'), endpoint('b', 'healthy'), endpoint('c', 'critical')]
    );
    mountStrip();

    // Two of three overview endpoints are offline/critical; the strip surfaces
    // that as the degraded reason.
    const reason = document.querySelector('.status-strip .status-reason');
    expect(reason?.textContent?.trim()).toBe('2 offline');
  });

  it('suppresses the reason when the overview payload has no endpoints array', async () => {
    await pollSystem(system('degraded-empty', { status: 'degraded' }));
    mountStrip();

    // No endpoint data to count from - the strip stays quiet rather than
    // show a fabricated or stale count (the endpoints store above still holds
    // five offline rows, so silence also proves it is not the fallback).
    const reason = document.querySelector('.status-strip .status-reason');
    expect(reason).toBeNull();
  });

  it('stays quiet when degraded but every listed endpoint is healthy', async () => {
    await pollSystem(system('degraded-healthy', { status: 'degraded' }), [endpoint('a', 'healthy')]);
    mountStrip();

    const reason = document.querySelector('.status-strip .status-reason');
    expect(reason).toBeNull();
  });
});

describe('StatusStrip response-rate cell', () => {
  // Coherent no-traffic UX: when the has_traffic flag is false the
  // backend emits success_rate as "N/A", which the strip used to render
  // literally. Mirror OverviewPanel's dash so both surfaces agree.

  function respRateCell(): Element | null {
    const cells = document.querySelectorAll('.status-strip .status-cell');
    return [...cells].find((c) => c.querySelector('dt')?.textContent?.trim() === 'Resp. rate') ?? null;
  }

  it('renders a no-data dash when has_traffic is false', async () => {
    await pollSystem(system('no-traffic', { has_traffic: false, total_requests: 0, success_rate: 'N/A' }));
    mountStrip();

    const cell = respRateCell();
    expect(cell).toBeTruthy();
    const dd = cell!.querySelector('dd')!;
    expect(dd.querySelector('.dash')?.textContent?.trim()).toBe('—');
    expect(dd.textContent).not.toContain('N/A');
  });

  it('renders the live success_rate when has_traffic is true', async () => {
    await pollSystem(system('with-traffic', { has_traffic: true, total_requests: 50, success_rate: '97.5%' }));
    mountStrip();

    const cell = respRateCell();
    expect(cell).toBeTruthy();
    const dd = cell!.querySelector('dd')!;
    expect(dd.textContent?.trim()).toBe('97.5%');
    expect(dd.querySelector('.dash')).toBeNull();
  });
});

describe('uptime freeze during outages', () => {
  it('freezes at the last good poll while stale and advances on recovery', async () => {
    pollScheduler.start(); // global timer base, as App.svelte does
    startClock(); // the shared ticking instant - without it no re-render
    const t0 = Date.now();
    const start = new Date(t0 - 3660_000).toISOString(); // uptime 1h01m (floor-stable)
    await pollSystem(system('freeze-1', { start_time: start }));
    mountStrip();
    const strip = () => document.querySelector('.status-strip')!.textContent!;
    expect(strip()).toContain('1h');

    await failPoll();
    // Two hours of wall-clock pass (shared clock ticks to +2h) while the
    // poll is down: a live clock would read 3h, but the gateway never
    // confirmed that - the figure must stay pinned to the last answered
    // poll.
    vi.spyOn(Date, 'now').mockReturnValue(t0 + 2 * 3600_000);
    await new Promise((r) => setTimeout(r, 1100)); // shared-clock ticks
    flushSync();
    expect(strip()).toContain('1h');

    // Recovery: re-anchor on the new response: start -1h01m, now +2h -> 3h.
    await pollSystem(system('freeze-2', { start_time: start }));
    expect(strip()).toContain('3h');
    vi.restoreAllMocks();
  });
});
