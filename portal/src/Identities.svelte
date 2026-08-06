<script lang="ts">
  import { api } from './api';
  import * as Table from '$lib/components/ui/table/index';

  let workspaces = $state<any[]>([]);
  let error = $state('');

  api.get('/_emulator/portal/workspaces')
    .then((w) => (workspaces = w.value))
    .catch((e) => (error = e.message));

  const provisioned = $derived(workspaces.filter((w) => w.workspaceIdentity));
</script>

<h1>Workspace identities</h1>
<p class="muted">
  Identities are provisioned via <code>POST /v1/workspaces/{'{id}'}/provisionIdentity</code>
  and live in entra-emulator (the app registration + service principal + token
  mint). This view shows the fabric-side link; entra's portal shows the
  identity objects themselves.
</p>
{#if error}<p class="error">{error}</p>{/if}
{#if provisioned.length === 0}
  <p class="muted">No workspace has a provisioned identity.</p>
{:else}
  <Table.Root>
    <Table.Header>
      <Table.Row>
        <Table.Head>Workspace</Table.Head>
        <Table.Head>Application id</Table.Head>
        <Table.Head>Service principal id</Table.Head>
      </Table.Row>
    </Table.Header>
    <Table.Body>
      {#each provisioned as w}
        <Table.Row>
          <Table.Cell>{w.displayName}</Table.Cell>
          <Table.Cell class="mono">{w.workspaceIdentity.applicationId}</Table.Cell>
          <Table.Cell class="mono">{w.workspaceIdentity.servicePrincipalId}</Table.Cell>
        </Table.Row>
      {/each}
    </Table.Body>
  </Table.Root>
{/if}
