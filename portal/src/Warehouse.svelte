<script>
  import { api } from './api.js';

  let wh = $state(null);
  let error = $state('');

  api.get('/_emulator/portal/warehouse')
    .then((w) => (wh = w))
    .catch((e) => (error = e.message));

  const listenerLabel = {
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
  <table>
    <tbody>
      <tr>
        <td><code>FABRIC_SQL_TDS_ADDR</code></td>
        <td>
          {#if wh.sqlTdsConfigured}
            <span class="chip succeeded">configured</span>
          {:else}
            <span class="chip">not configured</span>
          {/if}
        </td>
      </tr>
      <tr>
        <td><code>FABRIC_WAREHOUSE_SQL_URL</code></td>
        <td>
          {#if wh.warehouseSqlConfigured}
            <span class="chip succeeded">configured</span>
          {:else}
            <span class="chip">not configured</span>
          {/if}
        </td>
      </tr>
      <tr>
        <td>TDS listener</td>
        <td>
          <span class="chip {wh.tdsListener === 'off' ? '' : 'succeeded'}">{wh.tdsListener}</span>
          <span class="muted">{listenerLabel[wh.tdsListener] || ''}</span>
        </td>
      </tr>
    </tbody>
  </table>
{/if}
