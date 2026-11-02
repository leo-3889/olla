import { flushSync, mount, unmount } from 'svelte';
import { describe, it, expect, afterEach } from 'vitest';
import DashboardLayout from './DashboardLayout.svelte';
import { bootstrap, DEFAULT_UI_CONFIG } from '../lib/stores/bootstrap.svelte';

let component: ReturnType<typeof mount> | undefined;
afterEach(() => {
  if (component) unmount(component);
  document.body.innerHTML = '';
  bootstrap.config = DEFAULT_UI_CONFIG;
});

// The footer badge is the one place that states the EFFECTIVE metrics
// backend + poll cadence; a token-fallback boot (config says influx, the
// sampler answers) must be readable at a glance, not in tooltips.
describe('footer metrics badge', () => {
  it('shows the effective source and poll cadence', () => {
    bootstrap.config = { ...DEFAULT_UI_CONFIG, metrics_source: 'influx' };
    component = mount(DashboardLayout, { target: document.body, props: { children: (() => {}) as never } });
    flushSync();
    const badge = document.querySelector('.footer-badge')!;
    expect(badge.textContent).toBe('metrics: influx · polls 5s/5s/15s/10s');
  });

  it('reports sampler when the manifest says sampler', () => {
    component = mount(DashboardLayout, { target: document.body, props: { children: (() => {}) as never } });
    flushSync();
    expect(document.querySelector('.footer-badge')?.textContent).toContain('metrics: sampler');
  });
});
