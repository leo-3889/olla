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
  // Feeds the per-endpoint trend rings (endpoints-page chart). Successful
  // ticks only; see endpoint-history.svelte.ts for the gap semantics.
  onTick: (data, ok) => sampleEndpoints(data, ok),
});
