import { describe, it, expect, beforeEach, vi } from 'vitest';
import { render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { AuthForm } from './AuthForm';
import api from '../../../services/api';
import { useAuthStore } from '../../../store/authStore';
import type { AuthResponse, User } from '~/api/openapi';

// Replace the wrapped axios instance's `post` so we can dictate the
// login/register response without touching the real network. The form
// imports `api` as a default; replacing `post` on it is enough.
vi.spyOn(api, 'post');

// Mock the auth store so we can assert how `setAuth` was called and
// inspect end-state without hitting the real Zustand persist middleware
// (which writes to localStorage in jsdom and pollutes later tests).
vi.mock('../../../store/authStore');

const mockedPost = api.post as unknown as ReturnType<typeof vi.fn>;

const baseUser: User = {
  id: 7,
  username: 'newbie',
  email: 'newbie@example.com',
  created_at: '2024-01-01T00:00:00Z',
  updated_at: '2024-01-01T00:00:00Z',
};

const authResponse: AuthResponse = {
  token: 'access-abc',
  refresh_token: 'refresh-xyz',
  expires_at: '2024-01-01T00:15:00Z',
  user: baseUser,
};

describe('AuthForm', () => {
  let setAuth: ReturnType<typeof vi.fn>;
  let logout: ReturnType<typeof vi.fn>;
  let onSuccess: ReturnType<typeof vi.fn>;
  let onCancel: ReturnType<typeof vi.fn>;

  beforeEach(() => {
    mockedPost.mockReset();
    setAuth = vi.fn();
    logout = vi.fn();
    onSuccess = vi.fn();
    onCancel = vi.fn();

    // `AuthForm` reads `setAuth` from `useAuthStore((s) => s.setAuth)`.
    // Since the module is fully mocked, that selector returns
    // `undefined` by default — wire it up here so we can assert calls.
    (useAuthStore as unknown as { mockImplementation: (impl: unknown) => void }).mockImplementation(
      ((selector: (state: { setAuth: typeof setAuth; logout: typeof logout }) => unknown) => {
        return selector({ setAuth, logout });
      }) as unknown,
    );
  });

  describe('login', () => {
    it('POSTs to /login, calls setAuth with the full response, and fires onSuccess', async () => {
      mockedPost.mockResolvedValueOnce({ data: authResponse });

      render(<AuthForm onSuccess={onSuccess} onCancel={onCancel} />);

      await userEvent.type(screen.getByLabelText(/username/i), 'newbie');
      await userEvent.type(screen.getByLabelText(/password/i), 'hunter2');

      await userEvent.click(screen.getByRole('button', { name: /^login$/i }));

      await waitFor(() => {
        expect(mockedPost).toHaveBeenCalledWith('/login', {
          username: 'newbie',
          email: '',
          password: 'hunter2',
        });
      });

      // The contract: setAuth receives the whole AuthResponse so the
      // store can pull access token, refresh token, expires_at, and
      // user out of one envelope. Passing just `{user, token}` would
      // silently drop the refresh credentials.
      await waitFor(() => {
        expect(setAuth).toHaveBeenCalledWith(authResponse);
      });

      expect(onSuccess).toHaveBeenCalledTimes(1);
      expect(onCancel).not.toHaveBeenCalled();
      // A successful login must NOT trigger logout. The form only
      // calls logout on the error branch; pinning this here guards
      // against a future refactor that accidentally wires logout
      // into the success path.
      expect(logout).not.toHaveBeenCalled();
    });
  });

  describe('register', () => {
    it('POSTs to /register with email + credentials', async () => {
      mockedPost.mockResolvedValueOnce({ data: authResponse });

      render(<AuthForm onSuccess={onSuccess} onCancel={onCancel} />);

      // Toggle to the register form (login is the default).
      await userEvent.click(screen.getByRole('button', { name: /^register$/i }));

      await userEvent.type(screen.getByLabelText(/username/i), 'newbie');
      await userEvent.type(screen.getByLabelText(/email/i), 'newbie@example.com');
      await userEvent.type(screen.getByLabelText(/password/i), 'hunter2');

      await userEvent.click(screen.getByRole('button', { name: /^register$/i }));

      await waitFor(() => {
        expect(mockedPost).toHaveBeenCalledWith('/register', {
          username: 'newbie',
          email: 'newbie@example.com',
          password: 'hunter2',
        });
      });

      await waitFor(() => {
        expect(setAuth).toHaveBeenCalledWith(authResponse);
      });

      expect(onSuccess).toHaveBeenCalledTimes(1);
      expect(logout).not.toHaveBeenCalled();
    });
  });

  describe('auth store end state', () => {
    it('surfaces every refresh field through to the store', async () => {
      // We don't assert on the internal setter; the contract is that
      // `setAuth` received the full envelope so the store can persist
      // access + refresh + expiry + user atomically. Verify the
      // exact payload the form hands off.
      mockedPost.mockResolvedValueOnce({ data: authResponse });

      render(<AuthForm onSuccess={onSuccess} onCancel={onCancel} />);

      await userEvent.type(screen.getByLabelText(/username/i), 'newbie');
      await userEvent.type(screen.getByLabelText(/password/i), 'hunter2');
      await userEvent.click(screen.getByRole('button', { name: /^login$/i }));

      await waitFor(() => expect(setAuth).toHaveBeenCalled());

      const [passed] = setAuth.mock.calls[0] as [AuthResponse];
      expect(passed.token).toBe('access-abc');
      expect(passed.refresh_token).toBe('refresh-xyz');
      expect(passed.expires_at).toBe('2024-01-01T00:15:00Z');
      expect(passed.user).toEqual(baseUser);
    });
  });
});