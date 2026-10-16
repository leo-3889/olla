import { flushSync, mount, unmount } from 'svelte';
import { describe, it, expect, afterEach } from 'vitest';
import ObservePower from './ObservePower.svelte';
import type { ObservePowerRow } from '../lib/types';

let component: ReturnType<typeof mount> | undefined;
afterEach(() => {
  if (component) unmount(component);
  document.body.innerHTML = '';
});

describe('ObservePower', () => {
  it('names meter, endpoint and node, and dashes unreported fields', () => {
    const rows: ObservePowerRow[] = [
      { target: 'shelly-ai-395', endpoint: 'halogen-395', device_name: 'Shelly Plug S', node: 'leo395max', watts: 9.4, temp_c: null, meter_valid: 1, overpower: 0, output_on: 1, age_s: 3 },
    ];
    component = mount(ObservePower, { target: document.body, props: { rows, ageS: 3, degraded: false } });
    flushSync();
    const text = document.querySelector('.section')!.textContent!;
    expect(text).toContain('halogen-395');
    expect(text).toContain('9.4');
    expect(text).toContain('—'); // unreported temp never renders as 0
  });
});
