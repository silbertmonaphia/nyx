import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { installMatchMedia } from '../test/matchMedia';

const STORAGE_KEY = 'nyx.theme';

async function loadTheme() {
  vi.resetModules();
  return await import('./theme');
}

describe('theme store', () => {
  let os: ReturnType<typeof installMatchMedia>;

  beforeEach(() => {
    window.localStorage.clear();
    document.documentElement.className = '';
    document.documentElement.style.colorScheme = '';
    os = installMatchMedia(false);
  });

  afterEach(() => {
    vi.restoreAllMocks();
  });

  describe('resolve', () => {
    it.each([
      ['light', false, 'light'],
      ['light', true, 'light'],
      ['dark', false, 'dark'],
      ['dark', true, 'dark'],
      [null, false, 'light'],
      [null, true, 'dark'],
    ])(
      'stored %s with OS dark=%s resolves to %s',
      async (stored, darkScheme, expected) => {
        os.set(darkScheme as boolean);
        const theme = await loadTheme();
        if (stored === null) window.localStorage.removeItem(STORAGE_KEY);
        else window.localStorage.setItem(STORAGE_KEY, stored as string);

        expect(theme.resolve()).toBe(expected);
      },
    );

    it('treats an unrecognized stored value as no choice', async () => {
      os.set(true);
      window.localStorage.setItem(STORAGE_KEY, 'solarized');
      const theme = await loadTheme();

      expect(theme.getChoice()).toBeNull();
      expect(theme.resolve()).toBe('dark');
    });

    it('re-reads the OS scheme on every resolve', async () => {
      const theme = await loadTheme();
      expect(theme.resolve()).toBe('light');
      os.set(true);
      expect(theme.resolve()).toBe('dark');
    });
  });

  describe('persistence', () => {
    it('writes the choice to the shared storage key', async () => {
      const theme = await loadTheme();

      theme.setChoice('dark');
      expect(window.localStorage.getItem(STORAGE_KEY)).toBe('dark');

      theme.setChoice('light');
      expect(window.localStorage.getItem(STORAGE_KEY)).toBe('light');
    });

    it('restores the choice in a fresh module instance (reload)', async () => {
      os.set(false);
      const theme = await loadTheme();
      theme.setChoice('dark');

      const reloaded = await loadTheme();
      expect(reloaded.getChoice()).toBe('dark');
      expect(reloaded.resolve()).toBe('dark');
    });

    it('clears the key when the choice is dropped', async () => {
      const theme = await loadTheme();
      theme.setChoice('dark');

      theme.setChoice(null);
      expect(window.localStorage.getItem(STORAGE_KEY)).toBeNull();
      expect(theme.getChoice()).toBeNull();
    });

    it('publishes the choice for the boot script listener', async () => {
      const theme = await loadTheme();

      theme.setChoice('dark');
      expect(window.__nyxTheme).toBe('dark');

      theme.setChoice(null);
      expect(window.__nyxTheme).toBeNull();
    });
  });

  describe('apply', () => {
    it('mirrors the OS scheme onto the document when no choice is stored', async () => {
      os.set(true);
      const theme = await loadTheme();

      theme.apply();
      expect(document.documentElement.classList.contains('dark')).toBe(true);
      expect(document.documentElement.style.colorScheme).toBe('dark');
    });

    it('overrides a dark OS scheme once light is chosen', async () => {
      os.set(true);
      const theme = await loadTheme();
      theme.apply();

      theme.setChoice('light');
      expect(document.documentElement.classList.contains('dark')).toBe(false);
      expect(document.documentElement.style.colorScheme).toBe('light');
    });

    it('sets the class and color-scheme for a dark choice on a light OS', async () => {
      os.set(false);
      const theme = await loadTheme();

      theme.setChoice('dark');
      expect(document.documentElement.classList.contains('dark')).toBe(true);
      expect(document.documentElement.style.colorScheme).toBe('dark');
    });
  });

  describe('unavailable storage', () => {
    beforeEach(() => {
      vi.spyOn(Storage.prototype, 'getItem').mockImplementation(() => {
        throw new Error('storage disabled');
      });
      vi.spyOn(Storage.prototype, 'setItem').mockImplementation(() => {
        throw new Error('storage disabled');
      });
      vi.spyOn(Storage.prototype, 'removeItem').mockImplementation(() => {
        throw new Error('storage disabled');
      });
    });

    it('toggles in-session without throwing', async () => {
      os.set(false);
      const theme = await loadTheme();

      expect(() => theme.setChoice('dark')).not.toThrow();
      expect(theme.resolve()).toBe('dark');
      expect(document.documentElement.classList.contains('dark')).toBe(true);

      expect(() => theme.setChoice('light')).not.toThrow();
      expect(theme.resolve()).toBe('light');
      expect(document.documentElement.classList.contains('dark')).toBe(false);
    });

    it('falls back to the OS scheme after a reload', async () => {
      os.set(false);
      const theme = await loadTheme();
      theme.setChoice('light');
      os.set(true);
      expect(theme.resolve()).toBe('light');

      const reloaded = await loadTheme();
      expect(reloaded.getChoice()).toBeNull();
      expect(reloaded.resolve()).toBe('dark');
    });
  });

  describe('storage that rejects writes', () => {
    it('keeps the in-session choice ahead of a stale stored value', async () => {
      window.localStorage.setItem(STORAGE_KEY, 'light');
      vi.spyOn(Storage.prototype, 'setItem').mockImplementation(() => {
        throw new Error('quota exceeded');
      });
      vi.spyOn(Storage.prototype, 'removeItem').mockImplementation(() => {
        throw new Error('quota exceeded');
      });
      os.set(true);
      const theme = await loadTheme();

      expect(() => theme.setChoice('dark')).not.toThrow();
      expect(theme.getChoice()).toBe('dark');
      expect(theme.resolve()).toBe('dark');
      expect(window.__nyxTheme).toBe('dark');
      expect(document.documentElement.classList.contains('dark')).toBe(true);
    });
  });

  describe('subscribe', () => {
    it('notifies subscribers on every choice change', async () => {
      const theme = await loadTheme();
      const seen: Array<string | null> = [];
      const unsubscribe = theme.subscribe(() => {
        seen.push(theme.getChoice());
      });

      theme.setChoice('dark');
      theme.setChoice('light');
      expect(seen).toEqual(['dark', 'light']);

      unsubscribe();
    });

    it('stops notifying after unsubscribe', async () => {
      const theme = await loadTheme();
      const listener = vi.fn();
      const unsubscribe = theme.subscribe(listener);

      theme.setChoice('dark');
      expect(listener).toHaveBeenCalledTimes(1);

      unsubscribe();
      theme.setChoice('light');
      expect(listener).toHaveBeenCalledTimes(1);
    });
  });
});
