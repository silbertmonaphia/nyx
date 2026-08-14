import { describe, it, expect, beforeEach } from 'vitest';
import { useAuthStore } from './authStore';
import type { AuthResponse, User } from '~/api/openapi';

const baseUser: User = {
  id: 1,
  username: 'tester',
  email: 'tester@example.com',
  created_at: '2024-01-01T00:00:00Z',
  updated_at: '2024-01-01T00:00:00Z',
};

const fullPayload: AuthResponse = {
  token: 'access-1',
  refresh_token: 'refresh-1',
  expires_at: '2024-01-01T00:15:00Z',
  user: baseUser,
};

/**
 * Each test starts from a known-clean store. We reset every persisted
 * field rather than relying on `logout()` so a future field added to
 * the store would still surface here.
 */
function resetStore() {
  useAuthStore.setState({
    user: null,
    token: null,
    refreshToken: null,
    expiresAt: null,
    isAuthenticated: false,
  });
}

describe('useAuthStore', () => {
  beforeEach(() => {
    resetStore();
  });

  describe('setAuth', () => {
    it('populates all four wire fields and flips isAuthenticated to true', () => {
      useAuthStore.getState().setAuth(fullPayload);

      const state = useAuthStore.getState();
      expect(state.token).toBe('access-1');
      expect(state.refreshToken).toBe('refresh-1');
      expect(state.expiresAt).toBe('2024-01-01T00:15:00Z');
      expect(state.user).toEqual(baseUser);
      expect(state.isAuthenticated).toBe(true);
    });

    it('treats a missing refresh_token as null', () => {
      const payloadNoRefresh: AuthResponse = {
        token: 'access-only',
        // refresh_token intentionally absent (matches the wire shape: the
        // field is `omitempty` and may not be present).
        user: baseUser,
      };

      useAuthStore.getState().setAuth(payloadNoRefresh);

      expect(useAuthStore.getState().refreshToken).toBeNull();
      expect(useAuthStore.getState().token).toBe('access-only');
      expect(useAuthStore.getState().isAuthenticated).toBe(true);
    });

    it('treats a missing expires_at as null', () => {
      const payloadNoExpiry: AuthResponse = {
        token: 'access-only',
        refresh_token: 'refresh-only',
        user: baseUser,
      };

      useAuthStore.getState().setAuth(payloadNoExpiry);

      expect(useAuthStore.getState().expiresAt).toBeNull();
      expect(useAuthStore.getState().refreshToken).toBe('refresh-only');
    });
  });

  describe('setAccessToken', () => {
    it('updates only the token when expiresAt is omitted', () => {
      // Seed the store so we can verify the user/refreshToken survive.
      useAuthStore.getState().setAuth(fullPayload);
      const before = useAuthStore.getState();

      useAuthStore.getState().setAccessToken('access-2');

      const after = useAuthStore.getState();
      expect(after.token).toBe('access-2');
      expect(after.user).toEqual(before.user);
      expect(after.refreshToken).toBe(before.refreshToken);
      // Without an explicit value, expiresAt is preserved from the
      // previous state — the refresh path always supplies a fresh one,
      // but the signature allows omission and we shouldn't clobber.
      expect(after.expiresAt).toBe(before.expiresAt);
    });

    it('updates token and expiresAt when both are provided', () => {
      useAuthStore.getState().setAuth(fullPayload);

      useAuthStore.getState().setAccessToken(
        'access-2',
        '2024-01-01T00:30:00Z',
      );

      const after = useAuthStore.getState();
      expect(after.token).toBe('access-2');
      expect(after.expiresAt).toBe('2024-01-01T00:30:00Z');
    });
  });

  describe('logout', () => {
    it('clears every wire field and flips isAuthenticated to false', () => {
      useAuthStore.getState().setAuth(fullPayload);
      expect(useAuthStore.getState().isAuthenticated).toBe(true);

      useAuthStore.getState().logout();

      const state = useAuthStore.getState();
      expect(state.user).toBeNull();
      expect(state.token).toBeNull();
      expect(state.refreshToken).toBeNull();
      expect(state.expiresAt).toBeNull();
      expect(state.isAuthenticated).toBe(false);
    });
  });

  describe('persist partialize', () => {
    it('excludes isAuthenticated from the persisted shape', () => {
      // Inspect what partialize actually returns. Zustand exposes the
      // configured options via `persist.getOptions()`; we call the
      // function directly so we don't have to round-trip through
      // localStorage (which is a noop in this environment anyway).
      const persistApi = useAuthStore.persist;
      const options = persistApi.getOptions();
      expect(options.partialize).toBeDefined();

      // Drive the store through its public action so the wire fields
      // match the production shape, then assert what partialize yields.
      useAuthStore.getState().setAuth(fullPayload);
      expect(useAuthStore.getState().isAuthenticated).toBe(true);

      const partial = options.partialize!(useAuthStore.getState());

      expect(partial).toEqual({
        user: baseUser,
        token: 'access-1',
        refreshToken: 'refresh-1',
        expiresAt: '2024-01-01T00:15:00Z',
      });
      // Crucially: `isAuthenticated` is NOT part of the persisted shape.
      expect(Object.keys(partial)).not.toContain('isAuthenticated');
    });
  });
});