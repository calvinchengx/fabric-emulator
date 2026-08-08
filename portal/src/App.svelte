<script lang="ts">
  import Dashboard from './Dashboard.svelte';
  import Workspaces from './Workspaces.svelte';
  import Operations from './Operations.svelte';
  import Clock from './Clock.svelte';
  import Faults from './Faults.svelte';
  import Identities from './Identities.svelte';
  import Connections from './Connections.svelte';
  import Shortcuts from './Shortcuts.svelte';
  import Lakehouses from './Lakehouses.svelte';
  import Capacities from './Capacities.svelte';
  import Jobs from './Jobs.svelte';
  import Warehouse from './Warehouse.svelte';
  import Flow from './Flow.svelte';
  import Models from './Models.svelte';
  import { api } from './api';
  import { parse, href, onRouteChange } from './router';
  import * as theme from './theme';
  import { Badge } from '$lib/components/ui/badge/index';
  import { Button } from '$lib/components/ui/button/index';

  // `#view` and `#view/param`. The param is what makes a detail page
  // addressable — see router.ts for why that matters more than it looks.
  let current = $state(parse(location.hash));
  onRouteChange((r) => (current = r));
  let route = $derived(current.view);
  let param = $derived(current.param);

  let health = $state<any>(null);
  api.get('/health').then((h) => (health = h)).catch(() => {});

  // The sidebar folds away. The flow view earns its keep when the terminal,
  // the graph and the event stream share one screen, and on a laptop — or in
  // a 1600px recording — the 240px of navigation is the difference between
  // that fitting and not. Persisted because a preference that resets on every
  // reload is a nag, not a preference.
  // The theme, and the toggle that cycles it. Three states, because "follow
  // the OS" is a real answer a two-way switch cannot express.
  let themePref = $state<theme.Theme>(theme.stored());
  const themeBadge = $derived(theme.badge(themePref));
  function cycleTheme() {
    themePref = theme.next(themePref);
    theme.set(themePref);
  }
  // While following the OS, track it: a machine that switches at sunset should
  // take the portal with it without a reload.
  $effect(() => theme.followOS());

  let navOpen = $state(localStorage.getItem('fe.nav') !== 'closed');
  function toggleNav() {
    navOpen = !navOpen;
    localStorage.setItem('fe.nav', navOpen ? 'open' : 'closed');
  }

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
      ['lakehouses', 'Lakehouses'],
      ['shortcuts', 'OneLake shortcuts'],
      ['warehouse', 'Warehouse SQL'],
      ['models', 'Semantic models'],
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
  <!-- Plain glyphs, not an icon font: the portal ships no icon assets and one
       hamburger is not the reason to start. -->
  <Button
    variant="ghost"
    size="icon-sm"
    class="nav-toggle"
    aria-label="Toggle sidebar"
    title="Toggle sidebar"
    onclick={toggleNav}
  >
    {navOpen ? '⟨' : '☰'}
  </Button>
  <strong class="text-[15px] font-semibold tracking-tight">Fabric Emulator</strong>
  <Badge variant="secondary" class="uppercase">Local emulator</Badge>
  <!-- The build, beside the badge on purpose. A screenshot of a run, a frame of
       a recording, or a bug report that quotes the top bar all name exactly
       which emulator produced what is being shown — which is otherwise the
       first question and the slowest one to answer. -->
  {#if health?.build}
    <span class="build" title="fabric-emulator build">{health.build}</span>
  {/if}
  {#if health}
    <span class="health"><span class="dot"></span>{health.status}</span>
  {/if}
  <Button
    variant="ghost"
    size="icon-sm"
    class={health ? '' : 'ml-auto'}
    aria-label={themeBadge.label}
    title={themeBadge.label}
    onclick={cycleTheme}
  >
    {themeBadge.glyph}
  </Button>
</div>
<div class="shell">
  {#if navOpen}
    <nav class="sidenav">
      {#each sections as [title, items]}
        <div class="section-label">{title}</div>
        {#each items as [id, label]}
          <a href={href(id)} class:active={route === id}>{label}</a>
        {/each}
      {/each}
    </nav>
  {/if}
  <main class:wide={!navOpen}>
    {#if route === 'workspaces'}<Workspaces />
    {:else if route === 'lakehouses'}<Lakehouses />
    {:else if route === 'connections'}<Connections />
    {:else if route === 'capacities'}<Capacities />
    {:else if route === 'operations'}<Operations />
    {:else if route === 'jobs'}<Jobs />
    {:else if route === 'flow'}<Flow />
    {:else if route === 'models'}<Models id={param} />
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
  /* With the sidebar folded, the cap comes off: the point of folding it was
     the width, and keeping a 1400px ceiling would hand the saved space to the
     margins instead of the content. */
  main.wide {
    @apply max-w-none;
  }
  .nav-toggle {
    @apply -ml-1 flex h-8 w-8 items-center justify-center rounded-md border
      bg-transparent text-base leading-none text-muted-foreground
      transition-colors hover:bg-muted;
  }
</style>
