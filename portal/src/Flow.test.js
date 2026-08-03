import { fireEvent, render, screen, waitFor } from '@testing-library/svelte';
import { vi, describe, it, expect, beforeEach, afterEach } from 'vitest';
import Flow from './Flow.svelte';

// A stand-in EventSource: the component subscribes to it exactly as it would to
// the emulator's SSE endpoint, and the test pushes frames through it.
class FakeEventSource {
  static last = null;
  constructor(url) {
    this.url = url;
    this.listeners = {};
    FakeEventSource.last = this;
  }
  addEventListener(kind, fn) {
    (this.listeners[kind] ||= []).push(fn);
  }
  close() {
    this.closed = true;
  }
  open() {
    this.onopen?.();
  }
  emit(kind, payload) {
    const m = { data: JSON.stringify(payload) };
    for (const fn of this.listeners[kind] || []) fn(m);
  }
}

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
function mockApi({ lineage = edges, workspaces = [], table = null } = {}) {
  vi.spyOn(globalThis, 'fetch').mockImplementation((url) => {
    const body = url.includes('/portal/lineage')
      ? { value: lineage }
      : url.includes('/portal/workspaces')
        ? { value: workspaces }
        : url.includes('/portal/table')
          ? table
          : { value: [] };
    return Promise.resolve({ ok: true, status: 200, json: () => Promise.resolve(body) });
  });
}

function mockLineage(value = edges) {
  mockApi({ lineage: value });
}

