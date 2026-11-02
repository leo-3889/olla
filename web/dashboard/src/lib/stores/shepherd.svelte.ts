// /internal/shepherd poller. Drives ShepherdPanel (node power / WOL watcher).
//
// The mutation (enable/disable) lives on this store, mirroring the sibling
// panels' "mutation owns the payload the UI shows" contract: a POST returns
// the full status payload, which becomes the store's data immediately so the
// toggle is confirmed from the SERVER response, not a local guess. The next
// poll tick overwrites it - the scheduler's per-job abort already guarantees a
// running tick is superseded, so no extra merge logic is needed.
import type { ShepherdResponse, ShepherdEditable } from '../types';
import { createPollStore } from './poll-store.svelte';
import { bootstrap } from './bootstrap.svelte';

const SHEPHERD_INTERVAL_MS = 15000;
const URL = '/internal/shepherd';
const ENABLED_URL = '/internal/shepherd/enabled';
const CONFIG_URL = '/internal/shepherd/config';

const poll = createPollStore<ShepherdResponse>({
  name: 'shepherd',
  url: URL,
  intervalMs: () => bootstrap.config.poll_ms.models || SHEPHERD_INTERVAL_MS,
});

let busy = $state(false);
let lastMutationError = $state<string | null>(null);
let saving = $state(false);
let saveErrors = $state<Record<string, string> | null>(null);
let dirNotWritable = $state(false);
let saveBanner = $state<string | null>(null);

export interface ShepherdStore {
  readonly name: string;
  readonly data: ShepherdResponse | null;
  readonly status: 'loading' | 'ok' | 'error' | 'stale';
  readonly lastUpdated: Date | null;
  readonly hasData: boolean;
  /** True while an enable/disable POST is in flight. Drives the button's
   *  optimistic-disable: the label stays at the last server truth until the
   *  response lands, so a double-click cannot race two mutations. */
  readonly busy: boolean;
  /** Last mutation failure text (null when clean). Kept separate from the
   *  poll store's `error` (a poll outage) and the payload's `state.last_error`
   *  (the watcher's own failure): three distinct failure surfaces. */
  readonly mutationError: string | null;
  /** True while a config-save POST is in flight. */
  readonly saving: boolean;
  /** Per-field validation errors from a 400 response (null when clean). */
  readonly saveErrors: Record<string, string> | null;
  /** True when the last save failed with 409 (watcher dir not writable). */
  readonly dirNotWritable: boolean;
  /** Non-null when a 400 response carried a generic {"error":"text"} shape (banner). */
  readonly saveBanner: string | null;
  start(): void;
  stop(): void;
  refresh(): void;
  setEnabled(enabled: boolean): Promise<void>;
  saveConfig(payload: ShepherdEditable): Promise<void>;
  revertConfig(): Promise<void>;
}

export const shepherd: ShepherdStore = {
  get name() {
    return poll.name;
  },
  get data() {
    return poll.data;
  },
  get status() {
    return poll.status;
  },
  get lastUpdated() {
    return poll.lastUpdated;
  },
  get hasData() {
    return poll.hasData;
  },
  get busy() {
    return busy;
  },
  get mutationError() {
    return lastMutationError;
  },
  get saving() {
    return saving;
  },
  get saveErrors() {
    return saveErrors;
  },
  get dirNotWritable() {
    return dirNotWritable;
  },
  get saveBanner() {
    return saveBanner;
  },
  start() {
    poll.start();
  },
  stop() {
    poll.stop();
  },
  refresh() {
    poll.refresh();
  },
  async setEnabled(enabled: boolean): Promise<void> {
    busy = true;
    lastMutationError = null;
    try {
      const resp = await fetch(ENABLED_URL, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json', Accept: 'application/json' },
        body: JSON.stringify({ enabled }),
      });
      if (!resp.ok) throw new Error(`HTTP ${resp.status}`);
      const body = (await resp.json()) as ShepherdResponse;
      // Refresh from the response (server truth), not an optimistic flip.
      poll.refreshWith(body);
    } catch (e) {
      lastMutationError = e instanceof Error ? e.message : String(e);
    } finally {
      busy = false;
    }
  },
  async saveConfig(payload: ShepherdEditable): Promise<void> {
    saving = true;
    saveErrors = null;
    saveBanner = null;
    dirNotWritable = false;
    try {
      const resp = await fetch(CONFIG_URL, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json', Accept: 'application/json' },
        body: JSON.stringify(payload),
      });
      if (resp.status === 409) {
        dirNotWritable = true;
        return;
      }
      if (resp.status === 400) {
        const body = (await resp.json()) as { errors?: Record<string, string>; error?: string };
        if (body.errors) {
          saveErrors = body.errors;
        } else if (body.error) {
          saveBanner = body.error;
        }
        return;
      }
      if (!resp.ok) throw new Error(`HTTP ${resp.status}`);
      const body = (await resp.json()) as ShepherdResponse;
      poll.refreshWith(body);
    } catch (e) {
      lastMutationError = e instanceof Error ? e.message : String(e);
    } finally {
      saving = false;
    }
  },
  async revertConfig(): Promise<void> {
    saving = true;
    saveErrors = null;
    saveBanner = null;
    dirNotWritable = false;
    try {
      const resp = await fetch(CONFIG_URL, {
        method: 'DELETE',
        headers: { Accept: 'application/json' },
      });
      if (resp.status === 409) {
        dirNotWritable = true;
        return;
      }
      if (resp.status === 400) {
        const body = (await resp.json()) as { errors?: Record<string, string>; error?: string };
        if (body.errors) {
          saveErrors = body.errors;
        } else if (body.error) {
          saveBanner = body.error;
        }
        return;
      }
      if (!resp.ok) throw new Error(`HTTP ${resp.status}`);
      const body = (await resp.json()) as ShepherdResponse;
      poll.refreshWith(body);
    } catch (e) {
      lastMutationError = e instanceof Error ? e.message : String(e);
    } finally {
      saving = false;
    }
  },
};
