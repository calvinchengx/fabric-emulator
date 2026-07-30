<script>
  import { api } from './api.js';

  let connections = $state([]);
  let error = $state('');

  function load() {
    api.get('/_emulator/portal/connections')
      .then((c) => (connections = c.value))
      .catch((e) => (error = e.message));
  }
  load();

  // Credential kinds that reference another emulated system rather than
  // carrying their own secret material.
  const flags = {
    AzureKeyVaultReference: 'Key Vault ref',
    WorkspaceIdentity: 'Workspace identity',
  };
</script>

<h1>Connections</h1>
<p class="muted">
  Credential secret material is write-only — the emulator stores it but never
  serializes it back, so only the credential metadata shows here.
</p>
<button onclick={load}>Refresh</button>
{#if error}<p class="error">{error}</p>{/if}
{#if connections.length === 0}
  <p class="muted">No connections yet — create one via <code>POST /v1/connections</code>.</p>
{:else}
  <table>
    <thead>
      <tr><th>Name</th><th>Id</th><th>Connectivity</th><th>Credential</th><th>Encryption</th></tr>
    </thead>
    <tbody>
      {#each connections as c}
        <tr>
          <td>{c.displayName}</td>
          <td class="mono">{c.id}</td>
          <td>{c.connectivityType || '—'}</td>
          <td>
            {c.credentialType || '—'}
            {#if flags[c.credentialType]}
              <span class="chip notstarted">{flags[c.credentialType]}</span>
            {/if}
          </td>
          <td>{c.connectionEncryption || '—'}</td>
        </tr>
      {/each}
    </tbody>
  </table>
{/if}
