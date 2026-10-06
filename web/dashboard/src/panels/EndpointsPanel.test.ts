import { flushSync, mount, unmount } from 'svelte';
import { describe, it, expect, afterEach, vi } from 'vitest';
import { endpoints } from '../lib/stores/endpoints.svelte';
import EndpointsPanel from './EndpointsPanel.svelte';

// Regression coverage for finding 6: the panel used to derive the RangeBar's
// average from `response_time` (parseInt on the last HEALTH-CHECK probe's
// formatted latency string) instead of the API's own `avg_latency_ms`
// (average PROXY request latency - a different metric entirely). A 1500ms
// health check formatted as "1.5s" and parseInt("1.5s", 10) silently
// produced 1, so the bar rendered a 1ms average for an endpoint that took
// 1.5 SECONDS to answer its last health check.
//
// Note: deliberately static imports, no vi.resetModules() - the `endpoints`
// store is a module singleton, and resetting modules mid-file desyncs
// Svelte's internal client runtime from the already-mounted component.

let component: ReturnType<typeof mount> | undefined;
afterEach(() => {
  if (component) unmount(component);
  document.body.innerHTML = '';
});

function jsonResponse(body: unknown) {
  return {
    status: 200,
    ok: true,
    headers: { get: () => null },
    json: async () => body,
  };
}

async function refreshWithEndpoint(endpoint: Record<string, unknown>) {
  global.fetch = vi.fn(async () =>
    jsonResponse({ endpoints: [endpoint], total_count: 1, healthy_count: 1, routable_count: 1 })
  );
  endpoints.refresh();
  // Wait for this specific endpoint's payload, not just status === 'ok' -
  // the store is a module singleton reused across tests, so status may
  // already read 'ok' from a previous test before this refresh resolves.
  await vi.waitFor(() => expect(endpoints.data?.endpoints?.[0]?.name).toBe(endpoint.name));
  flushSync();
}

async function refreshWithEndpoints(endpointList: Record<string, unknown>[]) {
  global.fetch = vi.fn(async () =>
    jsonResponse({
      endpoints: endpointList,
      total_count: endpointList.length,
      healthy_count: endpointList.length,
      routable_count: endpointList.length,
    })
  );
  endpoints.refresh();
  const lastName = endpointList[endpointList.length - 1].name;
  await vi.waitFor(() =>
    expect(endpoints.data?.endpoints?.[endpointList.length - 1]?.name).toBe(lastName)
  );
  flushSync();
}

describe('EndpointsPanel latency column', () => {
  it('uses avg_latency_ms, not a parse of response_time, for the RangeBar average', async () => {
    component = mount(EndpointsPanel, { target: document.body });
    flushSync();

    await refreshWithEndpoint({
      name: 'ollama-1',
      type: 'ollama',
      status: 'healthy',
      priority: 100,
      success_rate: '99.0%',
      request_count: 40,
      model_count: 1,
      // Last health-check probe took 1.5s - an unrelated metric that must
      // never leak into the proxy-latency average.
      response_time: '1.5s',
      avg_latency_ms: 42,
      min_latency_ms: 10,
      max_latency_ms: 90,
    });

    const label = document.querySelector('.range-bar[role="img"]')!.getAttribute('aria-label');
    expect(label).toContain('average 42ms');
    // The old bug rendered "1ms" here (parseInt("1.5s", 10)).
    expect(label).not.toContain('average 1ms');
    expect(document.querySelector('.range-labels')!.firstChild!.textContent!.trim()).toBe('42ms');
  });

  it('falls back to the no-data placeholder when avg_latency_ms is absent (backend not upgraded yet)', async () => {
    component = mount(EndpointsPanel, { target: document.body });
    flushSync();

    await refreshWithEndpoint({
      name: 'ollama-2',
      type: 'ollama',
      status: 'healthy',
      priority: 100,
      success_rate: '99.0%',
      request_count: 40,
      model_count: 1,
      response_time: '1.5s',
      // no avg_latency_ms field at all
      min_latency_ms: 10,
      max_latency_ms: 90,
    });

    const avgText = document.querySelector('.range-labels')!.firstChild!.textContent!.trim();
    expect(avgText).toBe('—');
    // Must not have silently coerced the missing field, or response_time, to 0/1.
    expect(avgText).not.toBe('0ms');
    expect(avgText).not.toBe('1ms');
  });

  // Regression test for the globalLatencyMax/globalMax wiring bug: the panel
  // passed `{globalLatencyMax}` (shorthand -> prop named globalLatencyMax) but
  // RangeBar destructures `globalMax`, so the prop was always its default 0
  // and every bar rendered at 0% width with its peak tick collapsed to the
  // left edge, table-wide, while the text labels kept reading correctly.
  // RangeBar.test.js alone can't catch this because it passes the
  // correctly-named prop directly - the mistake only exists at the call site
  // in this file, so the test has to go through EndpointsPanel. Two
  // endpoints with different maxima so a genuinely-wired globalMax produces
  // distinct, non-zero, non-100% geometry - a false pass isn't available by
  // coincidence.
  it('wires globalLatencyMax into RangeBar as globalMax so bars render non-zero geometry', async () => {
    component = mount(EndpointsPanel, { target: document.body });
    flushSync();

    await refreshWithEndpoints([
      {
        name: 'ollama-low',
        type: 'ollama',
        status: 'healthy',
        priority: 100,
        success_rate: '99.0%',
        request_count: 40,
        model_count: 1,
        avg_latency_ms: 100,
        min_latency_ms: 10,
        max_latency_ms: 200,
      },
      {
        name: 'ollama-high',
        type: 'ollama',
        status: 'healthy',
        priority: 90,
        success_rate: '99.0%',
        request_count: 40,
        model_count: 1,
        avg_latency_ms: 50,
        min_latency_ms: 5,
        max_latency_ms: 800,
      },
    ]);

    // globalLatencyMax across both rows is 800 (the second endpoint's max).
    const bars = [...document.querySelectorAll('.range-bar[role="img"]')];
    const lowBar = bars.find((b) => b.getAttribute('aria-label')!.includes('average 100ms'))!;
    const highBar = bars.find((b) => b.getAttribute('aria-label')!.includes('average 50ms'))!;
    expect(lowBar).toBeTruthy();
    expect(highBar).toBeTruthy();

    // scalePct(avg, 800) = sqrt(avg/800)*100. With the pre-fix binding
    // globalMax defaults to 0 and both of these collapse to "0.0".
    const lowFill = lowBar.querySelector('.fill')!.getAttribute('style');
    const lowTick = lowBar.querySelector('.tick')!.getAttribute('style');
    const highFill = highBar.querySelector('.fill')!.getAttribute('style');
    const highTick = highBar.querySelector('.tick')!.getAttribute('style');

    expect(lowFill).toBe('width: 35.4%;');
    expect(lowTick).toBe('left: 50%;');
    expect(highFill).toBe('width: 25%;');
    expect(highTick).toBe('left: 100%;');
  });
});

