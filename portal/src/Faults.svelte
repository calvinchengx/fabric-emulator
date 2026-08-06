<script lang="ts">
  import { api } from './api';
  import { Button } from '$lib/components/ui/button/index';
  import * as Card from '$lib/components/ui/card/index';
  import { Input } from '$lib/components/ui/input/index';
  import { Label } from '$lib/components/ui/label/index';

  let failNext = $state(1);
  let rejectNext = $state(1);
  let lroDelay = $state(30);
  let message = $state('');
  let error = $state('');

  async function post(body: unknown, note: string) {
    message = '';
    error = '';
    try {
      await api.post('/_emulator/faults', body);
      message = note;
    } catch (e) {
      error = (e as Error).message;
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

<Card.Root class="mt-4">
  <Card.Header>
    <Card.Title>Fail operations</Card.Title>
    <Card.Description>The next N async operations end <code>Failed</code> with a Fabric-shaped error body.</Card.Description>
  </Card.Header>
  <Card.Content class="flex flex-wrap items-end gap-3">
    <div class="grid gap-1.5">
      <Label for="fail-next">N</Label>
      <Input id="fail-next" type="number" bind:value={failNext} min="0" class="w-28" />
    </div>
    <Button variant="outline" onclick={() => post({ failNextOperations: Number(failNext) }, `next ${failNext} operation(s) will fail`)}>Arm</Button>
  </Card.Content>
</Card.Root>

<Card.Root class="mt-4">
  <Card.Header>
    <Card.Title>Reject requests</Card.Title>
    <Card.Description>The next N API requests get a 5xx before reaching a handler.</Card.Description>
  </Card.Header>
  <Card.Content class="flex flex-wrap items-end gap-3">
    <div class="grid gap-1.5">
      <Label for="reject-next">N</Label>
      <Input id="reject-next" type="number" bind:value={rejectNext} min="0" class="w-28" />
    </div>
    <Button variant="outline" onclick={() => post({ rejectNextRequests: Number(rejectNext) }, `next ${rejectNext} request(s) will be rejected`)}>Arm</Button>
  </Card.Content>
</Card.Root>

<Card.Root class="mt-4">
  <Card.Header>
    <Card.Title>LRO delay</Card.Title>
    <Card.Description>Override how many virtual seconds operations stay <code>Running</code>.</Card.Description>
  </Card.Header>
  <Card.Content class="flex flex-wrap items-end gap-3">
    <div class="grid gap-1.5">
      <Label for="lro-delay">Seconds</Label>
      <Input id="lro-delay" type="number" bind:value={lroDelay} min="0" class="w-28" />
    </div>
    <Button variant="outline" onclick={() => post({ lroDelaySeconds: Number(lroDelay) }, `operations now stay Running ${lroDelay}s`)}>Set</Button>
  </Card.Content>
</Card.Root>
