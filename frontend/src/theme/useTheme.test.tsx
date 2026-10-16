import { render, screen, act } from '@testing-library/react';
import { afterAll, beforeAll, beforeEach, describe, expect, it, vi } from 'vitest';
import { installMatchMedia } from '../test/matchMedia';
import { setChoice, subscribe, useTheme } from './theme';

function Probe() {
  const theme = useTheme();
  return <span data-testid="theme">{theme}</span>;
}

const resolved = () => screen.getByTestId('theme').textContent;

describe('useTheme', () => {
  let os: ReturnType<typeof installMatchMedia>;

  beforeAll(() => {
    os = installMatchMedia(false);
  });

  afterAll(() => {
    setChoice(null);
  });

  beforeEach(() => {
    window.localStorage.clear();
    document.documentElement.className = '';
    document.documentElement.style.colorScheme = '';
    setChoice(null);
    os.set(false);
  });

  it('re-applies the resolved theme on mount', () => {
    os.set(true);
    document.documentElement.classList.remove('dark');

    render(<Probe />);

    expect(resolved()).toBe('dark');
    expect(document.documentElement.classList.contains('dark')).toBe(true);
    expect(document.documentElement.style.colorScheme).toBe('dark');
  });

  it('follows a live OS change while no choice is stored', () => {
    render(<Probe />);
    expect(resolved()).toBe('light');

    act(() => {
      os.set(true);
    });

    expect(resolved()).toBe('dark');
    expect(document.documentElement.classList.contains('dark')).toBe(true);
  });

  it('ignores a live OS change once a choice is stored', () => {
    setChoice('light');
    const listener = vi.fn();
    const unsubscribe = subscribe(listener);
    render(<Probe />);
    expect(resolved()).toBe('light');
    listener.mockClear();

    act(() => {
      os.set(true);
    });

    expect(resolved()).toBe('light');
    expect(document.documentElement.classList.contains('dark')).toBe(false);
    expect(listener).not.toHaveBeenCalled();

    unsubscribe();
  });

  it('keeps exactly one OS listener across unmount and remount', () => {
    const first = render(<Probe />);
    expect(os.listenerCount()).toBe(1);

    first.unmount();
    expect(os.listenerCount()).toBe(1);

    render(<Probe />);
    expect(os.listenerCount()).toBe(1);

    act(() => {
      os.set(true);
    });
    expect(resolved()).toBe('dark');
  });

  it('reflects a persisted choice on first render', () => {
    window.localStorage.setItem('nyx.theme', 'dark');

    render(<Probe />);

    expect(resolved()).toBe('dark');
  });
});
