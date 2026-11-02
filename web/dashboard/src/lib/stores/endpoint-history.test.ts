import { describe, it, expect, beforeEach } from 'vitest';
import {
  envCurrent,
  endpointKey,
  samplesFromPayload,
  sampleEndpoints,
  windowAvg,
  binSamples,
  endpointHistory,
  MAX_SAMPLES,
  type EndpointSample,
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

  it('304-style ticks (no body, ok) keep every ring intact', () => {
    sampleEndpoints(payload([ep('a', 1, 1), ep('b', 2, 2)]), true, 1);
    sampleEndpoints(null, true, 2); // ETag cache hit: data===null, ok===true
    expect(endpointHistory.samplesFor('a')).toHaveLength(1);
    expect(endpointHistory.samplesFor('b')).toHaveLength(1);
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
  it('weights samples by time: a lone busy-period sample does not outvote an idle stretch', () => {
    // One 100 tok/s sample at t=0, then a quiet 10 tok/s every 20 s for
    // 200 s. A plain sample mean gives ~18.5; the time-weighted mean sits
    // near 10 - the first sample's weight is its half-gap (10 s), the quiet
    // samples carry ~20 s each (interior) / 10 s (last).
    const list = [{ t: 0, gen: 100, prefill: null }];
    for (let i = 1; i <= 10; i++) list.push({ t: i * 20_000, gen: 10, prefill: null });
    const avg = windowAvg(list, 'gen')!;
    expect(avg).toBeLessThan(20);
    expect(avg).toBeGreaterThan(9);
  });
  it('caps a silent gap at 120 s of weight (a gap is a gap, not evidence)', () => {
    // 50 at t=0, nothing for an hour, 1 at t=3600s.
    const list = [
      { t: 0, gen: 50, prefill: null },
      { t: 3_600_000, gen: 1, prefill: null },
    ];
    const avg = windowAvg(list, 'gen')!;
    // Weights: 120s cap each => plain mean of the two.
    expect(avg).toBeCloseTo(25.5, 6);
  });
  it('all-gap window is null; zeros count', () => {
    expect(windowAvg([{ t: 1, gen: null, prefill: null }], 'gen')).toBeNull();
    expect(windowAvg([{ t: 1, gen: 0, prefill: 0 }], 'gen')).toBe(0);
  });
});

describe('binSamples', () => {
  it('averages per 60 s bin and emits null pairs for empty bins', () => {
    const bins = binSamples([
      { t: 1_000, gen: 10, prefill: 100 },
      { t: 31_000, gen: 30, prefill: 300 },
      { t: 90_000, gen: null, prefill: null },
      { t: 130_000, gen: 20, prefill: 200 },
    ]);
    expect(bins).toEqual([
      { t: 0, gen: 20, prefill: 200 }, // means of 10/30 and 100/300
      { t: 60_000, gen: null, prefill: null }, // sample present, no values: null pair
      { t: 120_000, gen: 20, prefill: 200 },
    ]);
  });
  it('zeros are averaged, never treated as gaps', () => {
    expect(binSamples([{ t: 5, gen: 0, prefill: 0 }])).toEqual([{ t: 0, gen: 0, prefill: 0 }]);
  });
  it('series-level nulls: one series gap, the other averaged', () => {
    expect(
      binSamples([
        { t: 1, gen: null, prefill: 10 },
        { t: 2, gen: 4, prefill: null },
      ]),
    ).toEqual([{ t: 0, gen: 4, prefill: 10 }]);
  });
  it('honours a custom bin width', () => {
    expect(binSamples([{ t: 0, gen: 1, prefill: 1 }, { t: 25_000, gen: 3, prefill: 3 }], 10_000)).toEqual([
      { t: 0, gen: 1, prefill: 1 },
      { t: 10_000, gen: null, prefill: null }, // empty bin on the custom grid
      { t: 20_000, gen: 3, prefill: 3 },
    ]);
  });
  it('empty ring yields no bins', () => {
    expect(binSamples([])).toEqual([]);
  });
});

describe('samplesFor snapshot identity (live 2026-10-23 freeze)', () => {
  beforeEach(() => endpointHistory.reset());

  const payload = (gen: number) =>
    ({
      endpoints: [
        {
          id: 'a',
          runtime_metrics: {
            stale: false,
            metrics: [
              { key: 'generation_tps', current: gen },
              { key: 'prefill_tps', current: gen * 2 },
            ],
          },
        },
      ],
    }) as never;

  it('returns a FRESH array reference per read so child props update', () => {
    // The ring mutates in place (push). Returning the live reference makes
    // every version-keyed $derived produce the SAME reference, Svelte
    // dedupes the prop assignment, and the chart freezes at its first
    // snapshot while the ring grows (the -128 bug). Contract: two reads
    // bracketing a tick must NOT be the same object.
    sampleEndpoints(payload(1), true, 1000);
    const first = endpointHistory.samplesFor('a');
    sampleEndpoints(payload(2), true, 2000);
    const second = endpointHistory.samplesFor('a');
    expect(second).toHaveLength(2);
    expect(second).not.toBe(first);
    expect(second.map((s) => s.gen)).toEqual([1, 2]);
    // Mutating the returned snapshot must never corrupt the ring itself.
    second.length = 0;
    expect(endpointHistory.samplesFor('a')).toHaveLength(2);
  });
});

describe('binSamples grid bound (clock-jump hazard)', () => {
  it('mixed epochs produce a bounded sparse grid, never a dense explosion', () => {
    // A ring whose span exceeds MAX_SAMPLES bins (NTP step, seeded fixture
    // times colliding with live Date.now ticks) must NOT materialise the
    // dense first->last grid: 30 M bins allocated and spread into
    // Math.max overflowed the stack in the live chart (2026-10-23).
    const ring: EndpointSample[] = [
      { t: 1_000, gen: 5, prefill: 50 },
      { t: 1_792_000_000_000, gen: 7, prefill: 70 },
    ];
    const bins = binSamples(ring);
    expect(bins.length).toBe(2);
    expect(bins[0].t).toBe(0);
    expect(bins[1].t).toBe(Math.floor(1_792_000_000_000 / 60_000) * 60_000);
    expect(bins.map((b) => b.gen)).toEqual([5, 7]);
  });

  it('a normal span keeps the dense grid with its honest empty bins', () => {
    const ring: EndpointSample[] = [
      { t: 1_000, gen: 5, prefill: 50 },
      { t: 130_000, gen: 9, prefill: 90 },
    ];
    const bins = binSamples(ring);
    // bin 0 and bin 120 s occupied, the 60 s bin is the dense honest gap
    expect(bins.map((b) => b.gen)).toEqual([5, null, 9]);
  });
});
