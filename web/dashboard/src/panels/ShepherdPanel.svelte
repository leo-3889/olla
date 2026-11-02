<script lang="ts">
  import { shepherd } from '../lib/stores/shepherd.svelte';
  import StatusBanner from '../components/StatusBanner.svelte';
  import StatTile from '../components/StatTile.svelte';
  import { fmtDuration, fmtAgo } from '../lib/format';
  import { getNow as liveNow } from '../lib/clock.svelte';

  // Node power / Wake-on-LAN watcher. Consumes the shepherd contract verbatim:
  // the panel formats, it never interprets. Every empty string renders a dash
  // (the invariants forbid a fabricated "never"/0); a non-empty state.last_error
  // is the one warning row; available=false collapses the whole panel to a
  // single honest "watcher not mounted" cell.

  const data = $derived(shepherd.data);
  const loading = $derived(shepherd.status === 'loading' && data === null);

  // This panel owns the shepherd store's poll lifecycle (siblings' pattern):
  // start fires an immediate tick on mount, stop deactivates + aborts on
  // unmount so a non-visible panel stops polling.
  $effect(() => {
    shepherd.start();
    return () => shepherd.stop();
  });

  const now = $derived(liveNow());

  // Empty/absent -> dash (the "never fake a value" invariant). Non-empty ->
  // verbatim.
  function text(v: string | null | undefined): string {
    return v && v.trim() ? v : '\u2014';
  }

  // Instant -> "Xs ago" (fmtAgo returns "" for empty/malformed so callers
  // choose the placeholder; here the honest placeholder is a dash, not "never",
  // because a blank last_* field is "not known", not "has never happened").
  function ago(iso: string | null | undefined): string {
    const s = fmtAgo(iso, now);
    return s || '\u2014';
  }

  // Seconds -> coarse human form ("30m", "2h") mirroring the shared
  // fmtDuration, so the idle timeout reads like every other duration in the
  // dashboard. Non-finite/negative -> dash, never a 0 that implies "immediately".
  function humanize(seconds: number | null | undefined): string {
    if (data?.config_readable === false) return '\u2014';
    if (typeof seconds !== 'number' || !Number.isFinite(seconds) || seconds < 0) return '\u2014';
    return fmtDuration(seconds * 1000);
  }

  const enabled = $derived(data?.enabled === true);
  const nextEnabled = $derived(!enabled);

  async function toggle(): Promise<void> {
    await shepherd.setEnabled(nextEnabled);
  }

  // --- Wake triggers editable sub-block ------------------------------------

  // Local form state seeded from data.editable; reset whenever a fresh payload
  // lands (poll tick, mutation response).
  let formWindow = $state<number>(0);
  let formKeepAsleep = $state<string[]>([]);

  $effect(() => {
    if (data?.editable) {
      formWindow = Number.isFinite(data.editable.wake_window_seconds)
        ? data.editable.wake_window_seconds
        : 0;
      formKeepAsleep = [...(data.editable.keep_asleep_if_up ?? [])];
    }
  });

  const formValid = $derived(Number.isFinite(formWindow) && formWindow >= 0);
  const isDirty = $derived(
    data?.editable != null &&
    (formWindow !== data.editable.wake_window_seconds ||
     formKeepAsleep.length !== (data.editable.keep_asleep_if_up ?? []).length ||
     formKeepAsleep.some((v, i) => v !== (data.editable.keep_asleep_if_up ?? [])[i]))
  );

  // Revert to YAML defaults: POST the defaults object as-is to /internal/shepherd/config.
  // Contract: server accepts the same editable-shape payload; defaults are the
  // canonical "undo all overrides" target.
  async function save(): Promise<void> {
    await shepherd.saveConfig({
      wake_window_seconds: formWindow,
      keep_asleep_if_up: formKeepAsleep,
    });
  }

  async function revertToDefaults(): Promise<void> {
    await shepherd.revertConfig();
  }

  // Tag-list editor for keep_asleep_if_up
  let tagInput = $state('');
  const constraints = $derived(data?.constraints);
  const pattern = $derived(constraints ? new RegExp(constraints.keep_asleep_if_up.pattern) : null);
  const maxItems = $derived(constraints?.keep_asleep_if_up.max_items ?? Infinity);

  const tagPatternOk = $derived(!pattern || pattern.test(tagInput));
  const tagMaxOk = $derived(formKeepAsleep.length < maxItems);
  const tagCanAdd = $derived(tagInput.trim() !== '' && tagPatternOk && tagMaxOk);

  function addTag(): void {
    if (!tagCanAdd) return;
    formKeepAsleep = [...formKeepAsleep, tagInput.trim()];
    tagInput = '';
  }

  function removeTag(idx: number): void {
    formKeepAsleep = formKeepAsleep.filter((_, i) => i !== idx);
  }

  // Override labelling: field value differs from YAML default
  function isOverride(field: 'wake_window_seconds' | 'keep_asleep_if_up'): boolean {
    if (!data?.editable || !data.defaults) return false;
    if (field === 'wake_window_seconds') return data.editable.wake_window_seconds !== data.defaults.wake_window_seconds;
    return JSON.stringify(data.editable.keep_asleep_if_up) !== JSON.stringify(data.defaults.keep_asleep_if_up);
  }
