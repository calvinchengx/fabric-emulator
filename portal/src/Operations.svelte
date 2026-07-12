<script>
  import { api } from './api.js';

  let operations = $state([]);
  let error = $state('');

  function load() {
    api.get('/_emulator/portal/operations')
      .then((o) => (operations = o.value))
      .catch((e) => (error = e.message));
  }
  load();

  function fmt(epoch) {
    return new Date(epoch * 1000).toISOString().replace('T', ' ').slice(0, 19);
  }
</script>

<h1>Operations</h1>
<p class="muted">
  Long-running operations, newest first. Status derives from the emulator
  clock — freeze or advance it on the <a href="#clock">Clock</a> page to hold
  or complete them.
</p>
<button onclick={load}>Refresh</button>
{#if error}<p class="error">{error}</p>{/if}
{#if operations.length === 0}
  <p class="muted">No operations yet — any async mutation on /v1 creates one.</p>
{:else}
  <table>
    <thead><tr><th>Status</th><th>Kind</th><th>Id</th><th>Created</th><th>Result</th></tr></thead>
    <tbody>
      {#each operations as op}
        <tr>
          <td><span class="chip {op.status.toLowerCase()}">{op.status}</span></td>
          <td>{op.kind}</td>
          <td class="mono">{op.id}</td>
          <td class="mono">{fmt(op.createdAt)}</td>
          <td class="mono">{op.resultRef || '—'}</td>
        </tr>
      {/each}
    </tbody>
  </table>
{/if}
