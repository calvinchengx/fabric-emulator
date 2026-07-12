<script>
  import { api } from './api.js';

  let failNext = $state(1);
  let rejectNext = $state(1);
  let lroDelay = $state(30);
  let message = $state('');
  let error = $state('');

  async function post(body, note) {
    message = '';
    error = '';
    try {
      await api.post('/_emulator/faults', body);
      message = note;
    } catch (e) {
      error = e.message;
    }
  }
</script>

<h1>Fault injection</h1>
<p class="muted">
  Break things on purpose: retry logic, poll-until-failed branches, and error
  surfaces get tested without touching the client under test.
</p>
{#if message}<p class="ok">{message}</p>{/if}
{#if error}<p class="error">{error}</p>{/if}

<div class="panel">
  <h3>Fail operations</h3>
  <p class="muted">The next N async operations end <code>Failed</code> with a Fabric-shaped error body.</p>
  <label>N <input type="number" bind:value={failNext} min="0" /></label>
  <button onclick={() => post({ failNextOperations: Number(failNext) }, `next ${failNext} operation(s) will fail`)}>Arm</button>
</div>

<div class="panel">
  <h3>Reject requests</h3>
  <p class="muted">The next N API requests get a 5xx before reaching a handler.</p>
  <label>N <input type="number" bind:value={rejectNext} min="0" /></label>
  <button onclick={() => post({ rejectNextRequests: Number(rejectNext) }, `next ${rejectNext} request(s) will be rejected`)}>Arm</button>
</div>

<div class="panel">
  <h3>LRO delay</h3>
  <p class="muted">Override how many virtual seconds operations stay <code>Running</code>.</p>
  <label>Seconds <input type="number" bind:value={lroDelay} min="0" /></label>
  <button onclick={() => post({ lroDelaySeconds: Number(lroDelay) }, `operations now stay Running ${lroDelay}s`)}>Set</button>
</div>