describe('EndpointsPanel banner isolation from panel dim', () => {
  // Structural regression, mirrors OverviewPanel's equivalent: opacity/filter
  // on an ancestor compound down the subtree, so a CSS override on .banner is
  // a no-op. The banner must live OUTSIDE the [data-state]-bearing wrapper to
  // stay at full contrast during an outage. Assert the DOM structure, not
  // computed style - jsdom has no layout.
  it('renders the StatusBanner as a sibling of, not inside, the data-state wrapper', async () => {
    component = mount(EndpointsPanel, { target: document.body });
    flushSync();

    await refreshWithEndpoint({ name: 'ep', type: 'ollama', status: 'healthy', priority: 100 });

    // Force the store into error status so the wrapper carries data-state='error'.
    global.fetch = vi.fn(async () => ({ status: 500, ok: false, headers: { get: () => null } }));
    endpoints.refresh();
    await vi.waitFor(() => expect(endpoints.status === 'error' || endpoints.status === 'stale').toBe(true));
    flushSync();

    const panel = document.getElementById('panel-endpoints')!;
    expect(panel).toBeTruthy();
    const dimmed = panel.querySelector('[data-state="stale"], [data-state="error"]');
    expect(dimmed).toBeTruthy();

    const banner = panel.querySelector('.banner');
    expect(banner).toBeTruthy();
    // The banner must NOT be inside the dimmed wrapper - it should be a
    // sibling of it within the panel root.
    expect(dimmed!.contains(banner)).toBe(false);
    expect(banner!.parentElement).toBe(panel);
  });
});


