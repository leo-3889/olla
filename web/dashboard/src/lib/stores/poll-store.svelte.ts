// Shared factory for the three poll stores. Each store wraps a single endpoint
// and exposes the same getter shape (data/status/lastUpdated/error) so the
// panels can render states uniformly. The status state machine lives here:
// loading -> ok on first success; error/stale on failure, with the last
// known-good data preserved and greyed rather than blanked.

import type { PollStatus } from '../types';
import { pollScheduler, STALE_MULTIPLIER, BACKGROUND_BACKOFF } from '../poll-scheduler';

// Hard ceiling on a single tick. The scheduler only re-arms after tick()
// resolves, so a fetch that hangs forever (server alive but wedged) would
// freeze this store in its last state indefinitely while the header keeps
// claiming the data is live. A blown deadline is reported as a normal failure
// tick (error -> stale escalation like any other). 3x the poll interval,
// floored at 5s so a very short interval cannot time out a slow-but-healthy
// server on every tick.
interface TickSignal {
  /** signal the fetch must use (supersede signal OR deadline, whichever fires) */
  signal: AbortSignal;
  /** true iff the deadline - not the scheduler - killed the tick */
  timedOut(): boolean;
}

function withDeadline(signal: AbortSignal, intervalMs: number): TickSignal {
  if (typeof AbortSignal === 'undefined' || typeof AbortSignal.timeout !== 'function') {
    return { signal, timedOut: () => false };
  }
  const deadline = AbortSignal.timeout(Math.max(5_000, intervalMs * 3));
  const timedOut = () => deadline.aborted && !signal.aborted;
  if (typeof AbortSignal.any === 'function') {
    return { signal: AbortSignal.any([signal, deadline]), timedOut };
  }
  // No AbortSignal.any (older engines/jsdom): link both sources into one
  // controller manually. Silently DROPPING the deadline here would recreate
  // the hang-forever bug exactly where the feature is least expected.
  const ctrl = new AbortController();
  if (signal.aborted || deadline.aborted) ctrl.abort();
  signal.addEventListener('abort', () => ctrl.abort(), { once: true });
  deadline.addEventListener('abort', () => ctrl.abort(), { once: true });
  return { signal: ctrl.signal, timedOut };
}

export interface PollStoreOptions<T> {
  /** scheduler job name */
  name: string;
  /** absolute URL to fetch */
  url: string;
  /** foreground poll interval */
  intervalMs: number;
  /** Fires on every completed tick, success or failure. ok=true for 200/304
   *  (data is the parsed body on 200, null on 304); ok=false on failure.
   *  Optional and additive, so stores that don't care (endpoints/models) are
   *  unaffected. The overview store uses this to push per-tick samples into
   *  the history ring buffer, including failed ticks so the chart can render
   *  outage markers. */
  onTick?: (data: T | null, ok: boolean) => void;
}

export interface PollStore<T> {
  readonly name: string;
  readonly intervalMs: number;
  readonly data: T | null;
  readonly status: PollStatus;
  readonly error: Error | null;
  readonly lastUpdated: Date | null;
  readonly hasData: boolean;
  /** Activate this store's scheduler job and fire an immediate tick. Pair
   *  with stop() on panel unmount so inactive panels stop polling. */
  start(): void;
  /** Deactivate this store's job, clear its timer and abort any in-flight tick. */
  stop(): void;
  refresh(): void;
}

