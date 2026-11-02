import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';

// Regression coverage for finding 7: refresh(name) (the "retry now" button)
// ran the job immediately without clearing the timer already queued by the
// previous #scheduleNext. The old timer still fired later, and since #run's
// finally unconditionally rescheduled, that produced a second live recurring
// chain - one extra permanent chain per manual retry, with no way to cancel
// the leaked ones.

beforeEach(() => {
  vi.resetModules();
  vi.useFakeTimers();
});

afterEach(() => {
  vi.useRealTimers();
});

describe('pollScheduler dynamic intervals', () => {
  it('re-arms with the resolver value on the next tick (dashboard.polling takes effect live)', async () => {
    const { pollScheduler } = await import('./poll-scheduler');

    let interval = 1000;
    let calls = 0;
    pollScheduler.register('dyn', () => interval, async () => {
      calls++;
    });
    pollScheduler.start();
    pollScheduler.start('dyn');
    await vi.advanceTimersByTimeAsync(0);
    expect(calls).toBe(1);

    // A config change lands; the CURRENT pending timer still uses the old
    // cadence, but the re-arm after this tick must use the new one.
    interval = 10_000;
    await vi.advanceTimersByTimeAsync(1200); // old 1000ms cadence fires
    expect(calls).toBe(2);

    // Nothing on the old cadence any more...
    await vi.advanceTimersByTimeAsync(3000);
    expect(calls).toBe(2);
    // ...and the new cadence ticks.
    await vi.advanceTimersByTimeAsync(11_000);
    expect(calls).toBe(3);

    pollScheduler.stop();
  });
});

describe('pollScheduler.refresh', () => {
  it('a burst of manual refreshes leaves exactly one live recurring chain', async () => {
    const { pollScheduler } = await import('./poll-scheduler');

    let calls = 0;
    const tick = vi.fn(async () => {
      calls++;
    });
    pollScheduler.register('job', 1000, tick);
    pollScheduler.start();
    pollScheduler.start('job');

    // start() fires an immediate tick (delay 0).
    await vi.advanceTimersByTimeAsync(0);
    expect(calls).toBe(1);

    // Three manual "retry now" clicks in quick succession, each completing
    // before the next fires.
    pollScheduler.refresh('job');
    await vi.advanceTimersByTimeAsync(0);
    pollScheduler.refresh('job');
    await vi.advanceTimersByTimeAsync(0);
    pollScheduler.refresh('job');
    await vi.advanceTimersByTimeAsync(0);
    expect(calls).toBe(4);

    // Advance one full interval plus the maximum jitter window. With the bug,
    // each refresh leaked a duplicate chain, so this window would fire 4
    // more ticks (one per leaked chain); fixed, it fires exactly one.
    const before = calls;
    await vi.advanceTimersByTimeAsync(1200);
    expect(calls - before).toBe(1);

    pollScheduler.stop();
  });

  it('a superseded in-flight run does not also schedule a duplicate chain', async () => {
    const { pollScheduler } = await import('./poll-scheduler');

    const resolvers: Array<() => void> = [];
    const tick = vi.fn(
      (signal: AbortSignal) =>
        new Promise<void>((resolve) => {
          resolvers.push(resolve);
          // Mirrors the real tick(): it catches AbortError internally from
          // the fetch and just returns, it never rejects.
          signal.addEventListener('abort', () => resolve());
        })
    );
    pollScheduler.register('job', 1000, tick);
    pollScheduler.start();
    pollScheduler.start('job');

    // Initial run starts; its tick promise is left pending (in-flight).
    await vi.advanceTimersByTimeAsync(0);
    expect(tick).toHaveBeenCalledTimes(1);

    // Refresh while the first tick is still in-flight - this aborts and
    // supersedes it before its own finally ever runs.
    pollScheduler.refresh('job');
    await vi.advanceTimersByTimeAsync(0);

    // Resolve whatever is left pending (the new run's tick).
    resolvers.forEach((r) => r());
    await vi.advanceTimersByTimeAsync(0);

    // Exactly one recurring chain should be alive now.
    const before = tick.mock.calls.length;
    await vi.advanceTimersByTimeAsync(1200);
    expect(tick.mock.calls.length - before).toBe(1);

    pollScheduler.stop();
  });
});

