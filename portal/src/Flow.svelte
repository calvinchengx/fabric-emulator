<script lang="ts">
  import { api } from './api.js';
  import { EVENT_KINDS, VIEW_KINDS, KIND_DOC, isEventKind, isViewKind } from './eventKinds';
  import type { EmulatorEvent, RawEmulatorEvent, ViewKind } from './eventKinds';
  import { href as modelHref } from './router.js';
  import { Button } from '$lib/components/ui/button/index.js';

  // The flow view: the emulator's own event stream, live.
  //
  // Two halves that answer different questions. The *graph* answers "what does
  // this pipeline do" — it comes from recorded lineage, so it is there before
  // anything runs. The *log* answers "what is happening right now, and what
  // broke" — it comes from the SSE stream. Nodes light up as table events land
  // on them, which is what ties the two together.
  //
  // WHERE THE TYPES STOP, AND WHY. The event stream is typed to the hilt because
  // its contract is GENERATED from internal/store/bus.go — the types cannot drift
  // from the server. The other endpoints this view reads (lineage, table preview,
  // workspaces) are not generated, so a hand-written interface for them would be
  // an unchecked claim about the server: precisely the drift that made a
  // hand-written kind list a bug. They stay `any` until they are generated too.

  const MAX_LOG = 300;

  /** An event the log renders: everything except `dropped`, which reports on
   *  this browser rather than on the platform and is counted, not listed. */
  type LoggedEvent = EmulatorEvent & { kind: ViewKind };

  /** A box in the graph: one OneLake path, or one source system outside Fabric.
   *  Local to this view — the layout is drawn here, not served. */
  type Node = {
    key: string;
    itemId: string;
    item?: string;
    path: string;
    depth: number;
    source: boolean;
    x: number;
    y: number;
  };

  /** An arrow between two nodes, keyed by nodeKey. */
  type Link = { from: string; to: string; producer?: string; activityName?: string };

  /** Proof at COMPILE TIME that a value cannot occur.
   *
   * `never` accepts no value at all, so a call that still type-checks is the
   * compiler agreeing it has already excluded every kind. Declare a kind in Go
   * and the calls below stop building — which is the whole reason the event
   * contract is generated as TypeScript instead of a list of strings.
   */
  function impossible(_kind: never): void {}

  // The terminal pane. Availability is ASKED, not assumed: the emulator dials
  // ttyd rather than reporting what was configured, so a stack whose `terminal`
  // profile is off says so instead of offering a pane that dies when clicked.
  // A configured-but-absent sidecar is exactly how the spark-agent profile bug
  // produced a failure that named nothing.
  let termAvailable = $state(false);
  let termReason = $state('');
  let termOpen = $state(false);
  // Typed, never fetched. The portal is unauthenticated and reachable by anyone
  // who can reach the port, so an endpoint serving this token would be the same
  // as having none. The emulator prints it once at startup.
  let termToken = $state('');
  let termStarted = $state(false);


  let events = $state<LoggedEvent[]>([]);
  let edges = $state<any[]>([]);
  let error = $state('');
  // Three states, not two: before the first connection resolves we are
  // *connecting*, and flashing a red "disconnected" in that window would be a
  // lie the user has to learn to ignore.
  let link = $state('connecting');
  let dropped = $state(0);
  // Kinds this build does not know. Generation makes drift impossible for a
  // portal embedded in its own emulator, but this stream is also readable by
  // anything else (`curl -N /_emulator/events`), and a filter keyed on an
  // unknown kind is `undefined` — which would drop it from the log with no
  // trace. Counted so an impossible thing is visible if it ever happens.
  let unknown = $state(0);
  // Built from the generated list, so a new kind appears here the moment it
  // is declared in Go rather than whenever someone remembers this line.
  // `file` starts off: a busy run emits far more of them than anything else.
  //
  // The cast is Object.fromEntries losing what its input knew: it always returns
  // an index signature, so the fact that these keys are exactly VIEW_KINDS has to
  // be restated. It is checked either way — a key that is not a ViewKind cannot
  // be read out of this record.
  let kinds = $state<Record<ViewKind, boolean>>(
    Object.fromEntries(VIEW_KINDS.map((k) => [k, k !== 'file'])) as Record<ViewKind, boolean>,
  );
  // itemId|Tables/name -> client clock (ms) of the last write. Client clock,
  // not the emulator's: this drives a *visual* decay, and the emulator's clock
  // can be frozen or advanced by hours, which would make "recent" meaningless.
  let touched = $state<Record<string, number>>({});
  // Ticks so freshness decays on its own; without it a node stays lit until
  // some other event happens to re-render the graph.
  let now = $state(Date.now());
  let workspace = $state('');
  let workspaces = $state<any[]>([]);
  let selected = $state<Node | null>(null);
  let detail = $state<any>(null);
  let detailError = $state('');
  // itemId|Tables/name -> true when an activity writing it failed.
  let broken = $state<Record<string, boolean>>({});

  let source: EventSource | null = null;
  // itemId -> client clock of the last query against it (the Power BI hop).
  let queried = $state<Record<string, number>>({});

  // Which item ids are semantic models, so those nodes can link to their own
  // page. Until `#models/{id}` existed the Power BI hop was a dead end: the
  // graph lit a node up when a client queried it and there was nowhere to go
  // from there. Read from the same endpoint the models page uses, so the two
  // cannot disagree about what a model is.
  let modelIds = $state<Set<string>>(new Set());
  api
    .get('/_emulator/portal/models')
    .then((r) => (modelIds = new Set<string>((r.value || []).map((m: any) => m.itemId))))
    .catch(() => {}); // the graph is useful without the link

  // Coalesce graph reloads: a build emits many edges in a burst and they all
  // describe the same redraw.
  let reloadTimer: ReturnType<typeof setTimeout> | null = null;
  function scheduleLineageReload() {
    if (reloadTimer) return;
    reloadTimer = setTimeout(() => {
      reloadTimer = null;
      loadLineage();
    }, 400);
  }

  // A failed load RETRIES ITSELF. Clearing the banner on the next success
  // (below) closed half the hole: reloads are triggered by lineage events, and
  // a stack that has finished its run emits none — so one transient failure
  // during a container recreate painted `HTTP 404` and it sat there for hours,
  // describing a moment long past as if it were now. An error banner is a
  // claim about the present, so the failure path re-asks until it can either
  // clear itself or say a CURRENT failure. Backoff, capped: a dead emulator
  // should cost a request every half minute, not a hammering.
  let retryTimer: ReturnType<typeof setTimeout> | null = null;
  let retryDelay = 3000;
  function loadLineage() {
    api.get('/_emulator/portal/lineage')
      .then((r) => {
        edges = r.value || [];
        // Clear on success. This reloads on a debounce for the whole session,
        // so without this one transient failure paints the banner and it stays
        // there — beside a green `streaming` chip and a finished run, which
        // reads as a broken platform. Found in a demo recording, where a stale
        // `HTTP 404` sat under a pipeline that had just passed 16/16.
        error = '';
        retryDelay = 3000;
        if (retryTimer) {
          clearTimeout(retryTimer);
          retryTimer = null;
        }
      })
      .catch((e) => {
        error = e.message;
        if (retryTimer) clearTimeout(retryTimer);
        retryTimer = setTimeout(() => {
          retryTimer = null;
          loadLineage();
        }, retryDelay);
        retryDelay = Math.min(retryDelay * 2, 30000);
      });
  }
  loadLineage();

  function loadTerminal() {
    api.get('/_emulator/portal/terminal/status')
      .then((st) => {
        termAvailable = !!st?.available;
        termReason = st?.reason || '';
      })
      // A 404 is the ordinary case: no terminal configured, so no route
      // mounted. Not an error worth showing.
      .catch(() => {
        termAvailable = false;
        termReason = '';
      });
  }
  loadTerminal();

  /** The framed ttyd URL. The token rides as a query parameter because a
   * browser cannot set headers on an iframe or a WebSocket handshake. */
  const termSrc = $derived(
    termStarted && termToken
      ? `/_emulator/portal/terminal/?token=${encodeURIComponent(termToken)}`
      : ''
  );

  api.get('/_emulator/portal/workspaces')
    .then((r) => (workspaces = r.value || []))
    .catch(() => {});

  // FRESH_MS is how long a write keeps a node bright. Long enough to notice in
  // a run, short enough that "recent" still means something afterwards.
  const FRESH_MS = 10000;

  /** inspect loads what a table holds now — the question that follows "it changed". */
  function inspect(n: Node) {
    selected = n;
    detail = null;
    detailError = '';
    // Nothing to read: the emulator does not hold this system's data, it only
    // knows the platform authenticated through this connection to reach it.
    if (n.source) return;
    if (!n.path.startsWith('Tables/')) return;
    api.get(`/_emulator/portal/table?itemId=${encodeURIComponent(n.itemId)}&table=${encodeURIComponent(n.path)}`)
      .then((d) => (detail = d))
      .catch((e) => (detailError = e.message));
  }

  // Optional args on purpose: the callers pass event fields, and every field
  // but seq/at/kind is omitted when it does not apply to that kind.
  function nodeKey(itemId?: string, path?: string) {
    return `${itemId}|${path}`;
  }

  // Every event kind is subscribed to; filtering happens here so toggling a
  // checkbox never loses history the stream already delivered.
  function connect() {
    if (source) return; // never open a second stream: events would double up
    source = new EventSource('/_emulator/events');
    source.onopen = () => {
      link = 'streaming';
      error = '';
    };
    source.onerror = () => {
      // EventSource reconnects on its own; onopen will say so.
      link = 'reconnecting';
    };
    source.onmessage = (m) => ingest(m.data);
    // EVERY kind, from the generated contract. The stream names each frame, so
    // a kind missing here never arrives — silently, which is why this list is
    // not written by hand.
    //
    // The cast is EventSource's own type map, which knows only message/open/
    // error. These frames are named by the server, so the listener sees a
    // MessageEvent that the DOM signature cannot promise.
    for (const k of EVENT_KINDS) {
      source.addEventListener(k, (m) => ingest((m as MessageEvent).data));
    }
  }

  function ingest(raw: string) {
    let ev: RawEmulatorEvent;
    try {
      ev = JSON.parse(raw);
    } catch {
      return;
    }
    // `kind` is read out FIRST so the narrowing below applies to a value the
    // compiler can track: the frame itself is parsed JSON and stays untrusted.
    const kind = ev.kind;
    if (!isEventKind(kind)) {
      unknown += 1;
      return;
    }
    if (kind === 'dropped') {
      dropped += ev.dropped ?? 0;
      return;
    }
    if (!isViewKind(kind)) {
      // Declared in Go, neither `dropped` nor renderable: nothing here knows
      // what to do with it. `impossible` makes that a BUILD failure — a kind
      // added to AllKinds without a home lands here, where its type is `never`
      // only for as long as one of the two lists covers everything.
      impossible(kind);
      unknown += 1;
      return;
    }
    const logged: LoggedEvent = { ...ev, kind };
    // Newest first, bounded: an unbounded log is how a long run turns a
    // debugging tool into a memory leak.
    events = [logged, ...events].slice(0, MAX_LOG);

    if (logged.kind === 'table') {
      touched = { ...touched, [nodeKey(logged.itemId, logged.table)]: Date.now() };
      // The open inspector is now stale — the table it describes just changed.
      if (
        selected &&
        nodeKey(selected.itemId, selected.path) === nodeKey(logged.itemId, logged.table)
      ) {
        inspect(selected);
      }
    }
    if (logged.kind === 'activity' && logged.status === 'Failed') {
      // Mark whatever this activity is known to write. The lineage edge is what
      // knows that — the event itself only names the activity.
      const next = { ...broken };
      for (const e of edges) {
        if (e.jobId === logged.jobId && e.activityName === logged.activityName) {
          next[nodeKey(e.targetItemId, e.targetPath)] = true;
        }
      }
      broken = next;
    }
    // A recorded movement changes the graph, so redraw as it happens rather
    // than at the end of a job — a warehouse build has no job to end.
    // Coalesced: one dbt model emits an edge per source, and reloading per
    // edge would be a request storm for one redraw.
    if (logged.kind === 'lineage') scheduleLineageReload();
    // A query is the last hop: light the model up as Power BI reads it.
    if (logged.kind === 'query') {
      touched = {
        ...touched,
        [nodeKey(logged.itemId, 'Tables/' + (logged.dataset || ''))]: Date.now(),
      };
      queried = { ...queried, [String(logged.itemId)]: Date.now() };
    }
    // A new run may have produced edges the graph has not seen yet.
    if (logged.kind === 'job' && logged.status !== 'Started') loadLineage();
  }

  $effect(() => {
    const t = setInterval(() => (now = Date.now()), 1000);
    connect();
    return () => {
      clearInterval(t);
      source?.close();
      source = null;
    };
  });

  function clear() {
    events = [];
    dropped = 0;
    touched = {};
    broken = {};
    selected = null;
    detail = null;
  }

  // ---- graph layout ----
  //
  // Nodes are laid out in columns by how far they are from a source, which on a
  // medallion draws itself: landing → bronze → silver → gold. Depth is computed
  // by relaxation rather than a topological sort, so a cyclic graph still
  // renders (bounded by the node count) instead of hanging.
  let graph = $derived.by(() => {
    const nodes = new Map<string, Node>();
    const add = (itemId: string, item: string, path: string, source?: boolean) => {
      const key = nodeKey(itemId, path);
      if (!nodes.has(key)) {
        nodes.set(key, { key, itemId, item, path, depth: 0, source: !!source, x: 0, y: 0 });
      }
      return key;
    };
    const links: Link[] = [];
    for (const e of edges) {
      // A source system has no path — the system IS the node — so it keys on
      // the connection id alone. Marked `source` so it can be drawn as what it
      // is: something outside Fabric that the platform reads FROM, never a
      // table anyone can click into.
      const src = e.sourceKind === 'connection';
      const from = add(e.sourceItemId, e.sourceItem, e.sourcePath, src);
      const to = add(e.targetItemId, e.targetItem, e.targetPath);
      links.push({ from, to, producer: e.producer, activityName: e.activityName });
    }
    for (let i = 0; i < nodes.size; i++) {
      let moved = false;
      for (const l of links) {
        // Both ends were put in the map by `add` above, which is what returned
        // the keys this link is built from.
        const a = nodes.get(l.from)!;
        const b = nodes.get(l.to)!;
        if (b.depth < a.depth + 1) {
          b.depth = a.depth + 1;
          moved = true;
        }
      }
      if (!moved) break;
    }
    const byDepth = new Map<number, Node[]>();
    for (const n of nodes.values()) {
      const col: Node[] = byDepth.get(n.depth) || [];
      col.push(n);
      byDepth.set(n.depth, col);
    }
    const colW = 220;
    const rowH = 64;
    for (const [depth, col] of byDepth) {
      col.forEach((n, i) => {
        n.x = 20 + depth * colW;
        n.y = 20 + i * rowH;
      });
    }
    const width = 40 + (Math.max(0, ...[...byDepth.keys()]) + 1) * colW;
    const height = 40 + Math.max(1, ...[...byDepth.values()].map((c) => c.length)) * rowH;
    return { nodes: [...nodes.values()], links, width, height };
  });

  function label(n: Node) {
    // A source system has no path — its name is the connection's display name,
    // and falling through to the path logic would label it with an empty
    // string or a raw GUID.
    if (n.source) return n.item || n.itemId.slice(0, 8);
    const leaf = n.path.split('/').filter(Boolean).pop() || n.path;
    return leaf;
  }

  // Two levels, because they answer different questions: what just changed,
  // and what this session has written at all.
  function nodeClass(n: Node) {
    let c = 'node';
    if (n.source) c += ' source';
    if (broken[n.key]) c += ' broken';
    else if (touched[n.key]) c += now - touched[n.key] < FRESH_MS ? ' fresh' : ' written';
    if (queried[n.itemId] && now - queried[n.itemId] < FRESH_MS) c += ' queried';
    if (selected && selected.key === n.key) c += ' selected';
    return c;
  }

  function edgePath(l: Link) {
    const a = graph.nodes.find((n) => n.key === l.from);
    const b = graph.nodes.find((n) => n.key === l.to);
    if (!a || !b) return '';
    const x1 = a.x + 160;
    const y1 = a.y + 18;
    const x2 = b.x;
    const y2 = b.y + 18;
    const mid = (x1 + x2) / 2;
    return `M${x1},${y1} C${mid},${y1} ${mid},${y2} ${x2},${y2}`;
  }

  let shown = $derived(
    events.filter((e) => kinds[e.kind] && (!workspace || e.workspaceId === workspace)),
  );

  function fmt(epoch: number) {
    return new Date(epoch * 1000).toISOString().replace('T', ' ').slice(11, 19);
  }

  // EXHAUSTIVE, and enforced. The switch covers every ViewKind; the default arm
  // narrows `ev.kind` to `never`, so declaring a kind in Go and regenerating the
  // contract fails this build until this function says what it looks like. The
  // old JavaScript had `default: return ev.kind`, which printed a bare kind name
  // in the What column and read as a rendered row.
  function describe(ev: LoggedEvent): string {
    switch (ev.kind) {
      case 'file':
        return `${ev.eventType?.split('.').pop() || 'File'} ${ev.path}`;
      case 'table':
        return `${ev.table} → v${ev.version}` +
          (ev.rowsAdded ? ` (+${ev.rowsAdded} rows)` : '') +
          (ev.filesRemoved ? `, ${ev.filesRemoved} file(s) removed` : '');
      case 'activity':
        return `${ev.activityName} (${ev.activityType}) ${ev.status}` +
          (ev.error ? ` — ${ev.error}` : '');
      case 'job':
        return `job ${ev.status}` + (ev.failureReason ? ` — ${ev.failureReason}` : '');
      case 'lineage': {
        // A source system has no path, so the template would print
        // "undefined". Its name comes from the graph's edges, which the view
        // already reloads when a lineage event lands.
        if (ev.sourceKind === 'connection') {
          const named = edges.find((e) => e.sourceItemId === ev.sourceItemId);
          const who = named?.sourceItem || ev.sourceItemId?.slice(0, 8) || 'source system';
          return `${who} (source system) → ${ev.targetPath}`;
        }
        return `${ev.sourcePath} → ${ev.targetPath}`;
      }
      case 'query':
        return `${ev.dataset || 'model'} queried` +
          ((ev.queries ?? 0) > 1 ? ` (${ev.queries} queries)` : '') +
          (ev.status === 'Failed' ? ' — failed' : '');
      default:
        impossible(ev.kind);
        return ev.kind;
    }
  }

  function who(ev: LoggedEvent): string {
    if (ev.kind === 'lineage') return ev.activityName || ev.producer || '';
    if (ev.kind === 'query') return 'Power BI';
    const a = ev.attribution;
    if (!a) return '';
    if (a.activityName) return a.activityName;
    if (a.cellIndex !== undefined && a.cellIndex !== null) return `cell[${a.cellIndex}]`;
    return a.jobId ? a.jobId.slice(0, 8) : '';
  }