export function createPollStore<T>(opts: PollStoreOptions<T>): PollStore<T> {
  const { name, url, intervalMs, onTick } = opts;

  let data: T | null = $state(null);
  let status: PollStatus = $state('loading');
  let error: Error | null = $state(null);
  let lastUpdated: Date | null = $state(null);
  let lastSuccessAt: number = $state(0); // epoch ms, for stale detection
  let etag: string | null = $state(null); // for If-None-Match / 304 handling

  async function tick(signal: AbortSignal): Promise<void> {
    const headers: Record<string, string> = { Accept: 'application/json' };
    if (etag) headers['If-None-Match'] = etag;

    const tickSignal = withDeadline(signal, intervalMs);

    let resp: Response;
    try {
      resp = await fetch(url, { headers, signal: tickSignal.signal, cache: 'no-store' });
    } catch (e) {
      if (signal.aborted) return; // superseded by a newer tick / stopped
      if (tickSignal.timedOut()) {
        onFailure(new Error('status API did not respond in time'));
        return;
      }
      if (e instanceof Error && e.name === 'AbortError') return;
      onFailure(e instanceof Error ? e : new Error(String(e || 'network error')));
      return;
    }

    if (resp.status === 304) {
      // Body unchanged. The ETag is deliberately stable when the model set
      // hasn't changed, so the panel keeps the last known-good data and the
      // success clock advances.
      const newEtag = resp.headers.get('ETag');
      if (newEtag) etag = newEtag;
      onSuccess(null);
      return;
    }

    if (!resp.ok) {
      onFailure(new Error(`HTTP ${resp.status}`));
      return;
    }

    let body: T;
    try {
      body = (await resp.json()) as T;
    } catch {
      // Aborting a fetch errors the body stream too, so a supersede/stop that
      // lands mid-body arrives HERE. Without the abort guards a "Retry now"
      // click or a tab-return during a slow response surfaces a bogus
      // "invalid JSON" banner over perfectly good cached data.
      if (signal.aborted) return;
      if (tickSignal.timedOut()) {
        onFailure(new Error('status API did not respond in time'));
        return;
      }
      onFailure(new Error('invalid JSON from status API'));
      return;
    }

    const newEtag = resp.headers.get('ETag');
    if (newEtag) etag = newEtag;

    onSuccess(body);
  }

  // Shared by the 200 and 304 branches. A null body (304 path) leaves the
  // cached data untouched but still advances the success clock.
  function onSuccess(body: T | null): void {
    if (body !== null) data = body;
    error = null;
    status = 'ok';
    lastUpdated = new Date();
    lastSuccessAt = Date.now();
    onTick?.(body, true);
  }

  function onFailure(err: Error): void {
    error = err;
    const hasPrior = data !== null;
    const ageMs = Date.now() - lastSuccessAt;
    // Once the last success is older than STALE_MULTIPLIER intervals (several
    // consecutive failures), escalate from error to stale. While the document
    // is hidden the scheduler throttles ticks by BACKGROUND_BACKOFF, so the
    // same number of failed ticks spans 4x longer in wall-clock time; without
    // scaling, the first background failure jumps straight to stale and the
    // error state becomes unreachable the moment you switch tabs.
    const hidden = typeof document !== 'undefined' && document.visibilityState === 'hidden';
    const staleAfterMs = intervalMs * STALE_MULTIPLIER * (hidden ? BACKGROUND_BACKOFF : 1);
    if (hasPrior && ageMs > staleAfterMs) {
      status = 'stale';
    } else {
      // First-ever poll failed or still within the stale window: we want the
      // panel to surface the error rather than sit on a blank skeleton.
      status = 'error';
    }
    onTick?.(null, false);
  }

  // Register with the shared scheduler. Caller (App.svelte) starts it.
  pollScheduler.register(name, intervalMs, tick);

  return {
    get name() {
      return name;
    },
    get intervalMs() {
      return intervalMs;
    },
    get data() {
      return data;
    },
    get status() {
      return status;
    },
    get error() {
      return error;
    },
    get lastUpdated() {
      return lastUpdated;
    },
    /** True if the store has at least one successful poll under its belt. */
    get hasData() {
      return data !== null;
    },
    /** Activate the scheduler job and fire an immediate tick. Called by the
     *  owning panel on mount so a tab switch doesn't sit on stale data. */
    start() {
      pollScheduler.start(name);
    },
    /** Deactivate the scheduler job; in-flight ticks are aborted by the scheduler. */
    stop() {
      pollScheduler.stop(name);
    },
    /** Force an immediate refresh (used by "retry now" buttons). */
    refresh() {
      pollScheduler.refresh(name);
    },
  };
}
