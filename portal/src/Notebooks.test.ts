import { render, screen, waitFor } from '@testing-library/svelte';
import { vi, describe, it, expect, beforeEach } from 'vitest';
import Notebooks from './Notebooks.svelte';
import { errRes, res } from './testing';

const one = {
  itemId: 'nb-1',
  name: 'bronze',
  workspaceId: 'ws-1',
  workspace: 'analytics',
  cells: 3,
  codeCells: 2,
  hasDefinition: true,
};

function server(list: unknown[]) {
  vi.spyOn(globalThis, 'fetch').mockResolvedValue(res({ notebooks: list }));
}

describe('Notebooks', () => {
  beforeEach(() => {
    vi.restoreAllMocks();
  });

  it('renders the empty state', async () => {
    server([]);
    render(Notebooks);
    await waitFor(() => expect(screen.getByText(/No notebooks yet/)).toBeInTheDocument());
  });

  it('surfaces a load failure rather than rendering an empty list', async () => {
    // An error that rendered as "no notebooks" would read as a working emulator
    // with nothing in it — a different fact entirely.
    vi.spyOn(globalThis, 'fetch').mockResolvedValue(errRes('store unavailable'));
    render(Notebooks);
    await waitFor(() => expect(screen.getByText(/store unavailable/)).toBeInTheDocument());
  });

  it('lists a notebook with its cell counts and workspace', async () => {
    server([one]);
    render(Notebooks);
    await waitFor(() => expect(screen.getByText('bronze')).toBeInTheDocument());
    expect(screen.getByText('analytics')).toBeInTheDocument();
    expect(screen.getByText(/3 cells, 2 code cells/)).toBeInTheDocument();
  });

  it('uses the singular for a one-cell notebook', async () => {
    server([{ ...one, cells: 1, codeCells: 1 }]);
    render(Notebooks);
    await waitFor(() => expect(screen.getByText(/1 cell, 1 code cell/)).toBeInTheDocument());
  });

  it('marks a notebook that has no definition instead of showing 0 cells', async () => {
    // "0 cells" and "never defined" are different states, and only one of them
    // can be run — a RunNotebook job on this one fails rather than completing.
    server([{ ...one, cells: 0, codeCells: 0, hasDefinition: false }]);
    render(Notebooks);
    await waitFor(() => expect(screen.getByText('no definition')).toBeInTheDocument());
    expect(screen.queryByText(/0 cells/)).not.toBeInTheDocument();
  });

  it('links each notebook to its own address', async () => {
    // A link, not a button: it has a URL, so it must be openable in a new tab
    // and reachable through history.
    server([one]);
    render(Notebooks);
    const link = await screen.findByRole('link', { name: /bronze/ });
    expect(link).toHaveAttribute('href', '#notebooks/nb-1');
  });

  it('tolerates a response with no notebooks key', async () => {
    vi.spyOn(globalThis, 'fetch').mockResolvedValue(res({}));
    render(Notebooks);
    await waitFor(() => expect(screen.getByText(/No notebooks yet/)).toBeInTheDocument());
  });
});