describe("EndpointsPanel runtime metrics", () => {
  it("renders fresh current/avg/max values and hides stale or unsupported values", async () => {
    component = mount(EndpointsPanel, { target: document.body });
    flushSync();

    await refreshWithEndpoint({
      name: "runtime-node",
      type: "vllm",
      status: "healthy",
      priority: 100,
      success_rate: "99.0%",
      request_count: 40,
      model_count: 1,
      min_latency_ms: 10,
      max_latency_ms: 90,
      runtime_metrics: {
        sampled_at: "2026-09-24T20:00:00Z",
        available_since: "2026-09-24T14:32:10Z",
        generation_tps: { current: 70.06, avg: 62, max: 91 },
        prefill_tps: { current: 502, avg: 440, max: 690 },
        cache_hit_percent: { current: 98.1, avg: 96.8 },
        kv_cache_usage_percent: 37,
        queue_depth: 6,
        requests_running: 3,
        spec_accept_percent: { current: 61.4, avg: 60.2 },
      },
    });

    expect([...document.querySelectorAll(".runtime-current")].map((el) => el.textContent!.trim())).toEqual([
      "70.1 tok/s", "502 tok/s", "98.1%", "37.0%", "6",
    ]);
    expect([...document.querySelectorAll(".runtime-summary")].map((el) => el.textContent!.trim())).toEqual([
      "avg 62 · max 91", "avg 440 · max 690", "avg 96.8%", "3 running",
    ]);
    expect(document.querySelector(".runtime-metric")!.getAttribute("title")).toContain("avg/max over last 10m");

    await refreshWithEndpoint({
      name: "runtime-node-stale",
      type: "vllm",
      status: "healthy",
      priority: 100,
      success_rate: "99.0%",
      request_count: 40,
      model_count: 1,
      min_latency_ms: 10,
      max_latency_ms: 90,
      runtime_metrics: {
        stale: true,
        sampled_at: "2026-09-24T20:00:00Z",
        generation_tps: { current: 70.06 },
        queue_depth: 6,
        requests_running: 3,
        spec_accept_percent: { current: 61.4 },
      },
    });
    expect([...document.querySelectorAll(".runtime-metric .dash")]).toHaveLength(5);
    expect(document.querySelector(".runtime-metric")!.getAttribute("title")).toContain("stale");
  });
});


