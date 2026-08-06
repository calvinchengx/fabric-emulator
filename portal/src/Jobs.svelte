<script lang="ts">
  import { api } from './api';
  import StatusBadge from '$lib/StatusBadge.svelte';
  import { Button } from '$lib/components/ui/button/index';
  import * as Table from '$lib/components/ui/table/index';

  let jobs = $state<any[]>([]);
  let error = $state('');

  function load() {
    api.get('/_emulator/portal/jobs')
      .then((j) => (jobs = j.value))
      .catch((e) => (error = e.message));
  }
  load();

  function fmt(epoch: number) {
    return new Date(epoch * 1000).toISOString().replace('T', ' ').slice(0, 19);
  }
</script>

<h1>Jobs</h1>
<p class="muted">
  Item job instances, newest first. Like operations, status derives from the
  emulator clock — freeze or advance it on the <a href="#clock">Clock</a> page.
</p>
<Button variant="outline" size="sm" onclick={load}>Refresh</Button>
{#if error}<p class="error">{error}</p>{/if}
{#if jobs.length === 0}
  <p class="muted">No jobs yet — run one via <code>POST /v1/workspaces/{'{wid}'}/items/{'{iid}'}/jobs/instances</code>.</p>
{:else}
  <Table.Root>
    <Table.Header>
      <Table.Row>
        <Table.Head>Status</Table.Head>
        <Table.Head>Job type</Table.Head>
        <Table.Head>Item</Table.Head>
        <Table.Head>Invoke</Table.Head>
        <Table.Head>Id</Table.Head>
        <Table.Head>Created</Table.Head>
      </Table.Row>
    </Table.Header>
    <Table.Body>
      {#each jobs as j}
        <Table.Row>
          <Table.Cell><StatusBadge status={j.status} /></Table.Cell>
          <Table.Cell>{j.jobType}</Table.Cell>
          <Table.Cell>
            {#if j.itemName}
              <code>{j.itemType}</code> {j.itemName} <span class="mono muted">{j.itemId}</span>
            {:else}
              <span class="muted">deleted item</span> <span class="mono muted">{j.itemId}</span>
            {/if}
          </Table.Cell>
          <Table.Cell>{j.invokeType}</Table.Cell>
          <Table.Cell class="mono">{j.id}</Table.Cell>
          <Table.Cell class="mono">{fmt(j.createdAt)}</Table.Cell>
        </Table.Row>
      {/each}
    </Table.Body>
  </Table.Root>
{/if}
