<script lang="ts">
  // Rack power per endpoint, from ollametrics' llm_host_power series. Rows
  // are config-attributed (metrics_source.influx.power_attribution): a meter
  // appears only when the config says which endpoint it measures.
  import type { ObservePowerRow } from '../lib/types';

  interface Props {
    rows: ObservePowerRow[];
    /** max age_s over the visible rows, for the source freshness stamp */
    ageS: number | null;
    /** true when the observe store reports stale/error */
    degraded: boolean;
  }
  let { rows, ageS, degraded }: Props = $props();

  function fmt(value: number | null | undefined, digits = 1): string {
    if (typeof value !== 'number' || !Number.isFinite(value)) return '—';
    return value.toLocaleString('en-AU', { maximumFractionDigits: digits });
  }
  const ageText = $derived(ageS === null ? 'unknown age' : Math.max(0, Math.round(ageS)) + 's old');
</script>

<div class="section observe-power">
  <div class="section-head">
    <h2>Power</h2>
    <span class="section-note observe-src" class:stale={degraded}>ollametrics · {ageText}{#if degraded} (stale){/if}</span>
  </div>
  <div class="tile-grid" data-state={degraded ? 'stale' : null}>
    {#each rows as row (row.target)}
      <div class="meter" class:off={row.output_on === 0}>
        <div class="meter-head">
          <span class="meter-name" title={row.device_name || row.target}>{row.endpoint}</span>
          {#if row.overpower === 1}<span class="chip bad" title="Meter reports overpower right now">overpower</span>{/if}
          {#if row.meter_valid === 0}<span class="chip bad" title="Meter hardware error: its readings are not trustworthy">meter invalid</span>{/if}
          {#if row.output_on === 0}<span class="chip">off</span>{/if}
        </div>
        <div class="meter-watts">{fmt(row.watts)}<span class="unit">W</span></div>
        <div class="meter-meta">{row.device_name ?? '—'}{#if row.node}&nbsp;· {row.node}{/if} · {fmt(row.temp_c)} °C</div>
      </div>
    {/each}
  </div>
</div>

<style>
  .meter {
    border: 1px solid var(--border);
    border-radius: 8px;
    padding: 10px 12px;
    background: var(--bg-elevated, transparent);
  }
  .meter.off { opacity: 0.6; }
  .meter-head { display: flex; gap: 6px; align-items: center; flex-wrap: wrap; }
  .meter-name { font-weight: 600; }
  .meter-watts { font-size: 1.4em; margin-top: 4px; }
  .unit { font-size: 0.6em; color: var(--text-dim); margin-left: 3px; }
  .meter-meta { color: var(--text-dim); font-size: 0.85em; margin-top: 4px; }
  .chip {
    font-size: 0.72em;
    border: 1px solid var(--border-strong);
    border-radius: 999px;
    padding: 1px 8px;
    color: var(--text-dim);
  }
  .chip.bad { color: var(--red); border-color: var(--red); }
  .observe-src.stale { color: var(--red); }
  [data-state='stale'] { opacity: 0.6; }
</style>
