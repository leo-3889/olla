<script lang="ts">
  // Dual-series inline-SVG trend for the SELECTED endpoint on the endpoints
  // page: generation + prefill tok/s sampled from the same runtime envelope
  // the table cells render (see endpoint-history.svelte.ts). Hand-rolled SVG,
  // no chart library (hard rule).
  //
  // x is TIME-based: backgrounded tabs stretch the gaps instead of silently
  // compressing the axis, and null samples break the line - a gap is a gap,
  // never a fabricated bridge (SparkStrip rule). A genuinely idle engine
  // reports 0 and 0 draws at the floor: gaps and zeros are different facts.

  import { nearestSampleIndex } from './SparkStrip.svelte';
  import { fmtDuration } from '../lib/format';
  import { windowAvg, MAX_SAMPLES, type EndpointSample } from '../lib/stores/endpoint-history.svelte';

  interface Props {
    samples: EndpointSample[];
    // Display name of the selected endpoint (title only; data comes via
    // samples so the component stays a pure view).
    name: string;
  }
  let { samples, name }: Props = $props();

  const W = 600;
  const H = 150;
  const PAD = 6;

  const n = $derived(samples.length);
  const t0 = $derived(n > 0 ? samples[0].t : 0);
  const t1 = $derived(n > 0 ? samples[n - 1].t : 0);
  const span = $derived(t1 > t0 ? t1 - t0 : 0);

  function x(t: number): number {
    if (span <= 0) return PAD;
    return PAD + ((t - t0) / span) * (W - 2 * PAD);
  }

  const maxV = $derived(Math.max(1, ...samples.flatMap((s) => [s.gen ?? 0, s.prefill ?? 0])));
  function y(v: number): number {
    const clamped = Math.max(0, Math.min(v, maxV));
    return H - PAD - (clamped / maxV) * (H - 2 * PAD);
  }

  // One M..L sub-path per contiguous non-null run (gaps break the line).
  function lineFor(which: 'gen' | 'prefill'): string {
    let d = '';
    let inRun = false;
    for (const s of samples) {
      const v = s[which];
      if (typeof v === 'number') {
        d += (inRun ? 'L' : 'M') + x(s.t).toFixed(1) + ',' + y(v).toFixed(1);
        inRun = true;
      } else {
        inRun = false;
      }
    }
    return d;
  }
  const genPath = $derived(lineFor('gen'));
  const prefillPath = $derived(lineFor('prefill'));

  const genAvg = $derived(windowAvg(samples, 'gen'));
  const prefillAvg = $derived(windowAvg(samples, 'prefill'));
  // Every sample a gap: the backend reports neither counter (e.g. a lane
  // without metrics). Say so; do not render an empty frame.
  const allGaps = $derived(n >= 2 && genAvg === null && prefillAvg === null);

  function fmt1(v: number): string {
    return v.toLocaleString('en-AU', { maximumFractionDigits: 1 });
  }
  function fmtAvg(v: number | null): string {
    return v === null ? '—' : fmt1(v);
  }
  const windowLabel = $derived(span > 0 ? fmtDuration(span) : '');

  // --- pointer hover ---
  let hoverIdx = $state(-1);
  function onMove(ev: PointerEvent): void {
    const rect = (ev.currentTarget as SVGSVGElement).getBoundingClientRect();
    hoverIdx = nearestSampleIndex((ev.clientX - rect.left) / rect.width, n);
  }
  function onLeave(): void {
    hoverIdx = -1;
  }
  const hover = $derived(hoverIdx >= 0 && hoverIdx < n ? samples[hoverIdx] : null);
  function hhmmss(t: number): string {
    return new Date(t).toLocaleTimeString('en-GB', { hour12: false });
  }
</script>

<div class="trend" data-testid="endpoint-trend">
  <div class="trend-head">
    <span class="trend-title">Generation / prefill · <strong>{name}</strong></span>
    <span class="trend-legend">
      <span class="swatch swatch-gen" aria-hidden="true"></span>gen avg {fmtAvg(genAvg)} tok/s
      <span class="swatch swatch-prefill" aria-hidden="true"></span>prefill avg {fmtAvg(prefillAvg)} tok/s
    </span>
    <span class="trend-meta">window {windowLabel || '—'} · {n} samples · peak {fmt1(maxV)} tok/s</span>
  </div>
  {#if n < 2}
    <div class="trend-empty">collecting samples… ({n}/{MAX_SAMPLES})</div>
  {:else if allGaps}
    <div class="trend-empty">this endpoint reports no generation/prefill counters</div>
  {:else}
    <svg
      class="trend-svg"
      viewBox="0 0 {W} {H}"
      preserveAspectRatio="none"
      role="img"
      aria-label="Generation and prefill rate trend for {name}"
      onpointermove={onMove}
      onpointerleave={onLeave}
    >
      <path class="line line-gen" d={genPath} vector-effect="non-scaling-stroke" />
      <path class="line line-prefill" d={prefillPath} vector-effect="non-scaling-stroke" />
      {#if hover}
        <line class="hover-x" x1={x(hover.t)} x2={x(hover.t)} y1="0" y2={H} vector-effect="non-scaling-stroke" />
      {/if}
    </svg>
    <div class="trend-readout">
      {#if hover}
        <span class="readout-t">{hhmmss(hover.t)}</span>
        <span class="readout-gen">gen {hover.gen === null ? '—' : fmt1(hover.gen) + ' tok/s'}</span>
        <span class="readout-prefill">prefill {hover.prefill === null ? '—' : fmt1(hover.prefill) + ' tok/s'}</span>
      {:else}
        <span class="readout-hint">hover for per-sample values · table cells carry the live current + backend avg/max</span>
      {/if}
    </div>
  {/if}
</div>

<style>
  .trend {
    border: 1px solid var(--border);
    border-radius: 8px;
    background: var(--bg-elevated);
    padding: var(--space-3) var(--space-4);
    margin: 0 0 var(--space-3);
  }
  .trend-head {
    display: flex;
    flex-wrap: wrap;
    align-items: baseline;
    gap: var(--space-2) var(--space-4);
  }
  .trend-title {
    font-weight: 700;
  }
  .trend-legend {
    display: inline-flex;
    align-items: center;
    gap: 6px;
    color: var(--text-dim);
  }
  .swatch {
    display: inline-block;
    width: 10px;
    height: 3px;
    border-radius: 2px;
    margin-left: 8px;
  }
  .swatch-gen {
    background: var(--accent);
  }
  .swatch-prefill {
    background: var(--blue);
  }
  .trend-meta {
    margin-left: auto;
    color: var(--text-faint);
  }
  .trend-svg {
    display: block;
    width: 100%;
    height: 150px;
    margin-top: var(--space-2);
  }
  .line {
    fill: none;
    stroke-width: 1.6;
  }
  .line-gen {
    stroke: var(--accent);
  }
  .line-prefill {
    stroke: var(--blue);
  }
  .hover-x {
    stroke: var(--text-faint);
    stroke-width: 1;
    stroke-dasharray: 3 3;
  }
  .trend-readout {
    display: flex;
    gap: var(--space-4);
    margin-top: 4px;
    color: var(--text-dim);
  }
  .readout-t {
    color: var(--text-faint);
    font-variant-numeric: tabular-nums;
  }
  .trend-empty {
    margin-top: var(--space-2);
    color: var(--text-faint);
  }
</style>
