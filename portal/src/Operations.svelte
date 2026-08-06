<script lang="ts">
  import { api } from './api';
  import StatusBadge from '$lib/StatusBadge.svelte';
  import { Button } from '$lib/components/ui/button/index';
  import * as Table from '$lib/components/ui/table/index';

  let operations = $state<any[]>([]);
  let error = $state('');

  function load() {
    api.get('/_emulator/portal/operations')
      .then((o) => (operations = o.value))
      .catch((e) => (error = e.message));
  }
  load();

  function fmt(epoch: number) {
    return new Date(epoch * 1000).toISOString().replace('T', ' ').slice(0, 19);
  }
</script>

<h1>Operations</h1>
<p class="muted">
  Long-running operations, newest first. Status derives from the emulator
  clock — freeze or advance it on the <a href="#clock">Clock</a> page to hold
  or complete them.
</p>
<Button variant="outline" size="sm" onclick={load}>Refresh</Button>
{#if error}<p class="error">{error}</p>{/if}
{#if operations.length === 0}
  <p class="muted">No operations yet — any async mutation on /v1 creates one.</p>
{:else}
  <Table.Root>
    <Table.Header>
      <Table.Row>
        <Table.Head>Status</Table.Head>
        <Table.Head>Kind</Table.Head>
        <Table.Head>Id</Table.Head>
        <Table.Head>Created</Table.Head>
        <Table.Head>Result</Table.Head>
      </Table.Row>
    </Table.Header>
    <Table.Body>
      {#each operations as op}
        <Table.Row>
          <Table.Cell><StatusBadge status={op.status} /></Table.Cell>
          <Table.Cell>{op.kind}</Table.Cell>
          <Table.Cell class="mono">{op.id}</Table.Cell>
          <Table.Cell class="mono">{fmt(op.createdAt)}</Table.Cell>
          <Table.Cell class="mono">{op.resultRef || '—'}</Table.Cell>
        </Table.Row>
      {/each}
    </Table.Body>
  </Table.Root>
{/if}
