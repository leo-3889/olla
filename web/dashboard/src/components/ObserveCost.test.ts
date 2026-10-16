import { flushSync, mount, unmount } from 'svelte';
import { describe, it, expect, afterEach } from 'vitest';
import ObserveCost from './ObserveCost.svelte';
import type { ObserveCost as ObserveCostData } from '../lib/types';

let component: ReturnType<typeof mount> | undefined;
afterEach(() => {
  if (component) unmount(component);
  document.body.innerHTML = '';
});

function mountCost(cost: ObserveCostData) {
  component = mount(ObserveCost, { target: document.body, props: { cost, degraded: false } });
  flushSync();
  return [...document.querySelectorAll('tbody td')].map((td) => td.textContent?.trim() ?? '');
}

describe('ObserveCost per-entry currency', () => {
  it('labels each cell with the entry currency, not the global one', () => {
    const cells = mountCost({
      currency: 'EUR',
      hour: { 'us-box': { cost: 0.25, currency: 'USD', period_status: 'open', period_start: '2026-10-16T09:00:00Z' } },
      day: { 'us-box': { cost: 3.14, currency: 'USD', period_status: 'open', period_start: '2026-10-15T22:00:00Z' } },
    });
    expect(cells[0]).toContain('0.250 USD');
    expect(cells[1]).toContain('3.14 USD');
    expect(cells[0]).not.toContain('EUR');
  });

  it('falls back to the global currency when entries omit it', () => {
    const cells = mountCost({
      currency: 'EUR',
      hour: { 'eu-box': { cost: 0.4, period_status: 'open' } },
      day: {},
    });
    expect(cells[0]).toContain('0.400 EUR');
  });
});

describe('ObserveCost dropped disclosure', () => {
  it('names endpoints dropped by the liveness guard', () => {
    mountCost({
      currency: 'EUR',
      dropped: ['ghost-b'],
      hour: { 'live-a': { cost: 0.1, period_status: 'open' } },
      day: {},
    });
    expect(document.querySelector('.section-note')?.textContent).toContain('no current rollup: ghost-b');
  });
});
