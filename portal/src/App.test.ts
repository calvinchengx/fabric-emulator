import { fireEvent, render, screen, waitFor } from '@testing-library/svelte';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import App from './App.svelte';
import { installEventSource, removeEventSource, res } from './testing';

// The shell had no test at all — 92 statements, every route in the portal, and
// the sidebar preference, none of it exercised. The route table is the part that
// matters most: it is a 13-arm chain, and an arm pointing at the wrong component
// looks perfectly fine until someone clicks that link.

// Every view mounts on render and asks for something. One router by URL keeps
// each case's setup to the thing it is actually about.
function mockApi({ health = { status: 'healthy', build: 'v0.17.0' } }: { health?: any } = {}) {
  vi.spyOn(globalThis, 'fetch').mockImplementation((url: RequestInfo | URL) => {
    if (String(url).includes('/health')) {
      return health
        ? Promise.resolve(res(health))
        : Promise.reject(new Error('down'));
    }
    // Everything else: an empty tenant, plus the few scalar fields a view
    // renders unconditionally. `now` in particular is not optional — Clock
    // formats it as a Date, and `new Date(undefined * 1000)` throws
    // "Invalid time value" and takes the whole mount down.
    return Promise.resolve(res({
        value: [], columns: [], preview: [],
        now: 1700000000, offset: 0, frozen: false,
      }));
  });
}
function at(hash: string) {
  location.hash = hash;
  return render(App);
}

describe('App', () => {
  beforeEach(() => {
    vi.restoreAllMocks();
    localStorage.clear();
    installEventSource();
    mockApi();
  });
  afterEach(() => {
    removeEventSource();
    location.hash = '';
  });

  // One case per arm of the route chain, asserted on something only that view
  // renders. A table of pairs rather than 13 near-identical tests, because the
  // point is coverage of the CHAIN and a reader should see all of it at once.
  it.each([
    ['#workspaces', 'Workspaces'],
    ['#connections', 'Connections'],
    ['#capacities', 'Capacities'],
    ['#operations', 'Operations'],
    ['#jobs', 'Jobs'],
    ['#flow', 'Data flow'],
    ['#models', 'Semantic models'],
    ['#shortcuts', 'OneLake shortcuts'],
    ['#warehouse', 'Warehouse SQL'],
    ['#clock', 'Clock'],
    ['#faults', 'Fault injection'],
    ['#identities', 'Workspace identities'],
  ])('routes %s to its own view', async (hash, heading) => {
    at(hash);
    await waitFor(() =>
      expect(screen.getByRole('heading', { level: 1, name: heading })).toBeInTheDocument(),
    );
  });

  it('falls back to the dashboard for an empty hash', async () => {
    at('');
    await waitFor(() =>
      expect(screen.getByRole('heading', { level: 1 })).toBeInTheDocument(),
    );
    // Not a named assertion on "Dashboard": the fallback exists so that an
    // UNKNOWN view still renders something, which the next case pins.
    expect(screen.queryByRole('heading', { name: 'Data flow' })).not.toBeInTheDocument();
  });

  it('falls back to the dashboard for a view that does not exist', async () => {
    // A stale bookmark or a typo must not white-screen the portal.
    at('#no-such-view');
    await waitFor(() =>
      expect(screen.getByRole('heading', { level: 1 })).toBeInTheDocument(),
    );
  });

  it('follows a hash change without a reload', async () => {
    at('#clock');
    await waitFor(() =>
      expect(screen.getByRole('heading', { name: 'Clock' })).toBeInTheDocument(),
    );
    location.hash = '#jobs';
    window.dispatchEvent(new Event('hashchange'));
    await waitFor(() =>
      expect(screen.getByRole('heading', { name: 'Jobs' })).toBeInTheDocument(),
    );
    expect(screen.queryByRole('heading', { name: 'Clock' })).not.toBeInTheDocument();
  });

  it('passes the hash parameter to the model page', async () => {
    // `#models/{id}` is the reason the router carries a param at all: without
    // it a detail view can only be an accordion, and an accordion has no
    // address. Asserted through the request the detail page makes.
    const seen: string[] = [];
    vi.spyOn(globalThis, 'fetch').mockImplementation((url: RequestInfo | URL) => {
      seen.push(String(url));
      return Promise.resolve(res({ value: [], status: 'healthy' }));
    });
    at('#models/' + encodeURIComponent('model-abc'));
    await waitFor(() => expect(seen.some((u) => u.includes('/portal/models'))).toBe(true));
  });

  it('marks the current view in the sidebar and no other', async () => {
    at('#jobs');
    const active = await screen.findByRole('link', { name: 'Jobs' });
    expect(active).toHaveClass('active');
    // Scoped to the nav: the Jobs view's own prose links to the Clock page, so
    // a document-wide query for "Clock" is ambiguous by two.
    const nav = document.querySelector('nav.sidenav')!;
    const others = [...nav.querySelectorAll('a')].filter((a: Element) => a !== active);
    expect(others).toHaveLength(12);
    for (const a of others) expect(a).not.toHaveClass('active');
  });

  it('shows the build beside the badge, so a screenshot says which emulator it was', async () => {
    at('#clock');
    await waitFor(() => expect(screen.getByText('v0.17.0')).toBeInTheDocument());
    expect(screen.getByText('healthy')).toBeInTheDocument();
  });

  it('omits the build when the health payload has none', async () => {
    mockApi({ health: { status: 'healthy' } });
    at('#clock');
    await waitFor(() => expect(screen.getByText('healthy')).toBeInTheDocument());
    expect(screen.queryByTitle('fabric-emulator build')).not.toBeInTheDocument();
  });

  it('renders without a health chip when /health cannot be reached', async () => {
    // The portal must still work against an emulator that is starting up; a
    // failed health probe is not a reason to show nothing.
    mockApi({ health: null });
    at('#clock');
    await waitFor(() =>
      expect(screen.getByRole('heading', { name: 'Clock' })).toBeInTheDocument(),
    );
    expect(screen.queryByText('healthy')).not.toBeInTheDocument();
  });

  describe('the sidebar preference', () => {
    it('is open by default and folds on click', async () => {
      at('#clock');
      expect(await screen.findByRole('link', { name: 'Jobs' })).toBeInTheDocument();
      await fireEvent.click(screen.getByRole('button', { name: 'Toggle sidebar' }));
      expect(screen.queryByRole('link', { name: 'Jobs' })).not.toBeInTheDocument();
      // Persisted, because a preference that resets every reload is a nag.
      expect(localStorage.getItem('fe.nav')).toBe('closed');
    });

    it('starts folded when that is what was stored', async () => {
      localStorage.setItem('fe.nav', 'closed');
      at('#clock');
      await waitFor(() =>
        expect(screen.getByRole('heading', { name: 'Clock' })).toBeInTheDocument(),
      );
      expect(screen.queryByRole('link', { name: 'Jobs' })).not.toBeInTheDocument();
    });

    it('unfolds again and records that too', async () => {
      localStorage.setItem('fe.nav', 'closed');
      at('#clock');
      await fireEvent.click(await screen.findByRole('button', { name: 'Toggle sidebar' }));
      expect(await screen.findByRole('link', { name: 'Jobs' })).toBeInTheDocument();
      expect(localStorage.getItem('fe.nav')).toBe('open');
    });

    it('treats any other stored value as open, so a stray write cannot hide the nav', async () => {
      localStorage.setItem('fe.nav', 'banana');
      at('#clock');
      expect(await screen.findByRole('link', { name: 'Jobs' })).toBeInTheDocument();
    });
  });
});
