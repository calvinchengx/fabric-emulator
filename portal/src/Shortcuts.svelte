<script>
  import { api } from './api.js';

  let shortcuts = $state([]);
  let error = $state('');

  function load() {
    api.get('/_emulator/portal/shortcuts')
      .then((s) => (shortcuts = s.value))
      .catch((e) => (error = e.message));
  }
  load();

  // Group rows per owning workspace, preserving server order.
  const grouped = $derived.by(() => {
    const byWs = new Map();
    for (const sc of shortcuts) {
      if (!byWs.has(sc.workspaceId)) byWs.set(sc.workspaceId, { name: sc.workspaceName, rows: [] });
      byWs.get(sc.workspaceId).rows.push(sc);
    }
    return [...byWs.entries()];
  });
</script>

<h1>OneLake shortcuts</h1>
<p class="muted">
  Shortcuts are symlinks: reads resolve into the target's OneLake folder, no
  data is copied. A target deleted after creation leaves the shortcut dangling.
</p>
<button onclick={load}>Refresh</button>
{#if error}<p class="error">{error}</p>{/if}
{#if shortcuts.length === 0}
  <p class="muted">No shortcuts yet — create one via <code>POST /v1/workspaces/{'{wid}'}/items/{'{iid}'}/shortcuts</code>.</p>
{:else}
  {#each grouped as [wsId, group]}
    <h2>{group.name} <span class="mono muted">{wsId}</span></h2>
    <table>
      <thead>
        <tr><th>Item</th><th>Shortcut</th><th>Target</th><th>State</th></tr>
      </thead>
      <tbody>
        {#each group.rows as sc}
          <tr>
            <td>{sc.itemName} <span class="mono muted">{sc.itemId}</span></td>
            <td class="mono">{sc.path}/{sc.name}</td>
            <td class="mono">{sc.targetWorkspaceId}/{sc.targetItemId}/{sc.targetPath || ''}</td>
            <td>
              {#if sc.dangling}
                <span class="chip failed">dangling</span>
              {:else}
                <span class="chip succeeded">resolves</span>
              {/if}
            </td>
          </tr>
        {/each}
      </tbody>
    </table>
  {/each}
{/if}
