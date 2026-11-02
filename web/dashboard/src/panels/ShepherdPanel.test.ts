import { flushSync, mount, unmount } from 'svelte';
import { describe, it, expect, afterEach, vi } from 'vitest';
import { pollScheduler } from '../lib/poll-scheduler';
import { shepherd } from '../lib/stores/shepherd.svelte';
import ShepherdPanel from './ShepherdPanel.svelte';

// Contract surface (do not invent fields):
//   GET  /internal/shepherd          -> ShepherdResponse
//   POST /internal/shepherd/enabled  {enabled} -> same payload
//   POST /internal/shepherd/config   {wake_window_seconds,keep_asleep_if_up} -> full payload | 400 {errors} | 409
//
// Idiom mirrors the sibling panel tests: static store import, module-singleton
// store, mount + flushSync, fetch stubbed per test, waitFor on the observable.

let component: ReturnType<typeof mount> | undefined;
afterEach(() => {
  if (component) unmount(component);
  component = undefined;
  document.body.innerHTML = '';
  shepherd.stop();
});

function jsonResponse(body: unknown, status = 200) {
  return {
    status,
    ok: status >= 200 && status < 300,
    headers: { get: () => null },
    json: async () => body,
  };
}

function payload(
  over: Partial<{
    available: boolean;
    enabled: boolean;
    dry_run: boolean;
    editable: { wake_window_seconds: number; keep_asleep_if_up: string[] };
    defaults: { wake_window_seconds: number; keep_asleep_if_up: string[] };
  }> = {},
) {
  return {
    available: true,
    enabled: true,
    dry_run: false,
    config: {
      node_name: 'node-a',
      mac_masked: 'aa:bb:**:**:**:**',
      idle_shutdown_seconds: 1800,
      wake_window_seconds: 300,
    },
    state: {
      last_check: '2026-10-13T09:00:00Z',
      last_action: 'idle-shutdown',
      last_wake: '2026-10-13T07:30:00Z',
      last_shutdown: '2026-10-13T08:15:00Z',
      last_error: '',
    },
    editable: { wake_window_seconds: 300, keep_asleep_if_up: ['eth0'] },
    defaults: { wake_window_seconds: 180, keep_asleep_if_up: [] },
    constraints: {
      wake_window_seconds: { min: 10, max: 3600 },
      keep_asleep_if_up: { max_items: 5, pattern: '^[a-zA-Z0-9_.-]+$' },
    },
    ...over,
  };
}

// Mount, drive one GET, and wait for THIS payload's observable (the store is a
// singleton reused across tests, so status may already read 'ok').
async function mountWith(body: Record<string, unknown>) {
  global.fetch = vi.fn(async () => jsonResponse(body));
  component = mount(ShepherdPanel, { target: document.body });
  flushSync();
  shepherd.refresh();
  // Wait for THIS payload's observable, not just "not null" - the store is a
  // module singleton reused across tests, so data may still hold the previous
  // test's payload when this refresh resolves (the EndpointsPanel helper warns
  // about exactly this). The masked MAC is a per-payload discriminator.
  const mac = (body.config as Record<string, unknown> | undefined)?.mac_masked;
  await vi.waitFor(() => expect(shepherd.data?.config.mac_masked).toBe(mac));
  flushSync();
}

function statusText() {
  return document.querySelector('.status-text')?.textContent?.trim();
}

describe('ShepherdPanel watcher-not-mounted', () => {
  it('renders a single honest "watcher not mounted" cell when available=false', async () => {
    await mountWith({
      available: false,
      enabled: false,
      dry_run: false,
      config: { node_name: '', mac_masked: '', idle_shutdown_seconds: 0, wake_window_seconds: 0 },
      state: { last_check: '', last_action: '', last_wake: '', last_shutdown: '', last_error: '' },
    });

    const missing = document.querySelector('.watcher-missing');
    expect(missing).toBeTruthy();
    expect(missing!.textContent!.trim()).toBe('watcher not mounted');
    // No fabricated zeros / fake config grid / toggle: the whole body collapses.
    expect(document.querySelector('.tile-grid')).toBeNull();
    expect(document.querySelector('.toggle-btn')).toBeNull();
    expect(document.body.textContent).not.toContain('0m');
    expect(document.body.textContent).not.toContain('disabled');
  });
});

