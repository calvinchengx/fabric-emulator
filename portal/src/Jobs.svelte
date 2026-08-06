<script lang="ts">
  import { api } from './api';

  let jobs = $state<any[]>([]);
  let error = $state('');

  function load() {
    api.get('/_emulator/portal/jobs')
      .then((j) => (jobs = j.value))
      .catch((e) => (error = e.message));
  }
  load();

  function fmt(epoch: number) {
    return new Date(epoch * 1000).toISOString().replace('T', ' ').slice(0, 19);
  }
</script>

<h1>Jobs</h1>
<p class="muted">
  Item job instances, newest first. Like operations, status derives from the
  emulator clock — freeze or advance it on the <a href="#clock">Clock</a> page.
</p>
<button onclick={load}>Refresh</button>
{#if error}<p class="error">{error}</p>{/if}
{#if jobs.length === 0}
  <p class="muted">No jobs yet — run one via <code>POST /v1/workspaces/{'{wid}'}/items/{'{iid}'}/jobs/instances</code>.</p>
{:else}
  <table>
    <thead>
      <tr><th>Status</th><th>Job type</th><th>Item</th><th>Invoke</th><th>Id</th><th>Created</th></tr>
    </thead>
    <tbody>
      {#each jobs as j}
        <tr>
          <td><span class="chip {j.status.toLowerCase()}">{j.status}</span></td>
          <td>{j.jobType}</td>
          <td>
            {#if j.itemName}
              <code>{j.itemType}</code> {j.itemName} <span class="mono muted">{j.itemId}</span>
            {:else}
              <span class="muted">deleted item</span> <span class="mono muted">{j.itemId}</span>
            {/if}
          </td>
          <td>{j.invokeType}</td>
          <td class="mono">{j.id}</td>
          <td class="mono">{fmt(j.createdAt)}</td>
        </tr>
      {/each}
    </tbody>
  </table>
{/if}
