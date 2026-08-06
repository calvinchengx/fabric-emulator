<script lang="ts">
  import { api } from './api';
  import StatusBadge from '$lib/StatusBadge.svelte';
  import { Badge } from '$lib/components/ui/badge/index';
  import * as Card from '$lib/components/ui/card/index';
  import * as Table from '$lib/components/ui/table/index';

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
  <Card.Root class="mt-4">
    <Card.Header>
      <Card.Title>{cap.displayName}</Card.Title>
      <Card.Description class="flex flex-wrap items-center gap-2 pt-1">
        <Badge variant="outline" class="font-mono text-xs">{cap.sku}</Badge>
        <Badge variant="outline" class="font-mono text-xs">{cap.region}</Badge>
        <!-- The tone is the view's decision, not the word's: any state that is
             not Active reads as caution, whatever it is called. -->
        <StatusBadge status={cap.state} tone={cap.state === 'Active' ? 'success' : 'caution'} />
        <span class="mono muted">{cap.id}</span>
      </Card.Description>
    </Card.Header>
    <Card.Content>
      <h3>Assigned workspaces</h3>
      {#if cap.workspaces.length === 0}
        <p class="muted">none</p>
      {:else}
        <Table.Root>
          <Table.Header>
            <Table.Row><Table.Head>Name</Table.Head><Table.Head>Id</Table.Head></Table.Row>
          </Table.Header>
          <Table.Body>
            {#each cap.workspaces as w}
              <Table.Row>
                <Table.Cell>{w.displayName}</Table.Cell>
                <Table.Cell class="mono">{w.id}</Table.Cell>
              </Table.Row>
            {/each}
          </Table.Body>
        </Table.Root>
      {/if}
    </Card.Content>
  </Card.Root>
{/each}
