import { describe, it, expect } from 'vitest';
import { statusMeta } from './status';

// The shared status vocabulary: routable-but-degraded states must never
// render with the red "down" language, and an unknown future status must
// default to neutral, not outage.
describe('statusMeta vocabulary', () => {
  // The enum->class/label table is a direct mirror of status.ts and is not
  // re-pinned here (drift on known statuses is caught by the shared Go enum
  // + app.css drift-guard). What MUST not regress is the DEFAULT branch:
  // an unseen status must never render as a red outage.
  it('unknown statuses render neutral with raw text (never red)', () => {
    const m = statusMeta('draining_soon');
    expect(m.cls).toBe('neutral');
    expect(m.label).toBe('draining_soon');
    expect(statusMeta(undefined).label).toBe('unknown');
  });
});
