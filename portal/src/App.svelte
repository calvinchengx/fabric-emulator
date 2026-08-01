<script>
  import Dashboard from './Dashboard.svelte';
  import Workspaces from './Workspaces.svelte';
  import Operations from './Operations.svelte';
  import Clock from './Clock.svelte';
  import Faults from './Faults.svelte';
  import Identities from './Identities.svelte';
  import Connections from './Connections.svelte';
  import Shortcuts from './Shortcuts.svelte';
  import Capacities from './Capacities.svelte';
  import Jobs from './Jobs.svelte';
  import Warehouse from './Warehouse.svelte';
  import Flow from './Flow.svelte';
  import { api } from './api.js';

  let route = $state(location.hash.slice(1) || 'dashboard');
  window.addEventListener('hashchange', () => (route = location.hash.slice(1) || 'dashboard'));

  let health = $state(null);
  api.get('/health').then((h) => (health = h)).catch(() => {});

  // Grouped navigation: the control plane's state, the data-plane surfaces,
  // the Go-native testing levers, and the entra-emulator identity handshake.
  const sections = [
    ['Control plane', [
      ['dashboard', 'Dashboard'],
      ['workspaces', 'Workspaces'],
      ['connections', 'Connections'],
      ['capacities', 'Capacities'],
      ['operations', 'Operations'],
      ['jobs', 'Jobs'],
    ]],
    ['Data plane', [
      ['flow', 'Data flow'],
      ['shortcuts', 'OneLake shortcuts'],
      ['warehouse', 'Warehouse SQL'],
    ]],
    ['Testing tools', [
      ['clock', 'Clock'],
      ['faults', 'Fault injection'],
    ]],
    ['Identity', [
      ['identities', 'Workspace identities'],
    ]],
  ];
</script>

<div class="topbar">
  <strong class="text-[15px] font-semibold tracking-tight">Fabric Emulator</strong>
  <span class="badge">Local emulator</span>
  {#if health}
    <span class="health"><span class="dot"></span>{health.status}</span>
  {/if}
</div>
<div class="shell">
  <nav class="sidenav">
    {#each sections as [title, items]}
      <div class="section-label">{title}</div>
      {#each items as [id, label]}
        <a href={'#' + id} class:active={route === id}>{label}</a>
      {/each}
    {/each}
    <div class="note muted">Not for production use.</div>
  </nav>
  <main>
    {#if route === 'workspaces'}<Workspaces />
    {:else if route === 'connections'}<Connections />
    {:else if route === 'capacities'}<Capacities />
    {:else if route === 'operations'}<Operations />
    {:else if route === 'jobs'}<Jobs />
    {:else if route === 'flow'}<Flow />
    {:else if route === 'shortcuts'}<Shortcuts />
    {:else if route === 'warehouse'}<Warehouse />
    {:else if route === 'clock'}<Clock />
    {:else if route === 'faults'}<Faults />
    {:else if route === 'identities'}<Identities />
    {:else}<Dashboard />{/if}
  </main>
</div>

<style>
  @reference '../src/app.css';

  .topbar {
    @apply sticky top-0 z-10 flex h-12 items-center gap-3 border-b
      bg-card px-4 backdrop-blur;
  }
  .health {
    @apply ml-auto flex items-center gap-1.5 text-xs text-muted-foreground;
  }
  .dot {
    @apply inline-block h-2 w-2 rounded-full bg-success;
  }
  .shell {
    @apply flex;
    min-height: calc(100vh - 48px);
  }
  .sidenav {
    @apply flex w-60 shrink-0 flex-col gap-0.5 border-r bg-sidebar p-2;
  }
  .section-label {
    @apply px-3 pt-4 pb-1 text-[11px] font-semibold uppercase tracking-wider
      text-muted-foreground;
  }
  .sidenav a {
    @apply relative block rounded-md px-3 py-2 font-medium text-sidebar-foreground
      no-underline transition-colors hover:bg-muted;
  }
  .sidenav a.active {
    @apply bg-sidebar-accent font-semibold text-sidebar-accent-foreground;
  }
  /* The active marker is a pseudo-element rather than a border, so selecting an
     item does not shift its label by the border width. */
  .sidenav a.active::before {
    @apply absolute left-0 top-1.5 bottom-1.5 w-0.5 rounded-full bg-primary;
    content: '';
  }
  main {
    @apply w-full max-w-[1400px] flex-1 p-6 lg:p-8;
  }
  .note {
    @apply mt-auto p-3;
  }
</style>
