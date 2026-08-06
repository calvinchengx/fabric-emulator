import { render, screen, waitFor } from '@testing-library/svelte';
import { beforeEach, describe, expect, it, vi } from 'vitest';
import Identities from './Identities.svelte';

// This view answers one question — which workspaces have a provisioned
// identity — and it answers it by FILTERING, which is the part worth pinning: a
// workspace without `workspaceIdentity` must not reach the table, because the
// row would read the application id off `undefined`.

const withIdentity = {
  id: 'ws-1',
  displayName: 'analytics',
  workspaceIdentity: { applicationId: 'app-111', servicePrincipalId: 'sp-222' },
};
const withoutIdentity = { id: 'ws-2', displayName: 'scratch' };

function mockWorkspaces(value) {
  vi.spyOn(globalThis, 'fetch').mockResolvedValue({
    ok: true,
    status: 200,
    json: () => Promise.resolve({ value }),
  });
}

describe('Identities', () => {
  beforeEach(() => {
    vi.restoreAllMocks();
  });

  it('lists a provisioned identity with both of its ids', async () => {
    mockWorkspaces([withIdentity]);
    render(Identities);
    await waitFor(() => expect(screen.getByText('analytics')).toBeInTheDocument());
    // Both ids, because they are different objects in entra — the app
    // registration and the service principal — and a view that showed one
    // twice would look right.
    expect(screen.getByText('app-111')).toBeInTheDocument();
    expect(screen.getByText('sp-222')).toBeInTheDocument();
  });

  it('says plainly that nothing is provisioned rather than showing an empty table', async () => {
    mockWorkspaces([]);
    render(Identities);
    await waitFor(() =>
      expect(screen.getByText('No workspace has a provisioned identity.')).toBeInTheDocument(),
    );
    expect(screen.queryByRole('table')).not.toBeInTheDocument();
  });

  it('keeps an unprovisioned workspace out of the table', async () => {
    // The filter is the whole logic of this view. Without it the row renders
    // `undefined.applicationId` and the page throws.
    mockWorkspaces([withoutIdentity, withIdentity]);
    render(Identities);
    await waitFor(() => expect(screen.getByText('analytics')).toBeInTheDocument());
    expect(screen.queryByText('scratch')).not.toBeInTheDocument();
    expect(screen.getAllByRole('row')).toHaveLength(2); // header + one identity
  });

  it('shows the empty state when every workspace is unprovisioned', async () => {
    mockWorkspaces([withoutIdentity]);
    render(Identities);
    await waitFor(() =>
      expect(screen.getByText('No workspace has a provisioned identity.')).toBeInTheDocument(),
    );
  });

  it('surfaces a failed listing instead of looking like an empty tenant', async () => {
    vi.spyOn(globalThis, 'fetch').mockResolvedValue({
      ok: false,
      status: 503,
      json: () => Promise.resolve({ error: { message: 'store unavailable' } }),
    });
    render(Identities);
    await waitFor(() => expect(screen.getByText('store unavailable')).toBeInTheDocument());
    // The empty state still shows — there is genuinely nothing to list — but the
    // error is what says why, and losing it would report "none provisioned" for
    // a request that never succeeded.
    expect(screen.getByText('No workspace has a provisioned identity.')).toBeInTheDocument();
  });
});
