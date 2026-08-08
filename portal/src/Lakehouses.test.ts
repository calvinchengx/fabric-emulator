import { fireEvent, render, screen, waitFor } from '@testing-library/svelte';
import { vi, describe, it, expect, beforeEach } from 'vitest';
import Lakehouses from './Lakehouses.svelte';
import { errRes, fetchCalls, res } from './testing';

const one = {
  itemId: 'lh-1',
  name: 'lake',
  workspaceId: 'ws-1',
  workspace: 'analytics',
  schemaEnabled: false,
  tables: ['orders'],
  files: ['landing/day1.csv'],
  fileCount: 1,
};

// A file list capped below its true count — the case where showing the listed
// length as the total would read as a nearly-empty lakehouse.
const capped = { ...one, itemId: 'lh-2', name: 'big', files: ['a.csv', 'b.csv'], fileCount: 207 };

const previewBody = {
  itemId: 'lh-1',
  table: 'Tables/orders',
  version: 3,
  readable: true,
  columns: ['id', 'name'],
  rowCount: 2,
  preview: [['1', 'ada'], ['2', 'grace']],
  truncated: false,
};

/** Answer the browse call with `list`, and any /portal/table call with `table`. */
function server(list: unknown[], table?: unknown) {
  vi.spyOn(globalThis, 'fetch').mockImplementation((url: any) =>
    String(url).includes('/portal/table')
      ? Promise.resolve(res(table))
      : Promise.resolve(res({ lakehouses: list })),
  );
}