</script>

<h1>Data flow</h1>
<p class="muted">
  Every byte that moves through OneLake, live. The emulator owns its storage
  layer, so a write is seen at the source whoever made it — an ADLS client,
  azcopy, delta-rs, Sail, a Copy activity, the mirror writer. Tail it without a
  browser with <code>curl -N /_emulator/events</code>.
</p>

<div class="mt-4 flex flex-wrap items-center gap-2">
  <span class="chip {link === 'streaming' ? 'completed' : link === 'connecting' ? 'notstarted' : 'failed'}">{link}</span>
  {#if dropped > 0}
    <span class="chip failed" title="This browser fell behind; the emulator was never slowed down.">
      {dropped} event(s) dropped
    </span>
  {/if}
  {#if unknown > 0}
    <span
      class="chip failed"
      title="The emulator sent a kind this portal was not built for. It is embedded in the binary, so this should be impossible — say so rather than swallowing it."
    >
      {unknown} event(s) of an unknown kind
    </span>
  {/if}
  <Button variant="outline" size="sm" onclick={clear}>Clear</Button>
  <Button variant="outline" size="sm" onclick={loadLineage}>Reload graph</Button>
  {#if termAvailable}
    <Button variant="outline" size="sm" onclick={() => (termOpen = !termOpen)}>
      {termOpen ? 'Hide terminal' : 'Terminal'}
    </Button>
  {/if}
</div>

<!-- ONE SCREEN, TWO HALVES, once the pane is live. The point of the terminal
     is driving the pipeline while watching it run — and that claim is only
     true if the command, the graph and the event stream are visible at the
     same time. Until the pane connects (or when there is none) this wrapper
     is a plain column and changes nothing. -->
<div class="flow-body" class:split={termAvailable && termOpen && termStarted}>
<div class="flow-side">
{#if termAvailable && termOpen}
  <section class="terminal-pane mt-4">
    {#if !termStarted}
      <p class="muted">
        The emulator prints a terminal token once at startup
        (<code>fabric-emulator terminal pane enabled</code>). Paste it here.
        It is deliberately not served by any endpoint — the portal is
        unauthenticated, so an endpoint that handed it out would be the same as
        having no token.
      </p>
      <form
        class="mt-2 flex flex-wrap items-center gap-2"
        onsubmit={(e) => {
          e.preventDefault();
          if (termToken.trim()) termStarted = true;
        }}
      >
        <!-- svelte-ignore a11y_autofocus -->
        <input
          class="term-token"
          type="password"
          placeholder="terminal token"
          bind:value={termToken}
          aria-label="terminal token"
        />
        <Button type="submit" variant="outline" size="sm" disabled={!termToken.trim()}>
          Connect
        </Button>
      </form>
    {:else}
      <div class="flex items-center justify-between">
        <span class="muted">Terminal — a shell in the emulator's stack.</span>
        <Button
          variant="outline"
          size="sm"
          onclick={() => {
            termStarted = false;
            termToken = '';
          }}>Disconnect</Button
        >
      </div>
      <iframe class="term-frame mt-2" src={termSrc} title="Terminal"></iframe>
    {/if}
  </section>
{:else if termReason}
  <!-- Configured but unreachable: say which knob to turn rather than showing a
       toggle that fails when clicked. -->
  <p class="muted mt-2">Terminal unavailable — {termReason}</p>
{/if}
</div>
<div class="flow-main">

{#if error}<p class="error">{error}</p>{/if}

<h2>Graph</h2>
{#if graph.nodes.length === 0}
  <p class="muted">
    No lineage recorded yet. Run a pipeline with a Copy activity, or report a
    notebook run, and its source → target edges appear here.
  </p>
{:else}
  <div class="graph-scroll">
    <svg width={graph.width} height={graph.height} role="img" aria-label="Data flow graph">
      {#each graph.links as l}
        <path class="link {l.producer?.toLowerCase() || ''}" d={edgePath(l)} />
      {/each}
      {#each graph.nodes as n}
        {#if modelIds.has(n.itemId)}
          <!-- A semantic model goes to its own page rather than the inspector:
               what a reader wants from this node is the model's tables and DAX,
               and that now has an address. An <a> so it opens in a new tab and
               lands in browser history like any other link.

               The transform sits on a wrapping <g>, where a group's placement
               belongs anyway: `<a>` inside an <svg> is still typed as the HTML
               anchor, which has no `transform`. -->
          <g transform="translate({n.x},{n.y})">
            <a
              class={nodeClass(n) + ' linked'}
              href={modelHref('models', n.itemId)}
              aria-label={`Open semantic model ${label(n)}`}
            >
              <rect width="160" height="36" rx="6" />
              <text x="10" y="15">{label(n)}</text>
              <text class="sub" x="10" y="28">semantic model →</text>
            </a>
          </g>
        {:else}
          <g
            class={nodeClass(n)}
            transform="translate({n.x},{n.y})"
            role="button"
            tabindex="0"
            aria-label={n.source ? `Source system ${label(n)}` : `Inspect ${n.path}`}
            onclick={() => inspect(n)}
            onkeydown={(e) => (e.key === 'Enter' || e.key === ' ') && inspect(n)}
          >
            <rect width="160" height="36" rx="6" />
            <text x="10" y="15">{label(n)}</text>
            <text class="sub" x="10" y="28">{n.source ? 'source system' : n.item || n.itemId.slice(0, 8)}</text>
          </g>
        {/if}
      {/each}
    </svg>
  </div>
  <p class="muted mt-2">Select a node to see what it holds now.</p>
{/if}

{#if selected}
  <div class="panel">
    <div class="flex flex-wrap items-baseline gap-2">
      <strong class="text-base">{selected.path}</strong>
      <span class="muted">{selected.item || selected.itemId}</span>
      {#if detail && detail.version >= 0}
        <span class="chip completed">v{detail.version}</span>
      {/if}
      {#if detail?.readable}
        <span class="muted">{detail.rowCount} row{detail.rowCount === 1 ? '' : 's'}</span>
      {/if}
    </div>

    {#if detailError}
      <p class="error mt-2">{detailError}</p>
    {:else if selected.source}
      <p class="muted mt-2">
        A source system, reached through this connection. The emulator holds no
        data for it — it records that the platform read FROM here, which is why
        the medallion starts at a vendor rather than at a file in
        <code>Files/landing</code>.
      </p>
    {:else if !selected.path.startsWith('Tables/')}
      <p class="muted mt-2">
        A file in OneLake, not a Delta table — the flow stream reports its
        writes, but there is no schema to read.
      </p>
    {:else if detail === null}
      <p class="muted mt-2">Reading…</p>
    {:else if !detail.readable}
      <p class="muted mt-2">Not readable yet: {detail.message}</p>
    {:else}
      <div class="graph-scroll mt-3">
        <table>
          <thead>
            <tr>{#each detail.columns as c}<th>{c}</th>{/each}</tr>
          </thead>
          <tbody>
            {#each detail.preview as row}
              <tr>{#each row as cell}<td class="mono">{cell}</td>{/each}</tr>
            {/each}
          </tbody>
        </table>
      </div>
      {#if detail.truncated}
        <p class="muted mt-2">
          First {detail.preview.length} of {detail.rowCount} rows.
        </p>
      {/if}
    {/if}
  </div>
{/if}

<h2>Events</h2>
<div class="filters">
  {#each VIEW_KINDS as k}
    <label title={KIND_DOC[k]}><input type="checkbox" bind:checked={kinds[k]} /> {k}</label>
  {/each}
  {#if workspaces.length > 1}
    <label>
      workspace
      <select bind:value={workspace} class="w-48">
        <option value="">all</option>
        {#each workspaces as w}<option value={w.id}>{w.displayName}</option>{/each}
      </select>
    </label>
  {/if}
</div>

{#if shown.length === 0}
  <p class="muted">Nothing yet — start a job, or upload a file to OneLake.</p>
{:else}
  <table>
    <thead>
      <tr><th>Time</th><th>Kind</th><th>What</th><th>By</th></tr>
    </thead>
    <tbody>
      {#each shown as ev (ev.seq)}
        <tr class={ev.status === 'Failed' ? 'failed-row' : ''}>
          <td class="mono">{fmt(ev.at)}</td>
          <td><span class="chip {ev.kind}">{ev.kind}</span></td>
          <td>{describe(ev)}</td>
          <td class="mono muted">{who(ev)}</td>
        </tr>
      {/each}
    </tbody>
  </table>
{/if}
</div>
</div>

<style>
  /* The split: terminal on the left, graph and events on the right, one
     viewport. Grid rather than flex so the two columns keep their ratio as
     the graph grows, and min widths so neither side collapses into a strip. */
  .flow-body.split {
    display: grid;
    grid-template-columns: minmax(430px, 2fr) minmax(560px, 3fr);
    gap: 1.25rem;
    align-items: start;
  }
  /* The terminal stays put while the right column scrolls: the whole point of
     the pane is that the command never leaves the screen. */
  .flow-body.split .flow-side {
    position: sticky;
    top: 3.5rem;
  }
  /* Tall in split mode — the 22rem default is sized for a pane stacked above
     the graph, not beside it. */
  .flow-body.split .term-frame {
    height: calc(100vh - 15rem);
    min-height: 24rem;
  }
  .terminal-pane {
    border: 1px solid var(--border, #333);
    border-radius: 6px;
    padding: 0.75rem;
  }
  .term-frame {
    width: 100%;
    height: 22rem;
    border: 0;
    background: #000;
  }
  .term-token {
    font-family: ui-monospace, SFMono-Regular, Menlo, monospace;
    min-width: 22rem;
    padding: 0.35rem 0.5rem;
    border: 1px solid var(--border, #333);
    border-radius: 4px;
    background: transparent;
    color: inherit;
  }
  @reference '../src/app.css';

  .graph-scroll {
    @apply overflow-x-auto rounded-lg border bg-card p-2;
  }
  .link {
    fill: none;
    stroke: var(--border);
    stroke-width: 1.5;
  }
  /* Solid where the emulator moved the bytes itself, dashed where an engine
     reported the movement — the same distinction lineage records as `producer`.
     A warehouse edge is solid: the TDS front watched the engine accept the
     statement, so it is evidence, not a claim. */
  .link.notebook,
  .link.reported {
    stroke-dasharray: 4 3;
  }
  .link.warehouse,
  .link.notebookobserved {
    stroke: var(--success);
  }
  /* Direct Lake is a binding, not a copy: the model reads the Delta where it
     lies, so the hop is drawn but never as bytes moving. */
  .link.directlake {
    stroke-dasharray: 1 3;
  }
  .node rect {
    fill: var(--muted);
    stroke: var(--border);
  }
  /* A source system is not a table in the lakehouse and should not look like
     one: dashed, because what is inside it is outside this emulator. */
  .node.source rect {
    fill: var(--background);
    stroke: var(--primary);
    stroke-dasharray: 4 3;
  }
  .node text {
    fill: var(--foreground);
    font-size: 12px;
  }
  .node .sub {
    fill: var(--muted-foreground);
    font-size: 10px;
  }
  .node {
    cursor: pointer;
  }
  .node:focus-visible rect {
    outline: 2px solid var(--ring);
    outline-offset: 2px;
  }
  /* Just written: bright. Written earlier this session: a quieter mark, so a
     finished run still shows its shape without everything shouting. */
  .node.fresh rect {
    fill: var(--success-bg);
    stroke: var(--success);
  }
  /* The last hop: a model Power BI just read. A query moves nothing, so it
     gets its own mark rather than the write colours. */
  .node.queried rect {
    fill: var(--muted);
    stroke: var(--ring);
    stroke-width: 2;
  }
  .node.written rect {
    fill: var(--muted);
    stroke: var(--success);
  }
  .node.selected rect {
    stroke: var(--primary);
    stroke-width: 2;
  }
  .node.broken rect {
    fill: var(--danger-bg);
    stroke: var(--danger);
  }
  .filters {
    @apply flex flex-wrap items-center gap-4;
  }
  .filters label {
    @apply m-0 inline-flex items-center gap-1.5 font-normal;
  }
  .filters input[type='checkbox'] {
    @apply m-0 h-4 w-4;
  }
  .chip.file {
    @apply bg-muted text-muted-foreground border-transparent;
  }
  .chip.table {
    @apply bg-[var(--success-bg)] text-success border-transparent;
  }
  .chip.activity {
    @apply bg-[var(--caution-bg)] text-caution border-transparent;
  }
  .chip.job {
    @apply bg-accent text-accent-foreground border-transparent;
  }
  .failed-row td {
    @apply bg-[var(--danger-bg)];
  }
</style>
