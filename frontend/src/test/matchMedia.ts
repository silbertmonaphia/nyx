import { vi } from 'vitest';

type ChangeListener = (event: MediaQueryListEvent) => void;

export interface MatchMediaControl {
  readonly matches: boolean;
  set(dark: boolean): void;
  listenerCount(): number;
}

export function installMatchMedia(initialDark: boolean): MatchMediaControl {
  const listeners = new Set<ChangeListener>();
  const mql = {
    media: '(prefers-color-scheme: dark)',
    matches: initialDark,
    onchange: null,
    addEventListener: (type: string, listener: ChangeListener) => {
      if (type === 'change') listeners.add(listener);
    },
    removeEventListener: (type: string, listener: ChangeListener) => {
      if (type === 'change') listeners.delete(listener);
    },
    addListener: (listener: ChangeListener) => listeners.add(listener),
    removeListener: (listener: ChangeListener) => listeners.delete(listener),
    dispatchEvent: () => true,
  };

  vi.spyOn(window, 'matchMedia').mockImplementation(
    () => mql as unknown as MediaQueryList,
  );

  return {
    get matches() {
      return mql.matches;
    },
    set(dark: boolean) {
      mql.matches = dark;
      for (const listener of [...listeners]) {
        listener({ matches: dark } as MediaQueryListEvent);
      }
    },
    listenerCount: () => listeners.size,
  };
}
