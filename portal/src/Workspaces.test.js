import { render, screen, fireEvent, waitFor } from '@testing-library/svelte';
import { vi, describe, it, expect, beforeEach } from 'vitest';
import Workspaces from './Workspaces.svelte';

const ws = {
  id: 'aaaa-1',
  displayName: 'analytics',
  capacityId: 'cap-1',
  itemCount: 2,
  roleCount: 1,
  git: { branchName: 'main' },
  workspaceIdentity: null,
};

const detail = {
  workspace: ws,
  items: [{ id: 'it-1', type: 'Notebook', displayName: 'hello' }],
  roleAssignments: [{ id: 'ra-1', role: 'Admin', principal: { id: 'sp-1', type: 'ServicePrincipal' } }],
  git: {
    gitProviderType: 'AzureDevOps',
    organizationName: 'org',
    repositoryName: 'repo',
    branchName: 'main',
    directoryName: '/',
  },
};

describe('Workspaces', () => {
  beforeEach(() => {
    vi.restoreAllMocks();
  });

  it('renders the empty state', async () => {
    vi.spyOn(globalThis, 'fetch').mockResolvedValue({
      ok: true,
      status: 200,
      json: () => Promise.resolve({ value: [] }),
    });
    render(Workspaces);
    await waitFor(() => expect(screen.getByText(/No workspaces yet/)).toBeInTheDocument());
  });

  it('lists workspaces and expands the detail panel', async () => {
    vi.spyOn(globalThis, 'fetch').mockImplementation((url) => {
      const body = url.endsWith('/workspaces') ? { value: [ws] } : detail;
      return Promise.resolve({ ok: true, status: 200, json: () => Promise.resolve(body) });
    });

    render(Workspaces);
    await waitFor(() => expect(screen.getByText('analytics')).toBeInTheDocument());
    expect(screen.getByText('main')).toBeInTheDocument();

    await fireEvent.click(screen.getByText('analytics'));
    await waitFor(() => expect(screen.getByText('hello')).toBeInTheDocument());
    expect(screen.getByText('Admin')).toBeInTheDocument();
    expect(screen.getByText(/org\/repo/)).toBeInTheDocument();
  });

  it('surfaces load errors', async () => {
    vi.spyOn(globalThis, 'fetch').mockResolvedValue({
      ok: false,
      status: 500,
      json: () => Promise.resolve({ error: { message: 'db gone' } }),
    });
    render(Workspaces);
    await waitFor(() => expect(screen.getByText('db gone')).toBeInTheDocument());
  });
});
