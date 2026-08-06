<script lang="ts">
  import { api } from './api';
  import StatusBadge from '$lib/StatusBadge.svelte';
  import * as Table from '$lib/components/ui/table/index';

  let wh = $state<any>(null);
  let error = $state('');

  api.get('/_emulator/portal/warehouse')
    .then((w) => (wh = w))
    .catch((e) => (error = e.message));

  const listenerLabel: Record<string, string> = {
    off: 'no TDS listener',
    stub: 'answers the T1 probe result',
    relay: 'queries run on the configured SQL Server',
  };
</script>

<h1>Warehouse SQL</h1>
<p class="muted">
  The warehouse SQL endpoint is a TDS listener that terminates Entra FedAuth.
  This shows configuration presence only — the backend DSN carries credentials
  and is never exposed.
</p>
{#if error}<p class="error">{error}</p>{/if}
{#if wh}
  <Table.Root>
    <Table.Body>
      <Table.Row>
        <Table.Cell><code>FABRIC_SQL_TDS_ADDR</code></Table.Cell>
        <Table.Cell>
          {#if wh.sqlTdsConfigured}
            <StatusBadge tone="success">configured</StatusBadge>
          {:else}
            <StatusBadge>not configured</StatusBadge>
          {/if}
        </Table.Cell>
      </Table.Row>
      <Table.Row>
        <Table.Cell><code>FABRIC_WAREHOUSE_SQL_URL</code></Table.Cell>
        <Table.Cell>
          {#if wh.warehouseSqlConfigured}
            <StatusBadge tone="success">configured</StatusBadge>
          {:else}
            <StatusBadge>not configured</StatusBadge>
          {/if}
        </Table.Cell>
      </Table.Row>
      <Table.Row>
        <Table.Cell>TDS listener</Table.Cell>
        <Table.Cell>
          <StatusBadge status={wh.tdsListener} tone={wh.tdsListener === 'off' ? '' : 'success'} />
          <span class="muted">{listenerLabel[wh.tdsListener] || ''}</span>
        </Table.Cell>
      </Table.Row>
    </Table.Body>
  </Table.Root>
{/if}
