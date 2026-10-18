<script lang="ts">
  import { endpoints } from '../lib/stores/endpoints.svelte';
  import SortableTable from '../components/SortableTable.svelte';
  import type { Column, SortState } from '../components/SortableTable.svelte';
  import StatusBanner from '../components/StatusBanner.svelte';
  import StatusTag from '../components/StatusTag.svelte';
  import RangeBar from '../components/RangeBar.svelte';
  import PctBar from '../components/PctBar.svelte';
  import { fmtAgo, fmtMs, fmtPct, fmtUntil } from '../lib/format';
  import { bootstrap, DEFAULT_UI_CONFIG } from '../lib/stores/bootstrap.svelte';
  import { stableId } from '../lib/dom-id';
  import EndpointTrend from '../components/EndpointTrend.svelte';
  import { endpointHistory, envCurrent } from '../lib/stores/endpoint-history.svelte';
  import { getNow as liveNow } from '../lib/clock.svelte';
  import type { EndpointRuntimeMetrics, EndpointSummary } from '../lib/types';

  // View-model row: the contract row plus the numeric variants the table sorts
  // and the bars scale on. success_rate arrives pre-formatted ("98.5%") so a
  // numeric sibling field drives sort/PctBar; avg_latency_ms is normalised to
  // number|null so RangeBar can pick its no-data placeholder. Declared as a
  // type alias (not an interface) so it picks up an implicit index signature
  // and satisfies SortableTable's `Row extends Record<string, unknown>`.
  type EndpointRow = Omit<EndpointSummary, 'avg_latency_ms'> & {
    success_rate_num: number;
    avg_latency_ms: number | null;
    generation_tps_current: number | null;
    prefill_tps_current: number | null;
    cache_hit_percent_current: number | null;
    kv_cache_usage_percent: number | null;
    queue_depth_current: number | null;
    requests_running_current: number | null;
  };

  const data = $derived(endpoints.data?.endpoints ?? []);
  const loading = $derived(endpoints.status === 'loading');
  // App.svelte only mounts this panel when it is the active one, so the panel
  // is always rendered active once it appears.

  // This panel is the lifecycle owner of the endpoints store. Mount
  // activates the job (start() fires an immediate tick, so a tab switch
  // refreshes rather than showing stale data); unmount stops it so the
  // endpoints payload stops firing while Overview or Models is open. The
  // scheduler handles aborting the in-flight tick on stop.
  $effect(() => {
    endpoints.start();
    return () => endpoints.stop();
  });

  const globalLatencyMax = $derived(data.reduce((m, e) => Math.max(m, e.max_latency_ms ?? 0), 0));

  // success_rate arrives pre-formatted ("98.5%") so we expose a numeric
  // variant for sort/scaling. avg_latency_ms is the API's own numeric field
  // (average PROXY request latency) - it must NOT be derived from
  // response_time, which is the last HEALTH-CHECK probe's latency, an
  // unrelated metric. It may be absent (older backend) or null (no traffic
  // yet), in which case RangeBar shows the no-data placeholder rather than a
  // misleading 0.
  const rows: EndpointRow[] = $derived(
    data.map((e) => ({
      ...e,
      success_rate_num: parseFloat(e.success_rate) || 0,
      avg_latency_ms: typeof e.avg_latency_ms === 'number' ? e.avg_latency_ms : null,
      generation_tps_current: envCurrent(e.runtime_metrics, 'generation_tps'),
      prefill_tps_current: envCurrent(e.runtime_metrics, 'prefill_tps'),
      cache_hit_percent_current: envCurrent(e.runtime_metrics, 'cache_hit_percent'),
      kv_cache_usage_percent: envCurrent(e.runtime_metrics, 'kv_cache_usage_percent'),
      queue_depth_current: envCurrent(e.runtime_metrics, 'queue_depth'),
      requests_running_current: envCurrent(e.runtime_metrics, 'requests_running'),
    }))
  );

  // Re-evaluated each second so the relative "Xs ago" labels stay live
  // without per-row setInterval. Reading liveNow() inside $derived wires
  // the dependency on the shared clock.
  const now = $derived(liveNow());

  // Runtime columns come from the dashboard manifest (config.json), which
  // mirrors the metric-key contract; the built-in defaults keep the table
  // whole even when the manifest fetch failed. Row fields are flattened to
  // `${key}_current` so sorting keeps working on plain numbers.
  const runtimeCols = $derived(bootstrap.config.runtime_metric_columns?.length ? bootstrap.config.runtime_metric_columns : DEFAULT_UI_CONFIG.runtime_metric_columns);

  const columns: Column[] = $derived([
    { key: 'name', label: 'Endpoint', sortable: true, sticky: true },
    { key: 'type', label: 'Type', sortable: true },
    { key: 'priority', label: 'Priority', sortable: true, num: true, align: 'right' },
    {
      key: 'success_rate_num',
      label: 'Resp. rate',
      sortable: true,
      num: true,
      align: 'right',
      title: 'Counts any streamed response, regardless of HTTP status',
    },
    { key: 'avg_latency_ms', label: 'Latency', sortable: true, num: true, align: 'right' },
    ...runtimeCols.map((c) => ({
      key: c.key + '_current',
      label: c.unit === 'tok_s' ? c.label + ' tok/s' : c.label,
      sortable: true,
      num: true,
      align: 'right' as const,
      title: 'Backend-reported metric envelope: current' + (c.window ? ', then average and maximum over the reporting window' : ''),
    })),
    { key: 'model_count', label: 'Models', sortable: true, num: true, align: 'right' },
    { key: 'request_count', label: 'Requests', sortable: true, num: true, align: 'right' },
    { key: 'active_connections', label: 'Conn', sortable: true, num: true, align: 'right' },
    { key: 'url', label: 'URL', sortable: false },
    { key: 'health_check_at', label: 'Last chk', sortable: false },
    { key: 'next_check_at', label: 'Next chk', sortable: false },
    { key: 'issues', label: 'Issues', sortable: false },
  ]);

  const initialSort: SortState = { key: 'priority', dir: 'desc' };

  // --- trend chart selection ------------------------------------------------
  // Clicking an endpoint's name selects it; the chart above the table plots
  // the sampled generation/prefill rings for that row (identical envelope
  // numbers to the cells - endpoint-history.svelte.ts). Default selection is
  // the busiest endpoint, so the panel always shows a meaningful target.
  // Selection follows row identity (id ?? url ?? name): it survives re-sort
  // and polling; a departed endpoint falls back to the default.
  let selectedKey = $state<string | null>(null);
  const defaultKey = $derived.by(() => {
    let best = '';
    let bestReq = -1;
    for (const r of rows) {
      const rc = Number(r.request_count) || 0;
      if (rc > bestReq) {
        bestReq = rc;
        best = rowId(r);
      }
    }
    return best;
  });
  const activeKey = $derived(
    selectedKey && rows.some((r) => rowId(r) === selectedKey) ? selectedKey : defaultKey,
  );
  const activeRow = $derived(rows.find((r) => rowId(r) === activeKey) ?? null);
  const trendName = $derived(activeRow ? activeRow.name || activeRow.url || activeKey : '');
  // Reading .version establishes the reactive dependency on the store's
  // tick counter; the rings themselves live in a plain (non-reactive) Map.
  const trendSamples = $derived(
    endpointHistory.version >= 0 && activeKey ? endpointHistory.samplesFor(activeKey) : [],
  );
  function rowSelected(row: EndpointRow): string {
    return rowId(row) === activeKey ? 'ep-selected' : '';
  }

  // Svelte's keyed-each identity must be structurally unique. `row.id` is a
  // stable identifier the backend derives from the RAW (pre-sanitisation)
  // endpoint URL (see stableEndpointID in handler_status_endpoints.go) - it
  // survives sanitiseDisplayURL stripping query/fragment from `url`, so two
  // endpoints differing only by query string (both arriving with the same
  // displayed url) still get distinct rows. Falls back to url/name for
  // backends that predate the `id` field. Keying on name alone blanks the
  // table when two endpoints share a name (or both have empty names) -
  // each_key_duplicate, with no error boundary to contain it. SortableTable
  // additionally disambiguates any residual collision as a last line of
  // defence.
  function rowId(row: EndpointRow): string {
    return row.id ?? row.url ?? row.name;
  }
  // DOM id derived from the same identity via a collision-resistant hash -
  // NOT the lossy cssId slug, which made "node.a" and "node-a" both resolve
  // to ep-node-a so getElementById returned the wrong row. See lib/dom-id.
  function rowDomId(row: EndpointRow): string {
    return `ep-${stableId(row.id ?? row.url ?? row.name)}`;
  }

  function issueList(issues?: string): string[] {
    if (!issues) return [];
    return String(issues)
      .split(',')
      .map((s) => s.trim())
      .filter(Boolean);
  }

  // --- metric envelope helpers -----------------------------------------
  // All presentation semantics arrive in the payload (unit/scope/note/
  // attach_to); the panel formats, it does not interpret.

  function envMetric(metrics: EndpointRuntimeMetrics | undefined, key: string) {
    return metrics?.metrics?.find((m) => m.key === key);
  }

  function envPart(metrics: EndpointRuntimeMetrics | undefined, key: string, part: "avg" | "max"): number | null {
    const value = envMetric(metrics, key)?.[part];
    if (metrics?.stale || typeof value !== "number" || !Number.isFinite(value)) return null;
    return value;
  }

  function fmtNum(value: number): string {
    return value.toLocaleString("en-AU", { maximumFractionDigits: 1 });
  }

  function fmtByUnit(value: number, unit: string): string {
    if (unit === "percent") return fmtPct(value);
    if (unit === "tok_s") return fmtNum(value) + " tok/s";
    if (unit === "count") return fmtNum(value);
    if (unit === "ms") return fmtMs(value);
    // Unknown unit (newer backend, older UI): show the number, never throw.
    return String(value);
  }

  function summaryByUnit(value: number, unit: string): string {
    if (unit === "percent") return fmtPct(value);
    if (unit === "ms") return fmtMs(value);
    return fmtNum(value);
  }

  // One-line avg/max sub-row. Empty when the metric carries neither - an
  // avg-only gauge (cache hit) shows just the average, and a current-only
  // gauge shows no summary line at all.
  function summaryText(metrics: EndpointRuntimeMetrics | undefined, col: { key: string; unit: string }): string {
    const unit = envMetric(metrics, col.key)?.unit ?? col.unit;
    const avg = envPart(metrics, col.key, 'avg');
    const max = envPart(metrics, col.key, 'max');
    if (avg === null && max === null) return '';
    const a = avg === null ? '—' : summaryByUnit(avg, unit);
    return max === null ? 'avg ' + a : 'avg ' + a + ' · max ' + summaryByUnit(max, unit);
  }

  // `caveat` qualifies what the numbers cover. The provider owns the caveat
  // text (metric.note, e.g. the engine batch-aggregate note or the influx
  // gauge/counter provenance); these defaults only cover providers that
  // stayed silent about a window.
  function runtimeTitle(metrics: EndpointRuntimeMetrics | undefined, label: string, caveat = "avg/max over last 10m"): string {
    if (!metrics) return label + ": unsupported by this backend";
    if (metrics.stale) return metrics.sampled_at ? label + ": stale (last sampled " + metrics.sampled_at + ")" : label + ": stale";
    const sampled = metrics.sampled_at ? ": sampled " + metrics.sampled_at : "";
    return label + sampled + "; " + caveat;
  }

  // Where the numbers came from, straight from the payload: sampler vs
  // ollametrics, and how old the newest underlying point is.
  function sourceSuffix(metrics: EndpointRuntimeMetrics | undefined): string {
    if (!metrics?.source) return "";
    const age = typeof metrics.source_age_s === "number" && Number.isFinite(metrics.source_age_s) ? " (" + fmtNum(metrics.source_age_s) + "s old)" : "";
    return "; source " + metrics.source + age;
  }

  function cellTitle(metrics: EndpointRuntimeMetrics | undefined, col: { key: string; label: string; window: boolean }): string {
    const metric = envMetric(metrics, col.key);
    const caveat = metric?.note ?? (col.window ? "avg/max over last 10m" : "current gauge, latest scrape only");
    const value = envCurrent(metrics, col.key);
    // A present metrics object does not mean the backend reports every
    // signal - an engine with no scheduler queue exposes no queue gauge - so
    // a fresh absent metric is unsupported for THIS measurement, not sampled.
    let title: string;
    if (metrics && !metrics.stale && value === null) {
      title = col.label + ": unsupported by this backend";
    } else {
      title = runtimeTitle(metrics, col.label, caveat);
    }
    title += sourceSuffix(metrics);
    // Attached metrics (speculative-decode acceptance rides on generation)
    // explain WHY a headline number looks like it does - but only while the
    // payload is fresh: frozen figures are never presented as live.
    if (!metrics?.stale) {
      const attached = metrics?.metrics?.find((m) => m.attach_to === col.key);
      if (attached && typeof attached.current === "number") {
        title += "; Speculative decode acceptance: " + fmtPct(attached.current) + (typeof attached.avg === "number" ? " (avg " + fmtNum(attached.avg) + ")" : "");
      }
    }
    return title;
  }
