import { describe, it, expect, beforeEach, vi } from 'vitest';
import { endpoints } from './endpoints.svelte';
import { endpointHistory } from './endpoint-history.svelte';

// The 304-resample wiring: a healthy idle fleet produces semantically
// stable payloads, so the gateway ETag answers 304 minute after minute.
// The wiring must re-sample the store's retained payload on those ticks,
// or the endpoints-page trend chart freezes at one sample exactly when the
// fleet is quiet (the state most worth charting).

const payload = {
  endpoints: [
    {
      id: 'a',
      name: 'a',
      url: 'http://a:1',
      runtime_metrics: { stale: false, metrics: [{ key: 'generation_tps', current: 3 }, { key: 'prefill_tps', current: 40 }] },
    },
  ],
  total_count: 1,
  healthy_count: 1,
  routable_count: 1,
};

describe('endpoints store trend wiring', () => {
  beforeEach(() => endpointHistory.reset());

  it('200 tick samples; 304 tick re-samples the retained payload', async () => {
    let n = 0;
    global.fetch = vi.fn(async () => {
      n++;
      if (n === 1)
        return { status: 200, ok: true, headers: { get: () => 'W/"x"' }, json: async () => payload };
      return { status: 304, ok: false, headers: { get: () => null }, json: async () => ({}) };
    }) as never;

    endpoints.refresh();
    await vi.waitFor(() => {
      if (endpointHistory.samplesFor('a').length !== 1) throw new Error('no first sample');
    });

    endpoints.refresh();
    await vi.waitFor(() => {
      if (endpointHistory.samplesFor('a').length !== 2) throw new Error('304 did not resample');
    });
    expect(endpointHistory.samplesFor('a')[1]).toMatchObject({ gen: 3, prefill: 40 });
  });
});
