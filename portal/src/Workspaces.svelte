<script lang="ts">
  import { api } from './api';
  import * as Card from '$lib/components/ui/card/index';
  import * as Table from '$lib/components/ui/table/index';

  let workspaces = $state<any[]>([]);
  let error = $state('');
  let open = $state<any>(null); // workspace id whose detail panel is expanded
  let detail = $state<any>(null);

  function load() {
    api.get('/_emulator/portal/workspaces')
      .then((w) => (workspaces = w.value))
      .catch((e) => (error = e.message));
  }
  load();

  function toggle(id: string) {
    if (open === id) {
      open = null;
      detail = null;
      return;
    }
    open = id;
    detail = null;
    api.get('/_emulator/portal/workspaces/' + id)
      .then((d) => (detail = d))
      .catch((e) => (error = e.message));
  }
</script>

<h1>Workspaces</h1>
{#if error}<p class="error">{error}</p>{/if}
{#if workspaces.length === 0}
  <p class="muted">No workspaces yet — create one through the API (see the quickstart).</p>
{:else}
  <Table.Root>
    <Table.Header>
      <Table.Row>
        <Table.Head>Name</Table.Head>
        <Table.Head>Id</Table.Head>
        <Table.Head>Capacity</Table.Head>
        <Table.Head>Items</Table.Head>
        <Table.Head>Roles</Table.Head>
        <Table.Head>Git</Table.Head>
        <Table.Head>Identity</Table.Head>
      </Table.Row>
    </Table.Header>
    <Table.Body>
      {#each workspaces as w}
        <Table.Row class="row" onclick={() => toggle(w.id)}>
          <Table.Cell>{w.displayName}</Table.Cell>
          <Table.Cell class="mono">{w.id}</Table.Cell>
          <Table.Cell class="mono">{w.capacityId || '—'}</Table.Cell>
          <Table.Cell>{w.itemCount}</Table.Cell>
          <Table.Cell>{w.roleCount}</Table.Cell>
          <Table.Cell>{w.git ? w.git.branchName : '—'}</Table.Cell>
          <Table.Cell>{w.workspaceIdentity ? 'provisioned' : '—'}</Table.Cell>
        </Table.Row>
        {#if open === w.id && detail}
          <Table.Row><Table.Cell colspan={7}>
            <Card.Root>
              <Card.Content>
              <h3>Items</h3>
              {#if detail.items.length === 0}<p class="muted">none</p>{:else}
                <ul>{#each detail.items as it}<li><code>{it.type}</code> {it.displayName} <span class="mono muted">{it.id}</span></li>{/each}</ul>
              {/if}
              <h3>Role assignments</h3>
              <ul>{#each detail.roleAssignments as ra}<li><strong>{ra.role}</strong> — {ra.principal.type} <span class="mono muted">{ra.principal.id}</span></li>{/each}</ul>
              <h3>Git</h3>
              {#if detail.git}
                <p><code>{detail.git.gitProviderType}</code> {detail.git.organizationName}/{detail.git.repositoryName} @ {detail.git.branchName} <span class="muted">({detail.git.directoryName || '/'})</span></p>
              {:else}
                <p class="muted">not connected</p>
              {/if}
              </Card.Content>
            </Card.Root>
          </Table.Cell></Table.Row>
        {/if}
      {/each}
    </Table.Body>
  </Table.Root>
{/if}
