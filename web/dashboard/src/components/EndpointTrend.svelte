<script lang="ts">
  // Dual-series inline-SVG trend of PAIRED BARS for the SELECTED endpoint
  // on the endpoints page: generation + prefill tok/s sampled from the same
  // runtime envelope the table cells render (see endpoint-history.svelte.ts),
  // binned into 60 s bars (binSamples): generation on the LEFT half of the
  // bin slot, prefill on the RIGHT half, each series scaled to its OWN bin
  // max. A shared scale was the old defect: with gen ~103 tok/s next to
  // prefill ~2,234 tok/s, generation rendered as an invisible sliver.
  // Hand-rolled SVG, no chart library (hard rule).
  //
  // x is TIME-based: backgrounded tabs stretch the gaps instead of silently
  // compressing the axis. An empty bin renders NO bar - a gap is a gap,
  // never a fabricated bridge (the bars-era form of the SparkStrip rule).
  // A genuinely idle engine reports 0: a zero bin draws a 0.6 unit stub at
  // the floor so zero reads zero while absence reads a gap. Gaps and zeros
  // remain different facts (SparkStrip house rule).

  import { fmtDuration } from '../lib/format';
  import {
    BIN_MS,
    binSamples,
    windowAvg,
    MAX_SAMPLES,
    type EndpointSample,
  } from '../lib/stores/endpoint-history.svelte';

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
  const BASE = H - PAD;
  const PLOT_W = W - 2 * PAD;
  const PLOT_H = H - 2 * PAD;
  // Minimal visible bar for a genuine zero (viewBox units) - "0" must read
  // as a stub at the floor, never as the missing-bar gap.
  const STUB = 0.6;

  const n = $derived(samples.length);
  const t0 = $derived(n > 0 ? samples[0].t : 0);
  const t1 = $derived(n > 0 ? samples[n - 1].t : 0);
  const span = $derived(t1 > t0 ? t1 - t0 : 0);

  function x(t: number): number {
    if (span <= 0) return PAD;
    return PAD + ((t - t0) / span) * PLOT_W;
  }

  // --- paired 60 s bars, each series on its own scale ---------------------
  const bins = $derived(binSamples(samples));
  // One bin expressed in viewBox units on the SAME time scale as the axis,
  // so an empty bin stays visible as a missing bar.
  const binW = $derived(span > 0 ? (BIN_MS / span) * PLOT_W : PLOT_W);
  // Independent y-scales: generation scales to the tallest GEN bin, prefill
  // to the tallest PREFILL bin. Both series stay readable no matter how far
  // apart their magnitudes are (the measured 103 vs 2,234 tok/s fleet).
  const genMax = $derived(Math.max(1, ...bins.map((b) => b.gen ?? 0)));
  const preMax = $derived(Math.max(1, ...bins.map((b) => b.prefill ?? 0)));
  function scaleGen(v: number): number {
    return Math.max((Math.max(0, Math.min(v, genMax)) / genMax) * PLOT_H, STUB);
  }
  function scalePre(v: number): number {
    return Math.max((Math.max(0, Math.min(v, preMax)) / preMax) * PLOT_H, STUB);
  }

  interface Bar {
    t: number;
    genX: number;
    genW: number;
    preX: number;
    preW: number;
    gen: number | null;
    prefill: number | null;
    genH: number;
    preH: number;
  }
  // A bin with no value in EITHER series renders no rect at all: honest gap.
  // Each series owns the left/right half of the bin slot with a per-bar
  // inset (~15 % of the half, clamped), so the paired bars never touch.
  const bars = $derived<Bar[]>(
    bins
      .filter((b) => b.gen !== null || b.prefill !== null)
      .map((b) => {
        const left = Math.min(Math.max(x(b.t), PAD), W - PAD);
        const right = Math.min(Math.max(x(b.t) + binW, PAD), W - PAD);
        const half = Math.max(0, right - left) / 2;
        const inset = Math.min(1.5, half * 0.15);
        const w = Math.max(0, half - 2 * inset);
        return {
          t: b.t,
          genX: left + inset,
          genW: w,
          preX: left + half + inset,
          preW: w,
          gen: b.gen,
          prefill: b.prefill,
          genH: scaleGen(b.gen ?? 0),
          preH: scalePre(b.prefill ?? 0),
        };
      }),
  );

  // Legend + meta keep RAW-ring semantics (unchanged): averages over every
  // ring sample, peak over every single-sample value.
  const genAvg = $derived(windowAvg(samples, 'gen'));
  const prefillAvg = $derived(windowAvg(samples, 'prefill'));
  const peak = $derived(Math.max(1, ...samples.flatMap((s) => [s.gen ?? 0, s.prefill ?? 0])));
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

  // --- pointer hover: the bin under the cursor (bin means, not raw ticks) ---
  let hoverIdx = $state(-1);
  function onMove(ev: PointerEvent): void {
    const rect = (ev.currentTarget as SVGSVGElement).getBoundingClientRect();
    const t = t0 + ((ev.clientX - rect.left) / rect.width) * span;
    let idx = bins.findIndex((b) => t >= b.t && t < b.t + BIN_MS);
    if (idx < 0) idx = t < (bins[0]?.t ?? 0) ? 0 : bins.length - 1;
    hoverIdx = idx;
  }
  function onLeave(): void {
    hoverIdx = -1;
  }
  const hover = $derived(hoverIdx >= 0 && hoverIdx < bins.length ? bins[hoverIdx] : null);
  const hoverX = $derived(
    hover ? Math.min(Math.max(x(hover.t) + binW / 2, PAD), W - PAD) : 0,
  );
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
    <span class="trend-meta">window {windowLabel || '—'} · {n} samples · peak {fmt1(peak)} tok/s</span>
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
      aria-label="Generation and prefill rate trend (paired 60-second bars, each series on its own scale) for {name}"
      onpointermove={onMove}
      onpointerleave={onLeave}
    >
      {#each bars as bar (bar.t)}
        {#if bar.gen !== null}
          <rect
            class="bar bar-gen"
            x={bar.genX}
            width={bar.genW}
            y={BASE - bar.genH}
            height={bar.genH}
          />
        {/if}
        {#if bar.prefill !== null}
          <rect
            class="bar bar-prefill"
            x={bar.preX}
            width={bar.preW}
            y={BASE - bar.preH}
            height={bar.preH}
          />
        {/if}
      {/each}
      {#if hover}
        <line class="hover-x" x1={hoverX} x2={hoverX} y1="0" y2={H} vector-effect="non-scaling-stroke" />
      {/if}
    </svg>
    <div class="trend-readout">
      {#if hover}
        <span class="readout-t">{hhmmss(hover.t)}</span>
        <span class="readout-gen">gen {hover.gen === null ? '—' : fmt1(hover.gen) + ' tok/s'}</span>
        <span class="readout-prefill">prefill {hover.prefill === null ? '—' : fmt1(hover.prefill) + ' tok/s'}</span>
        <span class="readout-bin">60 s bin average</span>
      {:else}
        <span class="readout-hint">gen and prefill bars use independent scales (raw tok/s in legend + hover) · table cells carry the live current + backend avg/max</span>
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
  .bar-gen {
    fill: var(--accent);
  }
  .bar-prefill {
    fill: var(--blue);
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
  .readout-bin {
    color: var(--text-faint);
  }
  .trend-empty {
    margin-top: var(--space-2);
    color: var(--text-faint);
  }
</style>
