import { fireEvent, render, screen, waitFor } from '@testing-library/svelte';
import { vi, describe, it, expect, beforeEach } from 'vitest';
import NotebookDetail from './NotebookDetail.svelte';
import { errRes, fetchCalls, res } from './testing';

const detail = {
  itemId: 'nb-1',
  name: 'bronze',
  workspaceId: 'ws-1',
  workspace: 'analytics',
  readable: true,
  runsHere: true,
  cells: [
    { index: 0, kind: 'markdown', source: '# Bronze to silver' },
    { index: 1, kind: 'code', language: 'python', source: 'P_date = "2026-01-01"', parameters: true },
    { index: 2, kind: 'code', language: 'python', source: 'spark.sql("SELECT 1")' },
  ],
};

const runRecord = {
  status: 'InProgress',
  cells: [
    { index: 0, status: 'Completed', source: 'P_date = "2026-01-01"' },
    { index: 1, status: 'Pending', source: 'spark.sql("SELECT 1")' },
  ],
};

/** Route each call by URL: the detail GET, the run POST, the run-detail GET. */
function server(opts: { nb?: unknown; run?: unknown; runFails?: boolean; startFails?: boolean } = {}) {
  vi.spyOn(globalThis, 'fetch').mockImplementation((url: any, init?: any) => {
    const u = String(url);
    if (init?.method === 'POST') {
      return Promise.resolve(opts.startFails ? errRes('no such notebook', 404) : res({ jobId: 'job-9' }));
    }
    if (u.includes('/notebooks/runs/')) {
      return Promise.resolve(opts.runFails ? errRes('no run detail', 404) : res(opts.run ?? runRecord));
    }
    return Promise.resolve(res(opts.nb ?? detail));
  });
}

