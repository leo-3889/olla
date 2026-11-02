import { flushSync, mount, unmount } from 'svelte';
import { describe, it, expect, afterEach } from 'vitest';
import EndpointTrend from './EndpointTrend.svelte';
import type { EndpointSample } from '../lib/stores/endpoint-history.svelte';

let component: ReturnType<typeof mount> | undefined;
afterEach(() => {
  if (component) unmount(component);
  document.body.innerHTML = '';
});

function render(samples: EndpointSample[], name = 'radiance-9700-128'): void {
  component = mount(EndpointTrend, { target: document.body, props: { samples, name } });
  flushSync();
}

const s = (t: number, gen: number | null, prefill: number | null): EndpointSample => ({ t, gen, prefill });

// Mirrors the component's viewBox geometry (W=600 H=150 PAD=6).
const PAD = 6;
const BASE = 150 - PAD;
const PLOT_H = 150 - 2 * PAD;
const PLOT_W = 600 - 2 * PAD;
// Mirrors the component's zero stub: 0 must read as a stub, not a gap.
const STUB = 0.6;

function attrs(el: Element): Record<string, number> {
  const out: Record<string, number> = {};
  for (const k of ['x', 'y', 'width', 'height']) out[k] = parseFloat(el.getAttribute(k) ?? 'NaN');
  return out;
}

