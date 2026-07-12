<script>
  import { api } from './api.js';

  let workspaces = $state([]);
  let operations = $state([]);
  let error = $state('');

  Promise.all([
    api.get('/_emulator/portal/workspaces'),
    api.get('/_emulator/portal/operations'),
  ])
    .then(([w, o]) => {
      workspaces = w.value;
      operations = o.value;
    })
    .catch((e) => (error = e.message));

  const items = $derived(workspaces.reduce((n, w) => n + w.itemCount, 0));
  const identities = $derived(workspaces.filter((w) => w.workspaceIdentity).length);
  const running = $derived(operations.filter((o) => o.status === 'Running' || o.status === 'NotStarted').length);
</script>

<h1>Dashboard</h1>
{#if error}<p class="error">{error}</p>{/if}
<div class="cards">
  <div class="card"><div class="num">{workspaces.length}</div><div>workspaces</div></div>
  <div class="card"><div class="num">{items}</div><div>items</div></div>
  <div class="card"><div class="num">{operations.length}</div><div>recent operations</div></div>
  <div class="card"><div class="num">{running}</div><div>operations in flight</div></div>
  <div class="card"><div class="num">{identities}</div><div>workspace identities</div></div>
</div>
<p class="muted">
  The control plane is bearer-authenticated (<code>/v1</code>); this portal reads the
  emulator's state through the unauthenticated <code>/_emulator</code> surface.
  Mint tokens from entra-emulator to drive the API itself.
</p>
