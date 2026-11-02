import { describe, it, expect, vi, afterEach } from 'vitest';
import { getNow, setServerSkew } from './clock.svelte';

// setServerSkew updates the shared instant immediately (no timer needed).
describe('clock server skew', () => {
  afterEach(() => setServerSkew(null)); // reset to local time
  it('applies the server offset', () => {
    const iso = new Date(Date.now() + 60_000).toISOString();
    setServerSkew(iso);
    const d = getNow() - Date.now();
    expect(d).toBeGreaterThanOrEqual(59_000);
    expect(d).toBeLessThanOrEqual(61_000);
  });
  it('clamps absurd offsets to +/-15 minutes', () => {
    setServerSkew(new Date(Date.now() + 86_400_000).toISOString());
    expect(getNow() - Date.now()).toBeLessThanOrEqual(15 * 60_000 + 50);
  });
  it('ignores malformed timestamps', () => {
    setServerSkew(null);
    setServerSkew('garbage');
    expect(Math.abs(getNow() - Date.now())).toBeLessThan(50);
  });
});
