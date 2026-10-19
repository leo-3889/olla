<script lang="ts">
  // Cost rollups straight from ollametrics' llm_endpoint_cost_v3 series.
  // Rollup _time is the PERIOD START, and the current period ships with
  // period_status "open": that row is the running total of an unfinished
  // period, so it is labelled partial instead of pretending to be final.
  import type { ObserveCost } from '../lib/types';
  import { fmtInt } from '../lib/format';

  interface Props {
    cost: ObserveCost;
    degraded: boolean;
    /** 'stale' | 'source error' - why degraded, worded for the note */
    statusLabel?: string;
  }
  let { cost, degraded, statusLabel = 'stale' }: Props = $props();

  interface CostRow {
    endpoint: string;
    hourCost: number | null;
    hourOpen: boolean;
    dayCost: number | null;
    dayOpen: boolean;
    energy_kwh: number | null;
    tokens: number | null;
    hour_start: string | null;
    day_start: string | null;
    hourCurrency: string;
    dayCurrency: string;
  }

  const rows = $derived.by(() => {
    const names = Array.from(new Set([...Object.keys(cost?.hour ?? {}), ...Object.keys(cost?.day ?? {})])).sort();
    return names.map((endpoint): CostRow => {
      const h = cost?.hour?.[endpoint];
      const d = cost?.day?.[endpoint];
      return {
        endpoint,
        hourCost: typeof h?.cost === 'number' ? h.cost : null,
        hourOpen: h?.period_status === 'open',
        dayCost: typeof d?.cost === 'number' ? d.cost : null,
        dayOpen: d?.period_status === 'open',
        // Day rollup carries the verified energy/token totals; fall back to
        // the hour row while a fresh day rollup has not landed yet.
        energy_kwh: typeof d?.energy_kwh === 'number' ? d.energy_kwh : (typeof h?.energy_kwh === 'number' ? h.energy_kwh : null),
        tokens: typeof d?.tokens === 'number' ? d.tokens : (typeof h?.tokens === 'number' ? h.tokens : null),
        // Which period each cell reports (rollup _time = period start). The
        // backend drops rows older than the newest period, so this is the
        // open period's start - the label the "partial" chip needs.
        hour_start: h?.period_start ?? null,
        day_start: d?.period_start ?? null,
        // Per-entry currency wins: the server only sets the global
        // cost.currency when EVERY entry agrees, so a mixed fleet labels
        // each cell with its own money (a USD endpoint under a global EUR
        // header would render wrong numbers with the wrong sign).
        hourCurrency: h?.currency || d?.currency || cost?.currency || 'EUR',
        dayCurrency: d?.currency || h?.currency || cost?.currency || 'EUR',
      };
    });
  });

  // Freshness of the COST SERVICE (its heartbeat), not of a rollup row:
  // rollup _time is the period start and can never answer "how fresh are
  // these numbers" (power/GPU stamp per-row age; cost stamps the service).
  const ageS = $derived.by(() => {
    const entries = [...Object.values(cost?.hour ?? {}), ...Object.values(cost?.day ?? {})];
    const ages = entries.map((e) => e?.age_s).filter((v): v is number => typeof v === 'number' && Number.isFinite(v));
    return ages.length > 0 ? Math.max(...ages) : null;
  });
  const ageText = $derived(ageS === null ? 'rollups' : Math.max(0, Math.round(ageS)) + 's old');

  function periodLabel(iso: string | null): string {
    if (!iso) return '';
    const d = new Date(iso);
    if (Number.isNaN(d.getTime())) return '';
    return d.toLocaleString('en-AU', { hour12: false });
  }

  function money(value: number | null, currency: string): string {
    if (value === null) return '—';
    return value.toFixed(value >= 1 ? 2 : 3) + ' ' + currency;
  }
  function kwh(value: number | null): string {
    if (value === null) return '—';
    return value.toLocaleString('en-AU', { maximumFractionDigits: 3 }) + ' kWh';
  }
</script>

<div class="section observe-cost">
  <div class="section-head">
    <h2>Cost</h2>
    <span class="section-note observe-src" class:stale={degraded}>ollametrics · {ageText}{#if degraded} ({statusLabel}){/if}{#if cost?.errors?.length}{' · '}cost query failed: {cost.errors.join(', ')}{/if}{#if cost?.dropped?.length}{' · '}no current rollup: {cost.dropped.join(', ')}{/if}</span>
  </div>
  {#if rows.length === 0 && cost?.errors?.length}
    <p class="observe-empty" data-state="degraded">cost query failed ({cost.errors.join(', ')}) - values below, if any, are from the cached payload</p>
  {:else if rows.length === 0}
    <p class="observe-empty">no cost rollups in the queried window yet</p>
  {:else}
    <div data-state={degraded ? 'stale' : null}>
    <table class="observe-table">
      <thead>
        <tr>
          <th scope="col">Endpoint</th>
          <th scope="col" title="Current hour rollup; _time is the period start">This hour</th>
          <th scope="col" title="Current day rollup; _time is the period start">Today</th>
          <th scope="col" title="Meter-verified energy for the newest rollup">Energy</th>
          <th scope="col">Tokens</th>
        </tr>
      </thead>
      <tbody>
        {#each rows as row (row.endpoint)}
          <tr>
            <th scope="row" class="observe-endpoint">{row.endpoint}</th>
            <td title={row.hour_start ? 'period starting ' + periodLabel(row.hour_start) : ''}>{money(row.hourCost, row.hourCurrency)}{#if row.hourOpen}<span class="partial" title="Rollup for the period starting {periodLabel(row.hour_start) || '?'} - still in progress">partial</span>{/if}</td>
            <td title={row.day_start ? 'period starting ' + periodLabel(row.day_start) : ''}>{money(row.dayCost, row.dayCurrency)}{#if row.dayOpen}<span class="partial" title="Rollup for the period starting {periodLabel(row.day_start) || '?'} - still in progress">partial</span>{/if}</td>
            <td>{kwh(row.energy_kwh)}</td>
            <td>{row.tokens === null ? '—' : fmtInt(row.tokens)}</td>
          </tr>
        {/each}
      </tbody>
    </table>
    </div>
  {/if}
</div>

<style>
  .observe-table { width: 100%; border-collapse: collapse; }
  .observe-table th, .observe-table td { text-align: right; padding: 6px 10px; border-bottom: 1px solid var(--border); }
  .observe-table thead th { color: var(--text-dim); font-weight: 600; font-size: 0.85em; }
  .observe-table tbody th { text-align: left; font-weight: 600; }
  .observe-endpoint { font-weight: 600; }
  .partial {
    margin-left: 6px;
    font-size: 0.72em;
    border: 1px solid var(--border-strong);
    border-radius: 999px;
    padding: 1px 7px;
    color: var(--text-dim);
  }
  .observe-empty { color: var(--text-dim); }
  .observe-src.stale { color: var(--red); }
  [data-state='stale'] { opacity: 0.6; }
</style>
