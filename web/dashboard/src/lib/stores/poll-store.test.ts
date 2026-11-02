import { describe, it, expect, beforeEach, vi } from 'vitest';
import type { StatusResponse } from '../types';

// Covers the 304 / If-None-Match / ETag-capture path, which is the only
// machinery the store keeps beyond a straightforward 200 fetch. The server
// side of the 304 (ETag emission) is separate backend work; this test pins
// the client side: on a 304 the cached payload survives, the ETag is
// re-captured, and the success clock advances so a later failure can still
// escalate.

beforeEach(() => {
  vi.resetModules();
  vi.restoreAllMocks();
});

interface FetchLike {
  status: number;
  ok: boolean;
  headers: { get(name: string): string | null };
  json(): Promise<unknown>;
}

function jsonResponse(body: unknown, { status = 200, etag }: { status?: number; etag?: string } = {}): FetchLike {
  const headers = new Map<string, string>();
  if (etag) headers.set('ETag', etag);
  return {
    status,
    ok: status >= 200 && status < 300,
    headers: { get: (k: string) => headers.get(k) ?? null },
    json: async () => body,
  };
}

const SAMPLE_BODY: StatusResponse = {
  timestamp: '2026-01-01T00:00:00Z',
  proxy: { engine: 'olla', profile: 'ollama', balancer: 'recommended' },
  endpoints: [],
  security: { status: 'ok', blocked_ips: 0, violations: { rate_limits: 0, size_limits: 0 } },
  system: {
    start_time: '2026-01-01T00:00:00Z',
    status: 'healthy',
    endpoints_up: '0/0',
    success_rate: '0%',
    avg_latency: '0ms',
    total_traffic: '0',
    uptime: '0s',
    version: 'v0.0.0',
    commit: 'deadbeef',
    active_connections: 0,
    security_violations: 0,
    total_requests: 0,
    total_failures: 0,
    has_traffic: false,
    health_mode: 'host',
  },
};

describe('createPollStore: 304 / ETag handling', () => {
  it('re-captures the ETag and preserves cached data on a 304, advancing the success clock', async () => {
    const { createPollStore } = await import('./poll-store.svelte');

    let primaryCalls = 0;
    global.fetch = vi.fn(async () => {
      primaryCalls++;
      if (primaryCalls === 1) return jsonResponse(SAMPLE_BODY, { etag: '"v1"' });
      return jsonResponse(null, { status: 304, etag: '"v1"' });
    });

    const store = createPollStore<StatusResponse>({
      name: 'test-304',
      url: '/primary',
      intervalMs: 1000,
    });

    store.refresh();
    await vi.waitFor(() => expect(store.data).not.toBeNull());
    expect(store.status).toBe('ok');
    const firstData = store.data;
    expect(firstData).toStrictEqual(SAMPLE_BODY);

    // Second poll: 304. The cached payload object must be the SAME reference
    // (onSuccess leaves data untouched when the body is null) and the ETag
    // is re-captured so subsequent ticks keep sending If-None-Match.
    store.refresh();
    await vi.waitFor(() => expect(primaryCalls).toBe(2));
    expect(store.data).toBe(firstData);
    expect(store.status).toBe('ok');
  });
});

