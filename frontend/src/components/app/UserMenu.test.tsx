import { describe, it, expect, vi } from 'vitest';
import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { UserMenu } from './UserMenu';

describe('UserMenu', () => {
  it('opens the menu, focuses the item, and fires onChangePassword', async () => {
    const onChangePassword = vi.fn();
    render(<UserMenu username="alice" onChangePassword={onChangePassword} />);

    const trigger = screen.getByTestId('user-menu-button');
    expect(trigger).toHaveAttribute('aria-expanded', 'false');

    await userEvent.click(trigger);

    expect(trigger).toHaveAttribute('aria-expanded', 'true');
    const item = screen.getByRole('menuitem', { name: /change password/i });
    expect(item).toHaveFocus();

    await userEvent.click(item);

    expect(onChangePassword).toHaveBeenCalledTimes(1);
    expect(screen.queryByTestId('user-menu')).not.toBeInTheDocument();
    expect(trigger).toHaveAttribute('aria-expanded', 'false');
  });

  it('closes on Escape and returns focus to the trigger', async () => {
    render(<UserMenu username="alice" onChangePassword={vi.fn()} />);

    const trigger = screen.getByTestId('user-menu-button');
    await userEvent.click(trigger);
    expect(screen.getByTestId('user-menu')).toBeInTheDocument();

    await userEvent.keyboard('{Escape}');

    expect(screen.queryByTestId('user-menu')).not.toBeInTheDocument();
    expect(trigger).toHaveFocus();
  });

  it('closes when the pointer goes outside', async () => {
    render(<UserMenu username="alice" onChangePassword={vi.fn()} />);

    await userEvent.click(screen.getByTestId('user-menu-button'));
    expect(screen.getByTestId('user-menu')).toBeInTheDocument();

    await userEvent.click(document.body);

    expect(screen.queryByTestId('user-menu')).not.toBeInTheDocument();
  });
});
