// Per-endpoint ring buffers feeding the endpoints-page trend chart. One
// sample per SUCCESSFUL endpoints poll tick (5 s cadence) via the store's
// onTick - the same callback pattern the overview store uses for the
// SparkStrip history.
//
// Values are the SAME envelope numbers the table renders (the sampler and
// the influx provider both fill generation_tps / prefill_tps), so the chart
// can never disagree with the cells. A missing/stale metric samples as
// null = a visible gap; idle engines legitimately report 0 and 0 renders
// as 0 (this distinction is the whole point of the gap policy).
//
// Singleton module-scope state (history.svelte.ts pattern), keyed by the
// panel's row identity (id ?? url ?? name).

import type { EndpointRuntimeMetrics, EndpointStatusResponse, EndpointSummary } from '../types';

export interface EndpointSample {
  // epoch ms
  t: number;
  // tok/s; null = not reported this tick (gap, not zero)
  gen: number | null;
  prefill: number | null;
}

// ~30 min of samples at the 5 s endpoints cadence (the cadence follows the
// 5 s health tick so the chk clocks and the chart advance together).
export const MAX_SAMPLES = 360;

/** Envelope current-value read with the panel's exact semantics: a valid
 *  0 stays visible; absent, non-finite, or stale-envelope values are
 *  unavailable (null). Exported so the panel imports THE ONE implementation
 *  and the chart provably shows what the cells show. */
export function envCurrent(metrics: EndpointRuntimeMetrics | undefined, key: string): number | null {
  const value = metrics?.metrics?.find((m) => m.key === key)?.current;
  if (metrics?.stale || typeof value !== 'number' || !Number.isFinite(value)) return null;
  return value;
}

/** Row identity: identical to EndpointsPanel.rowId (id wins; url/name only
 *  for payloads predating the id field). */
export function endpointKey(e: Pick<EndpointSummary, 'id' | 'url' | 'name'>): string {
  return e.id ?? e.url ?? e.name ?? '';
}

/** Pure: one poll payload -> per-endpoint samples. Exported for tests. */
export function samplesFromPayload(
  data: EndpointStatusResponse | null,
  now: number,
): Array<{ key: string; sample: EndpointSample }> {
  if (!data || !Array.isArray(data.endpoints)) return [];
  return data.endpoints.map((e) => ({
    key: endpointKey(e),
    sample: {
      t: now,
      gen: envCurrent(e.runtime_metrics, 'generation_tps'),
      prefill: envCurrent(e.runtime_metrics, 'prefill_tps'),
    },
  }));
}

// --- singleton state ---

const series = new Map<string, EndpointSample[]>();
// Monotonic tick counter: the Map itself stays non-reactive (mutation
// tracking on deep Maps is a trap); consumers $derived off this version.
let version = $state(0);

/** onTick handler wired into the endpoints poll store. Failed ticks are
 *  SKIPPED (the poll store's stale banner + the chart's time gaps carry
 *  the outage; fabricating a gap sample per endpoint adds no information
 *  the x-axis does not already show). */
export function sampleEndpoints(data: EndpointStatusResponse | null, ok: boolean, now: number = Date.now()): void {
  // 304 cache hits fire onTick(null, true): no body is NOT an empty fleet.
  // Sampling the null payload would prune every ring (the fleet vanishes
  // between two ETag-cached ticks) - cached ticks simply add no sample.
  if (!ok || !data) return;
  const seen = new Set<string>();
  for (const { key, sample } of samplesFromPayload(data, now)) {
    seen.add(key);
    const arr = series.get(key) ?? [];
    arr.push(sample);
    if (arr.length > MAX_SAMPLES) arr.splice(0, arr.length - MAX_SAMPLES);
    series.set(key, arr);
  }
  // Endpoints that left the fleet stop growing AND get pruned: a retired
  // endpoint cannot be selected (no row), so its buffer is pure leak.
  for (const k of [...series.keys()]) if (!seen.has(k)) series.delete(k);
  version++;
}

/** Time-weighted mean over the ring for one series, skipping gaps. Each
 *  sample carries the half-gap to its neighbours as its weight: payload-
 *  change ticks land faster while the fleet is busy, and a plain sample
 *  mean over-sampled busy stretches. Weights cap at 120 s so a long silent
 *  gap is a gap, not minutes of evidence for one sample's value.
 *  null = no data. */
