import { render, screen } from '@testing-library/react';
import { describe, expect, it } from 'vitest';
import { LocaleToggle } from './LocaleToggle';

describe('LocaleToggle', () => {
  it('is hidden below the md breakpoint by default', () => {
    render(<LocaleToggle />);

    expect(screen.getByTestId('locale-toggle').className).toContain('hidden');
  });

  it('stays visible below the md breakpoint when alwaysVisible is set', () => {
    render(<LocaleToggle alwaysVisible />);

    const group = screen.getByTestId('locale-toggle');
    expect(group.className).toContain('inline-flex');
    expect(group.className).not.toContain('hidden');
  });
});
