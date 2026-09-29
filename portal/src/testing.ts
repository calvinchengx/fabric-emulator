// Test support: the things every suite here was hand-rolling.
//
// Each file used to build its own `{ ok, status, json }` object and hand it to
// `vi.spyOn(globalThis, 'fetch')`. That works at runtime — the portal only ever
// reads those three members — but it is not a `Response`, so under TypeScript
// every call site needed the same cast. Casting once, here, with the reason
// written down, beats casting nineteen times.
/** A stand-in Response carrying `body` as its JSON.
 *
 * Deliberately partial: the cast is the point. A real `Response` needs a dozen
 * members the portal never touches, and constructing one per test would obscure
 * what each test is actually saying.
 */
export function res(
  body: unknown,
  { ok = true, status = 200 }: { ok?: boolean; status?: number } = {},
): Response {
  return { ok, status, json: () => Promise.resolve(body) } as unknown as Response;
}

/** A failing response carrying the emulator's error envelope. */
export function errRes(message: string, status = 500): Response {
  return res({ error: { message } }, { ok: false, status });
}

/** The installed fetch spy, for `.mock.calls`. */
export function fetchCalls(): unknown[][] {
  return (globalThis.fetch as unknown as { mock: { calls: unknown[][] } }).mock.calls;
}

/** The JSON body of a recorded fetch call.
 *
 * `RequestInit['body']` is `BodyInit | null | undefined`, so every assertion
 * about what was POSTed otherwise needs the same two guards. A call that
 * carried no body is a test asserting the wrong call, and says so.
 */
export function sentBody(call: unknown[] | undefined): any {
  const body = (call?.[1] as RequestInit | undefined)?.body;
  if (typeof body !== 'string') {
    throw new Error('that fetch call carried no JSON body');
  }
  return JSON.parse(body);
}

/** A stand-in EventSource: the component subscribes to it exactly as it would
 * to the emulator's SSE endpoint, and the test pushes frames through it. */
export class FakeEventSource {
  static last: FakeEventSource | null = null;
  url: string;
  listeners: Record<string, ((m: { data: string }) => void)[]> = {};
  closed = false;
  onopen?: () => void;
  onerror?: () => void;
  onmessage?: (m: { data: string }) => void;

  constructor(url: string) {
    this.url = url;
    FakeEventSource.last = this;
  }
  addEventListener(kind: string, fn: (m: { data: string }) => void) {
    (this.listeners[kind] ||= []).push(fn);
  }
  close() {
    this.closed = true;
  }
  open() {
    this.onopen?.();
  }
  emit(kind: string, payload: unknown) {
    const m = { data: JSON.stringify(payload) };
    for (const fn of this.listeners[kind] || []) fn(m);
  }
}

/** The last constructed FakeEventSource, or a clear failure if none was. */
export function stream(): FakeEventSource {
  const s = FakeEventSource.last;
  if (!s) throw new Error('no EventSource was constructed — did the component mount?');
  return s;
}

/** Install the stub. jsdom has no EventSource, so this is not optional. */
export function installEventSource() {
  FakeEventSource.last = null;
  (globalThis as { EventSource?: unknown }).EventSource = FakeEventSource;
}

/** Remove it again. `Reflect.deleteProperty` rather than `delete`, which
 * TypeScript refuses on a non-optional global. */
export function removeEventSource() {
  Reflect.deleteProperty(globalThis, 'EventSource');
}

/** The `<g>` a label sits in, or a clear failure. Every graph assertion needs
 * this, and `closest()` is nullable. */
export function groupOf(el: HTMLElement | null): Element {
  const g = el?.closest('g');
  if (!g) throw new Error('element is not inside a <g> — the graph did not render it');
  return g;
}

/** Assert `probe()` stays falsy for the whole of `windowMs`.
 *
 * The negative assertion, checked continuously rather than once: `probe` is
 * polled every `intervalMs` until the window elapses, and the first truthy
 * answer fails immediately, naming what was found. A single
 * `setTimeout`-then-check only samples the last instant of the window — it
 * reads as clean if the wrong thing appeared and then disappeared again before
 * that one check ran, which is not "stayed absent", and it is exactly the
 * shape scripts/check_vitest_test_flakiness.py exists to keep out of this
 * suite. Modelled on this repo's own Go (`internal/testsupport.StaysFalse`)
 * and Python (`e2e/waiting.py stays_empty`) siblings, which poll the same way
 * for the same reason.
 *
 * Not a `*.test.ts` file, so the checker above does not see this function's
 * own `setTimeout` — the same reason `internal/testsupport/wait.go` and
 * `e2e/waiting.py` are exempt from their siblings: a helper is not a test, and
 * bounding it correctly here is what a caller is trusting instead of writing
 * its own sleep.
 */
export async function staysAbsent(
  probe: () => unknown,
  windowMs: number,
  intervalMs = 5,
): Promise<void> {
  const check = () => {
    const got = probe();
    if (got) throw new Error(`expected to stay absent, but found: ${String(got)}`);
  };
  const deadline = Date.now() + windowMs;
  while (Date.now() < deadline) {
    check();
    await new Promise((r) => setTimeout(r, intervalMs));
  }
  // Probed once more after the window closes: without this the last
  // `intervalMs` of the window is never actually observed, so the assertion
  // would cover slightly less time than it claims to.
  check();
}
