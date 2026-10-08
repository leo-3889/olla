<script lang="ts">
  // Single component enforcing "glyph + colour + text" for every status
  // indicator. Never colour alone (colour-blind / greyscale).
  import { statusMeta, type StatusMeta } from '../lib/status';

  interface Props {
    status?: string;
  }
  let { status = 'healthy' }: Props = $props();

  // Mapping lives in lib/status.ts (shared with OverviewPanel and pinned to
  // the full backend enum) - a tag and a glyph for the same endpoint can no
  // longer disagree.
  const meta: StatusMeta = $derived(statusMeta(status));
</script>

<span class="status-tag st-{meta.cls}">
  <span class="glyph g-{meta.cls}" aria-hidden="true">{meta.glyph}</span>{meta.label}
</span>
