<script lang="ts">
  import { api } from './api';
  import { Button } from '$lib/components/ui/button/index';
  import * as Card from '$lib/components/ui/card/index';
  import { Input } from '$lib/components/ui/input/index';
  import { Label } from '$lib/components/ui/label/index';

  let clock = $state<any>(null);
  let advanceBy = $state(3600);
  let error = $state('');

  function load() {
    api.get('/_emulator/clock').then((c) => (clock = c)).catch((e) => (error = e.message));
  }
  load();

  async function post(body: unknown) {
    error = '';
    try {
      clock = await api.post('/_emulator/clock', body);
    } catch (e) {
      error = (e as Error).message;
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
  <div class="grid gap-3 sm:grid-cols-3">
    <Card.Root><Card.Content class="py-4">
      <div class="num">{clock.frozen ? 'frozen' : 'running'}</div>
      <div class="text-muted-foreground text-sm">state</div>
    </Card.Content></Card.Root>
    <Card.Root><Card.Content class="py-4">
      <div class="num">{clock.offset}s</div>
      <div class="text-muted-foreground text-sm">offset</div>
    </Card.Content></Card.Root>
    <Card.Root><Card.Content class="py-4">
      <div class="num mono small">{new Date(clock.now * 1000).toISOString()}</div>
      <div class="text-muted-foreground text-sm">virtual now</div>
    </Card.Content></Card.Root>
  </div>
  <div class="mt-4 flex flex-wrap items-end gap-3">
    {#if clock.frozen}
      <Button variant="outline" onclick={() => post({ freeze: false })}>Unfreeze</Button>
    {:else}
      <Button variant="outline" onclick={() => post({ freeze: true })}>Freeze</Button>
    {/if}
    <div class="flex items-end gap-2">
      <div class="grid gap-1.5">
        <Label for="advance-by">Advance by</Label>
        <Input id="advance-by" type="number" bind:value={advanceBy} min="1" class="w-28" />
      </div>
      <span class="text-muted-foreground pb-2 text-sm">seconds</span>
    </div>
    <Button variant="outline" onclick={() => post({ advance: Number(advanceBy) })}>Advance</Button>
    <Button variant="outline" onclick={() => post({ offset: 0, freeze: false })}>Reset</Button>
  </div>
{/if}