describe('ShepherdPanel configuration grid', () => {
  it('shows the masked MAC verbatim and humanises the idle timeout', async () => {
    await mountWith(payload());

    const tiles = [...document.querySelectorAll('.tile')];
    const values = tiles.map((t) => t.querySelector('.value')?.textContent?.trim());
    // Masked MAC is not re-masked, not expanded; 1800s reads as the shared
    // fmtDuration form ("30m"), not a raw "1800".
    expect(values).toContain('aa:bb:**:**:**:**');
    expect(values).toContain('30m');
    expect(values).toContain('5m'); // 300s wake window
    expect(values).toContain('node-a');
    expect(document.body.textContent).not.toContain('1800');
  });
});

describe('ShepherdPanel enable/disable toggle', () => {
  it('POSTs /internal/shepherd/enabled with {enabled:false} and updates the UI from the response', async () => {
    const calls: { url: string; init?: RequestInit }[] = [];
    global.fetch = vi.fn(async (url: unknown, init?: RequestInit) => {
      calls.push({ url: String(url), init });
      if (String(url).endsWith('/internal/shepherd/enabled')) {
        // Server truth: enabled flipped to false.
        return jsonResponse(payload({ enabled: false }));
      }
      return jsonResponse(payload({ enabled: true }));
    });

    component = mount(ShepherdPanel, { target: document.body });
    flushSync();
    shepherd.refresh();
    await vi.waitFor(() => expect(shepherd.data?.enabled).toBe(true));
    flushSync();
    expect(statusText()).toBe('enabled');

    const btn = document.querySelector<HTMLButtonElement>('.toggle-btn')!;
    expect(btn.textContent!.trim()).toBe('Disable');
    btn.click();

    await vi.waitFor(() => expect(shepherd.data?.enabled).toBe(false));
    flushSync();

    const post = calls.find((c) => c.url.endsWith('/internal/shepherd/enabled'))!;
    expect(post).toBeTruthy();
    expect(post.init?.method).toBe('POST');
    expect(JSON.parse(String(post.init?.body))).toEqual({ enabled: false });
    // UI now reads from the POST response, not an optimistic flip.
    expect(statusText()).toBe('disabled');
    expect(document.querySelector<HTMLButtonElement>('.toggle-btn')!.textContent!.trim()).toBe('Enable');
  });

  it('optimistically disables the button during flight, then refreshes from the response', async () => {
    let release!: (v: unknown) => void;
    const gate = new Promise((r) => (release = r));
    global.fetch = vi.fn(async (url: unknown) => {
      if (String(url).endsWith('/internal/shepherd/enabled')) {
        await gate;
        return jsonResponse(payload({ enabled: false }));
      }
      return jsonResponse(payload({ enabled: true }));
    });

    component = mount(ShepherdPanel, { target: document.body });
    flushSync();
    shepherd.refresh();
    await vi.waitFor(() => expect(shepherd.data?.enabled).toBe(true));
    flushSync();

    document.querySelector<HTMLButtonElement>('.toggle-btn')!.click();
    flushSync();
    // In flight: the button is disabled and its label reads "saving\u2026", so a
    // second click cannot fire a racing POST.
    await vi.waitFor(() => expect(shepherd.busy).toBe(true));
    flushSync();
    expect(document.querySelector<HTMLButtonElement>('.toggle-btn')!.disabled).toBe(true);
    expect(document.querySelector('.toggle-btn')!.textContent!.trim()).toBe('saving\u2026');

    release(undefined);
    await vi.waitFor(() => expect(shepherd.busy).toBe(false));
    flushSync();
    expect(document.querySelector<HTMLButtonElement>('.toggle-btn')!.disabled).toBe(false);
  });
});

describe('ShepherdPanel last_error', () => {
  it('shows last_error as a warning row only when non-empty', async () => {
    await mountWith(payload());
    expect(document.querySelector('.state-row.warning')).toBeNull();

    global.fetch = vi.fn(async () =>
      jsonResponse({ ...payload(), state: { ...payload().state, last_error: 'WOL send failed: no route to host' } })
    );
    shepherd.refresh();
    await vi.waitFor(() => expect(shepherd.data?.state.last_error).toBe('WOL send failed: no route to host'));
    flushSync();

    const warn = document.querySelector('.state-row.warning');
    expect(warn).toBeTruthy();
    expect(warn!.textContent).toContain('WOL send failed: no route to host');
  });
});

describe('ShepherdPanel polling lifecycle', () => {
  it('activates the shepherd job on mount and deactivates on unmount', () => {
    global.fetch = vi.fn(async () => jsonResponse(payload()));
    expect(pollScheduler.isActive('shepherd')).toBe(false);

    component = mount(ShepherdPanel, { target: document.body });
    flushSync();
    expect(pollScheduler.isActive('shepherd')).toBe(true);

    unmount(component);
    component = undefined;
    flushSync();
    expect(pollScheduler.isActive('shepherd')).toBe(false);
  });
});

