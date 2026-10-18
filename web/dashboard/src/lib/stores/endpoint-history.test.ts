import { describe, it, expect, beforeEach } from 'vitest';
import {
  envCurrent,
  endpointKey,
  samplesFromPayload,
  sampleEndpoints,
  windowAvg,
  endpointHistory,
  MAX_SAMPLES,
} from './endpoint-history.svelte';

function env(metrics: unknown, stale = false) {
  return { metrics, stale } as never;
}

describe('envCurrent', () => {
  it('keeps a valid zero visible', () => {
    expect(envCurrent(env([{ key: 'generation_tps', current: 0 }]), 'generation_tps')).toBe(0);
  });
  it('null on stale envelope even with a valid value', () => {
    expect(envCurrent(env([{ key: 'generation_tps', current: 5 }], true), 'generation_tps')).toBeNull();
  });
  it('null on absent metric / non-number / non-finite', () => {
    expect(envCurrent(env([]), 'prefill_tps')).toBeNull();
    expect(envCurrent(env([{ key: 'prefill_tps', current: 'x' }]), 'prefill_tps')).toBeNull();
    expect(envCurrent(env([{ key: 'prefill_tps', current: NaN }]), 'prefill_tps')).toBeNull();
  });
  it('null on missing envelope', () => {
    expect(envCurrent(undefined, 'generation_tps')).toBeNull();
  });
});

describe('endpointKey', () => {
  it('id wins, then url, then name', () => {
    expect(endpointKey({ id: 'i', url: 'u', name: 'n' })).toBe('i');
    expect(endpointKey({ url: 'u', name: 'n' } as never)).toBe('u');
    expect(endpointKey({ name: 'n' } as never)).toBe('n');
  });
});

const payload = (list: Record<string, unknown>[]) => ({ endpoints: list }) as never;
const ep = (id: string, gen: number | null, prefill: number | null, stale = false) => ({
  id,
  name: id,
  url: 'http://x',
  runtime_metrics: {
    stale,
    metrics: [
      ...(gen === null ? [] : [{ key: 'generation_tps', current: gen }]),
      ...(prefill === null ? [] : [{ key: 'prefill_tps', current: prefill }]),
    ],
  },
});

describe('samplesFromPayload', () => {
  it('extracts gen/prefill with gaps as null', () => {
    const out = samplesFromPayload(payload([ep('a', 10, null), ep('b', null, 3)]), 111);
    expect(out).toEqual([
      { key: 'a', sample: { t: 111, gen: 10, prefill: null } },
      { key: 'b', sample: { t: 111, gen: null, prefill: 3 } },
    ]);
  });
  it('null payload yields nothing', () => {
    expect(samplesFromPayload(null, 1)).toEqual([]);
  });
});

describe('sampleEndpoints ring', () => {
  beforeEach(() => endpointHistory.reset());

  it('fills rings per endpoint on successful ticks', () => {
    sampleEndpoints(payload([ep('a', 1, 2)]), true, 10);
    sampleEndpoints(payload([ep('a', 3, 4)]), true, 20);
    expect(endpointHistory.samplesFor('a')).toEqual([
      { t: 10, gen: 1, prefill: 2 },
      { t: 20, gen: 3, prefill: 4 },
    ]);
  });

  it('skips failed ticks (no fabricated samples)', () => {
    sampleEndpoints(payload([ep('a', 1, 1)]), true, 10);
    sampleEndpoints(null, false, 20);
    expect(endpointHistory.samplesFor('a')).toHaveLength(1);
  });

  it('trims to MAX_SAMPLES keeping the newest', () => {
    for (let i = 0; i < MAX_SAMPLES + 5; i++) sampleEndpoints(payload([ep('a', i, i)]), true, i);
    const s = endpointHistory.samplesFor('a');
    expect(s).toHaveLength(MAX_SAMPLES);
    expect(s[0].gen).toBe(5);
    expect(s[s.length - 1].gen).toBe(MAX_SAMPLES + 4);
  });

  it('prunes endpoints that left the fleet', () => {
    sampleEndpoints(payload([ep('a', 1, 1), ep('gone', 2, 2)]), true, 1);
    sampleEndpoints(payload([ep('a', 3, 3)]), true, 2);
    expect(endpointHistory.samplesFor('gone')).toEqual([]);
    expect(endpointHistory.samplesFor('a')).toHaveLength(2);
  });

  it('stale envelope samples as gaps, not zeros', () => {
    sampleEndpoints(payload([ep('a', 9, 9, true)]), true, 1);
    expect(endpointHistory.samplesFor('a')).toEqual([{ t: 1, gen: null, prefill: null }]);
  });
});

describe('windowAvg', () => {
  it('averages numbers and skips gaps', () => {
    const list = [
      { t: 1, gen: 2, prefill: null },
      { t: 2, gen: null, prefill: 10 },
      { t: 3, gen: 4, prefill: 30 },
    ];
    expect(windowAvg(list, 'gen')).toBe(3);
    expect(windowAvg(list, 'prefill')).toBe(20);
  });
  it('all-gap window is null; zeros count', () => {
    expect(windowAvg([{ t: 1, gen: null, prefill: null }], 'gen')).toBeNull();
    expect(windowAvg([{ t: 1, gen: 0, prefill: 0 }], 'gen')).toBe(0);
  });
});
