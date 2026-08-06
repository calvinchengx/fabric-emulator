import { fireEvent, render, screen, waitFor } from '@testing-library/svelte';
import { vi, describe, it, expect, beforeEach, afterEach } from 'vitest';
import Flow from './Flow.svelte';
import { EVENT_KINDS, VIEW_KINDS } from './eventKinds';
import { FakeEventSource, errRes, fetchCalls, groupOf, installEventSource, removeEventSource, res, stream } from './testing';
const edges = [
  {
    jobId: 'job-1', activityName: 'IngestCustomers', producer: 'Copy',
    sourceItemId: 'lake-1', sourceItem: 'lake', sourcePath: 'Files/landing/customers.csv',
    targetItemId: 'lake-1', targetItem: 'lake', targetPath: 'Tables/bronze_customers',
    createdAt: 1700000000,
  },
  {
    jobId: 'job-2', activityName: 'Refine', producer: 'Notebook',
    sourceItemId: 'lake-1', sourceItem: 'lake', sourcePath: 'Tables/bronze_customers',
    targetItemId: 'lake-1', targetItem: 'lake', targetPath: 'Tables/silver_customers',
    createdAt: 1700000100,
  },
];

// The view calls several endpoints; route by URL so a test can say what each
// one returns without the others interfering.
function mockApi({
  lineage = edges,
  workspaces = [],
  table = null,
  terminal = null,
  models = [],
}: any = {}) {
  vi.spyOn(globalThis, 'fetch').mockImplementation((url: RequestInfo | URL) => {
    // The terminal status route 404s when no terminal is configured, which is
    // the ORDINARY case and must not surface as an error.
    if (String(url).includes('/portal/terminal/status')) {
      if (!terminal) {
        return Promise.resolve(res({ error: { code: 'NotFound' } }, { ok: false, status: 404 }));
      }
      return Promise.resolve(res(terminal));
    }
    const body = String(url).includes('/portal/lineage')
      ? { value: lineage }
      : String(url).includes('/portal/workspaces')
        ? { value: workspaces }
        : String(url).includes('/portal/models')
          ? { value: models }
          : String(url).includes('/portal/table')
            ? table
            : { value: [] };
    return Promise.resolve(res(body));
  });
}

function mockLineage(value: any = edges) {
  mockApi({ lineage: value });
}

