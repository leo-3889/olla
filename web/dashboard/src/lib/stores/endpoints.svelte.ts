// /internal/status/endpoints poller. Drives EndpointsPanel + Overview's herd glance.
import type { EndpointStatusResponse } from '../types';
import { createPollStore } from './poll-store.svelte';
import { bootstrap } from './bootstrap.svelte';

const ENDPOINTS_INTERVAL_MS = 5000;
const URL = '/internal/status/endpoints';

export const endpoints = createPollStore<EndpointStatusResponse>({
  name: 'endpoints',
  url: URL,
  intervalMs: () => bootstrap.config.poll_ms.endpoints || ENDPOINTS_INTERVAL_MS,
});
