import { describe, it, expect } from 'vitest';
import { statusMeta } from './status';

// The shared status vocabulary: routable-but-degraded states must never
// render with the red "down" language, and an unknown future status must
// default to neutral, not outage.
describe('statusMeta vocabulary', () => {
  it('renders routable degradation neutral, not red', () => {
    expect(statusMeta('busy').cls).toBe('neutral');
    expect(statusMeta('warming').cls).toBe('neutral');
  });
  it('renders real outages red', () => {
    expect(statusMeta('offline').cls).toBe('red');
    expect(statusMeta('critical').cls).toBe('red');
  });
  it('spaced labels for snake_case enum values', () => {
    expect(statusMeta('config_error').label).toBe('config error');
    expect(statusMeta('rate_limited').label).toBe('rate limited');
  });
  it('unknown statuses render neutral with raw text (never red)', () => {
    const m = statusMeta('draining_soon');
    expect(m.cls).toBe('neutral');
    expect(m.label).toBe('draining_soon');
    expect(statusMeta(undefined).label).toBe('unknown');
  });
});
