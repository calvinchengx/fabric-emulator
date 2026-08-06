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

  it('follows the OS when nothing was ever chosen', () => {
    osWants(true);
    expect(theme.stored()).toBe('system');
    expect(theme.apply('system')).toBe('dark');
    expect(document.documentElement.dataset.theme).toBe('dark');
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

  it('forgets the override when told to follow the system again', () => {
    osWants(false);
    theme.set('dark');
    expect(theme.stored()).toBe('dark');
    expect(theme.set('system')).toBe('light');
    // Cleared, not stored as the word "system": the absence IS the preference,
    // so a machine that switches later is still followed.
    expect(localStorage.getItem('fe.theme')).toBeNull();
    expect(theme.stored()).toBe('system');
  });

  it('treats a nonsense stored value as no preference', () => {
    osWants(true);
    localStorage.setItem('fe.theme', 'aubergine');
    expect(theme.stored()).toBe('system');
    expect(theme.resolve(theme.stored())).toBe('dark');
  });

  it('cycles system → light → dark → system', () => {
    // Three states because "follow the OS" is a real answer; a two-way switch
    // would pin the portal to whatever the machine was that afternoon.
    expect(theme.next('system')).toBe('light');
    expect(theme.next('light')).toBe('dark');
    expect(theme.next('dark')).toBe('system');
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
      theme.apply('system');
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
