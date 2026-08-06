import { render, screen, waitFor } from '@testing-library/svelte';
import { beforeEach, describe, expect, it, vi } from 'vitest';
import { installEventSource, res, stream } from './testing';

// A kind the SERVER declares that this build cannot place.
//
// With the real contract this cannot happen: `AllKinds` minus `ViewKinds` is
// exactly `dropped`, which `ingest` handles before the check, and TypeScript
// narrows the remaining arm to `never` — that is the guarantee
// scripts/check_kind_exhaustiveness.py exists to prove. But `/_emulator/events`
// is a public stream and nothing stops a client being pointed at a different
// build, so the runtime still counts what it cannot render.
//
// The only way to exercise that arm is to lie about the contract, which is what
// this file does: a generated module replaced by one naming a kind the portal
// has no home for. The mock lives in its own file because vi.mock is
// file-scoped, and because a faked contract must never leak into a test that is
// asserting the real one.
vi.mock('./eventKinds', async (importOriginal) => {
  const real: any = await importOriginal();
  return {
    ...real,
    EVENT_KINDS: [...real.EVENT_KINDS, 'ghost'],
    // 'ghost' is deliberately NOT a view kind: in AllKinds, unrenderable,
    // which is the shape `ingest` has to survive.
    VIEW_KINDS: real.VIEW_KINDS,
    KIND_DOC: { ...real.KIND_DOC, ghost: 'a kind from another build' },
    isEventKind: (k: string) => [...real.EVENT_KINDS, 'ghost'].includes(k),
    isViewKind: real.isViewKind,
  };
});

const Flow = (await import('./Flow.svelte')).default;

describe('Flow: a kind this build has no home for', () => {
  beforeEach(() => {
    vi.restoreAllMocks();
    installEventSource();
    vi.spyOn(globalThis, 'fetch').mockResolvedValue(res({ value: [] }));
  });

  it('subscribes to it, counts it, and does not put it in the log', async () => {
    render(Flow);
    await waitFor(() => expect(screen.getByText(/Nothing yet/)).toBeInTheDocument());

    // Subscribed: the generated list is what the subscription loop reads, so a
    // kind in it gets a listener even when nothing can render it.
    expect(stream().listeners.ghost).toHaveLength(1);

    stream().emit('ghost', { seq: 1, at: 1700000000, kind: 'ghost' });

    // Counted, not swallowed. The alternative — dropping it silently — is the
    // exact bug the whole event-contract effort exists to make impossible.
    await waitFor(() =>
      expect(screen.getByText('1 event(s) of an unknown kind')).toBeInTheDocument());
    // And it is not listed: the log renders view kinds, and this is not one.
    expect(screen.getByText(/Nothing yet/)).toBeInTheDocument();
  });
});