// Per-job start/stop. A job only polls while activated via start(name);
// stop(name) deactivates it and aborts the in-flight tick. The global start()
// /stop() still gates the visibility listener, but no longer schedules every
// registered job - only the active subset.
describe('pollScheduler per-job start/stop', () => {
  it('start(name) activates only that job; the others never fire', async () => {
    const { pollScheduler } = await import('./poll-scheduler');

    const tickA = vi.fn(async () => {});
    const tickB = vi.fn(async () => {});
    pollScheduler.register('a', 1000, tickA);
    pollScheduler.register('b', 1000, tickB);

    pollScheduler.start();
    expect(pollScheduler.isActive('a')).toBe(false);
    expect(pollScheduler.isActive('b')).toBe(false);

    pollScheduler.start('a');
    expect(pollScheduler.isActive('a')).toBe(true);
    expect(pollScheduler.isActive('b')).toBe(false);

    await vi.advanceTimersByTimeAsync(0);
    expect(tickA).toHaveBeenCalledTimes(1);
    expect(tickB).toHaveBeenCalledTimes(0);

    // b is still silent after several intervals.
    await vi.advanceTimersByTimeAsync(3000);
    expect(tickB).toHaveBeenCalledTimes(0);
    expect(tickA.mock.calls.length).toBeGreaterThan(1);

    pollScheduler.stop();
  });

  it('stop(name) deactivates the job, aborts its in-flight tick and halts its chain', async () => {
    const { pollScheduler } = await import('./poll-scheduler');

    const resolvers: Array<() => void> = [];
    const tick = vi.fn(
      (signal: AbortSignal) =>
        new Promise<void>((resolve) => {
          resolvers.push(resolve);
          signal.addEventListener('abort', () => resolve());
        })
    );
    pollScheduler.register('job', 1000, tick);
    pollScheduler.start();
    pollScheduler.start('job');

    // Leave the first tick pending (in-flight), then stop the job.
    await vi.advanceTimersByTimeAsync(0);
    expect(tick).toHaveBeenCalledTimes(1);

    pollScheduler.stop('job');
    expect(pollScheduler.isActive('job')).toBe(false);

    // The aborted in-flight run must resolve (abort listener) but NOT
    // reschedule, and no further ticks may fire.
    resolvers.forEach((r) => r());
    const before = tick.mock.calls.length;
    await vi.advanceTimersByTimeAsync(5000);
    expect(tick.mock.calls.length).toBe(before);

    pollScheduler.stop();
  });

  it('the always-on clock job keeps ticking after a data job is stopped', async () => {
    const { pollScheduler } = await import('./poll-scheduler');

    const clockTicks = vi.fn(() => {});
    const dataTicks = vi.fn(async () => {});
    pollScheduler.register('clock', 1000, clockTicks);
    pollScheduler.register('data', 1000, dataTicks);

    pollScheduler.start();
    pollScheduler.start('clock');
    pollScheduler.start('data');

    await vi.advanceTimersByTimeAsync(0);
    // Both fire once on activation.
    expect(clockTicks).toHaveBeenCalledTimes(1);
    expect(dataTicks).toHaveBeenCalledTimes(1);

    // Stop the data job only; the clock must continue.
    pollScheduler.stop('data');
    expect(pollScheduler.isActive('clock')).toBe(true);
    expect(pollScheduler.isActive('data')).toBe(false);

    const clockBefore = clockTicks.mock.calls.length;
    const dataBefore = dataTicks.mock.calls.length;
    await vi.advanceTimersByTimeAsync(3000);

    expect(clockTicks.mock.calls.length).toBeGreaterThan(clockBefore);
    expect(dataTicks.mock.calls.length).toBe(dataBefore);

    pollScheduler.stop();
  });

  it('start(name) before global start() defers the first tick until start() runs', async () => {
    const { pollScheduler } = await import('./poll-scheduler');

    const tick = vi.fn(async () => {});
    pollScheduler.register('job', 1000, tick);

    // Activate before the scheduler is globally started: job is marked
    // active but does not fire yet.
    pollScheduler.start('job');
    expect(pollScheduler.isActive('job')).toBe(true);
    await vi.advanceTimersByTimeAsync(5000);
    expect(tick).toHaveBeenCalledTimes(0);

    // Global start kicks every active job immediately.
    pollScheduler.start();
    await vi.advanceTimersByTimeAsync(0);
    expect(tick).toHaveBeenCalledTimes(1);

    pollScheduler.stop();
  });
});

// The hidden-tab policy is half of the polling budget: jobs slow down 4x
// (BACKGROUND_BACKOFF) while the document is hidden and fire an immediate
// burst the moment it becomes visible again. Nothing pinned either behaviour,
// so a refactor could just as well have kept full foreground cadence at
// 4x the battery/network cost - or lost the burst and left a returning user
// staring at stale data for a full (already throttled) interval.
describe('pollScheduler background backoff and visibility burst', () => {
  it('throttles hidden jobs to 4x interval and bursts immediately on return to visible', async () => {
    const visDesc = Object.getOwnPropertyDescriptor(document, 'visibilityState');
    let hidden = true;
    Object.defineProperty(document, 'visibilityState', {
      configurable: true,
      get: () => (hidden ? 'hidden' : 'visible'),
    });
    try {
      const { pollScheduler } = await import('./poll-scheduler');

      let calls = 0;
      const tick = vi.fn(async () => {
        calls++;
      });
      pollScheduler.register('job', 1000, tick);
      pollScheduler.start();
      pollScheduler.start('job');

      await vi.advanceTimersByTimeAsync(0);
      expect(calls).toBe(1); // immediate tick on activation

      // Hidden: cadence is 1000 * BACKGROUND_BACKOFF(4) = 4000ms (+/- jitter).
      // A full foreground interval plus margin must fire NOTHING...
      const before = calls;
      await vi.advanceTimersByTimeAsync(1500);
      expect(calls - before).toBe(0);
      // ...while the backoff window (plus its widest jitter) fires exactly one.
      await vi.advanceTimersByTimeAsync(3500); // total 5000 > 4000 * 1.1
      expect(calls - before).toBe(1);

      // Return to the foreground: immediate burst, no wait for the timer.
      hidden = false;
      const atReturn = calls;
      document.dispatchEvent(new Event('visibilitychange'));
      await vi.advanceTimersByTimeAsync(0);
      expect(calls - atReturn).toBe(1);

      // And the job is back on foreground cadence: one tick per interval.
      const afterBurst = calls;
      await vi.advanceTimersByTimeAsync(1500);
      expect(calls - afterBurst).toBe(1);

      pollScheduler.stop();
    } finally {
      if (visDesc) Object.defineProperty(document, 'visibilityState', visDesc);
      else delete (document as unknown as Record<string, unknown>).visibilityState;
    }
  });
});