// --- Wake triggers sub-block tests ----------------------------------------

describe('ShepherdPanel wake triggers', () => {
  it('renders inputs seeded from editable values', async () => {
    await mountWith(payload());

    const input = document.querySelector<HTMLInputElement>('#wol-window-seconds');
    expect(input).toBeTruthy();
    expect(input!.value).toBe('300');
    expect(input!.min).toBe('10');
    expect(input!.max).toBe('3600');

    // Tag-list shows editable tags
    const chips = [...document.querySelectorAll('.tag-chip')];
    expect(chips.map((c) => c.textContent?.replace(/\u00d7/g, '').trim())).toEqual(['eth0']);
  });

  it('save POSTs /internal/shepherd/config with edited values', async () => {
    const calls: { url: string; init?: RequestInit }[] = [];
    global.fetch = vi.fn(async (url: unknown, init?: RequestInit) => {
      calls.push({ url: String(url), init });
      if (String(url).endsWith('/internal/shepherd/config')) {
        // Return updated payload reflecting the save
        const body = JSON.parse(String(init?.body));
        return jsonResponse(payload({ editable: body }));
      }
      return jsonResponse(payload());
    });

    component = mount(ShepherdPanel, { target: document.body });
    flushSync();
    shepherd.refresh();
    await vi.waitFor(() => expect(shepherd.data?.config.mac_masked).toBe('aa:bb:**:**:**:**'));
    flushSync();

    // Edit the number input
    const input = document.querySelector<HTMLInputElement>('#wol-window-seconds')!;
    input.value = '600';
    input.dispatchEvent(new Event('input', { bubbles: true }));
    flushSync();

    // Save button becomes enabled
    const saveBtn = document.querySelector<HTMLButtonElement>('.save-btn')!;
    expect(saveBtn.disabled).toBe(false);
    saveBtn.click();

    await vi.waitFor(() => {
      const post = calls.find((c) => c.url.endsWith('/internal/shepherd/config'));
      expect(post).toBeTruthy();
    });

    const post = calls.find((c) => c.url.endsWith('/internal/shepherd/config'))!;
    expect(post.init?.method).toBe('POST');
    const body = JSON.parse(String(post.init?.body));
    expect(body.wake_window_seconds).toBe(600);
    expect(body.keep_asleep_if_up).toEqual(['eth0']);
  });

  it('400 map renders per-field reason inline', async () => {
    global.fetch = vi.fn(async (url: unknown, init?: RequestInit) => {
      if (String(url).endsWith('/internal/shepherd/config')) {
        return jsonResponse({ errors: { wake_window_seconds: 'must be at most 3600' } }, 400);
      }
      return jsonResponse(payload());
    });

    component = mount(ShepherdPanel, { target: document.body });
    flushSync();
    shepherd.refresh();
    await vi.waitFor(() => expect(shepherd.data?.config.mac_masked).toBe('aa:bb:**:**:**:**'));
    flushSync();

    // Edit and save
    const input = document.querySelector<HTMLInputElement>('#wol-window-seconds')!;
    input.value = '9999';
    input.dispatchEvent(new Event('input', { bubbles: true }));
    flushSync();

    document.querySelector<HTMLButtonElement>('.save-btn')!.click();
    await vi.waitFor(() => expect(shepherd.saveErrors).toBeTruthy());
    flushSync();

    const fieldErr = document.querySelector('.field-error');
    expect(fieldErr).toBeTruthy();
    expect(fieldErr!.textContent).toContain('must be at most 3600');
  });

  it('409 renders watcher dir not writable notice', async () => {
    global.fetch = vi.fn(async (url: unknown, init?: RequestInit) => {
      if (String(url).endsWith('/internal/shepherd/config')) {
        return jsonResponse({}, 409);
      }
      return jsonResponse(payload());
    });

    component = mount(ShepherdPanel, { target: document.body });
    flushSync();
    shepherd.refresh();
    await vi.waitFor(() => expect(shepherd.data?.config.mac_masked).toBe('aa:bb:**:**:**:**'));
    flushSync();

    // Edit and save
    const input = document.querySelector<HTMLInputElement>('#wol-window-seconds')!;
    input.value = '400';
    input.dispatchEvent(new Event('input', { bubbles: true }));
    flushSync();

    document.querySelector<HTMLButtonElement>('.save-btn')!.click();
    await vi.waitFor(() => expect(shepherd.dirNotWritable).toBe(true));
    flushSync();

    expect(document.body.textContent).toContain('watcher dir not writable');
  });

  it('override-vs-default labeling shows when editable differs from defaults', async () => {
    // editable.wake_window_seconds=300, defaults.wake_window_seconds=180 -> override
    await mountWith(payload());

    const overrides = [...document.querySelectorAll('.chip.override')];
    const yamlDefaults = [...document.querySelectorAll('.chip.yaml-default')];
    // wake_window_seconds: 300 !== 180 -> override
    // keep_asleep_if_up: ['eth0'] !== [] -> override
    expect(overrides.length).toBe(2);
    expect(yamlDefaults.length).toBe(0);
  });

  it('yaml-default labeling shows when editable matches defaults', async () => {
    // Unique MAC: the store is a module singleton, so a payload with the same
    // MAC as the prior test resolves the waitFor against stale prior data.
    const p = payload({
      editable: { wake_window_seconds: 180, keep_asleep_if_up: [] },
      defaults: { wake_window_seconds: 180, keep_asleep_if_up: [] },
    });
    p.config = { ...p.config, mac_masked: 'cc:dd:**:**:**:**' };
    await mountWith(p);

    const overrides = [...document.querySelectorAll('.chip.override')];
    const yamlDefaults = [...document.querySelectorAll('.chip.yaml-default')];
    expect(overrides.length).toBe(0);
    expect(yamlDefaults.length).toBe(2);
  });

  it('no fields for models/idle-shutdown exist in the wake triggers DOM', async () => {
    await mountWith(payload());

    // The wake triggers section must not contain idle-shutdown or model fields
    const section = [...document.querySelectorAll('.section')].find(
      (s) => s.querySelector('h2')?.textContent?.trim() === 'Wake triggers',
    );
    expect(section).toBeTruthy();
    expect(section!.textContent).not.toContain('idle_shutdown');
    expect(section!.textContent).not.toContain('Idle shutdown');
    expect(section!.textContent).not.toContain('model');
  });

  it('save button is disabled when form is clean', async () => {
    await mountWith(payload());
    const saveBtn = document.querySelector<HTMLButtonElement>('.save-btn')!;
    expect(saveBtn.disabled).toBe(true);
  });

  it('shows apply note line', async () => {
    await mountWith(payload());
    expect(document.querySelector('.apply-note')?.textContent).toContain(
      "changes apply on the watcher\u2019s next minute",
    );
  });
});


