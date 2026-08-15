import { create } from 'zustand';
import { persist, createJSONStorage } from 'zustand/middleware';
import type { User as ApiUser, AuthResponse } from '~/api/openapi';

// Wire type — sourced from the generated OpenAPI schema so the frontend
// stays in lockstep with the backend. The server also returns
// `created_at`/`updated_at`, which we ignore here.
export type User = ApiUser;

// Subset of the wire envelope we persist from login/register/refresh.
// `setAuth` accepts the whole `AuthResponse` so all three paths flow
// through one entry point.
type AuthPayload = Pick<AuthResponse, 'token' | 'refresh_token' | 'expires_at' | 'user'>;

interface AuthState {
  user: User | null;
  token: string | null;
  refreshToken: string | null;
  expiresAt: string | null; // ISO 8601 from the server; null when not provided.
  isAuthenticated: boolean; // derived; excluded from persistence.
  setAuth: (res: AuthPayload) => void;
  // Used by the silent refresh path in `services/api.ts` to swap in a
  // freshly minted access token without disturbing the refresh token
  // or user identity.
  setAccessToken: (token: string, expiresAt?: string) => void;
  logout: () => void;
}

// Safe storage: guards against SSR / test environments where `window` (and
// therefore `localStorage`) may be undefined. When storage is unavailable we
// fall back to a noop in-memory store so the app still works, but tokens
// will not persist across reloads in that environment.
const safeLocalStorage = createJSONStorage<AuthState>(() => {
  if (typeof window === 'undefined') {
    return {
      getItem: () => null,
      setItem: () => undefined,
      removeItem: () => undefined,
    };
  }
  return window.localStorage;
});

export const useAuthStore = create<AuthState>()(
  persist(
    (set) => ({
      user: null,
      token: null,
      refreshToken: null,
      expiresAt: null,
      isAuthenticated: false,
      setAuth: (res) =>
        set({
          user: res.user,
          token: res.token,
          refreshToken: res.refresh_token ?? null,
          expiresAt: res.expires_at ?? null,
          isAuthenticated: true,
        }),
      setAccessToken: (token, expiresAt) =>
        set((state) => ({
          token,
          // Preserve the existing expiresAt when the caller doesn't pass a
          // new one — refresh responses do carry a fresh value, but the
          // signature lets callers omit it if they ever need to.
          expiresAt: expiresAt ?? state.expiresAt,
        })),
      logout: () =>
        set({
          user: null,
          token: null,
          refreshToken: null,
          expiresAt: null,
          isAuthenticated: false,
        }),
    }),
    {
      name: 'nyx-auth-storage',
      storage: safeLocalStorage,
      // `isAuthenticated` is derived (`!!token && !!user`) so persisting
      // it would just re-introduce drift between the boolean and the
      // wire fields on the next rehydrate. Keep it out of storage and
      // recompute on read (callers already use it as a hint — a stale
      // `true` from a stale localStorage entry would be worse than
      // recomputing it from the wire fields).
      partialize: (state) => ({
        user: state.user,
        token: state.token,
        refreshToken: state.refreshToken,
        expiresAt: state.expiresAt,
      }),
    }
  )
);