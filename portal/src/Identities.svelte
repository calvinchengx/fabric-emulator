<script>
  import { api } from './api.js';

  let workspaces = $state([]);
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
  <table>
    <thead><tr><th>Workspace</th><th>Application id</th><th>Service principal id</th></tr></thead>
    <tbody>
      {#each provisioned as w}
        <tr>
          <td>{w.displayName}</td>
          <td class="mono">{w.workspaceIdentity.applicationId}</td>
          <td class="mono">{w.workspaceIdentity.servicePrincipalId}</td>
        </tr>
      {/each}
    </tbody>
  </table>
{/if}
