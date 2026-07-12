import { render, screen, waitFor } from '@testing-library/svelte';
import { vi, describe, it, expect, beforeEach } from 'vitest';
import Operations from './Operations.svelte';

describe('Operations', () => {
  beforeEach(() => {
    vi.restoreAllMocks();
  });

  it('renders operations with derived statuses', async () => {
    vi.spyOn(globalThis, 'fetch').mockResolvedValue({
      ok: true,
      status: 200,
      json: () =>
        Promise.resolve({
          value: [
            { id: 'op-1', kind: 'CreateItem', status: 'Succeeded', createdAt: 1700000000, resultRef: 'it-1' },
            { id: 'op-2', kind: 'UpdateFromGit', status: 'Running', createdAt: 1700000100, resultRef: '' },
          ],
        }),
    });

    render(Operations);
    await waitFor(() => expect(screen.getByText('CreateItem')).toBeInTheDocument());
    expect(screen.getByText('Succeeded')).toBeInTheDocument();
    expect(screen.getByText('Running')).toBeInTheDocument();
    expect(screen.getByText('it-1')).toBeInTheDocument();
  });

  it('renders the empty state', async () => {
    vi.spyOn(globalThis, 'fetch').mockResolvedValue({
      ok: true,
      status: 200,
      json: () => Promise.resolve({ value: [] }),
    });
    render(Operations);
    await waitFor(() => expect(screen.getByText(/No operations yet/)).toBeInTheDocument());
  });
});