// onTick is the per-tick signal that lets the history ring buffer sample on
// BOTH success and failure. Without it, a dropped poll produces no sample and
// the chart silently bridges the gap. These tests pin the contract: ok=true
// on 200/304 (data is body on 200, null on 304), ok=false on failure.
describe('createPollStore: onTick callback', () => {
  it('fires onTick(data, true) on a 200 success', async () => {
    const ticks: Array<{ data: unknown; ok: boolean }> = [];
    global.fetch = vi.fn(async () => jsonResponse(SAMPLE_BODY, { etag: '"v1"' }));

    const { createPollStore } = await import('./poll-store.svelte');
    const store = createPollStore<StatusResponse>({
      name: 'test-ontick-200',
      url: '/test',
      intervalMs: 1000,
      onTick: (data, ok) => ticks.push({ data, ok }),
    });

    store.refresh();
    await vi.waitFor(() => expect(ticks.length).toBe(1));
    expect(ticks[0].ok).toBe(true);
    expect(ticks[0].data).toStrictEqual(SAMPLE_BODY);
  });

  it('fires onTick(null, true) on a 304 (body unchanged, data null)', async () => {
    const ticks: Array<{ data: unknown; ok: boolean }> = [];
    let call = 0;
    global.fetch = vi.fn(async () => {
      call++;
      if (call === 1) return jsonResponse(SAMPLE_BODY, { etag: '"v1"' });
      return jsonResponse(null, { status: 304, etag: '"v1"' });
    });

    const { createPollStore } = await import('./poll-store.svelte');
    const store = createPollStore<StatusResponse>({
      name: 'test-ontick-304',
      url: '/test',
      intervalMs: 1000,
      onTick: (data, ok) => ticks.push({ data, ok }),
    });

    store.refresh();
    await vi.waitFor(() => expect(ticks.length).toBe(1));
    store.refresh();
    await vi.waitFor(() => expect(ticks.length).toBe(2));
    expect(ticks[1].ok).toBe(true);
    expect(ticks[1].data).toBeNull();
  });

  it('fires onTick(null, false) on a network failure', async () => {
    const ticks: Array<{ data: unknown; ok: boolean }> = [];
    global.fetch = vi.fn(async () => {
      throw new Error('network unreachable');
    });

    const { createPollStore } = await import('./poll-store.svelte');
    const store = createPollStore<StatusResponse>({
      name: 'test-ontick-fail',
      url: '/test',
      intervalMs: 1000,
      onTick: (data, ok) => ticks.push({ data, ok }),
    });

    store.refresh();
    await vi.waitFor(() => expect(ticks.length).toBe(1));
    expect(ticks[0].ok).toBe(false);
    expect(ticks[0].data).toBeNull();
  });

  it('does not fire onTick on an aborted tick (superseded by a newer poll)', async () => {
    const ticks: Array<{ data: unknown; ok: boolean }> = [];
    global.fetch = vi.fn(async (_url: RequestInfo | URL, init?: RequestInit) => {
      // Simulate the scheduler aborting the in-flight request mid-flight.
      if (init?.signal) {
        const controller = new AbortController();
        setTimeout(() => controller.abort(), 0);
        throw Object.assign(new Error('aborted'), { name: 'AbortError' });
      }
      return jsonResponse(SAMPLE_BODY);
    });

    const { createPollStore } = await import('./poll-store.svelte');
    const store = createPollStore<StatusResponse>({
      name: 'test-ontick-abort',
      url: '/test',
      intervalMs: 1000,
      onTick: (data, ok) => ticks.push({ data, ok }),
    });

    store.refresh();
    // Give the aborted fetch a moment to settle; onTick must NOT fire.
    await new Promise((r) => setTimeout(r, 50));
    expect(ticks.length).toBe(0);
  });
});

// Aborts must never masquerade as data errors. When the scheduler supersedes
// a tick (manual refresh, tab-return burst) or stops the job, the fetch is
// aborted - and if that lands while resp.json() is streaming, the body
// promise rejects too. The old body-catch had no abort check, so every
// "Retry now" click during a slow response flashed a bogus "invalid JSON"
// failure over perfectly good cached data.
describe('createPollStore: abort handling', () => {
  it('a supersede during the response body read is silent and the newer tick wins', async () => {
    const ticks: Array<{ data: unknown; ok: boolean }> = [];
    let call = 0;
    global.fetch = vi.fn(async (_url: RequestInfo | URL, init?: RequestInit) => {
      call++;
      if (call === 1) {
        // Headers "arrive", then the body stalls until the signal aborts -
        // exactly what a real fetch does when superseded mid-download.
        return {
          status: 200,
          ok: true,
          headers: { get: () => null },
          json: () =>
            new Promise((_res, rej) => {
              init?.signal?.addEventListener('abort', () =>
                rej(Object.assign(new Error('aborted'), { name: 'AbortError' }))
              );
            }),
        };
      }
      return jsonResponse(SAMPLE_BODY);
    });

    const { createPollStore } = await import('./poll-store.svelte');
    const store = createPollStore<StatusResponse>({
      name: 'test-abort-body',
      url: '/test',
      intervalMs: 60_000, // keep the scheduler's own timer out of the picture
      onTick: (data, ok) => ticks.push({ data, ok }),
    });

    store.refresh(); // tick 1: in-flight, body stalled
    await vi.waitFor(() => expect(call).toBe(1));
    store.refresh(); // tick 2 supersedes: scheduler aborts tick 1's signal
    await vi.waitFor(() => expect(ticks.length).toBe(1));

    expect(store.status).toBe('ok'); // tick 2's success
    expect(store.error).toBeNull(); // the abort produced no error state
    expect(ticks.every((t) => t.ok)).toBe(true); // no failure tick from the abort
  });
});