describe('Lakehouses', () => {
  beforeEach(() => {
    vi.restoreAllMocks();
  });

  it('renders the empty state', async () => {
    server([]);
    render(Lakehouses);
    await waitFor(() => expect(screen.getByText(/No lakehouses yet/)).toBeInTheDocument());
  });

  it('surfaces a browse failure rather than rendering an empty lakehouse list', async () => {
    // An error that rendered as "no lakehouses" would read as a working
    // emulator with no data — the wrong conclusion entirely.
    vi.spyOn(globalThis, 'fetch').mockResolvedValue(errRes('store unavailable'));
    render(Lakehouses);
    await waitFor(() => expect(screen.getByText(/store unavailable/)).toBeInTheDocument());
  });

  it('lists tables and the workspace it belongs to', async () => {
    server([one]);
    render(Lakehouses);
    await waitFor(() => expect(screen.getByText('orders')).toBeInTheDocument());
    expect(screen.getByText('analytics')).toBeInTheDocument();
    expect(screen.getByText('lh-1')).toBeInTheDocument();
    expect(screen.getByText(/Tables \(1\)/)).toBeInTheDocument();
  });

  it('marks a schema-enabled lakehouse', async () => {
    server([{ ...one, schemaEnabled: true, tables: ['silver/orders'] }]);
    render(Lakehouses);
    await waitFor(() => expect(screen.getByText('schema-enabled')).toBeInTheDocument());
    expect(screen.getByText('silver/orders')).toBeInTheDocument();
  });

  it('says a lakehouse has no tables rather than showing nothing', async () => {
    server([{ ...one, tables: [], files: [], fileCount: 0 }]);
    render(Lakehouses);
    await waitFor(() => expect(screen.getByText(/No Delta tables yet/)).toBeInTheDocument());
  });

  it('hides files until asked, then shows them', async () => {
    server([one]);
    render(Lakehouses);
    await waitFor(() => expect(screen.getByText(/Files \(1\)/)).toBeInTheDocument());
    expect(screen.queryByText('landing/day1.csv')).not.toBeInTheDocument();

    await fireEvent.click(screen.getByRole('button', { name: 'Show' }));
    expect(screen.getByText('landing/day1.csv')).toBeInTheDocument();

    await fireEvent.click(screen.getByRole('button', { name: 'Hide' }));
    expect(screen.queryByText('landing/day1.csv')).not.toBeInTheDocument();
  });

  it('says the file list is capped and the count is not', async () => {
    server([capped]);
    render(Lakehouses);
    await waitFor(() => expect(screen.getByText(/Files \(207\)/)).toBeInTheDocument());
    await fireEvent.click(screen.getByRole('button', { name: 'Show' }));
    expect(screen.getByText(/Showing 2 of 207/)).toBeInTheDocument();
  });

  it('reports an empty files list without claiming a cap', async () => {
    server([{ ...one, files: [], fileCount: 0 }]);
    render(Lakehouses);
    await waitFor(() => expect(screen.getByText(/Files \(0\)/)).toBeInTheDocument());
    await fireEvent.click(screen.getByRole('button', { name: 'Show' }));
    expect(screen.getByText('No files.')).toBeInTheDocument();
  });

  it('previews a table through the existing /portal/table endpoint', async () => {
    server([one], previewBody);
    render(Lakehouses);
    await waitFor(() => expect(screen.getByText('orders')).toBeInTheDocument());
    await fireEvent.click(screen.getByRole('button', { name: 'Preview' }));

    await waitFor(() => expect(screen.getByText('ada')).toBeInTheDocument());
    expect(screen.getByText(/version 3 · 2 rows/)).toBeInTheDocument();
    // The table is addressed as `Tables/<name>`, which is what that endpoint
    // expects — sending the bare name would 400.
    const asked = fetchCalls().map((c) => String(c[0])).find((u) => u.includes('/portal/table'));
    expect(asked).toContain('itemId=lh-1');
    expect(asked).toContain(encodeURIComponent('Tables/orders'));
  });

  it('says a table is not readable rather than showing an empty grid', async () => {
    // Not an error: a first commit may not have landed. An empty grid would
    // look like a table with no rows, which is a different fact.
    server([one], { ...previewBody, readable: false, message: 'no _delta_log' });
    render(Lakehouses);
    await waitFor(() => expect(screen.getByText('orders')).toBeInTheDocument());
    await fireEvent.click(screen.getByRole('button', { name: 'Preview' }));
    await waitFor(() =>
      expect(screen.getByText(/Not readable as Delta: no _delta_log/)).toBeInTheDocument(),
    );
  });

  it('reports a preview request failure', async () => {
    vi.spyOn(globalThis, 'fetch').mockImplementation((url: any) =>
      String(url).includes('/portal/table')
        ? Promise.resolve(errRes('read failed'))
        : Promise.resolve(res({ lakehouses: [one] })),
    );
    render(Lakehouses);
    await waitFor(() => expect(screen.getByText('orders')).toBeInTheDocument());
    await fireEvent.click(screen.getByRole('button', { name: 'Preview' }));
    await waitFor(() => expect(screen.getByText(/read failed/)).toBeInTheDocument());
  });

  it('says when a preview is truncated, and uses the singular for one row', async () => {
    server([one], { ...previewBody, rowCount: 1, preview: [['1', 'ada']], truncated: true });
    render(Lakehouses);
    await waitFor(() => expect(screen.getByText('orders')).toBeInTheDocument());
    await fireEvent.click(screen.getByRole('button', { name: 'Preview' }));
    await waitFor(() => expect(screen.getByText(/1 row(?!s)/)).toBeInTheDocument());
    expect(screen.getByText(/showing the first 1/)).toBeInTheDocument();
  });

  it('refetches on Refresh', async () => {
    server([one]);
    render(Lakehouses);
    await waitFor(() => expect(screen.getByText('orders')).toBeInTheDocument());
    const before = fetchCalls().length;
    await fireEvent.click(screen.getByRole('button', { name: 'Refresh' }));
    await waitFor(() => expect(fetchCalls().length).toBeGreaterThan(before));
  });

  it('tolerates a response with no lakehouses key', async () => {
    // Defensive: an older emulator answering this route without the key must
    // render the empty state, not crash the view.
    vi.spyOn(globalThis, 'fetch').mockResolvedValue(res({}));
    render(Lakehouses);
    await waitFor(() => expect(screen.getByText(/No lakehouses yet/)).toBeInTheDocument());
  });
});
