<script lang="ts">
  import type { Snippet } from 'svelte';
  import Header from './Header.svelte';
  import StatusStrip from './StatusStrip.svelte';
  import NavTabs from './NavTabs.svelte';
  import SparkStrip from '../components/SparkStrip.svelte';
  import { bootstrap } from '../lib/stores/bootstrap.svelte';

  // One glance at the footer must answer "which metrics backend is this
  // process actually serving, and how often does it poll" - the answer used
  // to be buried in per-cell tooltips (and invisible on a token-fallback
  // boot, where config said influx while the sampler answered).
  const badge = $derived.by(() => {
    const c = bootstrap.config;
    if (!c) return null;
    const p = c.poll_ms;
    return `metrics: ${c.metrics_source} · polls ${Math.round(p.overview / 1000)}s/${Math.round(p.endpoints / 1000)}s/${Math.round(p.models / 1000)}s/${Math.round(p.observe / 1000)}s`;
  });

  let { children }: { children: Snippet<[]> } = $props();
</script>

<header class="site-header">
  <Header />
  <StatusStrip />
  <SparkStrip />
  <NavTabs />
</header>

<main>
  {@render children?.()}
</main>

<footer>
  <span>Olla is a local, self-hosted proxy. This view never leaves your network.</span>
  {#if badge}<span class="footer-badge" title="Effective metrics backend, then overview/endpoints/models/observe poll intervals">{badge}</span>{/if}
  <span>Olla by <a class="footer-link" href="https://tensorfoundry.io" target="_blank" rel="noopener noreferrer">TensorFoundry</a></span>
</footer>
