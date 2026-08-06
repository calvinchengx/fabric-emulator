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

/** How many requests went to a path fragment. */
export function countRequests(fragment: string): number {
  return fetchCalls().filter((c) => String(c[0]).includes(fragment)).length;
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