describe('EndpointTrend', () => {
  it('shows the collecting state below two samples', () => {
    render([s(1, 5, 50)]);
    expect(document.body.textContent).toContain('collecting samples');
    expect(document.querySelector('svg.trend-svg')).toBeNull();
  });

  it('draws PAIRED bars (gen left, prefill right) on a shared floor', () => {
    render([s(1000, 10, 100), s(2000, 30, 300), s(3000, 20, 200)]);
    const gen = [...document.querySelectorAll('rect.bar-gen')];
    const pre = [...document.querySelectorAll('rect.bar-prefill')];
    // one 60 s bin holds all three samples: gen mean 20, prefill mean 200
    expect(gen.length).toBe(1);
    expect(pre.length).toBe(1);
    const g = attrs(gen[0]);
    const p = attrs(pre[0]);
    // OWN scales: each series defines its own max in a single bin, so both
    // bars span the full plot height (the old stacked/shared-scale drawing
    // made gen a 20/220 sliver - that contract is gone by design).
    expect(g.height).toBeCloseTo(PLOT_H, 6);
    expect(p.height).toBeCloseTo(PLOT_H, 6);
    // side by side, never stacked: same floor, gen owns the left half,
    // prefill the right half, and the bars do not overlap.
    expect(g.y + g.height).toBeCloseTo(BASE, 6);
    expect(p.y + p.height).toBeCloseTo(BASE, 6);
    expect(p.x).toBeGreaterThan(g.x);
    expect(p.x).toBeGreaterThanOrEqual(g.x + g.width);
    expect(g.width).toBeCloseTo(p.width, 6);
    // legend keeps RAW-ring semantics: avg of 10/30/20 = 20, 100/300/200 = 200
    expect(document.body.textContent).toContain('gen avg 20 tok/s');
    expect(document.body.textContent).toContain('prefill avg 200 tok/s');
    expect(document.body.textContent).toContain('radiance-9700-128');
    // honest naming: the accessible name and the hint both disclose
    // the paired form and the independent scales
    expect(document.querySelector('svg.trend-svg')?.getAttribute('aria-label')).toContain(
      'paired 60-second bars, each series on its own scale',
    );
    expect(document.body.textContent).toContain('independent scales');
  });

  it('each series scales on its OWN max: prefill >> gen keeps gen readable', () => {
    // The measured fleet split: gen ~103 vs prefill ~2,234 tok/s.
    render([s(1000, 80, 2000), s(2000, 80, 2000), s(61_000, 120, 2400), s(62_000, 120, 2400)]);
    const gen = [...document.querySelectorAll('rect.bar-gen')].map(attrs);
    const pre = [...document.querySelectorAll('rect.bar-prefill')].map(attrs);
    expect(gen.length).toBe(2);
    expect(pre.length).toBe(2);
    // gen scales to genMax 120: bin 1 = 80/120 of the plot (the old shared
    // 2080-total scale gave ~5 units - an invisible sliver, the user's bug).
    expect(gen[0].height).toBeCloseTo((80 / 120) * PLOT_H, 6);
    expect(gen[0].height).toBeGreaterThan(PLOT_H * 0.5);
    // prefill scales to preMax 2400: bin 1 = 2000/2400 of the plot
    expect(pre[0].height).toBeCloseTo((2000 / 2400) * PLOT_H, 6);
    // tallest gen bar and tallest prefill bar each reach the ceiling
    expect(Math.max(...gen.map((a) => a.height))).toBeCloseTo(PLOT_H, 6);
    expect(Math.max(...pre.map((a) => a.height))).toBeCloseTo(PLOT_H, 6);
  });

  it('an empty bin renders NO bar (honest gap, not a bridge)', () => {
    render([s(1000, 10, 100), s(70_000, null, null), s(130_000, 30, 300)]);
    const gen = [...document.querySelectorAll('rect.bar-gen')];
    const pre = [...document.querySelectorAll('rect.bar-prefill')];
    expect(gen.length).toBe(2);
    expect(pre.length).toBe(2);
    // the middle bin (60 s grid) produced a null pair: the two surviving
    // bars sit ~2 bins apart on the time axis, no rect in between
    const xs = gen.map((r) => attrs(r).x);
    const binW = (60_000 / 129_000) * PLOT_W; // span = 130000 - 1000
    expect(xs[1] - xs[0]).toBeGreaterThan(binW * 1.5);
  });

  it('a gap inside one bin is skipped by the mean (bin, not bridge)', () => {
    render([s(1000, 10, 100), s(2000, null, 200), s(3000, 30, 300)]);
    expect(document.querySelectorAll('rect.bar-gen').length).toBe(1);
    expect(document.querySelectorAll('rect.bar-prefill').length).toBe(1);
    const g = attrs(document.querySelector('rect.bar-gen')!);
    const p = attrs(document.querySelector('rect.bar-prefill')!);
    // gen mean skips the gap: (10+30)/2 = 20 -> genMax = 20 -> full height;
    // prefill (100+200+300)/3 = 200 -> preMax = 200 -> full height.
    expect(g.height).toBeCloseTo(PLOT_H, 6);
    expect(p.height).toBeCloseTo(PLOT_H, 6);
    expect(document.body.textContent).toContain('gen avg 20 tok/s');
  });

  it('all-gap windows say so instead of rendering an empty frame', () => {
    render([s(1000, null, null), s(2000, null, null)]);
    expect(document.body.textContent).toContain('reports no generation/prefill counters');
    expect(document.querySelector('svg.trend-svg')).toBeNull();
  });

  it('genuine zeros render as stubs (idle engine is not a gap)', () => {
    render([s(1000, 0, 0), s(2000, 0, 0)]);
    expect(document.querySelector('svg.trend-svg')).not.toBeNull();
    const g = attrs(document.querySelector('rect.bar-gen')!);
    const p = attrs(document.querySelector('rect.bar-prefill')!);
    // zero bins draw the 0.6-unit stub at the floor: visible "0", distinct
    // from the missing-bar gap (zero and absence remain different facts).
    expect(g.height).toBeCloseTo(STUB, 6);
    expect(p.height).toBeCloseTo(STUB, 6);
    expect(g.y + g.height).toBeCloseTo(BASE, 6);
    expect(document.body.textContent).toContain('gen avg 0 tok/s');
  });

  it('zero stub vs null gap in ONE window: 0 draws, absence does not', () => {
    // bin 0 = genuinely idle (0, 0); bin 60 s = null pair (nothing reported);
    // bin 120 s = real values. Zero must read as a stub, absence as nothing.
    render([s(0, 0, 0), s(60_000, null, null), s(120_000, 50, 1500), s(170_000, 50, 1500)]);
    const gen = [...document.querySelectorAll('rect.bar-gen')].map(attrs);
    const pre = [...document.querySelectorAll('rect.bar-prefill')].map(attrs);
    // the null pair bin produced NO rect in either series
    expect(gen.length).toBe(2);
    expect(pre.length).toBe(2);
    // the zero bin is present and visible: the 0.6-unit stub on the floor
    expect(gen[0].height).toBeCloseTo(STUB, 6);
    expect(gen[0].y + gen[0].height).toBeCloseTo(BASE, 6);
    expect(pre[0].height).toBeCloseTo(STUB, 6);
    // unmistakably a stub next to the real bar, and the gap bin is whitespace
    expect(gen[1].height).toBeCloseTo(PLOT_H, 6);
    expect(gen[1].height).toBeGreaterThan(100 * gen[0].height);
    expect(gen[1].x).toBeGreaterThan(gen[0].x + gen[0].width);
    // every prefill bar sits right of its gen partner (paired, not stacked)
    for (let i = 0; i < gen.length; i++) expect(pre[i].x).toBeGreaterThan(gen[i].x);
  });

  it('window label spans first to last sample', () => {
    render([s(0, 1, 1), s(61_000, 2, 2)]);
    expect(document.body.textContent).toContain('window 1m');
  });
});
