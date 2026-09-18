import { describe, it, expect, beforeEach, vi } from 'vitest';
import { render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { AuthForm } from './AuthForm';
import api, { tokenStore } from '../../../services/api';
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
  created_at: '2024-01-01T00:00:00Z',
  updated_at: '2024-01-01T00:00:00Z',
};

// AuthResponse shape that the backend actually emits (see
// backend/internal/user/huma_handler.go — loginOutput/registerOutput
// both wrap AuthResponse in the JSON body, NOT Set-Cookie). The token
// pair rides the JSON body per the Bearer contract (RFC 6750); the
// wrapped `api` request interceptor stamps them on subsequent
// requests and the tokenStore writes them to sessionStorage so a
// page reload can adopt them without a fresh login.
const authResponse: AuthResponse = {
  access_token: 'test.access.token',
  refresh_token: 'test.refresh.token',
  token_type: 'Bearer',
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

    // Reset the real tokenStore between tests — earlier cases may
    // have left stale in-memory tokens or sessionStorage entries.
    tokenStore.clear();
    sessionStorage.clear();

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
    it('POSTs to /login, calls setAuth with the response, stores tokens, and fires onSuccess', async () => {
      // The wire contract is end-to-end: a successful login must
      // populate BOTH the auth profile (for the UI hint) and the
      // in-memory + sessionStorage token store (for subsequent
      // Bearer headers AND for surviving an F5 — the IIFE in
      // services/api.ts reads sessionStorage on boot, and an empty
      // sessionStorage triggers the useAuthReconciliation logout
      // branch). Skipping setTokens after a 200 leaves the SPA in a
      // "logged in but every protected request will 401" state, and
      // an F5 logs the user out — both bugs were traced to this
      // single missing call.
      mockedPost.mockResolvedValueOnce({ data: authResponse });

      render(<AuthForm onSuccess={onSuccess} onCancel={onCancel} />);

      await userEvent.type(screen.getByLabelText(/username/i), 'newbie');
      await userEvent.type(screen.getByLabelText(/password/i), 'hunter2');

      await userEvent.click(screen.getByRole('button', { name: /^login$/i }));

      await waitFor(() => {
        expect(mockedPost).toHaveBeenCalledWith('/login', {
          username: 'newbie',
          password: 'hunter2',
        });
      });

      // setAuth receives the body envelope so the store can pull
      // `user` out.
      await waitFor(() => {
        expect(setAuth).toHaveBeenCalledWith(authResponse);
      });

      // The bearer pair from the response lands in the tokenStore
      // (which mirrors to sessionStorage for F5 survival) and in
      // memory for the request interceptor. sessionStorage's
      // 'nyx-token-store' key is what the IIFE re-hydrates from on
      // the next page load.
      expect(tokenStore.getAccessToken()).toBe('test.access.token');
      expect(tokenStore.getRefreshToken()).toBe('test.refresh.token');
      const persisted = JSON.parse(
        sessionStorage.getItem('nyx-token-store') ?? 'null',
      );
      expect(persisted).toEqual({
        accessToken: 'test.access.token',
        refreshToken: 'test.refresh.token',
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
    it('POSTs to /register with username + password and stores tokens', async () => {
      mockedPost.mockResolvedValueOnce({ data: authResponse });

      render(<AuthForm onSuccess={onSuccess} onCancel={onCancel} />);

      // Toggle to the register form (login is the default).
      await userEvent.click(screen.getByRole('button', { name: /^register$/i }));

      await userEvent.type(screen.getByLabelText(/username/i), 'newbie');
      await userEvent.type(screen.getByLabelText(/password/i), 'hunter2');

      await userEvent.click(screen.getByRole('button', { name: /^register$/i }));

      await waitFor(() => {
        expect(mockedPost).toHaveBeenCalledWith('/register', {
          username: 'newbie',
          password: 'hunter2',
        });
      });

      await waitFor(() => {
        expect(setAuth).toHaveBeenCalledWith(authResponse);
      });

      // Same token-storage contract as /login — a register success
      // also leaves the user logged in across a page reload.
      expect(tokenStore.getAccessToken()).toBe('test.access.token');
      expect(tokenStore.getRefreshToken()).toBe('test.refresh.token');

      expect(onSuccess).toHaveBeenCalledTimes(1);
      expect(logout).not.toHaveBeenCalled();
    });
  });

  describe('auth store end state', () => {
    it('surfaces user + tokens through to setAuth + tokenStore', async () => {
      mockedPost.mockResolvedValueOnce({ data: authResponse });

      render(<AuthForm onSuccess={onSuccess} onCancel={onCancel} />);

      await userEvent.type(screen.getByLabelText(/username/i), 'newbie');
      await userEvent.type(screen.getByLabelText(/password/i), 'hunter2');
      await userEvent.click(screen.getByRole('button', { name: /^login$/i }));

      await waitFor(() => expect(setAuth).toHaveBeenCalled());

      const [passed] = setAuth.mock.calls[0] as [AuthResponse];
      expect(passed.user).toEqual(baseUser);
      expect(passed.expires_at).toBe('2024-01-01T00:15:00Z');
      // Bearer transport: access + refresh ride the JSON body.
      expect(passed.access_token).toBe('test.access.token');
      expect(passed.refresh_token).toBe('test.refresh.token');
    });
  });
});