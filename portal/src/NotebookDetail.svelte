<script lang="ts">
  import { api } from './api';
  import StatusBadge from '$lib/StatusBadge.svelte';
  import { Button } from '$lib/components/ui/button/index';

  let { id } = $props();

  let nb = $state<any>(null);
  let error = $state('');
  let run = $state<any>(null);
  let runError = $state('');
  let jobId = $state('');
  let starting = $state(false);

  function load() {
    api
      .get(`/_emulator/portal/notebooks/${encodeURIComponent(id)}`)
      .then((r) => (nb = r))
      .catch((e) => (error = e.message));
  }
  load();

  // Starting a run is the ONLY write this view makes, and it is the documented
  // job — the same POST a pipeline or notebookutils makes, minus the token the
  // portal deliberately has none of. Progress is then read back, never
  // simulated here.
  function start() {
    starting = true;
    run = null;
    runError = '';
    api
      .post(`/_emulator/portal/notebooks/${encodeURIComponent(id)}/run`)
      .then((r) => {
        jobId = r.jobId;
        return refreshRun();
      })
      .catch((e) => (runError = e.message))
      .finally(() => (starting = false));
  }

  function refreshRun() {
    if (!jobId) return Promise.resolve();
    return api
      .get(`/_emulator/portal/notebooks/runs/${encodeURIComponent(jobId)}`)
      .then((r) => (run = r))
      .catch((e) => (runError = e.message));
  }

  const cellStatus = (i: number) =>
    run?.cells?.find((c: any) => c.index === i)?.status ?? '';

  const tone = (s: string) =>
    s === 'Completed' ? 'success' : s === 'Failed' ? 'danger' : 'muted';

  // Code cells are re-sequenced 0..n in the run record because an engine
  // iterates by index and markdown leaves no gap. To line a run status up with
  // the cell a reader is looking at, the same re-sequencing has to happen here.
  let codeIndex = $derived.by(() => {
    const map = new Map<number, number>();
    let n = 0;
    for (const c of nb?.cells ?? []) {
      if (c.kind === 'code') map.set(c.index, n++);
    }
    return map;
  });
</script>

<h1>{nb?.name ?? 'Notebook'}</h1>
<p class="muted">
  <a href="#notebooks">← All notebooks</a>
</p>

{#if error}<p class="error">{error}</p>{/if}

{#if nb}
  <p class="muted mono">{nb.itemId}</p>

  {#if !nb.readable}
    <p class="muted">Nothing to show: {nb.message}</p>
  {:else}
    <div class="runbar">
      <Button variant="outline" size="sm" onclick={start} disabled={starting}>
        {starting ? 'Starting…' : 'Run'}
      </Button>
      {#if jobId}
        <Button variant="outline" size="sm" onclick={refreshRun}>Refresh run</Button>
        <span class="muted mono">{jobId}</span>
      {/if}
    </div>

    {#if !nb.runsHere}
      <!-- Said BEFORE the button is pressed. With no Spark agent in the stack a
           RunNotebook job with cells parks deliberately (startJob sets its
           completion beyond any clock) rather than going green with nothing
           executed. A button that quietly produced such a job would be the same
           lie in a new place. -->
      <p class="muted warn">
        No Spark engine is attached, so a run will start and then wait: its cells
        stay Pending until an engine reports through
        <code>notebookRunResult</code>. Bring one up with
        <code>make up</code>.
      </p>
    {/if}

    {#if runError}<p class="error">{runError}</p>{/if}
    {#if run}
      <p class="muted">
        Run status: <StatusBadge tone={tone(run.status)}>{run.status}</StatusBadge>
      </p>
    {/if}

    {#each nb.cells as cell (cell.index)}
      <section class="cell">
        <div class="cell-head">
          <span class="muted mono">{cell.kind}</span>
          {#if cell.language}<span class="muted mono">{cell.language}</span>{/if}
          {#if cell.parameters}
            <StatusBadge title="Fabric adds a caller's overrides beneath this cell">
              parameters
            </StatusBadge>
          {/if}
          {#if cell.kind === 'code' && cellStatus(codeIndex.get(cell.index) ?? -1)}
            {@const s = cellStatus(codeIndex.get(cell.index) ?? -1)}
            <StatusBadge tone={tone(s)}>{s}</StatusBadge>
          {/if}
        </div>
        <pre class="src">{cell.source}</pre>
      </section>
    {/each}
  {/if}
{/if}

<style>
  @reference '../src/app.css';

  .runbar {
    @apply mb-3 flex items-center gap-2;
  }
  .warn {
    @apply mb-3 text-sm;
  }
  .cell {
    @apply mb-3 rounded-md border;
  }
  .cell-head {
    @apply flex items-center gap-2 border-b px-3 py-1.5 text-xs;
  }
  .src {
    @apply overflow-x-auto px-3 py-2 font-mono text-xs whitespace-pre-wrap;
  }
</style>
