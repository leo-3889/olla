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

describe('EndpointTrend', () => {
  it('shows the collecting state below two samples', () => {
    render([s(1, 5, 50)]);
    expect(document.body.textContent).toContain('collecting samples');
    expect(document.querySelector('svg.trend-svg')).toBeNull();
  });

  it('draws one line path per series and reports averages', () => {
    render([s(1000, 10, 100), s(2000, 30, 300), s(3000, 20, 200)]);
    const gen = document.querySelector('path.line-gen');
    const pre = document.querySelector('path.line-prefill');
    expect(gen?.getAttribute('d')).toMatch(/^M[\d.]+,[\d.]+L[\d.]+,[\d.]+L[\d.]+,[\d.]+$/);
    expect(pre?.getAttribute('d')).toContain('M');
    // avg of 10/30/20 = 20, of 100/300/200 = 200
    expect(document.body.textContent).toContain('gen avg 20 tok/s');
    expect(document.body.textContent).toContain('prefill avg 200 tok/s');
    expect(document.body.textContent).toContain('radiance-9700-128');
  });

  it('a null sample breaks the line into separate runs (gap, not bridge)', () => {
    render([s(1000, 10, 100), s(2000, null, 200), s(3000, 30, 300)]);
    const d = document.querySelector('path.line-gen')?.getAttribute('d') ?? '';
    expect((d.match(/M/g) ?? []).length).toBe(2);
    const p = document.querySelector('path.line-prefill')?.getAttribute('d') ?? '';
    expect((p.match(/M/g) ?? []).length).toBe(1);
  });

  it('all-gap windows say so instead of rendering an empty frame', () => {
    render([s(1000, null, null), s(2000, null, null)]);
    expect(document.body.textContent).toContain('reports no generation/prefill counters');
    expect(document.querySelector('svg.trend-svg')).toBeNull();
  });

  it('genuine zeros render (idle engine is not a gap)', () => {
    render([s(1000, 0, 0), s(2000, 0, 0)]);
    expect(document.querySelector('svg.trend-svg')).not.toBeNull();
    expect(document.body.textContent).toContain('gen avg 0 tok/s');
  });

  it('window label spans first to last sample', () => {
    render([s(0, 1, 1), s(61_000, 2, 2)]);
    expect(document.body.textContent).toContain('window 1m');
  });
});
