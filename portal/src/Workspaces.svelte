<script>
  import { api } from './api.js';

  let workspaces = $state([]);
  let error = $state('');
  let open = $state(null); // workspace id whose detail panel is expanded
  let detail = $state(null);

  function load() {
    api.get('/_emulator/portal/workspaces')
      .then((w) => (workspaces = w.value))
      .catch((e) => (error = e.message));
  }
  load();

  function toggle(id) {
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
  <table>
    <thead>
      <tr><th>Name</th><th>Id</th><th>Capacity</th><th>Items</th><th>Roles</th><th>Git</th><th>Identity</th></tr>
    </thead>
    <tbody>
      {#each workspaces as w}
        <tr class="row" onclick={() => toggle(w.id)}>
          <td>{w.displayName}</td>
          <td class="mono">{w.id}</td>
          <td class="mono">{w.capacityId || '—'}</td>
          <td>{w.itemCount}</td>
          <td>{w.roleCount}</td>
          <td>{w.git ? w.git.branchName : '—'}</td>
          <td>{w.workspaceIdentity ? 'provisioned' : '—'}</td>
        </tr>
        {#if open === w.id && detail}
          <tr><td colspan="7">
            <div class="panel">
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
            </div>
          </td></tr>
        {/if}
      {/each}
    </tbody>
  </table>
{/if}
