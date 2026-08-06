// Light, dark, or whatever the OS says.
//
// The palette used to hang off `@media (prefers-color-scheme: dark)` alone,
// with the reasoning that a local tool has "nowhere to store a preference".
// That stopped being true when the sidebar's fold began persisting to
// localStorage, and following the OS is the wrong default for the one thing
// this portal is most used for: a screenshot or a recording, where the author
// wants light regardless of how their machine is set.
//
// WHY THE CSS NEEDS NO MEDIA QUERY ANY MORE. The portal is a client-rendered
// Svelte app — with JavaScript off there is no portal to theme — so resolving
// the preference here and stamping `data-theme` on <html> is strictly more
// capable than a media query, and it keeps the dark palette written once.
// index.html stamps it before the bundle loads so the first paint is correct.

export type Theme = 'system' | 'light' | 'dark';
export type Resolved = 'light' | 'dark';

const KEY = 'fe.theme';

/** The stored preference, or 'system' when there is none (or it is nonsense). */
export function stored(): Theme {
  const raw = globalThis.localStorage?.getItem(KEY);
  return raw === 'light' || raw === 'dark' ? raw : 'system';
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

/** Store a preference and paint it. 'system' clears the stored override. */
export function set(theme: Theme): Resolved {
  if (theme === 'system') globalThis.localStorage?.removeItem(KEY);
  else globalThis.localStorage?.setItem(KEY, theme);
  return apply(theme);
}

/** The next theme in the cycle: system → light → dark → system.
 *
 * Three states rather than two, because "follow the OS" is a real answer and a
 * two-way switch cannot express it — once flipped, it would pin the portal to
 * whatever it was that afternoon.
 */
export function next(theme: Theme): Theme {
  return theme === 'system' ? 'light' : theme === 'light' ? 'dark' : 'system';
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
