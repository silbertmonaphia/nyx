import { create } from "zustand";
import { persist, createJSONStorage } from "zustand/middleware";
import axios from "axios";
import type { User as ApiUser, AuthResponse } from "~/api/openapi";

// Wire type — sourced from the generated OpenAPI schema so the
// frontend stays in lockstep with the backend. The server also
// returns `created_at`/`updated_at`, which we ignore here.
export type User = ApiUser;

// Subset of the wire envelope we persist from login/register/refresh.
// Post-cookie migration, the body no longer carries tokens — only the
// user profile and the access-token expiry. We persist `user` so the
// UI can keep the "Welcome, {username}" line across reloads without
// making a /me round-trip; the actual authentication state is owned
// by the browser via httpOnly __Host-nyx-access / __Host-nyx-refresh
// cookies.
type AuthPayload = Pick<AuthResponse, "user">;

interface AuthState {
  user: User | null;
  // `isAuthenticated` is derived from the presence of a user record;
  // a more accurate signal would be a server round-trip, but for the
  // UI hint we accept "I have a remembered user" as logged in.
  // Pages that need server-truth make the request and rely on the
  // 401 → /api/refresh → retry path in api.ts.
  isAuthenticated: boolean;
  setAuth: (res: AuthPayload) => void;
  logout: () => void;
}

// Safe storage: guards against SSR / test environments where `window`
// (and therefore `localStorage`) may be undefined. When storage is
// unavailable we fall back to a noop in-memory store so the app
// still works, but `user` will not persist across reloads in that
// environment.
const safeLocalStorage = createJSONStorage<AuthState>(() => {
  if (typeof window === "undefined") {
    return {
      getItem: () => null,
      setItem: () => undefined,
      removeItem: () => undefined,
    };
  }
  return window.localStorage;
});

// loginAxios is a bare axios instance pointing at /api (the same
// baseURL the main client uses, but WITHOUT the withCredentials +
// interceptors — calling /api/logout from a logout handler must
// avoid recursion through api.ts' response interceptor). We use
// this only inside logout() below.
const loginAxios = axios.create({ withCredentials: true });

export const useAuthStore = create<AuthState>()(
  persist(
    (set) => ({
      user: null,
      isAuthenticated: false,

      setAuth: (res) =>
        set({
          user: res.user,
          isAuthenticated: true,
        }),

      // logout clears local state immediately AND fires off a
      // background POST to /api/logout that revokes the refresh
      // family + clears both httpOnly cookies at the browser. The
      // local clear is synchronous so the UI snaps to logged-out
      // even if the network is hung or the server is unreachable —
      // a hung logout must not strand the user on a logged-in UI.
      // The backend call is best-effort (swallowed on failure): if
      // /api/logout fails, the refresh family will time out on its
      // own (default 7d) and the cookies expire the moment the
      // browser closes the session.
      //
      // The request uses a bare axios instance so the main client's
      // 401-handling interceptor doesn't loop back into a refresh
      // attempt we're trying to terminate.
      logout: () => {
        set({
          user: null,
          isAuthenticated: false,
        });
        void loginAxios.post("/logout", {}).catch(() => {
          // Intentional swallow: the local clear above is the
          // authoritative UX step. Network/server failures here
          // don't surface to the user.
        });
      },
    }),
    {
      name: "nyx-auth-storage",
      storage: safeLocalStorage,
      // Only `user` persists. Tokens used to live here too
      // (token / refreshToken / expiresAt) but they ride httpOnly
      // cookies now; persisting them to localStorage would defeat
      // the migration's whole point (XSS exfiltration). partialize
      // is the single source of truth for what gets written to disk.
      partialize: (state) => ({
        user: state.user,
      }),
      // migrate handles upgrades from the pre-cookie localStorage
      // shape, which carried token / refreshToken / expiresAt
      // alongside user. We strip every token field (no token gets
      // carried forward) and keep only the non-secret profile data.
      // The user re-logs in if their session isn't already
      // cookie-backed — fail-safe, never fail-open. The
      // `version` is bumped on any persist-shape change so existing
      // localStorage is migrated once, not silently dropped.
      version: 2,
      migrate: (persistedState) => {
        const stale = persistedState as Record<string, unknown> | null;
        if (!stale || typeof stale !== "object") {
          return { user: null, isAuthenticated: false };
        }
        // Drop every token-shaped field. Whitelist `user` only.
        const user = stale.user;
        return {
          user: user && typeof user === "object" ? (user as User) : null,
          isAuthenticated: false,
        };
      },
    },
  ),
);
