<script lang="ts">
  import { api } from './api';
  import StatusBadge from '$lib/StatusBadge.svelte';
  import { Button } from '$lib/components/ui/button/index';
  import * as Table from '$lib/components/ui/table/index';

  // Browse OneLake, then preview a table. docs/44 calls this the largest genuine
  // UI gap: /portal/table could already preview a table reached from the flow
  // graph, and nothing let you FIND one.
  //
  // Read-only by construction. Everything here is stored state rendered back,
  // which is what keeps it on the right side of docs/44's thin/thick line — a
  // writable data editor would be a client re-implementation, and users would
  // then develop against a path no real Fabric client takes.
  let lakehouses = $state<any[]>([]);
  let error = $state('');
  let open = $state<string | null>(null);
  let preview = $state<any>(null);
  let previewOf = $state('');
  let previewError = $state('');

  function load() {
    api.get('/_emulator/portal/lakehouses')
      .then((r) => (lakehouses = r.lakehouses ?? []))
      .catch((e) => (error = e.message));
  }
  load();

  function toggle(id: string) {
    open = open === id ? null : id;
  }

  function show(itemId: string, table: string) {
    previewOf = `${itemId}/${table}`;
    preview = null;
    previewError = '';
    api
      .get(
        `/_emulator/portal/table?itemId=${encodeURIComponent(itemId)}&table=${encodeURIComponent(table)}`,
      )
      .then((t) => (preview = t))
      .catch((e) => (previewError = e.message));
  }
</script>

<h1>Lakehouses</h1>
<p class="muted">
  What OneLake actually holds, read straight from the store. Tables are derived
  from paths rather than directory rows, so a table written by delta-rs — which
  creates no directories — still appears. Read-only: authoring belongs to real
  tools (Spark, dbt, the SDKs).
</p>
<Button variant="outline" size="sm" onclick={load}>Refresh</Button>
{#if error}<p class="error">{error}</p>{/if}

{#if lakehouses.length === 0}
  <p class="muted">
    No lakehouses yet — create one via
    <code>POST /v1/workspaces/{'{wid}'}/lakehouses</code>.
  </p>
{:else}
  {#each lakehouses as lh (lh.itemId)}
    <section class="lh">
      <h2>
        {lh.name}
        <span class="mono muted">{lh.workspace}</span>
        {#if lh.schemaEnabled}<StatusBadge tone="muted">schema-enabled</StatusBadge>{/if}
      </h2>
      <p class="muted mono">{lh.itemId}</p>

      <h3>Tables ({lh.tables.length})</h3>
      {#if lh.tables.length === 0}
        <p class="muted">No Delta tables yet.</p>
      {:else}
        <Table.Root>
          <Table.Body>
            {#each lh.tables as t (t)}
              <Table.Row>
                <Table.Cell class="mono">{t}</Table.Cell>
                <Table.Cell>
                  <Button variant="outline" size="sm" onclick={() => show(lh.itemId, `Tables/${t}`)}>
                    Preview
                  </Button>
                </Table.Cell>
              </Table.Row>
            {/each}
          </Table.Body>
        </Table.Root>
      {/if}

      <h3>
        Files ({lh.fileCount})
        <Button variant="outline" size="sm" onclick={() => toggle(lh.itemId)}>
          {open === lh.itemId ? 'Hide' : 'Show'}
        </Button>
      </h3>
      {#if open === lh.itemId}
        {#if lh.files.length === 0}
          <p class="muted">No files.</p>
        {:else}
          <ul class="files">
            {#each lh.files as f (f)}<li class="mono">{f}</li>{/each}
          </ul>
          {#if lh.fileCount > lh.files.length}
            <p class="muted">
              Showing {lh.files.length} of {lh.fileCount} — the list is capped, the count is not.
            </p>
          {/if}
        {/if}
      {/if}
    </section>
  {/each}
{/if}

{#if previewOf}
  <section class="preview">
    <h2>Preview <span class="mono muted">{previewOf}</span></h2>
    {#if previewError}<p class="error">{previewError}</p>{/if}
    {#if preview && !preview.readable}
      <!-- Not an error: a path may be a Files entry, or a table whose first
           commit has not landed. The endpoint says which, and so do we. -->
      <p class="muted">Not readable as Delta: {preview.message}</p>
    {:else if preview}
      <p class="muted">
        version {preview.version} · {preview.rowCount} row{preview.rowCount === 1 ? '' : 's'}
        {#if preview.truncated}· showing the first {preview.preview.length}{/if}
      </p>
      <Table.Root>
        <Table.Header>
          <Table.Row>
            {#each preview.columns as c (c)}<Table.Head class="mono">{c}</Table.Head>{/each}
          </Table.Row>
        </Table.Header>
        <Table.Body>
          {#each preview.preview as row, i (i)}
            <Table.Row>
              {#each row as cell, j (j)}<Table.Cell class="mono">{cell}</Table.Cell>{/each}
            </Table.Row>
          {/each}
        </Table.Body>
      </Table.Root>
    {/if}
  </section>
{/if}

<style>
  @reference '../src/app.css';

  .lh {
    @apply mb-6;
  }
  .files {
    @apply ml-4 list-disc text-sm;
  }
  .preview {
    @apply mt-6 border-t pt-4;
  }
</style>
