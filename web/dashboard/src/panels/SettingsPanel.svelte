<script lang="ts">
  import ShepherdPanel from "./ShepherdPanel.svelte";
</script>

<div
  id="panel-settings"
  class="panel is-active"
  role="tabpanel"
  aria-labelledby="tab-settings"
  tabindex="0"
>
  <section aria-labelledby="settings-heading-shepherd">
    <h2 id="settings-heading-shepherd">Shepherd (node power)</h2>
    <ShepherdPanel />
  </section>
</div>
