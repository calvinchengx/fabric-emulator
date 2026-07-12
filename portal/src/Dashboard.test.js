import { render, screen, waitFor } from '@testing-library/svelte';
import { vi, describe, it, expect, beforeEach } from 'vitest';
import Dashboard from './Dashboard.svelte';

describe('Dashboard', () => {
  beforeEach(() => {
    vi.restoreAllMocks();
  });

  it('aggregates counts from workspaces and operations', async () => {
    vi.spyOn(globalThis, 'fetch').mockImplementation((url) => {
      const body = url.includes('operations')
        ? { value: [{ id: 'op-1', status: 'Running' }, { id: 'op-2', status: 'Succeeded' }] }
        : {
            value: [
              { id: 'w1', itemCount: 2, workspaceIdentity: { applicationId: 'a' } },
              { id: 'w2', itemCount: 3, workspaceIdentity: null },
            ],
          };
      return Promise.resolve({ ok: true, status: 200, json: () => Promise.resolve(body) });
    });

    render(Dashboard);
    await waitFor(() => expect(screen.getByText('5')).toBeInTheDocument()); // items
    expect(screen.getByText('workspaces')).toBeInTheDocument();
    expect(screen.getByText('workspace identities')).toBeInTheDocument();
    const ones = screen.getAllByText('1'); // 1 running, 1 identity
    expect(ones.length).toBeGreaterThanOrEqual(2);
  });

  it('surfaces load errors', async () => {
    vi.spyOn(globalThis, 'fetch').mockResolvedValue({
      ok: false,
      status: 503,
      json: () => Promise.resolve(null),
    });
    render(Dashboard);
    await waitFor(() => expect(screen.getByText('HTTP 503')).toBeInTheDocument());
  });
});
