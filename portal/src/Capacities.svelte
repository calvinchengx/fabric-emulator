<script lang="ts">
  import { api } from './api';

  let capacities = $state<any[]>([]);
  let error = $state('');

  api.get('/_emulator/portal/capacities')
    .then((c) => (capacities = c.value))
    .catch((e) => (error = e.message));
</script>

<h1>Capacities</h1>
<p class="muted">
  The emulator seeds one deterministic capacity; workspaces created without an
  explicit <code>capacityId</code> are auto-assigned to it. Capacities are
  assignable objects only — no SKU billing or throttling model.
</p>
{#if error}<p class="error">{error}</p>{/if}
{#each capacities as cap}
  <div class="panel">
    <h2>{cap.displayName}</h2>
    <p>
      <span class="chip">{cap.sku}</span>
      <span class="chip">{cap.region}</span>
      <span class="chip {cap.state === 'Active' ? 'succeeded' : 'notstarted'}">{cap.state}</span>
      <span class="mono muted">{cap.id}</span>
    </p>
    <h3>Assigned workspaces</h3>
    {#if cap.workspaces.length === 0}
      <p class="muted">none</p>
    {:else}
      <table>
        <thead><tr><th>Name</th><th>Id</th></tr></thead>
        <tbody>
          {#each cap.workspaces as w}
            <tr><td>{w.displayName}</td><td class="mono">{w.id}</td></tr>
          {/each}
        </tbody>
      </table>
    {/if}
  </div>
{/each}
