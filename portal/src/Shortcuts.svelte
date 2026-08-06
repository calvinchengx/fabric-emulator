<script lang="ts">
  import { api } from './api';
  import StatusBadge from '$lib/StatusBadge.svelte';
  import { Button } from '$lib/components/ui/button/index';
  import * as Table from '$lib/components/ui/table/index';

  let shortcuts = $state<any[]>([]);
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
<Button variant="outline" size="sm" onclick={load}>Refresh</Button>
{#if error}<p class="error">{error}</p>{/if}
{#if shortcuts.length === 0}
  <p class="muted">No shortcuts yet — create one via <code>POST /v1/workspaces/{'{wid}'}/items/{'{iid}'}/shortcuts</code>.</p>
{:else}
  {#each grouped as [wsId, group]}
    <h2>{group.name} <span class="mono muted">{wsId}</span></h2>
    <Table.Root>
      <Table.Header>
        <Table.Row>
          <Table.Head>Item</Table.Head>
          <Table.Head>Shortcut</Table.Head>
          <Table.Head>Target</Table.Head>
          <Table.Head>State</Table.Head>
        </Table.Row>
      </Table.Header>
      <Table.Body>
        {#each group.rows as sc}
          <Table.Row>
            <Table.Cell>{sc.itemName} <span class="mono muted">{sc.itemId}</span></Table.Cell>
            <Table.Cell class="mono">{sc.path}/{sc.name}</Table.Cell>
            <Table.Cell class="mono">{sc.targetWorkspaceId}/{sc.targetItemId}/{sc.targetPath || ''}</Table.Cell>
            <Table.Cell>
              {#if sc.dangling}
                <StatusBadge tone="danger">dangling</StatusBadge>
              {:else}
                <StatusBadge tone="success">resolves</StatusBadge>
              {/if}
            </Table.Cell>
          </Table.Row>
        {/each}
      </Table.Body>
    </Table.Root>
  {/each}
{/if}