</script>

<div
  id="panel-endpoints"
  class="panel is-active"
  role="tabpanel"
  aria-labelledby="tab-endpoints"
  tabindex="0"
>
  <StatusBanner store={endpoints} />

  <!-- data-state lives on this inner wrapper, not the panel root: opacity/
       filter on an ancestor compound down the subtree, so putting it on the
       root would also dim the StatusBanner and its retry control - the UI
       the operator needs most during an outage. -->
  <div
    class="panel-data"
    data-state={endpoints.status === 'error' || endpoints.status === 'stale' ? endpoints.status : null}
  >
  {#if !loading && activeRow}
    <EndpointTrend samples={trendSamples} name={trendName} />
  {/if}
  {#if loading}
    <div class="table-scroll"><div class="table-loading">loading…</div>
      {#each Array(6) as _, i}<div class="skeleton row-skel" style="margin:6px 10px"></div>{/each}
    </div>
  {:else}
    <SortableTable
      {columns}
      {rows}
      {initialSort}
      {rowId}
      {rowDomId}
      rowClass={rowSelected}
    >
      {#snippet rowSnippet({ row: e, cellClass })}
        <td class="col-sticky">
          <div class="endpoint-cell">
            <button
              class="ep-name ep-select"
              type="button"
              aria-pressed={rowId(e) === activeKey}
              title="Show generation/prefill trend for {e.name}"
              onclick={() => (selectedKey = rowId(e))}
            >
              <StatusTag status={e.status} />
              <span class="name-text" title={e.name}>{e.name}</span>
            </button>
          </div>
        </td>
        <td>{e.type}</td>
        <td class={cellClass('priority')}>{e.priority}</td>
        <td class={cellClass('success_rate_num')}>
          <PctBar pct={e.success_rate_num} hasData={e.request_count > 0} status={e.status} />
        </td>
        <td class={cellClass('avg_latency_ms')}>
          <RangeBar
            min={e.min_latency_ms ?? 0}
            avg={e.avg_latency_ms}
            max={e.max_latency_ms ?? 0}
            globalMax={globalLatencyMax}
            label="latency"
          />
        </td>
        {#each runtimeCols as col (col.key)}
          {@const metric = envMetric(e.runtime_metrics, col.key)}
          {@const current = envCurrent(e.runtime_metrics, col.key)}
          <td class={cellClass(col.key + '_current')}>
            <div class="runtime-metric" title={cellTitle(e.runtime_metrics, col)}>
              {#if current !== null}
                <span class="runtime-current">{fmtByUnit(current, metric?.unit ?? col.unit)}</span>
                {#if col.window}
                  <span class="runtime-summary">{summaryText(e.runtime_metrics, col)}</span>
                {:else if col.key === 'queue_depth'}
                  <span class="runtime-summary">{#if e.requests_running_current !== null}{fmtNum(e.requests_running_current)}{:else}—{/if} running</span>
                {/if}
              {:else}<span class="dash">—</span>{/if}
            </div>
          </td>
        {/each}
        <td class={cellClass("model_count")}>{#if e.model_count === 0}<span class="dash">0</span>{:else}{e.model_count}{/if}</td>
        <td class={cellClass('request_count')}>{e.request_count?.toLocaleString('en-AU') ?? 0}</td>
        <td class={cellClass('active_connections')}>{#if e.active_connections > 0}{e.active_connections}{:else}<span class="dash">0</span>{/if}</td>
        <td class="url-cell" title={e.url}>{e.url || '—'}</td>
        <td>{fmtAgo(e.health_check_at, now) || 'never'}</td>
        <td>{fmtUntil(e.next_check_at, now) || '—'}</td>
        <td>
          {#if issueList(e.issues).length}
            <ul class="issues-list">
              {#each issueList(e.issues) as i}<li>{i}</li>{/each}
            </ul>
          {:else}
            <ul class="issues-list none"><li>none</li></ul>
          {/if}
        </td>
      {/snippet}
    </SortableTable>
  {/if}
  </div>
</div>
