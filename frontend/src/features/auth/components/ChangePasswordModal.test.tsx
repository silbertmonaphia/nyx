import { describe, it, expect, beforeEach, vi, type Mock } from 'vitest';
import { render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { ChangePasswordModal } from './ChangePasswordModal';
import api from '../../../services/api';
import { useAuthStore } from '../../../store/authStore';
import { useUiStore, type ToastType } from '../../../store/uiStore';

vi.spyOn(api, 'post');
vi.mock('../../../store/authStore');

const mockedPost = api.post as unknown as ReturnType<typeof vi.fn>;

async function fillAndSubmit(current: string, next: string, confirm: string) {
  await userEvent.type(screen.getByLabelText(/^Current password$/i), current);
  await userEvent.type(screen.getByLabelText(/^New password$/i), next);
  await userEvent.type(screen.getByLabelText(/^Confirm new password$/i), confirm);
  await userEvent.click(screen.getByTestId('change-password-submit'));
}

describe('ChangePasswordModal', () => {
  let logout: Mock<() => void>;
  let addToast: Mock<(message: string, type: ToastType) => void>;
  let onOpenChange: Mock<(open: boolean) => void>;

  beforeEach(() => {
    mockedPost.mockReset();
    logout = vi.fn();
    addToast = vi.fn();
    onOpenChange = vi.fn();
    useUiStore.setState({ addToast });
    (
      useAuthStore as unknown as { mockImplementation: (impl: unknown) => void }
    ).mockImplementation(
      ((selector: (state: { logout: typeof logout }) => unknown) =>
        selector({ logout })) as unknown,
    );
  });

  it('POSTs the current and new passwords, toasts, closes, and logs out', async () => {
    mockedPost.mockResolvedValue({ status: 204 });

    render(<ChangePasswordModal open onOpenChange={onOpenChange} />);
    await fillAndSubmit('oldpass1', 'newpass2', 'newpass2');

    await waitFor(() => expect(mockedPost).toHaveBeenCalledTimes(1));
    expect(mockedPost).toHaveBeenCalledWith(
      '/change-password',
      {
        current_password: 'oldpass1',
        new_password: 'newpass2',
      },
      { skipErrorToast: true, skipSentry: true },
    );
    await waitFor(() => expect(onOpenChange).toHaveBeenCalledWith(false));
    expect(addToast).toHaveBeenCalledWith(
      'Password changed. Please sign in again.',
      'success',
    );
    expect(logout).toHaveBeenCalledTimes(1);
  });

  it('rejects a new password shorter than six characters without calling the API', async () => {
    render(<ChangePasswordModal open onOpenChange={onOpenChange} />);
    await fillAndSubmit('oldpass1', '123', '123');

    expect(
      await screen.findByText('Password must be at least 6 characters'),
    ).toBeInTheDocument();
    expect(mockedPost).not.toHaveBeenCalled();
  });

  it('rejects a confirmation that does not match', async () => {
    render(<ChangePasswordModal open onOpenChange={onOpenChange} />);
    await fillAndSubmit('oldpass1', 'newpass2', 'newpass3');

    expect(
      await screen.findByText('New passwords do not match'),
    ).toBeInTheDocument();
    expect(mockedPost).not.toHaveBeenCalled();
  });

  it('rejects a new password equal to the current one', async () => {
    render(<ChangePasswordModal open onOpenChange={onOpenChange} />);
    await fillAndSubmit('oldpass1', 'oldpass1', 'oldpass1');

    expect(
      await screen.findByText('New password must differ from the current password'),
    ).toBeInTheDocument();
    expect(mockedPost).not.toHaveBeenCalled();
  });

  it('shows a wrong-current-password error and stays logged in on a 400', async () => {
    mockedPost.mockRejectedValue({ response: { status: 400 } });

    render(<ChangePasswordModal open onOpenChange={onOpenChange} />);
    await fillAndSubmit('wrongpass', 'newpass2', 'newpass2');

    expect(
      await screen.findByText('Current password is incorrect'),
    ).toBeInTheDocument();
    expect(logout).not.toHaveBeenCalled();
    expect(onOpenChange).not.toHaveBeenCalledWith(false);
  });

  it('shows a generic error on an unexpected failure', async () => {
    mockedPost.mockRejectedValue({ response: { status: 500 } });

    render(<ChangePasswordModal open onOpenChange={onOpenChange} />);
    await fillAndSubmit('oldpass1', 'newpass2', 'newpass2');

    expect(
      await screen.findByText('Could not change password. Please try again.'),
    ).toBeInTheDocument();
    expect(logout).not.toHaveBeenCalled();
  });

  it('disables the submit button while the request is pending', async () => {
    mockedPost.mockReturnValue(new Promise(() => {}));

    render(<ChangePasswordModal open onOpenChange={onOpenChange} />);
    await fillAndSubmit('oldpass1', 'newpass2', 'newpass2');

    const submit = screen.getByTestId('change-password-submit');
    await waitFor(() => expect(submit).toBeDisabled());
    expect(submit).toHaveTextContent('Changing…');
  });
});
