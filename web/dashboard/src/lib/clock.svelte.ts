// Single ticking instant for any component that needs to re-render
// time-derived values (relative "Xs ago", uptime, "in 30s"). Status
// surfaces render absolute instants from the API as relative strings, and
// those go stale every second; rather than give each panel its own
// setInterval, this module is the one shared clock - the poll scheduler
// owns all timers in the SPA. Reads via now() are reactive when consumed
// inside $derived.

import { pollScheduler } from './poll-scheduler';

const CLOCK_INTERVAL_MS = 1000;
const CLOCK_JOB_NAME = 'clock';

let now = $state(Date.now());

// Browser-to-server clock offset in ms (server timestamp - local Date.now()
// at the last successful poll). Relative labels everywhere ("Xs ago",
// "in Xs", uptime) measure SERVER-issued instants; against a skewed browser
// clock every label shifted by the skew. getNow() adds this offset so the
// shared clock is server-truth within poll latency.
let skewMs = 0;

function tick(): void {
  now = Date.now() + skewMs;
}

/** Align the shared clock with the server: pass the payload's top-level
 *  `timestamp`. Clamped to +/-15 min so a malformed/garbage timestamp can
 *  never teleport the clock and freeze the labels. */
export function setServerSkew(serverTimestampIso: string | undefined | null): void {
  if (serverTimestampIso === null) {
    skewMs = 0;
    now = Date.now();
    return;
  }
  if (!serverTimestampIso) return;
  const t = Date.parse(serverTimestampIso);
  if (Number.isNaN(t)) return;
  // Measure against the clock's own LOCAL base (now - current skew), not a
  // fresh Date.now(): mocked/frozen clocks (tests) and 1 s tick granularity
  // cancel out, and only the server-vs-local difference lands in skewMs.
  const local = now - skewMs;
  const d = t - local;
  skewMs = Math.max(-15 * 60_000, Math.min(15 * 60_000, d));
  now = local + skewMs;
}

let registered = false;
export function startClock(): void {
  if (registered) return;
  registered = true;
  pollScheduler.register(CLOCK_JOB_NAME, CLOCK_INTERVAL_MS, tick);
  // The clock is always-on: activate the job directly. It must NOT be subject
  // to per-panel start/stop - every panel's relative-time labels rely on it
  // ticking once per second regardless of which data store is polling.
  pollScheduler.start(CLOCK_JOB_NAME);
  // Kick once immediately so the first paint isn't up to 1s stale.
  tick();
}

/** Live "now" in epoch ms; read inside $derived so panels re-render per tick. */
export function getNow(): number {
  return now;
}