export function windowAvg(list: EndpointSample[], which: 'gen' | 'prefill'): number | null {
  let num = 0;
  let den = 0;
  for (let i = 0; i < list.length; i++) {
    const v = list[i][which];
    if (typeof v !== 'number') continue;
    const prevT = i > 0 ? list[i - 1].t : null;
    const nextT = i < list.length - 1 ? list[i + 1].t : null;
    let w: number;
    if (prevT !== null && nextT !== null) w = (nextT - prevT) / 2;
    else if (nextT !== null) w = nextT - list[i].t;
    else if (prevT !== null) w = list[i].t - prevT;
    else w = 1;
    if (w < 0) w = 0; // non-monotonic ring (clock jump): weight nothing
    if (w > 120_000) w = 120_000;
    num += v * w;
    den += w;
  }
  return den > 0 ? num / den : null;
}

/** Reactive accessor: reading .version inside $derived tracks ticks;
 *  samplesFor() then returns the current ring for an endpoint key. */
export const endpointHistory = {
  get version(): number {
    return version;
  },
  samplesFor(key: string): EndpointSample[] {
    // Fresh COPY per read: the ring array is mutated in place (push/splice),
    // so returning the live reference makes every version-keyed $derived
    // produce the SAME reference - Svelte dedupes the child prop update on
    // reference equality and the chart freezes at its first snapshot (live
    // bug 2026-10-23: -128 ring grew to 87 samples while the chart sat at
    // 1 until a row click changed another prop). Copy = a real new value
    // per tick; <= MAX_SAMPLES items, trivially cheap.
    const arr = series.get(key);
    return arr ? [...arr] : [];
  },
  reset(): void {
    series.clear();
    version++;
  },
};

/** Bin width of the trend chart's bars: the 10 s ring is aggregated into
 *  60 s bins, bin total = summed tok/s. Exported so the component and its
 *  tests share ONE definition of the bar grid. */
export const BIN_MS = 60_000;

export interface EndpointBin {
  // bin start on the absolute binMs grid (epoch ms)
  t: number;
  // mean of the bin's non-null gen samples; null = no gen value in this bin
  gen: number | null;
  prefill: number | null;
}

/** Pure: ring -> bins on a fixed binMs grid from the first to the last
 *  occupied bin. Per bin: the mean of the non-null values per series. A bin
 *  with no value for a series is null for it, and a bin with NO samples in
 *  either series is a null pair - the chart renders NO bar there. That is
 *  the honest gap that replaces the null-line break: gaps stay gaps, zeros
 *  stay zeros. Exported for tests. */
export function binSamples(list: EndpointSample[], binMs: number = BIN_MS): EndpointBin[] {
  if (list.length === 0) return [];
  const acc = new Map<number, { gs: number; gn: number; ps: number; pn: number }>();
  let first = Infinity;
  let last = -Infinity;
  for (const s of list) {
    const b = Math.floor(s.t / binMs) * binMs;
    if (b < first) first = b;
    if (b > last) last = b;
    let a = acc.get(b);
    if (!a) {
      a = { gs: 0, gn: 0, ps: 0, pn: 0 };
      acc.set(b, a);
    }
    if (typeof s.gen === 'number') {
      a.gs += s.gen;
      a.gn += 1;
    }
    if (typeof s.prefill === 'number') {
      a.ps += s.prefill;
      a.pn += 1;
    }
  }
  const bin = (t: number, a?: { gs: number; gn: number; ps: number; pn: number }): EndpointBin => ({
    t,
    gen: a && a.gn > 0 ? a.gs / a.gn : null,
    prefill: a && a.pn > 0 ? a.ps / a.pn : null,
  });
  const out: EndpointBin[] = [];
  // Sparse fallback: the dense grid is bounded by the ring's own size. A
  // span wider than MAX_SAMPLES bins can only come from a clock jump
  // (NTP step, ring seeds mixing epochs): a dense grid there is a
  // multi-million-bin allocation whose Math.max(...spread) consumer died
  // with a stack overflow (captured live 2026-10-23). Occupied bins are
  // <= ring length, so sparse output is ALWAYS bounded, and the chart's
  // x is time-based: an empty bin renders no bar in BOTH modes, so dense
  // empties were pure waste, never information.
  if (Math.floor((last - first) / binMs) + 1 > MAX_SAMPLES) {
    for (const t of [...acc.keys()].sort((a, b) => a - b)) out.push(bin(t, acc.get(t)));
    return out;
  }
  for (let t = first; t <= last; t += binMs) out.push(bin(t, acc.get(t)));
  return out;
}
