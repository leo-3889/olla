// Single source of truth for how an endpoint/system status renders and sorts.
//
// The keys MUST stay aligned with the backend enum: internal/core/domain/
// endpoint.go defines healthy, degraded, unhealthy, offline, critical,
// busy, warming, unknown, config_error and rate_limited. Overview and
// StatusTag previously kept two different tables, and both silently rendered
// the six Go statuses they did not know (busy, warming, config_error,
// rate_limited, ...) as red "○ <raw>" - a config mistake looked identical to
// an endpoint being down. One map, full enum, neutral rendering for anything
// unknown: never paint an unrecognised state as an outage.

export interface StatusMeta {
  glyph: string;
  /** colour class suffix: green | amber | red | neutral (g-* / st-*) */
  cls: string;
  /** human label; snake_case backend values get spaced out */
  label: string;
}

export const STATUS_META: Record<string, StatusMeta> = {
  healthy: { glyph: '●', cls: 'green', label: 'healthy' },
  degraded: { glyph: '◐', cls: 'amber', label: 'degraded' },
  unhealthy: { glyph: '◐', cls: 'amber', label: 'unhealthy' },
  config_error: { glyph: '◐', cls: 'amber', label: 'config error' },
  rate_limited: { glyph: '◐', cls: 'amber', label: 'rate limited' },
  offline: { glyph: '○', cls: 'red', label: 'offline' },
  critical: { glyph: '○', cls: 'red', label: 'critical' },
  // busy and warming are ROUTABLE (see domain IsRoutable): serving, just not
  // steady-state. Neutral, not amber, and definitely not red.
  busy: { glyph: '◐', cls: 'neutral', label: 'busy' },
  warming: { glyph: '◐', cls: 'neutral', label: 'warming' },
  unknown: { glyph: '○', cls: 'neutral', label: 'unknown' },
};

export function statusMeta(status: string | undefined | null): StatusMeta {
  if (status && STATUS_META[status]) return STATUS_META[status];
  // Unknown value: neutral glyph, raw text. A future backend status must not
  // inherit the "○ red = down" visual language by accident.
  return { glyph: '◐', cls: 'neutral', label: status || 'unknown' };
}

// Unhealthy-first ordering for the glance table: reds, then ambers, then
// green, then everything neutral (busy/warming/unknown/undiscovered) last.
const RANK_BY_CLS: Record<string, number> = { red: 0, amber: 1, green: 2, neutral: 3 };

export function statusRank(status: string | undefined | null): number {
  return RANK_BY_CLS[statusMeta(status).cls] ?? 3;
}
