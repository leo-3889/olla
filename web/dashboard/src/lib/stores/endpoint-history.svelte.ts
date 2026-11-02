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

// ~15 min of samples at the 5 s endpoints cadence.
export const MAX_SAMPLES = 180;

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

/** Mean over the ring for one series, skipping gaps. null = no data. */
export function windowAvg(list: EndpointSample[], which: 'gen' | 'prefill'): number | null {
  let sum = 0;
  let n = 0;
  for (const s of list) {
    const v = s[which];
    if (typeof v === 'number') {
      sum += v;
      n++;
    }
  }
  return n > 0 ? sum / n : null;
}

/** Reactive accessor: reading .version inside $derived tracks ticks;
 *  samplesFor() then returns the current ring for an endpoint key. */
export const endpointHistory = {
  get version(): number {
    return version;
  },
  samplesFor(key: string): EndpointSample[] {
    return series.get(key) ?? [];
  },
  reset(): void {
    series.clear();
    version++;
  },
};
