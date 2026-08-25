import { describe, it, expect, beforeEach } from 'vitest';
import { useAuthStore } from './authStore';
import type { User } from '~/api/openapi';

const baseUser: User = {
  id: 1,
  username: 'tester',
  email: 'tester@example.com',
  created_at: '2024-01-01T00:00:00Z',
  updated_at: '2024-01-01T00:00:00Z',
};

/**
 * Each test starts from a known-clean store. We reset every persisted
 * field rather than relying on `logout()` so a future field added to
 * the store would still surface here.
 */
function resetStore() {
  useAuthStore.setState({
    user: null,
    isAuthenticated: false,
  });
}

describe('useAuthStore', () => {
  beforeEach(() => {
    resetStore();
  });

  describe('setAuth', () => {
    it('populates user and flips isAuthenticated to true', () => {
      useAuthStore.getState().setAuth({ user: baseUser });

      const state = useAuthStore.getState();
      expect(state.user).toEqual(baseUser);
      expect(state.isAuthenticated).toBe(true);
    });
  });

  describe('logout', () => {
    it('clears user and flips isAuthenticated to false synchronously', () => {
      // Logout is synchronous for the local clear — the backend
      // POST is fire-and-forget so a hung server doesn't strand
      // the user on a logged-in UI. The test only cares about the
      // local state transition; the background POST is not
      // observable here and the swallow-on-failure behaviour is
      // covered implicitly by the lack of a real network.
      useAuthStore.getState().setAuth({ user: baseUser });
      expect(useAuthStore.getState().isAuthenticated).toBe(true);

      useAuthStore.getState().logout();

      const state = useAuthStore.getState();
      expect(state.user).toBeNull();
      expect(state.isAuthenticated).toBe(false);
    });
  });

  describe('persist partialize', () => {
    it('writes only `user` to storage', () => {
      // Inspect what partialize actually returns. Zustand exposes the
      // configured options via `persist.getOptions()`; we call the
      // function directly so we don't have to round-trip through
      // localStorage (which is a noop in this environment anyway).
      const persistApi = useAuthStore.persist;
      const options = persistApi.getOptions();
      expect(options.partialize).toBeDefined();

      // Drive the store through its public action so the wire fields
      // match the production shape, then assert what partialize yields.
      useAuthStore.getState().setAuth({ user: baseUser });
      expect(useAuthStore.getState().isAuthenticated).toBe(true);

      const partial = options.partialize!(useAuthStore.getState());

      expect(partial).toEqual({ user: baseUser });
      // Crucially: `isAuthenticated` is NOT part of the persisted shape.
      expect(Object.keys(partial)).not.toContain('isAuthenticated');
      // Token-shaped fields are NOT in the persisted shape — they ride
      // httpOnly cookies, not localStorage.
      expect(Object.keys(partial)).not.toContain('token');
      expect(Object.keys(partial)).not.toContain('refreshToken');
      expect(Object.keys(partial)).not.toContain('expiresAt');
    });
  });

  describe('migrate (v1 → v2)', () => {
    it('strips token fields from pre-cookie persisted state', () => {
      const persistApi = useAuthStore.persist;
      const options = persistApi.getOptions();
      expect(options.migrate).toBeDefined();

      const staleState = {
        user: baseUser,
        token: 'old-access',
        refreshToken: 'old-refresh',
        expiresAt: '2024-01-01T00:15:00Z',
        isAuthenticated: true,
      };

      const migrated = options.migrate!(staleState, 1);

      // user survives (non-secret profile data)
      expect(migrated.user).toEqual(baseUser);
      // every token-shaped field is gone
      expect((migrated as Record<string, unknown>).token).toBeUndefined();
      expect((migrated as Record<string, unknown>).refreshToken).toBeUndefined();
      expect((migrated as Record<string, unknown>).expiresAt).toBeUndefined();
      // isAuthenticated is recomputed, not carried over — fail-safe:
      // we don't trust a stale "true" from before the migration.
      expect(migrated.isAuthenticated).toBe(false);
    });

    it('handles empty / non-object persisted state', () => {
      const persistApi = useAuthStore.persist;
      const options = persistApi.getOptions();
      expect(options.migrate).toBeDefined();

      // Both null (corrupt storage) and undefined (no prior storage)
      // should yield a clean state rather than crash.
      expect(options.migrate!(null, 1)).toEqual({
        user: null,
        isAuthenticated: false,
      });
      expect(options.migrate!(undefined, 1)).toEqual({
        user: null,
        isAuthenticated: false,
      });
    });
  });
});