describe('NotebookDetail', () => {
  beforeEach(() => {
    vi.restoreAllMocks();
  });

  it('renders every cell, markdown included', async () => {
    // The run record keeps only code cells because an engine executes only
    // those. A reader needs the prose that explains them.
    server();
    render(NotebookDetail, { id: 'nb-1' });
    await waitFor(() => expect(screen.getByText('# Bronze to silver')).toBeInTheDocument());
    expect(screen.getByText('spark.sql("SELECT 1")')).toBeInTheDocument();
    expect(screen.getByText('parameters')).toBeInTheDocument();
  });

  it('surfaces a load failure', async () => {
    vi.spyOn(globalThis, 'fetch').mockResolvedValue(errRes('notebook gone'));
    render(NotebookDetail, { id: 'nb-1' });
    await waitFor(() => expect(screen.getByText(/notebook gone/)).toBeInTheDocument());
  });

  it('says why an unreadable notebook shows nothing, and offers no run button', async () => {
    // No definition is not an empty notebook. Offering Run here would start a
    // job that can only fail NotebookDefinitionInvalid.
    server({ nb: { ...detail, readable: false, message: 'this notebook has no definition yet', cells: [] } });
    render(NotebookDetail, { id: 'nb-1' });
    await waitFor(() =>
      expect(screen.getByText(/no definition yet/)).toBeInTheDocument(),
    );
    expect(screen.queryByRole('button', { name: 'Run' })).not.toBeInTheDocument();
  });

  it('warns BEFORE the button is pressed when no engine is attached', async () => {
    // Without a Spark agent the job parks by design rather than going green.
    // Saying so afterwards would leave a parked job to explain itself.
    server({ nb: { ...detail, runsHere: false } });
    render(NotebookDetail, { id: 'nb-1' });
    await waitFor(() => expect(screen.getByText(/No Spark engine is attached/)).toBeInTheDocument());
    expect(screen.getByRole('button', { name: 'Run' })).toBeInTheDocument();
  });

  it('does not warn when an engine is attached', async () => {
    server();
    render(NotebookDetail, { id: 'nb-1' });
    await waitFor(() => expect(screen.getByRole('button', { name: 'Run' })).toBeInTheDocument());
    expect(screen.queryByText(/No Spark engine is attached/)).not.toBeInTheDocument();
  });

  it('starts the documented job and shows the run it started', async () => {
    server();
    render(NotebookDetail, { id: 'nb-1' });
    await waitFor(() => expect(screen.getByRole('button', { name: 'Run' })).toBeInTheDocument());
    await fireEvent.click(screen.getByRole('button', { name: 'Run' }));

    await waitFor(() => expect(screen.getByText('job-9')).toBeInTheDocument());
    const posted = fetchCalls().find((c) => (c[1] as any)?.method === 'POST');
    expect(String(posted?.[0])).toContain('/portal/notebooks/nb-1/run');
    expect(screen.getByText('InProgress')).toBeInTheDocument();
  });

  it('lines a run status up with the cell it belongs to', async () => {
    // The run record re-sequences code cells 0..n, so its index 1 is the THIRD
    // cell on screen. Rendering by raw index would attach each status to the
    // wrong cell — and still look plausible.
    server();
    render(NotebookDetail, { id: 'nb-1' });
    await waitFor(() => expect(screen.getByRole('button', { name: 'Run' })).toBeInTheDocument());
    await fireEvent.click(screen.getByRole('button', { name: 'Run' }));

    await waitFor(() => expect(screen.getByText('Completed')).toBeInTheDocument());
    const cells = [...document.querySelectorAll('section.cell')];
    // The parameters cell (code index 0) is Completed; the last cell is Pending.
    expect(cells[1].textContent).toContain('Completed');
    expect(cells[2].textContent).toContain('Pending');
    // The markdown cell carries no run status at all.
    expect(cells[0].textContent).not.toContain('Completed');
  });

  it('does not ask for run detail when the start returned no job id', async () => {
    // Defensive, and the guard is load-bearing: without it the view would GET
    // `/runs/undefined` and render that 404 as though the run had failed, when
    // what actually happened is an emulator that answered without an id.
    vi.spyOn(globalThis, 'fetch').mockImplementation((url: any, init?: any) =>
      Promise.resolve(init?.method === 'POST' ? res({}) : res(detail)),
    );
    render(NotebookDetail, { id: 'nb-1' });
    await waitFor(() => expect(screen.getByRole('button', { name: 'Run' })).toBeInTheDocument());
    await fireEvent.click(screen.getByRole('button', { name: 'Run' }));

    await waitFor(() => expect(screen.getByRole('button', { name: 'Run' })).toBeEnabled());
    expect(fetchCalls().map((c) => String(c[0])).some((u) => u.includes('/notebooks/runs/'))).toBe(false);
  });

  it('reports a failure to start', async () => {
    server({ startFails: true });
    render(NotebookDetail, { id: 'nb-1' });
    await waitFor(() => expect(screen.getByRole('button', { name: 'Run' })).toBeInTheDocument());
    await fireEvent.click(screen.getByRole('button', { name: 'Run' }));
    await waitFor(() => expect(screen.getByText(/no such notebook/)).toBeInTheDocument());
  });

  it('reports a failure to read the run back', async () => {
    server({ runFails: true });
    render(NotebookDetail, { id: 'nb-1' });
    await waitFor(() => expect(screen.getByRole('button', { name: 'Run' })).toBeInTheDocument());
    await fireEvent.click(screen.getByRole('button', { name: 'Run' }));
    await waitFor(() => expect(screen.getByText(/no run detail/)).toBeInTheDocument());
  });

  it('refreshes the run on demand', async () => {
    server();
    render(NotebookDetail, { id: 'nb-1' });
    await waitFor(() => expect(screen.getByRole('button', { name: 'Run' })).toBeInTheDocument());
    await fireEvent.click(screen.getByRole('button', { name: 'Run' }));
    await waitFor(() => expect(screen.getByRole('button', { name: 'Refresh run' })).toBeInTheDocument());

    const before = fetchCalls().length;
    await fireEvent.click(screen.getByRole('button', { name: 'Refresh run' }));
    await waitFor(() => expect(fetchCalls().length).toBeGreaterThan(before));
  });

  it('tones a failed run as a failure', async () => {
    server({ run: { status: 'Failed', cells: [{ index: 0, status: 'Failed' }, { index: 1, status: 'Pending' }] } });
    render(NotebookDetail, { id: 'nb-1' });
    await waitFor(() => expect(screen.getByRole('button', { name: 'Run' })).toBeInTheDocument());
    await fireEvent.click(screen.getByRole('button', { name: 'Run' }));
    await waitFor(() => expect(screen.getAllByText('Failed').length).toBeGreaterThan(0));
  });

  it('renders a run record with no cells without crashing', async () => {
    // Defensive: an engine may report a run before any cell has a status.
    server({ run: { status: 'Pending' } });
    render(NotebookDetail, { id: 'nb-1' });
    await waitFor(() => expect(screen.getByRole('button', { name: 'Run' })).toBeInTheDocument());
    await fireEvent.click(screen.getByRole('button', { name: 'Run' }));
    await waitFor(() => expect(screen.getByText('Pending')).toBeInTheDocument());
  });
});