// --- B1 regression: null editable.keep_asleep_if_up with available:false ----
describe('ShepherdPanel B1: null keep_asleep_if_up (Go zero-value)', () => {
  it('renders watcher-not-mounted without throwing when editable.keep_asleep_if_up is null', async () => {
    // Go value-struct marshalling: zero-value string slice arrives as null.
    const nullPayload = {
      available: false,
      enabled: false,
      dry_run: false,
      config: { node_name: '', mac_masked: '', idle_shutdown_seconds: 0, wake_window_seconds: 0 },
      state: { last_check: '', last_action: '', last_wake: '', last_shutdown: '', last_error: '' },
      editable: { wake_window_seconds: 0, keep_asleep_if_up: null },
      defaults: { wake_window_seconds: 0, keep_asleep_if_up: null },
      constraints: {
        wake_window_seconds: { min: 10, max: 3600 },
        keep_asleep_if_up: { max_items: 5, pattern: '^[a-zA-Z0-9_.-]+$' },
      },
    };
    // Must not throw on mount.
    await mountWith(nullPayload);
    const missing = document.querySelector('.watcher-missing');
    expect(missing).toBeTruthy();
    expect(missing!.textContent!.trim()).toBe('watcher not mounted');
  });

  it('renders watcher-not-mounted when editable is null entirely', async () => {
    const nullPayload = {
      available: false,
      enabled: false,
      dry_run: false,
      config: { node_name: '', mac_masked: '', idle_shutdown_seconds: 0, wake_window_seconds: 0 },
      state: { last_check: '', last_action: '', last_wake: '', last_shutdown: '', last_error: '' },
      editable: null,
      defaults: null,
      constraints: null,
    };
    await mountWith(nullPayload);
    expect(document.querySelector('.watcher-missing')).toBeTruthy();
  });
});