// Failures must walk loading -> error -> stale on a schedule that matches how
// often we actually poll, including when the tab is hidden and the scheduler
// throttles ticks by BACKGROUND_BACKOFF.
describe('createPollStore: error/stale escalation', () => {
  it('non-2xx escalates error -> stale once the last success is older than 3 intervals', async () => {
    vi.useFakeTimers();
    try {
      let mode: 'ok' | 'fail' = 'ok';
      global.fetch = vi.fn(async () =>
        mode === 'ok' ? jsonResponse(SAMPLE_BODY) : jsonResponse(null, { status: 500 })
      );

      const { createPollStore } = await import('./poll-store.svelte');
      const store = createPollStore<StatusResponse>({
        name: 'test-escalate',
        url: '/test',
        intervalMs: 1000, // stale threshold: 3000ms (visible)
      });

      store.refresh();
      await vi.waitFor(() => expect(store.status).toBe('ok'));

      mode = 'fail';
      vi.advanceTimersByTime(2000); // still inside the 3x window
      store.refresh();
      await vi.waitFor(() => expect(store.status).toBe('error'));
      expect(store.error?.message).toBe('HTTP 500');

      vi.advanceTimersByTime(2000); // last success now 4000ms old
      store.refresh();
      await vi.waitFor(() => expect(store.status).toBe('stale'));
    } finally {
      vi.useRealTimers();
    }
  });

  it('does not escalate to stale on the first background failure: the threshold scales with the backoff', async () => {
    vi.useFakeTimers();
    const vis = Object.getOwnPropertyDescriptor(document, 'visibilityState');
    Object.defineProperty(document, 'visibilityState', { configurable: true, get: () => 'hidden' });
    try {
      let mode: 'ok' | 'fail' = 'ok';
      global.fetch = vi.fn(async () =>
        mode === 'ok' ? jsonResponse(SAMPLE_BODY) : jsonResponse(null, { status: 500 })
      );

      const { createPollStore } = await import('./poll-store.svelte');
      const store = createPollStore<StatusResponse>({
        name: 'test-hidden-stale',
        url: '/test',
        intervalMs: 1000, // hidden threshold: 3 * BACKGROUND_BACKOFF(4) * 1000 = 12000
      });

      store.refresh();
      await vi.waitFor(() => expect(store.status).toBe('ok'));

      mode = 'fail';
      // 4000ms: an UNSCALED 3x threshold would already say "stale" here,
      // making the error state unreachable the moment the tab is backgrounded.
      vi.advanceTimersByTime(4000);
      store.refresh();
      await vi.waitFor(() => expect(store.status).toBe('error'));

      vi.advanceTimersByTime(10_000); // 14s old > 12s scaled window -> stale
      store.refresh();
      await vi.waitFor(() => expect(store.status).toBe('stale'));
    } finally {
      if (vis) Object.defineProperty(document, 'visibilityState', vis);
      else delete (document as unknown as Record<string, unknown>).visibilityState;
      vi.useRealTimers();
    }
  });

  it('a blown response deadline fails the tick instead of hanging the store forever', async () => {
    // The real AbortSignal.timeout runs on a native timer that fake timers
    // cannot fast-forward; stub it with a hand-controlled signal so this test
    // pins the WIRING (deadline abort -> failure tick), not the clock.
    const ctrl = new AbortController();
    const realTimeout = AbortSignal.timeout;
    AbortSignal.timeout = (() => ctrl.signal) as typeof AbortSignal.timeout;
    try {
      global.fetch = vi.fn(
        (_url: RequestInfo | URL, init?: RequestInit) =>
          new Promise((_res, rej) =>
            init?.signal?.addEventListener('abort', () =>
              rej(Object.assign(new Error('timeout'), { name: 'AbortError' }))
            )
          )
      );

      const ticks: Array<{ ok: boolean; err?: string }> = [];
      const { createPollStore } = await import('./poll-store.svelte');
      const store = createPollStore<StatusResponse>({
        name: 'test-deadline',
        url: '/test',
        intervalMs: 60_000, // real deadline would be 5000ms floor; stub wins
        onTick: (data, ok) => ticks.push({ ok, err: ok ? undefined : 'fail' }),
      });

      store.refresh();
      await vi.waitFor(() => expect(global.fetch).toHaveBeenCalled());
      ctrl.abort(); // the deadline signal fires
      await vi.waitFor(() => expect(store.status).toBe('error'));
      expect(store.error?.message).toBe('status API did not respond in time');
      expect(ticks.length).toBe(1);
      expect(ticks[0].ok).toBe(false);
    } finally {
      AbortSignal.timeout = realTimeout;
    }
  });
});
