import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import * as theme from './theme';

// The theme is the one preference the portal persists besides the sidebar, and
// getting it wrong is loud: a recording made in the wrong palette, or a portal
// that ignores the machine it runs on.

/** Pretend the OS wants dark (or light), the way matchMedia reports it. */
function osWants(dark: boolean) {
  const listeners: (() => void)[] = [];
  const mq = {
    matches: dark,
    addEventListener: (_: string, fn: () => void) => listeners.push(fn),
    removeEventListener: (_: string, fn: () => void) => {
      const i = listeners.indexOf(fn);
      if (i >= 0) listeners.splice(i, 1);
    },
  };
  vi.stubGlobal('matchMedia', () => mq);
  return {
    /** The OS flips, and everything listening hears about it. */
    flip(toDark: boolean) {
      mq.matches = toDark;
      for (const fn of [...listeners]) fn();
    },
    get listenerCount() {
      return listeners.length;
    },
  };
}

describe('theme', () => {
  beforeEach(() => {
    localStorage.clear();
    document.documentElement.removeAttribute('data-theme');
    document.documentElement.style.colorScheme = '';
  });
  afterEach(() => vi.unstubAllGlobals());

  it('is dark when nothing was ever chosen, whatever the machine says', () => {
    // A fixed default is the point: the portal is read beside a terminal and in
    // recordings, and following the machine makes it look different to every
    // reader of the same document.
    osWants(false);
    expect(theme.stored()).toBe('dark');
    expect(theme.apply(theme.stored())).toBe('dark');
    expect(document.documentElement.dataset.theme).toBe('dark');
  });

  it('still follows the OS once that is explicitly chosen', () => {
    osWants(true);
    expect(theme.set('system')).toBe('dark');
    osWants(false);
    expect(theme.apply('system')).toBe('light');
  });

  it('sets color-scheme too, so the browser chrome matches', () => {
    // Without it the scrollbars and form controls stay light on a dark portal.
    osWants(true);
    theme.apply('system');
    expect(document.documentElement.style.colorScheme).toBe('dark');
  });

  it('lets an explicit choice beat the OS', () => {
    osWants(true);
    expect(theme.set('light')).toBe('light');
    expect(document.documentElement.dataset.theme).toBe('light');
    expect(theme.stored()).toBe('light');
  });

  it('writes down the choice to follow the system, rather than clearing it', () => {
    osWants(false);
    theme.set('dark');
    expect(theme.stored()).toBe('dark');
    expect(theme.set('system')).toBe('light');
    // Stored as the word, NOT cleared. Absence now means dark, so clearing
    // would silently turn "follow the OS" back into "dark" on the next load.
    expect(localStorage.getItem('fe.theme')).toBe('system');
    expect(theme.stored()).toBe('system');
  });

  it('treats a nonsense stored value as no preference, and so as the default', () => {
    osWants(false);
    localStorage.setItem('fe.theme', 'aubergine');
    expect(theme.stored()).toBe('dark');
    expect(theme.resolve(theme.stored())).toBe('dark');
  });

  it('cycles dark → light → system → dark', () => {
    // Three states because "follow the OS" is a real answer; a two-way switch
    // would pin the portal to whatever the machine was that afternoon.
    // Dark first, because dark is where a fresh portal starts and a first click
    // onto 'system' would usually look like nothing happened.
    expect(theme.next('dark')).toBe('light');
    expect(theme.next('light')).toBe('system');
    expect(theme.next('system')).toBe('dark');
  });

  it('labels the toggle with what it is and what it will do', () => {
    for (const t of ['system', 'light', 'dark'] as theme.Theme[]) {
      const { glyph, label } = theme.badge(t);
      expect(glyph).not.toBe('');
      expect(label.toLowerCase()).toContain(t === 'system' ? 'system' : t);
    }
  });

  describe('following the OS as it changes', () => {
    it('repaints when the machine switches and no override is set', () => {
      const os = osWants(false);
      // Explicitly, because absence means dark now and followOS only acts while
      // the stored preference is 'system'.
      theme.set('system');
      const seen: string[] = [];
      const stop = theme.followOS((shown) => seen.push(shown));
      os.flip(true);
      expect(seen).toEqual(['dark']);
      expect(document.documentElement.dataset.theme).toBe('dark');
      stop();
    });

    it('ignores the machine once a choice has been made', () => {
      const os = osWants(false);
      theme.set('light');
      const seen: string[] = [];
      const stop = theme.followOS((shown) => seen.push(shown));
      os.flip(true);
      expect(seen).toEqual([]);
      expect(document.documentElement.dataset.theme).toBe('light');
      stop();
    });

    it('detaches when unsubscribed', () => {
      const os = osWants(false);
      const stop = theme.followOS(() => {});
      expect(os.listenerCount).toBe(1);
      stop();
      expect(os.listenerCount).toBe(0);
    });

    it('is a no-op where matchMedia does not exist', () => {
      // jsdom has it, but a headless embedding might not, and the portal must
      // still render rather than throwing on startup.
      vi.stubGlobal('matchMedia', undefined);
      expect(theme.osPrefers()).toBe('light');
      expect(() => theme.followOS(() => {})()).not.toThrow();
    });
  });
});
