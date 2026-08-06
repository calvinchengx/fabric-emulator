import { fireEvent, render, screen, waitFor } from '@testing-library/svelte';
import { vi, describe, it, expect, beforeEach } from 'vitest';
import Operations from './Operations.svelte';
import { errRes, res } from './testing';

describe('Operations', () => {
  beforeEach(() => {
    vi.restoreAllMocks();
  });

  it('renders operations with derived statuses', async () => {
    vi.spyOn(globalThis, 'fetch').mockResolvedValue(res({
          value: [
            { id: 'op-1', kind: 'CreateItem', status: 'Succeeded', createdAt: 1700000000, resultRef: 'it-1' },
            { id: 'op-2', kind: 'UpdateFromGit', status: 'Running', createdAt: 1700000100, resultRef: '' },
          ],
        }));

    render(Operations);
    await waitFor(() => expect(screen.getByText('CreateItem')).toBeInTheDocument());
    expect(screen.getByText('Succeeded')).toBeInTheDocument();
    expect(screen.getByText('Running')).toBeInTheDocument();
    expect(screen.getByText('it-1')).toBeInTheDocument();
  });

  it('renders the empty state', async () => {
    vi.spyOn(globalThis, 'fetch').mockResolvedValue(res({ value: [] }));
    render(Operations);
    await waitFor(() => expect(screen.getByText(/No operations yet/)).toBeInTheDocument());
  });


  it('surfaces load errors', async () => {
    // The only view whose failure path was untested. Without it a portal
    // pointed at a stopped emulator shows "no operations yet", which reads as
    // a working tenant with nothing in it.
    vi.spyOn(globalThis, 'fetch').mockResolvedValue(errRes('operations unavailable', 500));
    render(Operations);
    await waitFor(() =>
      expect(screen.getByText('operations unavailable')).toBeInTheDocument());
  });

  it('dashes an operation that carries no result reference', async () => {
    vi.spyOn(globalThis, 'fetch').mockResolvedValue(res({ value: [{
        id: 'op-1', kind: 'CreateWorkspace', status: 'Running', createdAt: 1700000000,
      }] }));
    render(Operations);
    await screen.findByText('CreateWorkspace');
    expect(screen.getByText('—')).toBeInTheDocument();
  });

  it('refetches when Refresh is clicked', async () => {
    const fetchMock = vi.spyOn(globalThis, 'fetch').mockResolvedValue(res({ value: [] }));
    render(Operations);
    await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(1));
    await fireEvent.click(screen.getByRole('button', { name: 'Refresh' }));
    await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(2));
  });
});
