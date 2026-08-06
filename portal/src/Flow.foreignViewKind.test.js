import { render, screen, waitFor } from '@testing-library/svelte';
import { beforeEach, describe, expect, it, vi } from 'vitest';

// A kind the server declares AND offers as a filter, that this build cannot
// describe.
//
// The sibling Flow.foreignKind.test.js covers the other half: a kind in
// `AllKinds` with no home at all. This one is in `ViewKinds` too, so it reaches
// the log — and `describe()`'s default arm is what decides whether the row says
// something or the page throws.
//
// With the real contract this is impossible, and that is enforced at COMPILE
// time: `describe()` switches over every ViewKind and its default narrows to
// `never`, which scripts/check_kind_exhaustiveness.py proves by adding a kind
// and requiring the build to fail. This is the runtime companion — what a
// browser pointed at a newer emulator actually does — and the answer has to be
// "render the kind's name", not "white-screen the flow view".
vi.mock('./eventKinds', async (importOriginal) => {
  const real = await importOriginal();
  const kinds = [...real.EVENT_KINDS, 'ghost'];
  const views = [...real.VIEW_KINDS, 'ghost'];
  return {
    ...real,
    EVENT_KINDS: kinds,
    VIEW_KINDS: views,
    KIND_DOC: { ...real.KIND_DOC, ghost: 'a kind from another build' },
    isEventKind: (k) => kinds.includes(k),
    isViewKind: (k) => views.includes(k),
  };
});

const Flow = (await import('./Flow.svelte')).default;

class FakeEventSource {
  static last = null;
  constructor(url) {
    this.url = url;
    this.listeners = {};
    FakeEventSource.last = this;
  }
  addEventListener(kind, fn) {
    (this.listeners[kind] ||= []).push(fn);
  }
  close() {}
  emit(kind, payload) {
    for (const fn of this.listeners[kind] || []) fn({ data: JSON.stringify(payload) });
  }
}

describe('Flow: a filterable kind this build cannot describe', () => {
  beforeEach(() => {
    vi.restoreAllMocks();
    FakeEventSource.last = null;
    globalThis.EventSource = FakeEventSource;
    vi.spyOn(globalThis, 'fetch').mockResolvedValue({
      ok: true, status: 200, json: () => Promise.resolve({ value: [] }),
    });
  });

  it('lists it by name rather than failing to render the row', async () => {
    render(Flow);
    await waitFor(() => expect(screen.getByText(/Nothing yet/)).toBeInTheDocument());

    FakeEventSource.last.emit('ghost', {
      seq: 1, at: 1700000000, kind: 'ghost', itemId: 'lake-1',
    });

    // The row exists and the What column falls back to the kind itself: the
    // least wrong thing a build that has never heard of this kind can say.
    await waitFor(() => expect(screen.queryByText(/Nothing yet/)).not.toBeInTheDocument());
    const cells = screen.getAllByText('ghost');
    expect(cells.length).toBeGreaterThanOrEqual(2); // the chip, and the description
    // Not counted as unknown: the contract this build was given DOES name it.
    expect(screen.queryByText(/event\(s\) of an unknown kind/)).not.toBeInTheDocument();
  });
});
