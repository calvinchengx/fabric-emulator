<script lang="ts" module>
  // The portal's one status chip, over shadcn's Badge.
  //
  // WHY A WRAPPER RATHER THAN A BADGE VARIANT. `badge.svelte` is vendored:
  // `shadcn-svelte add` regenerates it, so a `success` variant added to its
  // `tv()` block would be overwritten by the next update. This lives outside
  // `ui/`, is ours, and maps the emulator's status vocabulary onto Badge's
  // `class` — which `cn()` merges, so nothing upstream has to change.
  //
  // The vocabulary is the wire's, not this file's invention: `Completed`,
  // `Failed`, `InProgress`, `NotStarted`, `Cancelled` are job and operation
  // statuses; `streaming`/`connecting` are the flow view's own. Matching is
  // case-insensitive because the API says `Completed` and the CSS said
  // `completed`.
  const TONES: Record<string, string> = {
    succeeded: 'success',
    completed: 'success',
    active: 'success',
    streaming: 'success',
    failed: 'danger',
    deduped: 'danger',
    running: 'caution',
    inprogress: 'caution',
    notstarted: 'caution',
    connecting: 'caution',
    reconnecting: 'caution',
    cancelled: 'muted',
  };

  /** The tone a status word carries, or '' for one that carries none. */
  export function toneOf(status: string | undefined | null): string {
    return TONES[String(status ?? '').toLowerCase().replace(/[\s_-]/g, '')] ?? '';
  }
</script>

<script lang="ts">
  import { Badge } from '$lib/components/ui/badge/index';
  import { cn } from '$lib/utils';

  let {
    /** The status word. Also the label, unless children are given. */
    status = undefined as string | undefined,
    /** Force a tone: 'success' | 'danger' | 'caution' | 'muted' | ''. */
    tone = undefined as string | undefined,
    class: className = undefined as string | undefined,
    children = undefined as any,
    ...rest
  } = $props();

  // The same colours the hand-rolled `.chip` classes carried, so this is a
  // change of mechanism rather than of appearance.
  const PAINT: Record<string, string> = {
    success: 'bg-[var(--success-bg)] text-success border-transparent',
    danger: 'bg-[var(--danger-bg)] text-danger border-transparent',
    caution: 'bg-[var(--caution-bg)] text-caution border-transparent',
    muted: 'bg-muted text-muted-foreground border-transparent',
  };

  const shown = $derived(tone ?? toneOf(status));
  const painted = $derived(PAINT[shown] ?? '');
</script>

<!-- `data-tone` is a deliberate contract, not a debugging leftover: it lets a
     test (or a person with dev tools) ask what a badge MEANS without asserting
     on Tailwind classes, which are an implementation detail that changes when
     the palette does. '' for a badge that carries no status. -->
<Badge
  variant="outline"
  data-tone={shown}
  class={cn('font-mono text-xs whitespace-nowrap', painted, className)}
  {...rest}
>
  {#if children}{@render children()}{:else}{status}{/if}
</Badge>
