import { flushSync, mount, unmount } from 'svelte';
import { describe, it, expect, afterEach } from 'vitest';
import ObserveGpu from './ObserveGpu.svelte';
import type { ObserveGpuRow } from '../lib/types';

let component: ReturnType<typeof mount> | undefined;
afterEach(() => {
  if (component) unmount(component);
  document.body.innerHTML = '';
});

const live: ObserveGpuRow = { endpoint: 'radiance-gpus-128', gpu: '0', node: 'leo9700', busy_percent: 97, age_s: 2 };

describe('ObserveGpu dead-exporter disclosure', () => {
  it('renders a no-data card per absent endpoint', () => {
    component = mount(ObserveGpu, { target: document.body, props: { gpus: [live], absent: ['radiance-gpus'], ageS: 2, degraded: false } });
    flushSync();
    const cards = [...document.querySelectorAll('.gpu')];
    expect(cards).toHaveLength(2);
    // Multi-box fleets need the ENDPOINT in the visible header: two boxes
    // both render "GPU 0" and the node tag can be absent server-side.
    expect(cards[0].querySelector('.gpu-name')?.textContent).toContain('radiance-gpus-128');
    expect(cards[1].className).toContain('nodead');
    expect(cards[1].textContent).toContain('radiance-gpus');
    expect(cards[1].textContent).toContain('no data');
  });

  it('stays quiet when every configured endpoint reports', () => {
    component = mount(ObserveGpu, { target: document.body, props: { gpus: [live], absent: [], ageS: 2, degraded: false } });
    flushSync();
    expect(document.querySelectorAll('.gpu')).toHaveLength(1);
    expect(document.body.textContent).not.toContain('exporter down');
  });
});