describe('Flow', () => {
  beforeEach(() => {
    vi.restoreAllMocks();
    installEventSource();
  });
  afterEach(() => {
    removeEventSource();
  });

  it('says it is connecting before claiming to be disconnected', async () => {
    mockLineage();
    render(Flow);
    // The window before the first connection resolves must not read as an error.
    await waitFor(() => expect(screen.getByText('connecting')).toBeInTheDocument());
    expect(screen.queryByText('disconnected')).not.toBeInTheDocument();
    stream().open();
    await waitFor(() => expect(screen.getByText('streaming')).toBeInTheDocument());
  });

  it('draws the lineage graph, layering sources before their targets', async () => {
    mockLineage();
    render(Flow);
    // Nodes are labelled by the leaf of their path, over the owning item.
    await waitFor(() => expect(screen.getByText('bronze_customers')).toBeInTheDocument());
    expect(screen.getByText('customers.csv')).toBeInTheDocument();
    expect(screen.getByText('silver_customers')).toBeInTheDocument();

    // landing → bronze → silver must land in three successive columns, which is
    // what makes a medallion draw itself.
    const xOf = (label: string) => {
      const g = groupOf(screen.getByText(label));
      return Number(g.getAttribute('transform')!.match(/translate\((-?\d+)/)![1]);
    };
    expect(xOf('customers.csv')).toBeLessThan(xOf('bronze_customers'));
    expect(xOf('bronze_customers')).toBeLessThan(xOf('silver_customers'));
  });

  it('shows the empty state before anything has run', async () => {
    mockLineage([]);
    render(Flow);
    await waitFor(() => expect(screen.getByText(/No lineage recorded yet/)).toBeInTheDocument());
  });

  it('logs table events and lights up the node they landed on', async () => {
    mockLineage();
    render(Flow);
    await waitFor(() => expect(screen.getByText('bronze_customers')).toBeInTheDocument());

    stream().open();
    await waitFor(() => expect(screen.getByText('streaming')).toBeInTheDocument());

    stream().emit('table', {
      seq: 1, at: 1700000200, kind: 'table', itemId: 'lake-1',
      table: 'Tables/bronze_customers', version: 3, rowsAdded: 1203, filesAdded: 1,
      attribution: { jobId: 'job-1', activityName: 'IngestCustomers' },
    });

    await waitFor(() =>
      expect(screen.getByText(/Tables\/bronze_customers → v3 \(\+1203 rows\)/)).toBeInTheDocument());
    // The attribution column names what caused it.
    expect(screen.getAllByText('IngestCustomers').length).toBeGreaterThan(0);
    // …and the node is marked as touched.
    const node = groupOf(screen.getByText('bronze_customers'));
    await waitFor(() => expect(node.getAttribute('class')).toContain('fresh'));
  });

  it('marks a failing activity red on the table it writes', async () => {
    mockLineage();
    render(Flow);
    await waitFor(() => expect(screen.getByText('bronze_customers')).toBeInTheDocument());
    stream().open();

    stream().emit('activity', {
      seq: 2, at: 1700000300, kind: 'activity', jobId: 'job-1',
      activityName: 'IngestCustomers', activityType: 'Copy', status: 'Failed',
      error: 'the source file is empty',
    });

    await waitFor(() => expect(screen.getByText(/the source file is empty/)).toBeInTheDocument());
    const node = groupOf(screen.getByText('bronze_customers'));
    await waitFor(() => expect(node.getAttribute('class')).toContain('broken'));
  });

  it('reports dropped events rather than hiding a gap', async () => {
    mockLineage();
    render(Flow);
    await waitFor(() => expect(screen.getByText('bronze_customers')).toBeInTheDocument());
    stream().open();

    stream().emit('dropped', { seq: 3, at: 1700000400, kind: 'dropped', dropped: 12 });
    await waitFor(() => expect(screen.getByText(/12 event\(s\) dropped/)).toBeInTheDocument());
  });

  it('filters by kind without losing events already received', async () => {
    mockLineage();
    render(Flow);
    await waitFor(() => expect(screen.getByText('bronze_customers')).toBeInTheDocument());
    stream().open();

    // `file` is off by default: the firehose is opt-in.
    stream().emit('file', {
      seq: 4, at: 1700000500, kind: 'file',
      eventType: 'Microsoft.Fabric.OneLake.FileCreated', path: 'Files/landing/customers.csv',
    });
    stream().emit('job', {
      seq: 5, at: 1700000501, kind: 'job', jobId: 'job-1', status: 'Started',
    });
    await waitFor(() => expect(screen.getByText('job Started')).toBeInTheDocument());
    expect(screen.queryByText(/FileCreated Files\/landing/)).not.toBeInTheDocument();

    // Turning `file` on reveals the event that already arrived.
    await fireEvent.click(screen.getByLabelText('file'));
    await waitFor(() =>
      expect(screen.getByText(/FileCreated Files\/landing\/customers.csv/)).toBeInTheDocument());
  });

  it('surfaces a lineage load error', async () => {
    vi.spyOn(globalThis, 'fetch').mockResolvedValue(errRes('db gone', 500));
    render(Flow);
    await waitFor(() => expect(screen.getByText('db gone')).toBeInTheDocument());
  });
  it('inspects a table node: what it holds, not just that it changed', async () => {
    mockApi({
      table: {
        itemId: 'lake-1', table: 'Tables/bronze_customers', version: 4, readable: true,
        columns: ['id', 'name'], rowCount: 1203,
        preview: [['1', 'ada'], ['2', 'grace']], truncated: true,
      },
    });
    render(Flow);
    await waitFor(() => expect(screen.getByText('bronze_customers')).toBeInTheDocument());

    await fireEvent.click(groupOf(screen.getByText('bronze_customers')));

    await waitFor(() => expect(screen.getByText('v4')).toBeInTheDocument());
    expect(screen.getByText('1203 rows')).toBeInTheDocument();
    // The schema and a sample of the data — the question that follows "it changed".
    expect(screen.getByText('id')).toBeInTheDocument();
    expect(screen.getByText('ada')).toBeInTheDocument();
    expect(screen.getByText(/First 2 of 1203 rows/)).toBeInTheDocument();
  });

  it('says plainly when a node is a file rather than a table', async () => {
    mockApi({});
    render(Flow);
    await waitFor(() => expect(screen.getByText('customers.csv')).toBeInTheDocument());
    await fireEvent.click(groupOf(screen.getByText('customers.csv')));
    await waitFor(() =>
      expect(screen.getByText(/no schema to read/)).toBeInTheDocument());
  });

  it('reports a table whose first commit has not landed', async () => {
    mockApi({
      table: { itemId: 'lake-1', table: 'Tables/bronze_customers', version: -1,
        readable: false, message: 'has no active data files' },
    });
    render(Flow);
    await waitFor(() => expect(screen.getByText('bronze_customers')).toBeInTheDocument());
    await fireEvent.click(groupOf(screen.getByText('bronze_customers')));
    await waitFor(() =>
      expect(screen.getByText(/Not readable yet/)).toBeInTheDocument());
  });

  it('filters the log by workspace as well as by kind', async () => {
    mockApi({ workspaces: [{ id: 'ws-1', displayName: 'Analytics' }, { id: 'ws-2', displayName: 'Sandbox' }] });
    render(Flow);
    await waitFor(() => expect(screen.getByText('bronze_customers')).toBeInTheDocument());
    stream().open();

    stream().emit('job', {
      seq: 1, at: 1700000000, kind: 'job', workspaceId: 'ws-1', jobId: 'a', status: 'Started',
    });
    stream().emit('activity', {
      seq: 2, at: 1700000001, kind: 'activity', workspaceId: 'ws-2',
      activityName: 'OtherWorkspaceStep', activityType: 'Wait', status: 'Succeeded',
    });
    await waitFor(() => expect(screen.getByText(/OtherWorkspaceStep/)).toBeInTheDocument());

    // Narrowing to one workspace hides the other's events without dropping them.
    const select = screen.getByLabelText(/workspace/) as HTMLSelectElement;
    select.value = 'ws-1';
    select.dispatchEvent(new Event('change', { bubbles: true }));
    await waitFor(() => expect(screen.queryByText(/OtherWorkspaceStep/)).not.toBeInTheDocument());
    expect(screen.getByText('job Started')).toBeInTheDocument();
  });
});

// The full chain — landing → bronze → silver → gold → semantic model — only
// draws if the view understands the hops that are not OneLake writes: a
// warehouse build (no job to end), and a Power BI read (no movement at all).
describe('Flow: the warehouse and Power BI hops', () => {
  beforeEach(() => {
    vi.restoreAllMocks();
    installEventSource();
  });
  afterEach(() => {
    removeEventSource();
  });

  const goldEdges = [
    ...edges,
    {
      jobId: '', activityName: 'CTAS', producer: 'Warehouse',
      sourceItemId: 'lake-1', sourceItem: 'lake', sourcePath: 'Tables/silver_customers',
      targetItemId: 'wh-1', targetItem: 'dw', targetPath: 'Tables/dim_customer',
      createdAt: 1700000200,
    },
    {
      jobId: '', activityName: 'DirectLake', producer: 'DirectLake',
      sourceItemId: 'wh-1', sourceItem: 'dw', sourcePath: 'Tables/dim_customer',
      targetItemId: 'sm-1', targetItem: 'ContosoRevenue', targetPath: 'Tables/Customer',
      createdAt: 1700000300,
    },
  ];

  it('draws gold and the semantic model as further columns of the same chain', async () => {
    mockLineage(goldEdges);
    render(Flow);
    await waitFor(() => expect(screen.getByText('dim_customer')).toBeInTheDocument());
    expect(screen.getByText('Customer')).toBeInTheDocument();

    const xOf = (label: string) => {
      const g = groupOf(screen.getByText(label));
      return Number(g.getAttribute('transform')!.match(/translate\((-?\d+)/)![1]);
    };
    // Source → landing → bronze → silver → gold → model, left to right.
    expect(xOf('silver_customers')).toBeLessThan(xOf('dim_customer'));
    expect(xOf('dim_customer')).toBeLessThan(xOf('Customer'));
  });

  // The Power BI hop was a dead end until `#models/{id}` existed: the graph lit
  // a node up when a client queried it and there was nowhere to go from there.
  // Only nodes the models endpoint claims are drawn as links, so this needs the
  // endpoint to answer — the plain-node case above proves the other branch.
  it('links a node the models endpoint claims, and lays it out like any other', async () => {
    mockApi({ lineage: goldEdges, models: [{ itemId: 'sm-1', displayName: 'ContosoRevenue' }] });
    render(Flow);
    const link = await waitFor(() =>
      screen.getByRole('link', { name: 'Open semantic model Customer' }),
    );
    expect(link.getAttribute('href')).toBe('#models/sm-1');
    // The transform sits on a wrapping <g>, not on the <a>: inside an <svg> an
    // <a> is still typed as the HTML anchor, which has no transform. Asserted
    // because moving it silently stacks every linked node at the origin.
    expect(link.closest('g')!.getAttribute('transform')).toMatch(/^translate\(\d+,\d+\)$/);
    // A model node is not clickable into the inspector — it goes to its page.
    expect(screen.queryByText('Select a node to see what it holds now.')).toBeInTheDocument();
  });

  it('redraws when a movement is recorded, without waiting for a job to end', async () => {
    // A warehouse build has no job, so the old "reload when a job finishes"
    // rule would never fire and gold would not appear until a manual reload.
    let served = edges;
    vi.spyOn(globalThis, 'fetch').mockImplementation((url: RequestInfo | URL) => {
      const body = String(url).includes('/portal/lineage') ? { value: served } : { value: [] };
      return Promise.resolve(res(body));
    });
    render(Flow);
    await waitFor(() => expect(screen.getByText('silver_customers')).toBeInTheDocument());
    expect(screen.queryByText('dim_customer')).not.toBeInTheDocument();

    served = goldEdges;
    stream().emit('lineage', {
      seq: 9, at: 1700000200, kind: 'lineage', workspaceId: 'ws-1', itemId: 'wh-1',
      sourceItemId: 'lake-1', sourcePath: 'Tables/silver_customers',
      targetPath: 'Tables/dim_customer', producer: 'Warehouse', activityName: 'CTAS',
    });
    await waitFor(() => expect(screen.getByText('dim_customer')).toBeInTheDocument(), { timeout: 3000 });
    // And the movement is in the log, named end to end.
    expect(screen.getByText('Tables/silver_customers → Tables/dim_customer')).toBeInTheDocument();
  });

  it('logs a Power BI query as the last hop', async () => {
    mockLineage(goldEdges);
    render(Flow);
    await waitFor(() => expect(screen.getByText('Customer')).toBeInTheDocument());
    stream().emit('query', {
      seq: 10, at: 1700000400, kind: 'query', workspaceId: 'ws-1', itemId: 'sm-1',
      dataset: 'ContosoRevenue', queries: 2, status: 'Completed',
    });
    await waitFor(() =>
      expect(screen.getByText('ContosoRevenue queried (2 queries)')).toBeInTheDocument());
    expect(screen.getByText('Power BI')).toBeInTheDocument();
  });

  // A medallion does not begin in Fabric. Before source-kind existed the first
  // node was whatever file already sat in Files/landing, and the vendor that
  // put it there could not be drawn at all.
  it('draws a source system as its connection, not as a table', async () => {
    mockLineage([
      {
        sourceItemId: 'conn-pos',
        sourceItem: 'contoso-pos-api',
        sourcePath: '',
        sourceKind: 'connection',
        targetItemId: 'lake',
        targetItem: 'contoso_lake',
        targetPath: 'Files/landing/pos/customers.csv',
        producer: 'Reported',
        activityName: 'ingest_pos',
        createdAt: 1700000200,
      },
    ]);
    render(Flow);
    // Labelled by the connection's display name — a path-derived label would be
    // empty here, since a source system has no path.
    const node = await screen.findByLabelText(/contoso-pos-api/);
    expect(node).toBeTruthy();
    expect(node.getAttribute('class')).toContain('source');
    expect(screen.getByText('source system')).toBeTruthy();
  });

  it('does not try to read a table for a source system', async () => {
    mockApi({
      lineage: [
        {
          sourceItemId: 'conn-pos',
          sourceItem: 'contoso-pos-api',
          sourcePath: '',
          sourceKind: 'connection',
          targetItemId: 'lake',
          targetItem: 'contoso_lake',
          targetPath: 'Files/landing/pos/customers.csv',
          producer: 'Reported',
          activityName: 'ingest_pos',
          createdAt: 1700000200,
        },
      ],
    });
    render(Flow);
    const node = await screen.findByLabelText(/contoso-pos-api/);
    await fireEvent.click(node);
    // The emulator holds no data for a system outside it, so the inspector
    // explains rather than erroring or firing a /portal/table request.
    expect(await screen.findByText(/The emulator holds no data for it/)).toBeTruthy();
    const asked = fetchCalls().some((c) => String(c[0]).includes('/portal/table'));
    expect(asked).toBe(false);
  });

  // A source system has no path, so the lineage event's sourcePath is empty and
  // the log rendered "undefined → Files/landing/…". Caught by looking at the
  // screen, not by a test — the graph handled it and the event log did not.
  it('names the source system in the event log rather than printing undefined', async () => {
    mockLineage([
      {
        sourceItemId: 'conn-pos',
        sourceItem: 'contoso-pos-api',
        sourcePath: '',
        sourceKind: 'connection',
        targetItemId: 'lake',
        targetItem: 'contoso_lake',
        targetPath: 'Files/landing/pos/customers.csv',
        producer: 'Reported',
        activityName: 'ingest_pos',
        createdAt: 1700000200,
      },
    ]);
    render(Flow);
    await screen.findByLabelText(/contoso-pos-api/);
    stream().emit('lineage', {
      seq: 9,
      at: 1700000200,
      kind: 'lineage',
      sourceItemId: 'conn-pos',
      sourceKind: 'connection',
      targetPath: 'Files/landing/pos/customers.csv',
      producer: 'Reported',
      activityName: 'ingest_pos',
    });
    const row = await screen.findByText(/contoso-pos-api \(source system\) → Files\/landing\/pos\/customers.csv/)!;
    expect(row).toBeTruthy();
    expect(screen.queryByText(/undefined/)).toBeNull();
  });
});

describe('terminal pane', () => {
  beforeEach(() => {
    vi.restoreAllMocks();
    installEventSource();
  });
  afterEach(() => {
    removeEventSource();
  });

  // No terminal configured is the DEFAULT shape of this product: the route is
  // not mounted, the status call 404s, and the view must show nothing at all —
  // no toggle, and no error either, because nothing is wrong.
  it('offers nothing when no terminal is configured', async () => {
    mockApi({});
    render(Flow);
    await waitFor(() => expect(screen.getByText('Graph')).toBeInTheDocument());
    expect(screen.queryByRole('button', { name: 'Terminal' })).toBeNull();
    expect(screen.queryByText(/Terminal unavailable/)).toBeNull();
  });

  // Configured but unreachable — the `terminal` profile is off. The view must
  // say which knob to turn rather than offering a toggle that fails when
  // clicked. This is the spark-agent profile bug's shape, refused in the UI.
  it('names the reason when configured but unreachable', async () => {
    mockApi({
      terminal: { available: false, reason: 'no terminal at ttyd:7681 — is the `terminal` compose profile enabled?' },
    });
    render(Flow);
    await waitFor(() =>
      expect(screen.getByText(/Terminal unavailable/)).toBeInTheDocument(),
    );
    expect(screen.getByText(/compose profile enabled/)).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Terminal' })).toBeNull();
  });

  // Available: a toggle appears, and opening it asks for the token rather than
  // connecting. The token is never fetched — the portal is unauthenticated, so
  // serving it would be the same as having none.
  it('asks for the token instead of fetching one', async () => {
    mockApi({ terminal: { available: true } });
    render(Flow);
    const toggle = await screen.findByRole('button', { name: 'Terminal' });
    await fireEvent.click(toggle);

    expect(screen.getByLabelText('terminal token')).toBeInTheDocument();
    // Nothing is framed until a token is supplied: an iframe with no token
    // would just 401 in a way the user cannot see.
    expect(document.querySelector('iframe.term-frame')!).toBeNull();
    // And no request ever asked the emulator for a token.
    const asked = fetchCalls().map(([u]) => String(u));
    expect(asked.some((u) => /token/i.test(u))).toBe(false);
  });

  it('frames ttyd only once a token is entered, and carries it in the URL', async () => {
    mockApi({ terminal: { available: true } });
    render(Flow);
    await fireEvent.click(await screen.findByRole('button', { name: 'Terminal' }));

    const input = screen.getByLabelText('terminal token');
    await fireEvent.input(input, { target: { value: 'deadbeef' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Connect' }));

    const frame = await waitFor(() => {
      const el = document.querySelector('iframe.term-frame')!;
      expect(el).not.toBeNull();
      return el;
    });
    // A browser cannot set a header on an iframe, so the token rides in the
    // query string — the proxy accepts both forms for that reason.
    expect(frame.getAttribute('src')).toContain('token=deadbeef');
    expect(frame.getAttribute('src')).toContain('/_emulator/portal/terminal/');
  });

  it('disconnecting drops the token, not just the frame', async () => {
    mockApi({ terminal: { available: true } });
    render(Flow);
    await fireEvent.click(await screen.findByRole('button', { name: 'Terminal' }));
    await fireEvent.input(screen.getByLabelText('terminal token'), {
      target: { value: 'deadbeef' },
    });
    await fireEvent.click(screen.getByRole('button', { name: 'Connect' }));
    await waitFor(() => expect(document.querySelector('iframe.term-frame')!).not.toBeNull());

    await fireEvent.click(screen.getByRole('button', { name: 'Disconnect' }));

    // Keeping the token in memory after disconnect would mean "Connect" silently
    // reuses a credential the operator thinks they revoked.
    expect(document.querySelector('iframe.term-frame')!).toBeNull();
    expect((screen.getByLabelText('terminal token') as HTMLInputElement).value).toBe('');
  });
});

describe('the error banner', () => {
  beforeEach(() => {
    vi.restoreAllMocks();
    installEventSource();
  });
  afterEach(() => {
    removeEventSource();
  });

  // The graph reloads on a debounce for the whole session. Without clearing on
  // success a single transient failure paints the banner permanently — which is
  // how a demo recording ended up showing a red `HTTP 404` beside a green
  // `streaming` chip and a pipeline that had just passed 16/16.
  it('clears a stale error once the graph loads again', async () => {
    let fail = true;
    vi.spyOn(globalThis, 'fetch').mockImplementation((url: RequestInfo | URL) => {
      if (String(url).includes('/portal/lineage') && fail) {
        return Promise.resolve(res(null, { ok: false, status: 404 }));
      }
      const body = String(url).includes('/portal/lineage')
        ? { value: edges }
        : { value: [] };
      return Promise.resolve(res(body));
    });

    render(Flow);
    await waitFor(() => expect(screen.getByText('HTTP 404')).toBeInTheDocument());

    // The next reload succeeds — the banner must go, not linger.
    fail = false;
    await fireEvent.click(screen.getByRole('button', { name: 'Reload graph' }));
    await waitFor(() => expect(screen.queryByText('HTTP 404')).toBeNull());
  });

  // Clearing on the NEXT success is only half the fix: reloads ride on lineage
  // events, and an idle stack emits none — so a transient during a container
  // recreate stayed on screen for hours, presenting a long-dead failure as
  // current. The failure path must recover by itself.
  it('a failed lineage load retries itself until the emulator answers', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    try {
      let fail = true;
      vi.spyOn(globalThis, 'fetch').mockImplementation((url: RequestInfo | URL) => {
        if (String(url).includes('/portal/lineage') && fail) {
          return Promise.resolve(res(null, { ok: false, status: 404 }));
        }
        const body = String(url).includes('/portal/lineage')
          ? { value: edges }
          : { value: [] };
        return Promise.resolve(res(body));
      });

      render(Flow);
      await waitFor(() => expect(screen.getByText('HTTP 404')).toBeInTheDocument());

      // No click and no lineage event: recovery has to come from the page.
      fail = false;
      await vi.advanceTimersByTimeAsync(3100);
      await waitFor(() => expect(screen.queryByText('HTTP 404')).toBeNull());
    } finally {
      vi.useRealTimers();
    }
  });
});

describe('the event contract', () => {
  beforeEach(() => {
    vi.restoreAllMocks();
    installEventSource();
  });
  afterEach(() => {
    removeEventSource();
  });

  // PER-KIND COMPLETENESS, not a coverage percentage. This iterates the
  // generated contract, so adding a kind in Go grows this test by itself — and a
  // kind the client cannot ingest fails here rather than going quiet in
  // production. Statement coverage would sit at 100% either way.
  it('subscribes to every kind the contract declares', async () => {
    mockLineage();
    render(Flow);
    await waitFor(() => expect(FakeEventSource.last).not.toBeNull());
    const subscribed = Object.keys(stream().listeners);
    for (const kind of EVENT_KINDS) {
      expect(subscribed, `no listener for '${kind}' — the stream names each frame, so it would never arrive`).toContain(kind);
    }
  });

  it('shows a row for every filterable kind the contract declares', async () => {
    mockLineage();
    render(Flow);
    await waitFor(() => expect(FakeEventSource.last).not.toBeNull());
    const es = FakeEventSource.last!;
    es.open();

    // `file` is off by default (a busy run emits far more of them), so turn
    // every filter on — the claim is about the CONTRACT, not the defaults.
    for (const box of screen.getAllByRole('checkbox')) {
      if (!(box as HTMLInputElement).checked) await fireEvent.click(box);
    }

    for (const [i, kind] of VIEW_KINDS.entries()) {
      es.emit(kind, {
        seq: i + 1, at: 1700000000, kind, workspaceId: 'ws-1',
        path: 'Files/x', table: 'Tables/t', jobId: 'j', activityName: 'a',
      });
    }

    // One row per kind, each labelled with its own kind. A kind that parses but
    // is discarded is the same invisible loss as one never subscribed to.
    for (const kind of VIEW_KINDS) {
      await waitFor(() =>
        expect(
          document.querySelector(`table tbody span.chip.${kind}`),
          `no log row for '${kind}'`,
        ).not.toBeNull(),
      );
    }
  });

  // Genericity as a property: an unrecognised kind must be COUNTED, not
  // swallowed. Before this, `kinds[ev.kind]` was undefined for an unknown kind,
  // so it vanished from the log with no trace — the same silent loss as never
  // subscribing, one layer further in.
  it('counts a kind it was never built for instead of swallowing it', async () => {
    mockLineage();
    render(Flow);
    await waitFor(() => expect(FakeEventSource.last).not.toBeNull());
    const es = FakeEventSource.last!;
    es.open();
    // An UNNAMED frame arrives on onmessage, not through addEventListener —
    // which is exactly the path a kind this build has never heard of takes.
    es.onmessage!({
      data: JSON.stringify({ seq: 99, at: 1700000000, kind: 'schedule', workspaceId: 'ws-1' }),
    });
    await waitFor(() =>
      expect(screen.getByText(/event\(s\) of an unknown kind/)).toBeInTheDocument(),
    );
  });
});

// The arms the suite above never reached: coalescing, the inspector's own
// failures, the log's per-kind phrasing, and the attribution column. Each is a
// separate `describe` so a failure names the behaviour, not "Flow".

describe('Flow: redraw coalescing and recovery', () => {
  beforeEach(() => {
    vi.restoreAllMocks();
    installEventSource();
  });
  afterEach(() => {
    removeEventSource();
    vi.useRealTimers();
  });

  it('coalesces a burst of lineage events into one reload', async () => {
    // One dbt model emits an edge per source. Reloading per edge is a request
    // storm for a single redraw, so the reload is debounced — and the guard
    // that makes it one reload is the thing worth pinning.
    vi.useFakeTimers();
    mockLineage();
    render(Flow);
    await vi.advanceTimersByTimeAsync(1);
    const before = fetchCalls()
      .filter((c: unknown[]) => String(c[0]).includes('/portal/lineage')).length;

    for (let i = 0; i < 5; i++) {
      stream().emit('lineage', {
        seq: 10 + i, at: 1700000000, kind: 'lineage',
        itemId: 'lake-1', sourcePath: 'a', targetPath: 'b',
      });
    }
    await vi.advanceTimersByTimeAsync(500);
    const after = fetchCalls()
      .filter((c: unknown[]) => String(c[0]).includes('/portal/lineage')).length;
    expect(after - before).toBe(1);
  });

  it('cancels a pending retry once a load succeeds', async () => {
    // Otherwise the retry fires anyway, and a stack that has recovered keeps
    // being re-asked on a schedule nothing cancels.
    vi.useFakeTimers();
    let ok = false;
    vi.spyOn(globalThis, 'fetch').mockImplementation((url: RequestInfo | URL) => {
      if (!String(url).includes('/portal/lineage')) {
        return Promise.resolve(res({ value: [] }));
      }
      return ok
        ? Promise.resolve(res({ value: edges }))
        : Promise.resolve(errRes('HTTP 404', 404));
    });
    render(Flow);
    await vi.advanceTimersByTimeAsync(1);
    expect(screen.getByText('HTTP 404')).toBeInTheDocument();

    // The retry lands on a healthy emulator: the banner clears and the timer is
    // cleared with it.
    ok = true;
    await vi.advanceTimersByTimeAsync(3100);
    expect(screen.queryByText('HTTP 404')).not.toBeInTheDocument();

    const settled = fetchCalls().length;
    await vi.advanceTimersByTimeAsync(30000);
    expect(fetchCalls()).toHaveLength(settled);
  });

  it('says it is reconnecting when the stream drops', async () => {
    // EventSource reconnects on its own, so this is not an error — but a green
    // `streaming` chip over a dead stream is a lie.
    mockLineage();
    render(Flow);
    stream().open();
    await waitFor(() => expect(screen.getByText('streaming')).toBeInTheDocument());
    stream().onerror!();
    await waitFor(() => expect(screen.getByText('reconnecting')).toBeInTheDocument());
    expect(screen.getByText('reconnecting')).toHaveClass('failed');
  });

  it('ignores a frame that is not JSON instead of dying on it', async () => {
    mockLineage();
    render(Flow);
    await waitFor(() => expect(screen.getByText('silver_customers')).toBeInTheDocument());
    // `curl -N` users can see this stream; a proxy that injects a keep-alive
    // comment must not take the log down.
    for (const fn of stream().listeners.table || []) fn({ data: 'not json {' });
    stream().emit('table', {
      seq: 1, at: 1700000000, kind: 'table', itemId: 'lake-1',
      table: 'Tables/bronze_customers', version: 0,
    });
    await waitFor(() =>
      expect(screen.getByText(/Tables\/bronze_customers → v0/)).toBeInTheDocument());
  });

  it('clears the log, the marks and the open inspector together', async () => {
    mockApi({
      table: { itemId: 'lake-1', table: 'Tables/bronze_customers', version: 1,
               readable: true, columns: ['id'], rowCount: 1, preview: [['1']] },
    });
    render(Flow);
    await waitFor(() => expect(screen.getByText('bronze_customers')).toBeInTheDocument());
    stream().emit('table', {
      seq: 1, at: 1700000000, kind: 'table', itemId: 'lake-1',
      table: 'Tables/bronze_customers', version: 1,
    });
    await fireEvent.click(groupOf(screen.getByText('bronze_customers')));
    await waitFor(() => expect(screen.getByText('v1')).toBeInTheDocument());

    await fireEvent.click(screen.getByRole('button', { name: 'Clear' }));
    // The panel goes with the log: it describes a table this session wrote, and
    // keeping it after Clear leaves a reading of something no longer listed.
    await waitFor(() => expect(screen.queryByText('v1')).not.toBeInTheDocument());
    expect(screen.getByText(/Nothing yet/)).toBeInTheDocument();
  });

  it('re-reads the open table when a commit lands on it', async () => {
    // The stream says a table CHANGED; the panel says what it holds. A panel
    // that does not re-read is quietly describing the previous version.
    let version = 1;
    vi.spyOn(globalThis, 'fetch').mockImplementation((url: RequestInfo | URL) => {
      if (String(url).includes('/portal/lineage'))
        return Promise.resolve(res({ value: edges }));
      if (String(url).includes('/portal/table'))
        return Promise.resolve(res({
          itemId: 'lake-1', table: 'Tables/bronze_customers', version, readable: true,
          columns: ['id'], rowCount: version, preview: [['1']] }));
      return Promise.resolve(res({ value: [] }));
    });
    render(Flow);
    await waitFor(() => expect(screen.getByText('bronze_customers')).toBeInTheDocument());
    await fireEvent.click(groupOf(screen.getByText('bronze_customers')));
    await waitFor(() => expect(screen.getByText('v1')).toBeInTheDocument());

    version = 2;
    stream().emit('table', {
      seq: 2, at: 1700000000, kind: 'table', itemId: 'lake-1',
      table: 'Tables/bronze_customers', version: 2,
    });
    await waitFor(() => expect(screen.getByText('v2')).toBeInTheDocument());
  });

  it('quietens a node written earlier in the session', async () => {
    // Two levels, not one: "just changed" and "written at all". With a single
    // state a finished run is uniformly lit and says nothing about order.
    vi.useFakeTimers();
    mockLineage();
    render(Flow);
    await vi.advanceTimersByTimeAsync(1);
    stream().emit('table', {
      seq: 1, at: 1700000000, kind: 'table', itemId: 'lake-1',
      table: 'Tables/bronze_customers', version: 1,
    });
    await vi.advanceTimersByTimeAsync(50);
    const node = () => groupOf(screen.getByText('bronze_customers'));
    expect(node()).toHaveClass('fresh');
    // FRESH_MS is 10s, and `now` ticks every second on its own.
    await vi.advanceTimersByTimeAsync(11000);
    expect(node()).toHaveClass('written');
    expect(node()).not.toHaveClass('fresh');
  });
});

describe('Flow: what the log says about each event', () => {
  beforeEach(() => {
    vi.restoreAllMocks();
    installEventSource();
    mockLineage();
  });
  afterEach(() => {
    removeEventSource();
  });

  const emit = (kind: string, ev: any) => stream().emit(kind, { seq: 99, at: 1700000000, kind, ...ev });

  it('reports files removed by a commit, not only rows added', async () => {
    render(Flow);
    await waitFor(() => expect(screen.getByText('silver_customers')).toBeInTheDocument());
    emit('table', { itemId: 'lake-1', table: 'Tables/s', version: 3,
                    rowsAdded: 10, filesRemoved: 2 });
    await waitFor(() => expect(
      screen.getByText('Tables/s → v3 (+10 rows), 2 file(s) removed')).toBeInTheDocument());
  });

  it('names why a job failed', async () => {
    render(Flow);
    await waitFor(() => expect(screen.getByText('silver_customers')).toBeInTheDocument());
    emit('job', { itemId: 'lake-1', status: 'Failed', failureReason: 'notebook exited 1' });
    await waitFor(() => expect(
      screen.getByText('job Failed — notebook exited 1')).toBeInTheDocument());
  });

  it('counts the queries in a batch when there was more than one', async () => {
    render(Flow);
    await waitFor(() => expect(screen.getByText('silver_customers')).toBeInTheDocument());
    emit('query', { itemId: 'sm-1', dataset: 'ContosoRevenue', queries: 3 });
    await waitFor(() => expect(
      screen.getByText('ContosoRevenue queried (3 queries)')).toBeInTheDocument());
  });

  it('says a query failed', async () => {
    render(Flow);
    await waitFor(() => expect(screen.getByText('silver_customers')).toBeInTheDocument());
    emit('query', { itemId: 'sm-1', dataset: 'ContosoRevenue', status: 'Failed' });
    await waitFor(() => expect(
      screen.getByText('ContosoRevenue queried — failed')).toBeInTheDocument());
  });

  it('falls back to a short id for a source system the graph has not seen', async () => {
    // The name comes from the edges, and a lineage event can arrive before the
    // reload that would carry it. "undefined (source system)" is the failure.
    render(Flow);
    await waitFor(() => expect(screen.getByText('silver_customers')).toBeInTheDocument());
    emit('lineage', { sourceKind: 'connection', sourceItemId: 'conn-abcdef123456',
                      targetPath: 'Files/landing/x.csv' });
    await waitFor(() => expect(
      screen.getByText('conn-abc (source system) → Files/landing/x.csv')).toBeInTheDocument());
  });

  it('still names the hop when a connection source carries no id at all', async () => {
    render(Flow);
    await waitFor(() => expect(screen.getByText('silver_customers')).toBeInTheDocument());
    emit('lineage', { sourceKind: 'connection', targetPath: 'Files/landing/y.csv' });
    // Reads "source system (source system)": the last-resort name and the
    // suffix are both generic, so it stutters. Asserted as it behaves rather
    // than as it reads — changing the copy is a separate, deliberate change.
    await waitFor(() => expect(
      screen.getByText('source system (source system) → Files/landing/y.csv')).toBeInTheDocument());
  });

  describe('the By column', () => {
    it('credits a lineage hop to its activity, then its producer', async () => {
      render(Flow);
      await waitFor(() => expect(screen.getByText('silver_customers')).toBeInTheDocument());
      emit('lineage', { sourcePath: 'a', targetPath: 'b', activityName: 'IngestCustomers' });
      await waitFor(() => expect(screen.getByText('IngestCustomers')).toBeInTheDocument());
    });

    it('falls back to the producer when no activity is named', async () => {
      render(Flow);
      await waitFor(() => expect(screen.getByText('silver_customers')).toBeInTheDocument());
      emit('lineage', { seq: 101, sourcePath: 'a', targetPath: 'c', producer: 'Warehouse' });
      await waitFor(() => expect(screen.getByText('Warehouse')).toBeInTheDocument());
    });

    it('credits a write to the activity that made it', async () => {
      render(Flow);
      await waitFor(() => expect(screen.getByText('silver_customers')).toBeInTheDocument());
      emit('table', { itemId: 'lake-1', table: 'Tables/t', version: 0,
                      attribution: { jobId: 'job-abcdef12', activityName: 'Refine' } });
      await waitFor(() => expect(screen.getByText('Refine')).toBeInTheDocument());
    });

    it('names the notebook cell when that is what is known', async () => {
      // cellIndex 0 is a real cell — the first one — which is why the field is
      // a pointer in Go and checked against null here rather than for truth.
      render(Flow);
      await waitFor(() => expect(screen.getByText('silver_customers')).toBeInTheDocument());
      emit('table', { itemId: 'lake-1', table: 'Tables/t', version: 0,
                      attribution: { jobId: 'job-abcdef12', cellIndex: 0 } });
      await waitFor(() => expect(screen.getByText('cell[0]')).toBeInTheDocument());
    });

    it('falls back to a short job id', async () => {
      render(Flow);
      await waitFor(() => expect(screen.getByText('silver_customers')).toBeInTheDocument());
      emit('table', { itemId: 'lake-1', table: 'Tables/t', version: 0,
                      attribution: { jobId: 'job-abcdef12' } });
      await waitFor(() => expect(screen.getByText('job-abcd')).toBeInTheDocument());
    });

    it('says nothing rather than guessing when attribution is empty', async () => {
      // Attribution is never inferred. An empty cell is the honest answer.
      render(Flow);
      await waitFor(() => expect(screen.getByText('silver_customers')).toBeInTheDocument());
      emit('table', { itemId: 'lake-1', table: 'Tables/t', version: 0, attribution: {} });
      await waitFor(() => expect(screen.getByText('Tables/t → v0')).toBeInTheDocument());
      const row = screen.getByText('Tables/t → v0').closest('tr')!;
      expect(row.querySelectorAll('td')[3].textContent.trim()).toBe('');
    });
  });
});

describe('Flow: the inspector', () => {
  beforeEach(() => {
    vi.restoreAllMocks();
    installEventSource();
  });
  afterEach(() => {
    removeEventSource();
  });

  const openNode = async (name = 'bronze_customers') => {
    render(Flow);
    await waitFor(() => expect(screen.getByText(name)).toBeInTheDocument());
    await fireEvent.click(groupOf(screen.getByText(name)));
  };

  it('reports a failure to read the table rather than staying on "Reading…"', async () => {
    vi.spyOn(globalThis, 'fetch').mockImplementation((url: RequestInfo | URL) => {
      if (String(url).includes('/portal/table'))
        return Promise.resolve(errRes('parquet is corrupt', 500));
      if (String(url).includes('/portal/lineage'))
        return Promise.resolve(res({ value: edges }));
      return Promise.resolve(res({ value: [] }));
    });
    await openNode();
    await waitFor(() => expect(screen.getByText('parquet is corrupt')).toBeInTheDocument());
  });

  it('says a table is not readable yet, with the reason', async () => {
    mockApi({ table: { readable: false, message: 'no commit has landed' } });
    await openNode();
    await waitFor(() =>
      expect(screen.getByText('Not readable yet: no commit has landed')).toBeInTheDocument());
  });

  it('counts one row in the singular', async () => {
    mockApi({ table: { readable: true, version: 0, columns: ['id'], rowCount: 1,
                       preview: [['1']] } });
    await openNode();
    await waitFor(() => expect(screen.getByText('1 row')).toBeInTheDocument());
  });

  it('says how much of a truncated preview is shown', async () => {
    mockApi({ table: { readable: true, version: 7, columns: ['id'], rowCount: 900,
                       preview: [['1'], ['2']], truncated: true } });
    await openNode();
    await waitFor(() => expect(screen.getByText(/First 2 of 900 rows/)).toBeInTheDocument());
  });

  it('shows the owning item beside the path', async () => {
    mockApi({ table: { readable: true, version: 0, columns: [], rowCount: 0, preview: [] } });
    await openNode();
    // Scoped to the panel: every node in the graph is also labelled with its
    // owning item, so a document-wide query for "lake" is ambiguous.
    await waitFor(() => expect(document.querySelector('.panel')!).toBeTruthy());
    expect(document.querySelector('.panel')!.textContent).toContain('lake');
    expect(document.querySelector('.panel')!.textContent).toContain('Tables/bronze_customers');
  });
});

describe('Flow: the last reachable arms', () => {
  beforeEach(() => {
    vi.restoreAllMocks();
    installEventSource();
  });
  afterEach(() => {
    removeEventSource();
    vi.useRealTimers();
  });

  it('cancels the pending retry when a manual reload succeeds first', async () => {
    // The retry clears its own handle before re-asking, so the success path's
    // "cancel the retry" arm is only reached when something ELSE succeeds
    // during the backoff — which is exactly what Reload graph does.
    vi.useFakeTimers();
    let ok = false;
    vi.spyOn(globalThis, 'fetch').mockImplementation((url: RequestInfo | URL) => {
      if (!String(url).includes('/portal/lineage'))
        return Promise.resolve(res({ value: [] }));
      return ok
        ? Promise.resolve(res({ value: edges }))
        : Promise.resolve(errRes('HTTP 404', 404));
    });
    render(Flow);
    await vi.advanceTimersByTimeAsync(1);
    expect(screen.getByText('HTTP 404')).toBeInTheDocument();

    ok = true;
    await fireEvent.click(screen.getByRole('button', { name: 'Reload graph' }));
    await vi.advanceTimersByTimeAsync(1);
    expect(screen.queryByText('HTTP 404')).not.toBeInTheDocument();

    // The retry that was pending must not fire now.
    const settled = fetchCalls().length;
    await vi.advanceTimersByTimeAsync(30000);
    expect(fetchCalls()).toHaveLength(settled);
  });

  it('opens the inspector from the keyboard', async () => {
    // The nodes are focusable and carry role="button"; a graph that can only be
    // clicked is unusable without a mouse.
    mockApi({ table: { readable: true, version: 2, columns: ['id'], rowCount: 1,
                       preview: [['1']] } });
    render(Flow);
    await waitFor(() => expect(screen.getByText('bronze_customers')).toBeInTheDocument());
    const node = groupOf(screen.getByText('bronze_customers'));
    await fireEvent.keyDown(node, { key: 'Enter' });
    await waitFor(() => expect(screen.getByText('v2')).toBeInTheDocument());
  });

  it('opens the inspector with the space bar too', async () => {
    mockApi({ table: { readable: true, version: 3, columns: ['id'], rowCount: 1,
                       preview: [['1']] } });
    render(Flow);
    await waitFor(() => expect(screen.getByText('bronze_customers')).toBeInTheDocument());
    await fireEvent.keyDown(groupOf(screen.getByText('bronze_customers')), { key: ' ' });
    await waitFor(() => expect(screen.getByText('v3')).toBeInTheDocument());
  });

  it('ignores other keys on a node', async () => {
    mockApi({ table: { readable: true, version: 4, columns: ['id'], rowCount: 1,
                       preview: [['1']] } });
    render(Flow);
    await waitFor(() => expect(screen.getByText('bronze_customers')).toBeInTheDocument());
    await fireEvent.keyDown(groupOf(screen.getByText('bronze_customers')), { key: 'a' });
    expect(screen.queryByText('v4')).not.toBeInTheDocument();
  });
});

describe('Flow: a retry that fails again', () => {
  beforeEach(() => {
    vi.restoreAllMocks();
    installEventSource();
  });
  afterEach(() => {
    removeEventSource();
    vi.useRealTimers();
  });

  it('replaces the pending retry rather than stacking a second one', async () => {
    // A manual reload during the backoff that ALSO fails: the catch has to
    // cancel the handle it is about to overwrite, or every failed reload leaves
    // another timer running and a dead emulator gets asked N times a second.
    vi.useFakeTimers();
    vi.spyOn(globalThis, 'fetch').mockImplementation((url: RequestInfo | URL) =>
      String(url).includes('/portal/lineage')
        ? Promise.resolve(errRes('HTTP 404', 404))
        : Promise.resolve(res({ value: [] })));
    render(Flow);
    await vi.advanceTimersByTimeAsync(1);
    expect(screen.getByText('HTTP 404')).toBeInTheDocument();

    // Reload while the first retry is still pending.
    await fireEvent.click(screen.getByRole('button', { name: 'Reload graph' }));
    await vi.advanceTimersByTimeAsync(1);

    // Exactly one retry may fire — the one this reload scheduled — and not the
    // orphaned original as well. The window is 6s+ because each failure doubles
    // the backoff, so the surviving timer is the 6s one.
    const before = fetchCalls()
      .filter((c: unknown[]) => String(c[0]).includes('/portal/lineage')).length;
    await vi.advanceTimersByTimeAsync(6100);
    const after = fetchCalls()
      .filter((c: unknown[]) => String(c[0]).includes('/portal/lineage')).length;
    expect(after - before).toBe(1);
  });
});

describe('Flow: payloads that omit what they usually carry', () => {
  beforeEach(() => {
    vi.restoreAllMocks();
    installEventSource();
  });
  afterEach(() => {
    removeEventSource();
  });

  it('treats every listing without a value key as empty', async () => {
    // Three endpoints, three `|| []` fallbacks: lineage, models and
    // workspaces. Any of them returning `{}` must leave the view usable rather
    // than iterating undefined.
    vi.spyOn(globalThis, 'fetch').mockResolvedValue(res({}));
    render(Flow);
    await waitFor(() => expect(screen.getByText(/No lineage recorded yet/)).toBeInTheDocument());
    expect(screen.getByText(/Nothing yet/)).toBeInTheDocument();
    // A single workspace (or none) means no workspace filter to offer.
    expect(screen.queryByRole('combobox')).not.toBeInTheDocument();
  });

  it('counts a dropped notice that does not say how many', async () => {
    // `ev.dropped ?? 0`: a notice with no count still means the log is
    // incomplete, and the chip must not read "undefined event(s) dropped".
    mockLineage();
    render(Flow);
    await waitFor(() => expect(screen.getByText('silver_customers')).toBeInTheDocument());
    stream().emit('dropped', { seq: 1, at: 1700000000, kind: 'dropped' });
    await new Promise((r) => setTimeout(r, 20));
    expect(screen.queryByText(/event\(s\) dropped/)).not.toBeInTheDocument();
  });

  it('labels a source system by its id when the connection has no name', async () => {
    mockLineage([{
      jobId: '', activityName: 'Ingest', producer: 'Copy', sourceKind: 'connection',
      sourceItemId: 'conn-abcdefgh', sourcePath: '',
      targetItemId: 'lake-1', targetItem: 'lake', targetPath: 'Files/landing/x.csv',
      createdAt: 1700000000,
    }]);
    render(Flow);
    // No sourceItem on the edge, so the node falls back to a short id.
    await waitFor(() => expect(screen.getByText('conn-abc')).toBeInTheDocument());
  });

  it('labels a node whose path has no leaf segment', async () => {
    // `.pop() || n.path`: a target of "Tables/" splits to nothing once empty
    // segments are filtered, and an unlabelled box is a box you cannot identify.
    mockLineage([{
      jobId: '', activityName: 'Odd', producer: 'Copy',
      sourceItemId: 'lake-1', sourceItem: 'lake', sourcePath: 'Tables/bronze',
      targetItemId: 'lake-1', targetItem: 'lake', targetPath: '/',
      createdAt: 1700000000,
    }]);
    render(Flow);
    await waitFor(() => expect(screen.getByText('bronze')).toBeInTheDocument());
    expect(screen.getByText('/')).toBeInTheDocument();
  });

  it('draws an edge whose producer is not recorded', async () => {
    // `l.producer?.toLowerCase() || ''` — the class is what colours the edge,
    // and an edge with no producer still has to be drawn.
    mockLineage([{
      jobId: '', activityName: '',
      sourceItemId: 'lake-1', sourceItem: 'lake', sourcePath: 'Tables/a',
      targetItemId: 'lake-1', targetItem: 'lake', targetPath: 'Tables/b',
      createdAt: 1700000000,
    }]);
    render(Flow);
    await waitFor(() => expect(screen.getByText('a')).toBeInTheDocument());
    const edge = document.querySelector('path.link')!;
    expect(edge).toBeTruthy();
    expect(edge.getAttribute('d')).toMatch(/^M[\d.]+,/);
  });

  it('refuses to connect the terminal on a blank token', async () => {
    // Submitting the form with whitespace must not frame ttyd with an empty
    // token — the proxy would refuse it and the pane would show ttyd's 401.
    mockApi({ terminal: { available: true } });
    render(Flow);
    await fireEvent.click(await screen.findByRole('button', { name: 'Terminal' }));
    const box = screen.getByLabelText('terminal token');
    await fireEvent.input(box, { target: { value: '   ' } });
    await fireEvent.submit(box.closest('form')!);
    expect(document.querySelector('iframe.term-frame')!).toBeNull();
  });
});

describe('Flow: a node whose owning item has no name', () => {
  beforeEach(() => {
    vi.restoreAllMocks();
    installEventSource();
  });
  afterEach(() => {
    removeEventSource();
  });

  it('falls back to a short item id in the node and in the inspector', async () => {
    // `n.item || n.itemId.slice(0, 8)` in two places. A lineage edge recorded
    // against an item that has since been deleted carries no display name, and
    // "undefined" under a box is worse than a truncated GUID.
    vi.spyOn(globalThis, 'fetch').mockImplementation((url: RequestInfo | URL) => {
      if (String(url).includes('/portal/lineage'))
        return Promise.resolve(res({ value: [{
          jobId: '', activityName: 'Ingest', producer: 'Copy',
          sourceItemId: 'lake-11112222', sourcePath: 'Files/landing/x.csv',
          targetItemId: 'lake-11112222', targetPath: 'Tables/bronze',
          createdAt: 1700000000,
        }] }));
      if (String(url).includes('/portal/table'))
        return Promise.resolve(res({
          readable: true, version: 0, columns: ['id'], rowCount: 2, preview: [['1'], ['2']] }));
      return Promise.resolve(res({ value: [] }));
    });
    render(Flow);
    await waitFor(() => expect(screen.getByText('bronze')).toBeInTheDocument());
    // The sub-label under the box.
    expect(screen.getAllByText('lake-111').length).toBeGreaterThan(0);

    await fireEvent.click(groupOf(screen.getByText('bronze')));
    // And the inspector's own heading line, which uses the same fallback.
    await waitFor(() => expect(document.querySelector('.panel')!).toBeTruthy());
    expect(document.querySelector('.panel')!.textContent).toContain('lake-11112222');
  });
});
