// Light, dark, or whatever the OS says. **Dark unless told otherwise.**
//
// The palette used to hang off `@media (prefers-color-scheme: dark)` alone,
// with the reasoning that a local tool has "nowhere to store a preference".
// That stopped being true when the sidebar's fold began persisting to
// localStorage.
//
// WHY DARK IS THE DEFAULT rather than the OS. This portal is looked at in two
// situations: beside a terminal while a pipeline runs, and in a screenshot or
// recording. Both are dark far more often than not, and following the machine
// makes the product look different to every reader of the same document. A
// fixed default is the one that can be designed for.
//
// `system` is still reachable — it is simply an explicit choice now rather than
// the absence of one, which is why `set` STORES it instead of clearing the key.
// Were absence to keep meaning "follow the OS", making dark the default would
// have made "follow the OS" unreachable.
//
// WHY THE CSS NEEDS NO MEDIA QUERY ANY MORE. The portal is a client-rendered
// Svelte app — with JavaScript off there is no portal to theme — so resolving
// the preference here and stamping `data-theme` on <html> is strictly more
// capable than a media query, and it keeps the dark palette written once.
// index.html stamps it before the bundle loads so the first paint is correct.

export type Theme = 'system' | 'light' | 'dark';
export type Resolved = 'light' | 'dark';

const KEY = 'fe.theme';

/** What a portal with no stored preference shows. */
export const DEFAULT: Theme = 'dark';

/** The stored preference, or the default when there is none (or it is nonsense). */
export function stored(): Theme {
  const raw = globalThis.localStorage?.getItem(KEY);
  return raw === 'light' || raw === 'dark' || raw === 'system' ? raw : DEFAULT;
}

/** What the OS is asking for right now. */
export function osPrefers(): Resolved {
  return globalThis.matchMedia?.('(prefers-color-scheme: dark)').matches ? 'dark' : 'light';
}

/** The theme a preference actually resolves to. */
export function resolve(theme: Theme): Resolved {
  return theme === 'system' ? osPrefers() : theme;
}

/** Paint it. The attribute is what every `:root[data-theme='dark']` rule reads. */
export function apply(theme: Theme): Resolved {
  const shown = resolve(theme);
  document.documentElement.dataset.theme = shown;
  // `color-scheme` is what makes form controls, scrollbars and the browser's
  // own UI match. Without it a dark portal keeps white scrollbars.
  document.documentElement.style.colorScheme = shown;
  return shown;
}

/** Store a preference and paint it.
 *
 * All three are stored, including 'system'. Clearing the key would mean "no
 * preference", and no preference now means dark — so choosing to follow the OS
 * has to be written down or it would not survive a reload.
 */
export function set(theme: Theme): Resolved {
  globalThis.localStorage?.setItem(KEY, theme);
  return apply(theme);
}

/** The next theme in the cycle: dark → light → system → dark.
 *
 * Three states rather than two, because "follow the OS" is a real answer and a
 * two-way switch cannot express it — once flipped, it would pin the portal to
 * whatever it was that afternoon.
 *
 * Starting at dark, the first click goes to LIGHT rather than to system: the
 * default is dark and most machines are too, so a first click landing on
 * 'system' would usually change nothing on screen and read as a broken button.
 */
export function next(theme: Theme): Theme {
  return theme === 'dark' ? 'light' : theme === 'light' ? 'system' : 'dark';
}

/** Track OS changes while the preference is 'system'. Returns an unsubscribe.
 *
 * `onChange` is optional: the repaint happens here, so a caller that only wants
 * the portal to follow the machine passes nothing.
 */
export function followOS(onChange?: (shown: Resolved) => void): () => void {
  const mq = globalThis.matchMedia?.('(prefers-color-scheme: dark)');
  if (!mq) return () => {};
  const handler = () => {
    if (stored() === 'system') onChange?.(apply('system'));
  };
  mq.addEventListener('change', handler);
  return () => mq.removeEventListener('change', handler);
}

/** What to show on the toggle: the glyph, and what it means. */
export function badge(theme: Theme): { glyph: string; label: string } {
  if (theme === 'light') return { glyph: '☀', label: 'Theme: light. Switch to dark.' };
  if (theme === 'dark') return { glyph: '☾', label: 'Theme: dark. Follow the system.' };
  return { glyph: '◐', label: 'Theme: system. Switch to light.' };
}
