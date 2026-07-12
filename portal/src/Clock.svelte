<script>
  import { api } from './api.js';

  let clock = $state(null);
  let advanceBy = $state(3600);
  let error = $state('');

  function load() {
    api.get('/_emulator/clock').then((c) => (clock = c)).catch((e) => (error = e.message));
  }
  load();

  async function post(body) {
    error = '';
    try {
      clock = await api.post('/_emulator/clock', body);
    } catch (e) {
      error = e.message;
    }
  }
</script>

<h1>Clock</h1>
<p class="muted">
  The virtual clock drives LRO completion and job state. Freeze it to pin
  operations in <code>Running</code>; advance it to complete them instantly.
</p>
{#if error}<p class="error">{error}</p>{/if}
{#if clock}
  <div class="cards">
    <div class="card"><div class="num">{clock.frozen ? 'frozen' : 'running'}</div><div>state</div></div>
    <div class="card"><div class="num">{clock.offset}s</div><div>offset</div></div>
    <div class="card"><div class="num mono small">{new Date(clock.now * 1000).toISOString()}</div><div>virtual now</div></div>
  </div>
  <div class="controls">
    {#if clock.frozen}
      <button onclick={() => post({ freeze: false })}>Unfreeze</button>
    {:else}
      <button onclick={() => post({ freeze: true })}>Freeze</button>
    {/if}
    <label>
      Advance by
      <input type="number" bind:value={advanceBy} min="1" /> seconds
    </label>
    <button onclick={() => post({ advance: Number(advanceBy) })}>Advance</button>
    <button onclick={() => post({ offset: 0, freeze: false })}>Reset</button>
  </div>
{/if}
