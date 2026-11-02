<script lang="ts">
  import './app.css';
  import DashboardLayout from './layout/DashboardLayout.svelte';
  import OverviewPanel from './panels/OverviewPanel.svelte';
  import EndpointsPanel from './panels/EndpointsPanel.svelte';
  import ModelsPanel from './panels/ModelsPanel.svelte';
  import { pollScheduler } from './lib/poll-scheduler';
  import { startClock } from './lib/clock.svelte';
  import { navigation } from './lib/stores/navigation.svelte';
  // Overview is always-on (StatusStrip renders on every panel), so App owns
  // its lifecycle. Endpoints/models stores are started/stopped by their panels.
  import { overview } from './lib/stores/overview.svelte';
  import { bootstrap } from './lib/stores/bootstrap.svelte';
  // Module side-effect: registers endpoints/models jobs with the scheduler.
  import './lib/stores/endpoints.svelte';
  import './lib/stores/models.svelte';
  import { startRouter, currentRoute, type Route } from './lib/router';
  import { jumpToEndpointKey } from './lib/jump-to-endpoint';

  // App is the router: it reads the same navigation store NavTabs does,
  // rather than keeping its own separate `current`. Two sources of truth for
  // the active section is exactly how a programmatic jump used to render the
  // right panel while the tab bar kept announcing the old one.

  // Lifecycle: start the shared scheduler on mount, stop on teardown. The
  // scheduler owns every setInterval/setTimeout in the SPA - nothing else
  // should call them directly. The clock registers + activates first;
  // overview is always-on; endpoints
  // and models are activated by their panels' own mount/unmount effects.
  $effect(() => {
    startClock();
    // One-shot manifest fetch BEFORE anything renders against defaults:
    // sections/columns/observe exist only if the backend says they can be
    // honest. load() never rejects; failures keep the sampler defaults.
    const ready = bootstrap.load().then(() => {
      if (bootstrap.observe) bootstrap.observe.start();
    });
    void ready;
    overview.start();
    pollScheduler.start();
    return () => {
      // Only the scheduler is torn down. The theme singleton (and its
      // matchMedia listener) deliberately outlives App: on HMR/unmount
      // nothing re-arms a destroyed listener, and OS-follow died for the
      // whole remaining page life.
      pollScheduler.stop();
    };
  });

  // URL hash routing. On mount, restore the panel (and any targeted endpoint
  // row) from the location hash so a refresh or shared link re-opens the
  // exact view. The hashchange listener then covers back/forward and manual
  // URL edits; pushState (used by tab clicks and jump-to-endpoint) is silent,
  // so it never re-enters this path - that's the idempotency contract between
  // click-driven nav and history-driven restore.
  $effect(() => {
    const restore = (r: Route) => {
      if (r.endpointKey) void jumpToEndpointKey(r.endpointKey);
    };
    const initial = currentRoute();
    navigation.set(initial.panel);
    restore(initial);
    return startRouter(restore);
  });
</script>

<DashboardLayout>
  {#if navigation.current === 'overview'}
    <OverviewPanel />
  {:else if navigation.current === 'endpoints'}
    <EndpointsPanel />
  {:else if navigation.current === 'models'}
    <ModelsPanel />
  {/if}
</DashboardLayout>
