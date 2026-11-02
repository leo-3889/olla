// /internal/status/endpoints poller. Drives EndpointsPanel + Overview's herd glance.
import type { EndpointStatusResponse } from '../types';
import { createPollStore } from './poll-store.svelte';
import { bootstrap } from './bootstrap.svelte';
import { sampleEndpoints } from './endpoint-history.svelte';

const ENDPOINTS_INTERVAL_MS = 5000;
const URL = '/internal/status/endpoints';

export const endpoints = createPollStore<EndpointStatusResponse>({
  name: 'endpoints',
  url: URL,
  intervalMs: () => bootstrap.config.poll_ms.endpoints || ENDPOINTS_INTERVAL_MS,
  // Feeds the per-endpoint trend rings (endpoints-page chart).
  // A 304 tick (ETag cache hit) carries no body, so re-sample the store's
  // retained payload: 304 means the CURRENT values are CONTINUING, and the
  // ring must keep advancing - a stalled ring freezes the chart at one
  // sample for an idle fleet, which is exactly the state a stable ETag
  // produces. Failed ticks (ok=false) add nothing (gap semantics in
  // endpoint-history.svelte.ts).
  onTick: (data, ok) => sampleEndpoints(data ?? endpoints.data, ok),
});
