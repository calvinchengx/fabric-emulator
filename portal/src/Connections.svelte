<script lang="ts">
  import { api } from './api';
  import StatusBadge from '$lib/StatusBadge.svelte';
  import { Button } from '$lib/components/ui/button/index';
  import * as Table from '$lib/components/ui/table/index';

  let connections = $state<any[]>([]);
  let error = $state('');

  function load() {
    api.get('/_emulator/portal/connections')
      .then((c) => (connections = c.value))
      .catch((e) => (error = e.message));
  }
  load();

  // Credential kinds that reference another emulated system rather than
  // carrying their own secret material.
  const flags: Record<string, string> = {
    AzureKeyVaultReference: 'Key Vault ref',
    WorkspaceIdentity: 'Workspace identity',
  };
</script>

<h1>Connections</h1>
<p class="muted">
  Credential secret material is write-only — the emulator stores it but never
  serializes it back, so only the credential metadata shows here.
</p>
<Button variant="outline" size="sm" onclick={load}>Refresh</Button>
{#if error}<p class="error">{error}</p>{/if}
{#if connections.length === 0}
  <p class="muted">No connections yet — create one via <code>POST /v1/connections</code>.</p>
{:else}
  <Table.Root>
    <Table.Header>
      <Table.Row>
        <Table.Head>Name</Table.Head>
        <Table.Head>Id</Table.Head>
        <Table.Head>Connectivity</Table.Head>
        <Table.Head>Credential</Table.Head>
        <Table.Head>Encryption</Table.Head>
      </Table.Row>
    </Table.Header>
    <Table.Body>
      {#each connections as c}
        <Table.Row>
          <Table.Cell>{c.displayName}</Table.Cell>
          <Table.Cell class="mono">{c.id}</Table.Cell>
          <Table.Cell>{c.connectivityType || '—'}</Table.Cell>
          <Table.Cell>
            {c.credentialType || '—'}
            {#if flags[c.credentialType]}
              <StatusBadge tone="caution">{flags[c.credentialType]}</StatusBadge>
            {/if}
          </Table.Cell>
          <Table.Cell>{c.connectionEncryption || '—'}</Table.Cell>
        </Table.Row>
      {/each}
    </Table.Body>
  </Table.Root>
{/if}
