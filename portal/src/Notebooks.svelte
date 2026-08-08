<script lang="ts">
  import { api } from './api';
  import StatusBadge from '$lib/StatusBadge.svelte';
  import * as Card from '$lib/components/ui/card/index';
  import { href } from './router';
  import NotebookDetail from './NotebookDetail.svelte';

  // One id turns this into the detail page — the Models pattern. Routing lives
  // in App; this component answers to the address it was given.
  let { id = null } = $props();

  let notebooks = $state<any[] | null>(null);
  let error = $state('');

  api
    .get('/_emulator/portal/notebooks')
    .then((r) => (notebooks = r.notebooks ?? []))
    .catch((e) => (error = e.message));

  const plural = (n: number, word: string) => `${n} ${word}${n === 1 ? '' : 's'}`;
</script>

{#if id}
  <NotebookDetail {id} />
{:else}
  <h1>Notebooks</h1>
  <p class="muted">
    The stored definition of every notebook, parsed by the same code an engine
    parses it with. Open one to read its cells and start a run through the
    documented job API. Read-only: authoring belongs to real tools — VS Code,
    Jupyter, or git and <code>fabric-cicd</code>.
  </p>

  {#if error}<p class="error">{error}</p>{/if}

  {#if notebooks && notebooks.length === 0}
    <p class="muted">
      No notebooks yet — create one via
      <code>POST /v1/workspaces/{'{wid}'}/items</code>.
    </p>
  {/if}

  {#each notebooks || [] as nb (nb.itemId)}
    <Card.Root class="mb-2.5 transition-colors hover:bg-muted/40">
      <a class="nb-head" href={href('notebooks', nb.itemId)}>
        <strong>{nb.name}</strong>
        <span class="muted">{nb.workspace}</span>
        {#if nb.hasDefinition}
          <span class="muted summary">
            {plural(nb.cells, 'cell')}, {plural(nb.codeCells, 'code cell')}
          </span>
        {:else}
          <!-- Not the same as a notebook with no cells: a RunNotebook job on
               this one fails NotebookDefinitionInvalid rather than completing,
               so the list says which it is. -->
          <StatusBadge title="created, but no definition has been uploaded yet">
            no definition
          </StatusBadge>
        {/if}
      </a>
    </Card.Root>
  {/each}
{/if}

<style>
  @reference '../src/app.css';

  .nb-head {
    @apply flex w-full items-center gap-2.5 px-3 py-2.5;
  }
  .summary {
    @apply ml-auto text-sm;
  }
</style>
