import { useEffect, useSyncExternalStore } from 'react';

export type Theme = 'light' | 'dark';

declare global {
  interface Window {
    __nyxTheme?: Theme | null;
  }
}

const THEME_STORAGE_KEY = 'nyx.theme';

const DARK_QUERY = '(prefers-color-scheme: dark)';

const listeners = new Set<() => void>();

let memoryChoice: Theme | null = null;
let osListenerBound = false;

function isTheme(value: unknown): value is Theme {
  return value === 'light' || value === 'dark';
}

function readStored(): Theme | null {
  try {
    const raw = window.localStorage.getItem(THEME_STORAGE_KEY);
    return isTheme(raw) ? raw : null;
  } catch {
    return null;
  }
}

function writeStored(choice: Theme | null): void {
  try {
    if (choice === null) window.localStorage.removeItem(THEME_STORAGE_KEY);
    else window.localStorage.setItem(THEME_STORAGE_KEY, choice);
  } catch {
    return;
  }
}

function systemTheme(): Theme {
  try {
    return window.matchMedia(DARK_QUERY).matches ? 'dark' : 'light';
  } catch {
    return 'light';
  }
}

function notify(): void {
  for (const listener of listeners) listener();
}

export function getChoice(): Theme | null {
  return memoryChoice ?? readStored();
}

export function resolve(): Theme {
  return getChoice() ?? systemTheme();
}

export function apply(): void {
  const theme = resolve();
  const root = document.documentElement;
  root.classList.toggle('dark', theme === 'dark');
  root.style.colorScheme = theme;
  notify();
}

export function setChoice(choice: Theme | null): void {
  memoryChoice = choice;
  window.__nyxTheme = choice;
  writeStored(choice);
  apply();
}

export function subscribe(listener: () => void): () => void {
  listeners.add(listener);
  return () => {
    listeners.delete(listener);
  };
}

function bindOsListener(): void {
  if (osListenerBound) return;
  osListenerBound = true;
  let mq: MediaQueryList;
  try {
    mq = window.matchMedia(DARK_QUERY);
  } catch {
    return;
  }
  const onChange = () => {
    if (getChoice() === null) apply();
  };
  if (mq.addEventListener) mq.addEventListener('change', onChange);
  else mq.addListener(onChange);
}

export function useTheme(): Theme {
  const theme = useSyncExternalStore(subscribe, resolve);

  useEffect(() => {
    bindOsListener();
    apply();
  }, []);

  return theme;
}
