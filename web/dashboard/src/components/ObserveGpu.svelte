<script lang="ts">
  // GPU device cards from ollametrics' device-target series (endpoint tag
  // metrics_source.influx.gpu_endpoint). A GPU the exporter reports as
  // absent is shown as absent - never as a zero-utilisation device.
  import type { ObserveGpuRow } from '../lib/types';

  interface Props {
    gpus: ObserveGpuRow[];
    ageS: number | null;
    degraded: boolean;
  }
  let { gpus, ageS, degraded }: Props = $props();

  function fmt(value: number | null | undefined, digits = 0): string {
    if (typeof value !== 'number' || !Number.isFinite(value)) return '—';
    return value.toLocaleString('en-AU', { maximumFractionDigits: digits });
  }
  function gib(value: number | null | undefined): string {
    if (typeof value !== 'number' || !Number.isFinite(value)) return '—';
    return (value / 1024 ** 3).toLocaleString('en-AU', { maximumFractionDigits: 1 }) + ' GiB';
  }
  function pct(used: number | null | undefined, total: number | null | undefined): number | null {
    if (typeof used !== 'number' || typeof total !== 'number' || total <= 0) return null;
    return Math.min(100, Math.max(0, (used / total) * 100));
  }
  const ageText = $derived(ageS === null ? 'unknown age' : Math.max(0, Math.round(ageS)) + 's old');
</script>

<div class="section observe-gpu">
  <div class="section-head">
    <h2>GPUs</h2>
    <span class="section-note observe-src" class:stale={degraded}>ollametrics · {ageText}{#if degraded} (stale){/if}</span>
  </div>
  <div class="tile-grid" data-state={degraded ? 'stale' : null}>
    {#each gpus as row (row.gpu)}
      {@const usage = pct(row.vram_used_bytes, row.vram_total_bytes)}
      <div class="gpu" class:absent={row.present === 0}>
        <div class="gpu-head">
          <span class="gpu-name">GPU {row.gpu}{#if row.node}&nbsp;· {row.node}{/if}</span>
          {#if row.present === 0}<span class="chip">absent</span>{/if}
        </div>
        {#if row.present === 0}
          <div class="gpu-absent">device not present</div>
        {:else}
          <div class="gpu-row"><span>busy</span><span class="gpu-val">{fmt(row.busy_percent)} %</span></div>
          <div class="bar" role="img" aria-label="busy {fmt(row.busy_percent, 0)} percent"><div class="bar-fill" style="width:{row.busy_percent ?? 0}%"></div></div>
          <div class="gpu-row"><span>VRAM</span><span class="gpu-val" title={gib(row.vram_used_bytes) + ' / ' + gib(row.vram_total_bytes)}>{usage === null ? '—' : fmt(usage, 1) + ' %'}</span></div>
          <div class="bar" role="img" aria-label="vram {usage === null ? 'unknown' : fmt(usage, 0)} percent"><div class="bar-fill" style="width:{usage ?? 0}%"></div></div>
          <div class="gpu-row"><span>power</span><span class="gpu-val">{fmt(row.socket_power_watts)} W</span></div>
          <div class="gpu-row"><span>edge temp</span><span class="gpu-val">{fmt(row.edge_temp_c, 1)} °C</span></div>
        {/if}
      </div>
    {/each}
  </div>
</div>

<style>
  .gpu { border: 1px solid var(--border); border-radius: 8px; padding: 10px 12px; background: var(--bg-elevated, transparent); }
  .gpu.absent { opacity: 0.6; }
  .gpu-head { display: flex; gap: 6px; align-items: center; }
  .gpu-name { font-weight: 600; }
  .gpu-row { display: flex; justify-content: space-between; margin-top: 6px; color: var(--text-dim); }
  .gpu-val { color: var(--text); }
  .bar { height: 4px; background: var(--bg-inset, var(--border)); border-radius: 2px; margin-top: 3px; overflow: hidden; }
  .bar-fill { height: 100%; background: var(--accent); }
  .gpu-absent { color: var(--text-dim); margin-top: 6px; }
  .chip { font-size: 0.72em; border: 1px solid var(--border-strong); border-radius: 999px; padding: 1px 8px; color: var(--text-dim); }
  .observe-src.stale { color: var(--red); }
  [data-state='stale'] { opacity: 0.6; }
</style>
