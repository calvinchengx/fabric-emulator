import { render, screen, waitFor } from '@testing-library/svelte';
import { vi, describe, it, expect, beforeEach } from 'vitest';
import Jobs from './Jobs.svelte';
import { errRes, res } from './testing';

const rows = [
  {
    id: 'job-1', itemId: 'it-1', itemName: 'nightly', itemType: 'Notebook',
    workspaceId: 'ws-1', jobType: 'RunNotebook', invokeType: 'Manual',
    status: 'InProgress', createdAt: 1700000000,
  },
  {
    id: 'job-2', itemId: 'it-gone', itemName: '', itemType: '',
    workspaceId: '', jobType: 'DefaultJob', invokeType: 'Manual',
    status: 'Completed', createdAt: 1700000100,
  },
];

describe('Jobs', () => {
  beforeEach(() => {
    vi.restoreAllMocks();
  });

  it('renders the empty state', async () => {
    vi.spyOn(globalThis, 'fetch').mockResolvedValue(res({ value: [] }));
    render(Jobs);
    await waitFor(() => expect(screen.getByText(/No jobs yet/)).toBeInTheDocument());
  });

  it('lists jobs with clock-derived status and handles deleted items', async () => {
    vi.spyOn(globalThis, 'fetch').mockResolvedValue(res({ value: rows }));
    render(Jobs);
    await waitFor(() => expect(screen.getByText('nightly')).toBeInTheDocument());
    expect(screen.getByText('RunNotebook')).toBeInTheDocument();
    expect(screen.getByText('InProgress')).toBeInTheDocument();
    expect(screen.getByText('Completed')).toBeInTheDocument();
    // A job whose item was deleted still lists, without item context.
    expect(screen.getByText('deleted item')).toBeInTheDocument();
    expect(screen.getByText('it-gone')).toBeInTheDocument();
  });

  it('surfaces load errors', async () => {
    vi.spyOn(globalThis, 'fetch').mockResolvedValue(errRes('db gone', 500));
    render(Jobs);
    await waitFor(() => expect(screen.getByText('db gone')).toBeInTheDocument());
  });
});
