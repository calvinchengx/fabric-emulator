import { render, screen, fireEvent, waitFor } from '@testing-library/svelte';
import { vi, describe, it, expect, beforeEach } from 'vitest';
import Workspaces from './Workspaces.svelte';
import { errRes, res } from './testing';

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
    vi.spyOn(globalThis, 'fetch').mockResolvedValue(res({ value: [] }));
    render(Workspaces);
    await waitFor(() => expect(screen.getByText(/No workspaces yet/)).toBeInTheDocument());
  });

  it('lists workspaces and expands the detail panel', async () => {
    vi.spyOn(globalThis, 'fetch').mockImplementation((url: RequestInfo | URL) => {
      const body = String(url).endsWith('/workspaces') ? { value: [ws] } : detail;
      return Promise.resolve(res(body));
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
    vi.spyOn(globalThis, 'fetch').mockResolvedValue(errRes('db gone', 500));
    render(Workspaces);
    await waitFor(() => expect(screen.getByText('db gone')).toBeInTheDocument());
  });


  it('collapses the detail panel when the same row is clicked again', async () => {
    // `toggle` has an early-return arm that closes what is open. Untested, a
    // second click would re-fetch and the panel would never close.
    vi.spyOn(globalThis, 'fetch').mockImplementation((url: RequestInfo | URL) =>
      String(url).match(/workspaces\/.+/)
        ? Promise.resolve(res(detail))
        : Promise.resolve(res({ value: [ws] })));
    render(Workspaces);
    const row = await screen.findByText('analytics');
    await fireEvent.click(row);
    await waitFor(() => expect(screen.getByText('Role assignments')).toBeInTheDocument());
    await fireEvent.click(row);
    await waitFor(() =>
      expect(screen.queryByText('Role assignments')).not.toBeInTheDocument());
  });

  it('surfaces a failure to load one workspace detail', async () => {
    // A different request from the listing, and its own failure path: without
    // this the panel simply never opens and nothing says why.
    vi.spyOn(globalThis, 'fetch').mockImplementation((url: RequestInfo | URL) =>
      String(url).match(/workspaces\/.+/)
        ? Promise.resolve(errRes('detail exploded', 500))
        : Promise.resolve(res({ value: [{
              id: 'ws-1', displayName: 'analytics', capacityId: 'cap-1',
              itemCount: 0, roleCount: 0 }] })));
    render(Workspaces);
    await fireEvent.click(await screen.findByText('analytics'));
    await waitFor(() => expect(screen.getByText('detail exploded')).toBeInTheDocument());
  });

  it('dashes what a workspace does not have', async () => {
    // capacityId, git and workspaceIdentity are all optional. Each has a dash
    // arm that had never rendered, and `undefined` in a cell looks like data.
    vi.spyOn(globalThis, 'fetch').mockResolvedValue(res({ value: [{
        id: 'ws-bare', displayName: 'bare', itemCount: 0, roleCount: 0 }] }));
    render(Workspaces);
    await screen.findByText('bare');
    expect(screen.getAllByText('—')).toHaveLength(3);
  });

  it('reports an empty workspace and an unconnected repo as facts', async () => {
    vi.spyOn(globalThis, 'fetch').mockImplementation((url: RequestInfo | URL) =>
      String(url).match(/workspaces\/.+/)
        ? Promise.resolve(res({
            id: 'ws-1', displayName: 'analytics', items: [], roleAssignments: [] }))
        : Promise.resolve(res({
            value: [{ id: 'ws-1', displayName: 'analytics', itemCount: 0, roleCount: 0 }] })));
    render(Workspaces);
    await fireEvent.click(await screen.findByText('analytics'));
    await waitFor(() => expect(screen.getByText('not connected')).toBeInTheDocument());
    expect(screen.getByText('none')).toBeInTheDocument();
  });

  it('shows a git-connected workspace, defaulting the directory to the root', async () => {
    vi.spyOn(globalThis, 'fetch').mockImplementation((url: RequestInfo | URL) =>
      String(url).match(/workspaces\/.+/)
        ? Promise.resolve(res({
            id: 'ws-1', displayName: 'analytics', items: [], roleAssignments: [],
            git: { gitProviderType: 'GitHub', organizationName: 'contoso',
                   repositoryName: 'fabric', branchName: 'main' } }))
        : Promise.resolve(res({
            value: [{ id: 'ws-1', displayName: 'analytics', itemCount: 0, roleCount: 0,
                      git: { branchName: 'main' } }] })));
    render(Workspaces);
    await fireEvent.click(await screen.findByText('analytics'));
    // No directoryName on the wire means the repo root, and "(undefined)" is
    // what renders if nothing defaults it.
    await waitFor(() => expect(screen.getByText('(/)')).toBeInTheDocument());
  });

  it('says provisioned for a workspace that has an identity', async () => {
    // The other side of the identity cell. Only the dash had ever rendered, so
    // nothing proved the positive case says anything at all.
    vi.spyOn(globalThis, 'fetch').mockResolvedValue(res({ value: [{
        id: 'ws-id', displayName: 'withid', itemCount: 0, roleCount: 0,
        workspaceIdentity: { applicationId: 'app-1' } }] }));
    render(Workspaces);
    await screen.findByText('withid');
    expect(screen.getByText('provisioned')).toBeInTheDocument();
  });
});
