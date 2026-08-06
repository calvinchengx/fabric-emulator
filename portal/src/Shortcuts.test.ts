import { render, screen, waitFor } from '@testing-library/svelte';
import { vi, describe, it, expect, beforeEach } from 'vitest';
import Shortcuts from './Shortcuts.svelte';
import { errRes, res } from './testing';

const rows = [
  {
    workspaceId: 'ws-1', workspaceName: 'analytics',
    itemId: 'it-1', itemName: 'src-lh',
    path: 'Files', name: 'linked',
    targetWorkspaceId: 'ws-2', targetItemId: 'it-2', targetPath: 'Files/raw',
    dangling: false,
  },
  {
    workspaceId: 'ws-1', workspaceName: 'analytics',
    itemId: 'it-1', itemName: 'src-lh',
    path: 'Tables', name: 'gone',
    targetWorkspaceId: 'ws-2', targetItemId: 'it-9', targetPath: '',
    dangling: true,
  },
];

describe('Shortcuts', () => {
  beforeEach(() => {
    vi.restoreAllMocks();
  });

  it('renders the empty state', async () => {
    vi.spyOn(globalThis, 'fetch').mockResolvedValue(res({ value: [] }));
    render(Shortcuts);
    await waitFor(() => expect(screen.getByText(/No shortcuts yet/)).toBeInTheDocument());
  });

  it('groups shortcuts by workspace and marks dangling targets', async () => {
    vi.spyOn(globalThis, 'fetch').mockResolvedValue(res({ value: rows }));
    render(Shortcuts);
    await waitFor(() => expect(screen.getByText(/analytics/)).toBeInTheDocument());
    expect(screen.getByText('Files/linked')).toBeInTheDocument();
    expect(screen.getByText(/ws-2\/it-2\/Files\/raw/)).toBeInTheDocument();
    expect(screen.getByText('resolves')).toBeInTheDocument();
    expect(screen.getByText('dangling')).toBeInTheDocument();
  });

  it('surfaces load errors', async () => {
    vi.spyOn(globalThis, 'fetch').mockResolvedValue(errRes('db gone', 500));
    render(Shortcuts);
    await waitFor(() => expect(screen.getByText('db gone')).toBeInTheDocument());
  });


  it('renders a shortcut whose target path is the item root', async () => {
    // `sc.targetPath || ''` — a shortcut to the item root carries no path, and
    // without the fallback the cell reads ".../undefined".
    vi.spyOn(globalThis, 'fetch').mockResolvedValue(res({ value: [{
        workspaceId: 'ws-1', workspaceName: 'analytics',
        itemId: 'it-1', itemName: 'lake', path: 'Tables', name: 'root_ref',
        targetWorkspaceId: 'ws-2', targetItemId: 'it-2', dangling: false,
      }] }));
    render(Shortcuts);
    await screen.findByText('Tables/root_ref');
    expect(screen.getByText('ws-2/it-2/')).toBeInTheDocument();
  });
});