describe("EndpointsPanel queue column and radiance rate caveats", () => {
  // Radiance reports engine-wide batch throughput and a scheduler queue; halogen
  // reports neither. The queue column therefore has to degrade to the same
  // "unsupported" em-dash as the other telemetry columns, and the rate cells
  // have to warn before an operator reads a batch figure as a per-stream speed.
  const BATCH_NOTE = "Engine throughput (batch-aggregate across concurrent requests), not per-stream speed";

  // Cells in column order: Gen, Prefill, Cache hit, KV used, Queue.
  function cell(index: number): { text: string; title: string } {
    const el = document.querySelectorAll(".runtime-metric")[index]!;
    return {
      text: el.textContent!.replace(/\s+/g, " ").trim(),
      title: el.getAttribute("title") ?? "",
    };
  }

  const base = {
    status: "healthy",
    priority: 100,
    success_rate: "99.0%",
    request_count: 40,
    model_count: 1,
    min_latency_ms: 10,
    max_latency_ms: 90,
  };

  it("shows queue depth with its running sub-line and folds spec acceptance into the Gen tooltip", async () => {
    component = mount(EndpointsPanel, { target: document.body });
    flushSync();

    await refreshWithEndpoint({
      ...base,
      name: "radiance-qfn",
      type: "radiance",
      runtime_metrics: {
        sampled_at: "2026-10-05T09:00:00Z",
        available_since: "2026-10-05T08:00:00Z",
        generation_tps: { current: 288.4 },
        prefill_tps: { current: 1904 },
        kv_cache_usage_percent: 96.5,
        queue_depth: 8,
        requests_running: 7,
        spec_accept_percent: { current: 55.3, avg: 54.1 },
      },
    });

    expect(cell(4).text).toBe("8 7 running");
    expect(cell(4).title).toContain("Queue depth");
    expect(cell(4).title).toContain("sampled 2026-10-05T09:00:00Z");

    // The batch caveat belongs on both rate cells; the acceptance figure only
    // explains generation, so it never rides on prefill or the other columns.
    expect(cell(0).title).toContain(BATCH_NOTE);
    expect(cell(0).title).toContain("Speculative decode acceptance: 55.3% (avg 54.1)");
    expect(cell(1).title).toContain(BATCH_NOTE);
    expect(cell(1).title).not.toContain("Speculative decode");
    expect(cell(2).title).not.toContain(BATCH_NOTE);
    expect(cell(3).title).not.toContain(BATCH_NOTE);
  });

  it("drops the average from the acceptance tooltip when the engine reports a gauge only", async () => {
    component = mount(EndpointsPanel, { target: document.body });
    flushSync();

    await refreshWithEndpoint({
      ...base,
      name: "radiance-gauge",
      type: "radiance",
      runtime_metrics: {
        sampled_at: "2026-10-05T09:00:00Z",
        generation_tps: { current: 300 },
        spec_accept_percent: { current: 55.3 },
      },
    });

    expect(cell(0).title).toContain("Speculative decode acceptance: 55.3%");
    expect(cell(0).title).not.toContain("(avg");
  });

  it("keeps the batch caveat without an acceptance figure the engine never emitted", async () => {
    component = mount(EndpointsPanel, { target: document.body });
    flushSync();

    await refreshWithEndpoint({
      ...base,
      name: "radiance-no-spec",
      type: "radiance",
      runtime_metrics: {
        sampled_at: "2026-10-05T09:00:00Z",
        generation_tps: { current: 300 },
        queue_depth: 0,
      },
    });

    expect(cell(0).title).toContain(BATCH_NOTE);
    expect(cell(0).title).not.toContain("Speculative decode");
    // A genuine zero backlog stays visible rather than collapsing to a dash.
    expect(cell(4).text).toBe("0 — running");
  });

  it("dashes the queue column for a backend whose metrics carry no queue gauge", async () => {
    component = mount(EndpointsPanel, { target: document.body });
    flushSync();

    await refreshWithEndpoint({
      ...base,
      name: "halogen-395",
      type: "vllm",
      runtime_metrics: {
        sampled_at: "2026-10-05T09:00:00Z",
        available_since: "2026-10-05T08:00:00Z",
        generation_tps: { current: 47.2, avg: 44, max: 51 },
        cache_hit_percent: { current: 99.6, avg: 98.4 },
      },
    });

    expect(cell(4).text).toBe("—");
    expect(cell(4).title).toBe("Queue depth: unsupported by this backend");
    // A single-stream backend must not inherit the batch caveat either.
    expect(cell(0).title).not.toContain(BATCH_NOTE);
    expect(cell(0).title).not.toContain("Speculative decode");
  });

  it("dashes the queue column when the backend reports no runtime metrics at all", async () => {
    component = mount(EndpointsPanel, { target: document.body });
    flushSync();

    await refreshWithEndpoint({ ...base, name: "ollama-1", type: "ollama" });

    expect(cell(4).text).toBe("—");
    expect(cell(4).title).toBe("Queue depth: unsupported by this backend");
  });

  it("hides queue depth and acceptance while the scrape is stale", async () => {
    component = mount(EndpointsPanel, { target: document.body });
    flushSync();

    await refreshWithEndpoint({
      ...base,
      name: "radiance-stale",
      type: "radiance",
      runtime_metrics: {
        stale: true,
        sampled_at: "2026-10-05T08:55:00Z",
        generation_tps: { current: 288.4 },
        queue_depth: 8,
        requests_running: 7,
        spec_accept_percent: { current: 55.3, avg: 54.1 },
      },
    });

    expect(cell(4).text).toBe("—");
    expect(cell(4).title).toContain("stale (last sampled 2026-10-05T08:55:00Z)");
    // Frozen figures are never presented as live measurements.
    expect(cell(0).title).toContain("stale");
    expect(cell(0).title).not.toContain("Speculative decode");
  });

  it("keys batch semantics on the resolved kind, not the endpoint type", async () => {
    component = mount(EndpointsPanel, { target: document.body });
    flushSync();

    // The documented deployment shape: radiance registered as
    // openai-compatible. It ships engine-batch-aggregated rates, so it must
    // carry the caveat the type check alone would miss.
    await refreshWithEndpoint({
      ...base,
      name: "rad-openai",
      type: "openai-compatible",
      runtime_metrics: {
        kind: "radiance",
        sampled_at: "2026-10-05T09:00:00Z",
        generation_tps: { current: 288.4 },
        spec_accept_percent: { current: 40 },
      },
    });
    expect(cell(0).title).toContain(BATCH_NOTE);
    expect(cell(0).title).toContain("Speculative decode acceptance: 40.0%");
  });

  it("withholds the batch caveat from a per-stream dialect pinned under a radiance type", async () => {
    component = mount(EndpointsPanel, { target: document.body });
    flushSync();

    await refreshWithEndpoint({
      ...base,
      name: "vllm-under-radiance-type",
      type: "radiance",
      runtime_metrics: {
        kind: "vllm",
        sampled_at: "2026-10-05T09:00:00Z",
        generation_tps: { current: 70 },
      },
    });
    // vLLM's decode-time denominator is per request time; no batch note, and
    // acceptance is not a vLLM signal.
    expect(cell(0).title).not.toContain(BATCH_NOTE);
    expect(cell(0).title).not.toContain("Speculative decode");
  });

  it("marks fresh-but-absent gauge columns unsupported instead of implying hidden samples", async () => {
    component = mount(EndpointsPanel, { target: document.body });
    flushSync();

    await refreshWithEndpoint({
      ...base,
      name: "halogen-no-queue",
      type: "halogen",
      runtime_metrics: {
        kind: "llamacpp",
        sampled_at: "2026-10-05T09:00:00Z",
        cache_hit_percent: { current: 55, avg: 50 },
      },
    });
    // Fresh payload, no KV gauge published: the tooltip must say so plainly.
    expect(cell(3).text).toBe("—");
    expect(cell(3).title).toBe("KV-cache utilisation: unsupported by this backend");
    expect(cell(2).title).toContain("avg over last 10m");
    expect(cell(2).title).not.toContain("max");
  });
});
