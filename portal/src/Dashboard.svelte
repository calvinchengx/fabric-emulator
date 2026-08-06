<script lang="ts">
  import { api } from './api';
  import * as Card from '$lib/components/ui/card/index';

  let workspaces = $state<any[]>([]);
  let operations = $state<any[]>([]);
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
<!-- One tile per number, all the same shape: the counts are peers, so nothing
     here should look more important than its neighbour. -->
<div class="grid gap-3 sm:grid-cols-2 lg:grid-cols-5">
  {#each [
    { n: workspaces.length, label: 'workspaces' },
    { n: items, label: 'items' },
    { n: operations.length, label: 'recent operations' },
    { n: running, label: 'operations in flight' },
    { n: identities, label: 'workspace identities' },
  ] as tile (tile.label)}
    <Card.Root>
      <Card.Content class="py-4">
        <div class="num">{tile.n}</div>
        <div class="text-muted-foreground text-sm">{tile.label}</div>
      </Card.Content>
    </Card.Root>
  {/each}
</div>
<p class="muted">
  The control plane is bearer-authenticated (<code>/v1</code>); this portal reads the
  emulator's state through the unauthenticated <code>/_emulator</code> surface.
  Mint tokens from entra-emulator to drive the API itself.
</p>
