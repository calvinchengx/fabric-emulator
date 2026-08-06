<script lang="ts">
  import { api } from './api';
  import StatusBadge from '$lib/StatusBadge.svelte';
  import { Badge } from '$lib/components/ui/badge/index';
  import * as Card from '$lib/components/ui/card/index';
  import { href } from './router';
  import ModelDetail from './ModelDetail.svelte';

  // One id turns this into the detail page. Routing lives in App; this
  // component just answers to the address it was given.
  let { id = null } = $props();

  let models = $state<any>(null);
  let error = $state('');

  api
    .get('/_emulator/portal/models')
    .then((r) => (models = r.value || []))
    .catch((e) => (error = e.message));

  // Pluralised through ONE helper rather than three inline ternaries. Two of
  // the three had one and `measures` did not, so a model with a single measure
  // read "1 measures" — invisible until a second model with exactly one turned
  // up, because the only model anyone had ever looked at has three.
  const plural = (n: number, word: string) => `${n} ${word}${n === 1 ? '' : 's'}`;

  const summary = (m: any) =>
    [
      plural(m.tables.length, 'table'),
      plural(m.tables.reduce((n: number, t: any) => n + t.measures.length, 0), 'measure'),
      plural(m.relationships.length, 'relationship'),
    ].join(', ');
</script>

{#if id}
  <ModelDetail {id} />
{:else}
  <h1>Semantic models</h1>
  <p class="muted">
    What each published model contains. Open one for its tables, their columns,
    every measure's DAX, and what each table is bound to — at its own address,
    so it can be linked to and shared.
  </p>

  {#if error}<p class="error">{error}</p>{/if}

  {#if models && models.length === 0}
    <p class="muted">No semantic models published yet.</p>
  {/if}

  {#each models || [] as m (m.itemId)}
    <!-- A LINK, not a button. It has a URL, so it must be openable in a new
         tab, copyable, and reachable by the browser's own history — which is
         the entire reason this stopped being an accordion. -->
    <Card.Root class="mb-2.5 transition-colors hover:bg-muted/40">
      <a class="model-head" href={href('models', m.itemId)}>
        <strong>{m.displayName}</strong>
        <span class="muted">{m.workspace}</span>
        {#if m.error}
          <StatusBadge tone="danger" title={m.error}>unreadable</StatusBadge>
        {:else}
          <Badge variant="outline" class="font-mono text-xs">{m.format}</Badge>
          {#if m.rowsLoaded}
            <StatusBadge tone="success" title="an inline data.json snapshot is present">
              rows loaded
            </StatusBadge>
          {:else}
            <StatusBadge title="no data.json — an import model with no rows answers every query with nothing">
              no rows
            </StatusBadge>
          {/if}
          <span class="muted summary">{summary(m)}</span>
        {/if}
      </a>
    </Card.Root>
  {/each}
{/if}

<style>
  .model { border: 1px solid var(--border); border-radius: 8px; margin-bottom: 10px; }
  .model-head {
    display: flex; align-items: center; gap: 10px; width: 100%;
    padding: 10px 12px; background: none; border: 0; cursor: pointer;
    font: inherit; color: inherit; text-align: left;
  }
  .caret { display: inline-block; transition: transform 0.12s; color: var(--muted); }
  .caret.open { transform: rotate(90deg); }
  .summary { margin-left: auto; }
  .detail { padding: 0 12px 12px 12px; }
  .meta { display: flex; gap: 14px; align-items: baseline; margin: 2px 0 12px 22px; }
  .mono { font-family: ui-monospace, monospace; font-size: 11px; }
  .tbl { margin: 0 0 14px 22px; }
  .tbl-head { display: flex; align-items: center; gap: 8px; margin-bottom: 4px; }
  .binding { font-size: 12px; }
  .chip.directlake { background: color-mix(in srgb, var(--accent) 18%, transparent); }
  .measures { margin-top: 6px; }
  .dax { white-space: pre-wrap; word-break: break-word; }
  table { width: 100%; }
</style>