describe('Flow', () => {
  beforeEach(() => {
    vi.restoreAllMocks();
    FakeEventSource.last = null;
    globalThis.EventSource = FakeEventSource;
  });
  afterEach(() => {
    delete globalThis.EventSource;
  });

  it('says it is connecting before claiming to be disconnected', async () => {
    mockLineage();
    render(Flow);
    // The window before the first connection resolves must not read as an error.
    await waitFor(() => expect(screen.getByText('connecting')).toBeInTheDocument());
    expect(screen.queryByText('disconnected')).not.toBeInTheDocument();
    FakeEventSource.last.open();
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
    const xOf = (label) => {
      const g = screen.getByText(label).closest('g');
      return Number(g.getAttribute('transform').match(/translate\((-?\d+)/)[1]);
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

    FakeEventSource.last.open();
    await waitFor(() => expect(screen.getByText('streaming')).toBeInTheDocument());

    FakeEventSource.last.emit('table', {
      seq: 1, at: 1700000200, kind: 'table', itemId: 'lake-1',
      table: 'Tables/bronze_customers', version: 3, rowsAdded: 1203, filesAdded: 1,
      attribution: { jobId: 'job-1', activityName: 'IngestCustomers' },
    });

    await waitFor(() =>
      expect(screen.getByText(/Tables\/bronze_customers → v3 \(\+1203 rows\)/)).toBeInTheDocument());
    // The attribution column names what caused it.
    expect(screen.getAllByText('IngestCustomers').length).toBeGreaterThan(0);
    // …and the node is marked as touched.
    const node = screen.getByText('bronze_customers').closest('g');
    await waitFor(() => expect(node.getAttribute('class')).toContain('fresh'));
  });

  it('marks a failing activity red on the table it writes', async () => {
    mockLineage();
    render(Flow);
    await waitFor(() => expect(screen.getByText('bronze_customers')).toBeInTheDocument());
    FakeEventSource.last.open();

    FakeEventSource.last.emit('activity', {
      seq: 2, at: 1700000300, kind: 'activity', jobId: 'job-1',
      activityName: 'IngestCustomers', activityType: 'Copy', status: 'Failed',
      error: 'the source file is empty',
    });

    await waitFor(() => expect(screen.getByText(/the source file is empty/)).toBeInTheDocument());
    const node = screen.getByText('bronze_customers').closest('g');
    await waitFor(() => expect(node.getAttribute('class')).toContain('broken'));
  });

  it('reports dropped events rather than hiding a gap', async () => {
    mockLineage();
    render(Flow);
    await waitFor(() => expect(screen.getByText('bronze_customers')).toBeInTheDocument());
    FakeEventSource.last.open();

    FakeEventSource.last.emit('dropped', { seq: 3, at: 1700000400, kind: 'dropped', dropped: 12 });
    await waitFor(() => expect(screen.getByText(/12 event\(s\) dropped/)).toBeInTheDocument());
  });

  it('filters by kind without losing events already received', async () => {
    mockLineage();
    render(Flow);
    await waitFor(() => expect(screen.getByText('bronze_customers')).toBeInTheDocument());
    FakeEventSource.last.open();

    // `file` is off by default: the firehose is opt-in.
    FakeEventSource.last.emit('file', {
      seq: 4, at: 1700000500, kind: 'file',
      eventType: 'Microsoft.Fabric.OneLake.FileCreated', path: 'Files/landing/customers.csv',
    });
    FakeEventSource.last.emit('job', {
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
    vi.spyOn(globalThis, 'fetch').mockResolvedValue({
      ok: false,
      status: 500,
      json: () => Promise.resolve({ error: { message: 'db gone' } }),
    });
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

    await fireEvent.click(screen.getByText('bronze_customers').closest('g'));

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
    await fireEvent.click(screen.getByText('customers.csv').closest('g'));
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
    await fireEvent.click(screen.getByText('bronze_customers').closest('g'));
    await waitFor(() =>
      expect(screen.getByText(/Not readable yet/)).toBeInTheDocument());
  });

  it('filters the log by workspace as well as by kind', async () => {
    mockApi({ workspaces: [{ id: 'ws-1', displayName: 'Analytics' }, { id: 'ws-2', displayName: 'Sandbox' }] });
    render(Flow);
    await waitFor(() => expect(screen.getByText('bronze_customers')).toBeInTheDocument());
    FakeEventSource.last.open();

    FakeEventSource.last.emit('job', {
      seq: 1, at: 1700000000, kind: 'job', workspaceId: 'ws-1', jobId: 'a', status: 'Started',
    });
    FakeEventSource.last.emit('activity', {
      seq: 2, at: 1700000001, kind: 'activity', workspaceId: 'ws-2',
      activityName: 'OtherWorkspaceStep', activityType: 'Wait', status: 'Succeeded',
    });
    await waitFor(() => expect(screen.getByText(/OtherWorkspaceStep/)).toBeInTheDocument());

    // Narrowing to one workspace hides the other's events without dropping them.
    const select = screen.getByLabelText(/workspace/);
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
    FakeEventSource.last = null;
    globalThis.EventSource = FakeEventSource;
  });
  afterEach(() => {
    delete globalThis.EventSource;
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

    const xOf = (label) => {
      const g = screen.getByText(label).closest('g');
      return Number(g.getAttribute('transform').match(/translate\((-?\d+)/)[1]);
    };
    // Source → landing → bronze → silver → gold → model, left to right.
    expect(xOf('silver_customers')).toBeLessThan(xOf('dim_customer'));
    expect(xOf('dim_customer')).toBeLessThan(xOf('Customer'));
  });

  it('redraws when a movement is recorded, without waiting for a job to end', async () => {
    // A warehouse build has no job, so the old "reload when a job finishes"
    // rule would never fire and gold would not appear until a manual reload.
    let served = edges;
    vi.spyOn(globalThis, 'fetch').mockImplementation((url) => {
      const body = url.includes('/portal/lineage') ? { value: served } : { value: [] };
      return Promise.resolve({ ok: true, status: 200, json: () => Promise.resolve(body) });
    });
    render(Flow);
    await waitFor(() => expect(screen.getByText('silver_customers')).toBeInTheDocument());
    expect(screen.queryByText('dim_customer')).not.toBeInTheDocument();

    served = goldEdges;
    FakeEventSource.last.emit('lineage', {
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
    FakeEventSource.last.emit('query', {
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
    const asked = globalThis.fetch.mock.calls.some((c) => String(c[0]).includes('/portal/table'));
    expect(asked).toBe(false);
  });
});