// --- M1: revertToDefaults issues DELETE ------------------------------------
describe('ShepherdPanel M1: revertToDefaults uses DELETE', () => {
  it('revert issues DELETE /internal/shepherd/config and updates from response', async () => {
    const calls: { url: string; init?: RequestInit }[] = [];
    global.fetch = vi.fn(async (url: unknown, init?: RequestInit) => {
      calls.push({ url: String(url), init });
      if (String(url).endsWith('/internal/shepherd/config') && init?.method === 'DELETE') {
        // Return refreshed payload (defaults restored)
        const p = payload({
          editable: { wake_window_seconds: 180, keep_asleep_if_up: [] },
        });
        p.config = { ...p.config, mac_masked: 'ff:ee:**:**:**:**' };
        return jsonResponse(p);
      }
      return jsonResponse(payload());
    });

    component = mount(ShepherdPanel, { target: document.body });
    flushSync();
    shepherd.refresh();
    await vi.waitFor(() => expect(shepherd.data?.config.mac_masked).toBe('aa:bb:**:**:**:**'));
    flushSync();

    // Click revert
    const revertBtn = document.querySelector<HTMLButtonElement>('.revert-btn')!;
    expect(revertBtn).toBeTruthy();
    revertBtn.click();

    await vi.waitFor(() => {
      const del = calls.find((c) => c.url.endsWith('/internal/shepherd/config') && c.init?.method === 'DELETE');
      expect(del).toBeTruthy();
    });

    // Response updated the store
    await vi.waitFor(() => expect(shepherd.data?.config.mac_masked).toBe('ff:ee:**:**:**:**'));
    flushSync();
    // No POST to config with defaults
    const posts = calls.filter((c) => c.url.endsWith('/internal/shepherd/config') && c.init?.method === 'POST');
    expect(posts.length).toBe(0);
  });
});

// --- M2: NaN field disables save; 400 {"error"} shows banner ---------------
describe('ShepherdPanel M2: NaN guard and banner', () => {
  it('NaN in the numeric field disables Save', async () => {
    await mountWith(payload());

    const input = document.querySelector<HTMLInputElement>('#wol-window-seconds')!;
    // Set a non-numeric value that produces NaN
    input.value = '';
    input.dispatchEvent(new Event('input', { bubbles: true }));
    flushSync();

    const saveBtn = document.querySelector<HTMLButtonElement>('.save-btn')!;
    // Empty input => NaN via bind:value={formWindow} on type=number => formValid is false => save disabled
    expect(saveBtn.disabled).toBe(true);
  });

  it('400 with {"error":"text"} renders banner text', async () => {
    global.fetch = vi.fn(async (url: unknown, init?: RequestInit) => {
      if (String(url).endsWith('/internal/shepherd/config')) {
        return jsonResponse({ error: 'config file is read-only' }, 400);
      }
      return jsonResponse(payload());
    });

    component = mount(ShepherdPanel, { target: document.body });
    flushSync();
    shepherd.refresh();
    await vi.waitFor(() => expect(shepherd.data?.config.mac_masked).toBe('aa:bb:**:**:**:**'));
    flushSync();

    // Edit and save
    const input = document.querySelector<HTMLInputElement>('#wol-window-seconds')!;
    input.value = '600';
    input.dispatchEvent(new Event('input', { bubbles: true }));
    flushSync();

    document.querySelector<HTMLButtonElement>('.save-btn')!.click();
    await vi.waitFor(() => expect(shepherd.saveBanner).toBe('config file is read-only'));
    flushSync();

    expect(document.body.textContent).toContain('config file is read-only');
  });
});

// --- M3: config_readable === false => dashes, no 0s -----------------------
describe('ShepherdPanel M3: config_readable false shows dashes', () => {
  it('config_readable:false payload renders dashes in config tiles, no 0s', async () => {
    const p = payload({ available: true }) as Record<string, unknown>;
    p.config_readable = false;
    // Unique MAC for store-singleton discriminator
    p.config = { ...(p.config as object), mac_masked: '11:22:**:**:**:**' };
    await mountWith(p);

    const tiles = [...document.querySelectorAll('.tile')];
    const values = tiles.map((t) => t.querySelector('.value')?.textContent?.trim());
    // All four config tiles should show the em-dash
    expect(values).toEqual(['\u2014', '\u2014', '\u2014', '\u2014']);
    // No "0s", "0m", "0h"
    expect(document.body.textContent).not.toMatch(/\b0[smh]\b/);
  });

  it('config_readable undefined (older backend) treats as true: values render', async () => {
    const p = payload() as Record<string, unknown>;
    p.config = { ...(p.config as object), mac_masked: '33:44:**:**:**:**' };
    // No config_readable field at all
    delete p.config_readable;
    await mountWith(p);

    const tiles = [...document.querySelectorAll('.tile')];
    const values = tiles.map((t) => t.querySelector('.value')?.textContent?.trim());
    expect(values).toContain('30m');
    expect(values).toContain('5m');
  });
});