</script>

<div class="shepherd-section">
  <StatusBanner store={shepherd} />

  <!-- data-state on the inner wrapper, not the panel root: opacity/filter on
       an ancestor dim the StatusBanner's retry affordance (siblings' rule). -->
  <div
    class="panel-data"
    data-state={shepherd.status === 'error' || shepherd.status === 'stale' ? shepherd.status : null}
  >
    {#if loading}
      <div class="skeleton tile-skel" style="margin:6px 0"></div>
      {#each Array(4) as _, i}<div class="skeleton row-skel" style="margin:6px 0"></div>{/each}
    {:else if data && !data.available}
      <!-- The single honest state when the watcher is not mounted: a dashed
           cell. Never zeros, never a fake "disabled" (a watcher that is not
           mounted has no enabled state to report). -->
      <div class="watcher-missing" role="status">watcher not mounted</div>
    {:else if data}
      <div class="shepherd-head">
        <span class="status-text" class:on={enabled}>{enabled ? 'enabled' : 'disabled'}</span>
        {#if data.dry_run}<span class="chip warn" title="Dry run: the watcher logs intended actions but does not touch power">dry run</span>{/if}
        <button
          type="button"
          class="theme-toggle toggle-btn"
          onclick={toggle}
          disabled={shepherd.busy}
          aria-live="polite"
        >
          {shepherd.busy ? 'saving\u2026' : enabled ? 'Disable' : 'Enable'}
        </button>
      </div>

      {#if shepherd.saveBanner}
        <div class="mutation-error" role="alert">
          <span class="glyph g-amber" aria-hidden="true">◐</span>
          {shepherd.saveBanner}
        </div>
      {/if}
      {#if shepherd.mutationError}
        <div class="mutation-error" role="alert">
          <span class="glyph g-amber" aria-hidden="true">◐</span>
          Couldn't update shepherd: {shepherd.mutationError}
        </div>
      {/if}

      <div class="section">
        <div class="section-head"><h2>Configuration</h2></div>
        <div class="tile-grid">
          <StatTile label="Node" value={data.config_readable === false ? '\u2014' : text(data.config.node_name)} />
          <StatTile label="MAC" value={data.config_readable === false ? '\u2014' : text(data.config.mac_masked)} sub="masked at source" />
          <StatTile label="Idle shutdown" value={humanize(data.config.idle_shutdown_seconds)} />
          <StatTile label="Wake window" value={humanize(data.config.wake_window_seconds)} />
        </div>
      </div>

      <!-- Wake triggers: editable sub-block -->
      {#if data.editable && data.constraints}
        <div class="section">
          <div class="section-head"><h2>Wake triggers</h2></div>
          <p class="apply-note">changes apply on the watcher’s next minute</p>

          {#if shepherd.dirNotWritable}
            <div class="mutation-error" role="alert">
              <span class="glyph g-amber" aria-hidden="true">◐</span>
              watcher dir not writable
            </div>
          {/if}

          <div class="field-group">
            <div class="field-row">
              <label class="field-label" for="wol-window-seconds">
                Wake window (seconds)
                {#if isOverride('wake_window_seconds')}
                  <span class="chip override">override</span>
                {:else}
                  <span class="chip yaml-default">yaml default</span>
                {/if}
              </label>
              <div class="field-input-wrap">
                <input
                  id="wol-window-seconds"
                  class="field-input"
                  type="number"
                  inputmode="numeric"
                  min={data.constraints.wake_window_seconds.min}
                  max={data.constraints.wake_window_seconds.max}
                  bind:value={formWindow}
                />
                <span class="human-hint">{humanize(formWindow)}</span>
              </div>
              {#if shepherd.saveErrors?.wake_window_seconds}
                <span class="field-error" role="alert">{shepherd.saveErrors.wake_window_seconds}</span>
              {/if}
            </div>

            <div class="field-row">
              <label class="field-label" for="wol-keep-asleep-input">
                Keep asleep if up (patterns)
                {#if isOverride('keep_asleep_if_up')}
                  <span class="chip override">override</span>
                {:else}
                  <span class="chip yaml-default">yaml default</span>
                {/if}
              </label>
              <div class="tag-list" role="list">
                {#each formKeepAsleep as tag, i}
                  <span class="chip tag-chip" role="listitem">
                    {tag}
                    <button type="button" class="tag-remove" onclick={() => removeTag(i)} aria-label="Remove {tag}">×</button>
                  </span>
                {/each}
              </div>
              <div class="tag-input-row">
                <input
                  id="wol-keep-asleep-input"
                  class="field-input"
                  type="text"
                  placeholder="add pattern…"
                  bind:value={tagInput}
                  onkeydown={(e) => { if (e.key === 'Enter') { e.preventDefault(); addTag(); } }}
                />
                <button type="button" class="tag-add-btn" onclick={addTag} disabled={!tagCanAdd} aria-label="Add pattern">Add</button>
              </div>
              {#if tagInput && !tagPatternOk}
                <span class="field-error" role="alert">does not match required pattern</span>
              {/if}
              {#if formKeepAsleep.length >= maxItems}
                <span class="field-hint">max {maxItems} items</span>
              {/if}
              {#if shepherd.saveErrors?.keep_asleep_if_up}
                <span class="field-error" role="alert">{shepherd.saveErrors.keep_asleep_if_up}</span>
              {/if}
            </div>
          </div>

          <div class="save-row">
            <button
              type="button"
              class="theme-toggle save-btn"
              onclick={save}
              disabled={!isDirty || !formValid || shepherd.saving}
            >
              {shepherd.saving ? 'Saving\u2026' : 'Save'}
            </button>
            <button
              type="button"
              class="revert-btn"
              onclick={revertToDefaults}
              disabled={shepherd.saving}
            >
              Revert to YAML defaults
            </button>
          </div>
        </div>
      {/if}

      <div class="section">
        <div class="section-head"><h2>State</h2></div>
        <dl class="state-list">
          <div class="state-row"><dt>Last check</dt><dd>{ago(data.state.last_check)}</dd></div>
          <div class="state-row"><dt>Last action</dt><dd>{text(data.state.last_action)}</dd></div>
          <div class="state-row"><dt>Last wake</dt><dd>{ago(data.state.last_wake)}</dd></div>
          <div class="state-row"><dt>Last shutdown</dt><dd>{ago(data.state.last_shutdown)}</dd></div>
          {#if data.state.last_error}
            <div class="state-row warning" role="alert">
              <dt><span class="glyph g-amber" aria-hidden="true">◐</span> Last error</dt>
              <dd>{data.state.last_error}</dd>
            </div>
          {/if}
        </dl>
      </div>
    {:else}
      <!-- No payload AND not loading: the first fetch failed. The banner is the
           one true story (siblings' rule); never claim "not mounted" here. -->
      <p class="panel-intro">No shepherd status available.</p>
    {/if}
  </div>
</div>

<style>
  .shepherd-head {
    display: flex;
    align-items: center;
    gap: var(--space-3);
    margin-bottom: var(--space-4);
    flex-wrap: wrap;
  }
  .status-text {
    font-size: 1.1rem;
    font-weight: 700;
    color: var(--text-dim);
  }
  .status-text.on {
    color: var(--green);
  }
  .toggle-btn {
    margin-left: auto;
  }
  .toggle-btn:disabled {
    opacity: 0.6;
    cursor: default;
  }
  .chip {
    font-size: 0.72em;
    border: 1px solid var(--border-strong);
    border-radius: 999px;
    padding: 1px 8px;
    color: var(--text-dim);
  }
  .chip.warn {
    color: var(--amber);
    border-color: var(--amber);
  }
  .chip.override {
    color: var(--amber);
    border-color: var(--amber);
  }
  .chip.yaml-default {
    color: var(--text-faint);
    border-color: var(--border);
  }
  .mutation-error {
    display: flex;
    align-items: center;
    gap: var(--space-2);
    color: var(--amber);
    font-size: 0.8rem;
    margin-bottom: var(--space-3);
  }
  .state-list {
    margin: 0;
    display: grid;
    gap: 1px;
    background: var(--border);
    border: 1px solid var(--border);
    border-radius: var(--radius-sm);
    overflow: hidden;
  }
  .state-row {
    display: flex;
    justify-content: space-between;
    gap: var(--space-3);
    background: var(--bg-elevated);
    padding: var(--space-2) var(--space-3);
    font-size: 0.82rem;
  }
  .state-row dt {
    color: var(--text-faint);
    margin: 0;
  }
  .state-row dd {
    margin: 0;
    text-align: right;
    word-break: break-word;
  }
  .state-row.warning dt,
  .state-row.warning dd {
    color: var(--amber);
  }
  .watcher-missing {
    border: 1px dashed var(--border-strong);
    border-radius: var(--radius-sm);
    padding: var(--space-4);
    text-align: center;
    color: var(--text-faint);
    font-size: 0.85rem;
  }
  .apply-note {
    font-size: 0.75rem;
    color: var(--text-faint);
    margin: 0 0 var(--space-3);
    font-style: italic;
  }
  .field-group {
    display: grid;
    gap: var(--space-4);
    margin-bottom: var(--space-4);
  }
  .field-row {
    display: grid;
    gap: var(--space-2);
  }
  .field-label {
    font-size: 0.82rem;
    font-weight: 600;
    color: var(--text-dim);
    display: flex;
    align-items: center;
    gap: var(--space-2);
  }
  .field-input-wrap {
    display: flex;
    align-items: center;
    gap: var(--space-2);
  }
  .field-input {
    background: var(--bg-elevated);
    border: 1px solid var(--border-strong);
    border-radius: var(--radius-sm);
    padding: var(--space-2) var(--space-3);
    color: var(--text);
    font-size: 0.85rem;
    width: 120px;
  }
  .field-input:focus {
    outline: 2px solid var(--accent);
    outline-offset: -1px;
  }
  .human-hint {
    font-size: 0.78rem;
    color: var(--text-faint);
  }
  .field-error {
    color: var(--red);
    font-size: 0.75rem;
  }
  .field-hint {
    color: var(--text-faint);
    font-size: 0.75rem;
  }
  .tag-list {
    display: flex;
    flex-wrap: wrap;
    gap: var(--space-2);
    min-height: 24px;
  }
  .tag-chip {
    display: inline-flex;
    align-items: center;
    gap: 4px;
  }
  .tag-remove {
    background: none;
    border: none;
    color: var(--text-faint);
    cursor: pointer;
    font-size: 1em;
    padding: 0 2px;
    line-height: 1;
  }
  .tag-remove:hover {
    color: var(--red);
  }
  .tag-input-row {
    display: flex;
    gap: var(--space-2);
    align-items: center;
  }
  .tag-add-btn {
    font-size: 0.78rem;
    padding: var(--space-1) var(--space-3);
    border: 1px solid var(--border-strong);
    border-radius: var(--radius-sm);
    background: var(--bg-elevated);
    color: var(--text-dim);
    cursor: pointer;
  }
  .tag-add-btn:disabled {
    opacity: 0.5;
    cursor: default;
  }
  .save-row {
    display: flex;
    gap: var(--space-3);
    align-items: center;
  }
  .save-btn:disabled {
    opacity: 0.5;
    cursor: default;
  }
  .revert-btn {
    font-size: 0.75rem;
    background: none;
    border: none;
    color: var(--text-faint);
    cursor: pointer;
    text-decoration: underline;
    padding: 0;
  }
  .revert-btn:disabled {
    opacity: 0.5;
    cursor: default;
  }
</style>
