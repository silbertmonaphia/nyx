import { render, screen, act } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterAll, beforeAll, beforeEach, describe, expect, it } from 'vitest';
import { installMatchMedia } from '~/test/matchMedia';
import { getChoice, resolve, setChoice } from '~/theme/theme';
import { ThemeToggle } from './ThemeToggle';

describe('ThemeToggle', () => {
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
    setChoice(null);
    os.set(false);
  });

  it('offers the opposite theme in its accessible name', () => {
    render(<ThemeToggle />);
    const toggle = screen.getByTestId('theme-toggle');

    expect(toggle).toHaveAttribute('type', 'button');
    expect(toggle).toHaveAccessibleName('Switch to dark theme');
  });

  it('flips the stored choice and the resolved theme when clicked', async () => {
    render(<ThemeToggle />);
    const toggle = screen.getByTestId('theme-toggle');

    await userEvent.click(toggle);

    expect(getChoice()).toBe('dark');
    expect(resolve()).toBe('dark');
    expect(window.localStorage.getItem('nyx.theme')).toBe('dark');
    expect(document.documentElement.classList.contains('dark')).toBe(true);
    expect(toggle).toHaveAccessibleName('Switch to light theme');
  });

  it('flips back to light on a second click', async () => {
    setChoice('dark');
    render(<ThemeToggle />);
    const toggle = screen.getByTestId('theme-toggle');
    expect(toggle).toHaveAccessibleName('Switch to light theme');

    await userEvent.click(toggle);

    expect(getChoice()).toBe('light');
    expect(document.documentElement.classList.contains('dark')).toBe(false);
    expect(toggle).toHaveAccessibleName('Switch to dark theme');
  });

  it('tracks a theme the OS drives while no choice is stored', () => {
    render(<ThemeToggle />);
    const toggle = screen.getByTestId('theme-toggle');
    expect(toggle).toHaveAccessibleName('Switch to dark theme');

    act(() => {
      os.set(true);
    });

    expect(getChoice()).toBeNull();
    expect(toggle).toHaveAccessibleName('Switch to light theme');
  });
});
