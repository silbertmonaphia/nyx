import { create } from "zustand";
import { persist, createJSONStorage } from "zustand/middleware";
import axios from "axios";
import type { User as ApiUser, AuthResponse } from "~/api/openapi";
import api, { tokenStore } from "../services/api";
import { setSentryUser } from "../services/sentry";

// Wire type — sourced from the generated OpenAPI schema so the
// frontend stays in lockstep with the backend.
export type User = ApiUser;

// Subset of the wire envelope we persist from login/register/refresh.
// Post-Bearer migration, the body carries access_token, refresh_token,
// token_type, expires_at, and user. We persist only `user` for the UI
// hint — the actual access / refresh tokens live in the tokenStore in
// services/api.ts, which owns their persistence (localStorage for a
// remembered login, sessionStorage otherwise). Keeping tokens out of
// this shape keeps the Zustand snapshot free of secrets.
type AuthPayload = Pick<AuthResponse, "user">;

export interface AuthState {
  user: User | null;
  // `isAuthenticated` is derived from the presence of a user record;
  // a more accurate signal would be a server round-trip, but for the
  // UI hint we accept "I have a remembered user" as logged in.
  // Pages that need server-truth make the request and rely on the
  // 401 → /api/refresh → retry path in api.ts.
  isAuthenticated: boolean;
  setAuth: (res: AuthPayload) => void;
  logout: () => void;
  logoutAll: () => Promise<boolean>;
}

// Safe storage: guards against SSR / test environments where `window`
// (and therefore `localStorage`) may be undefined. When storage is
// unavailable we fall back to a noop in-memory store so the app
// still works, but `user` will not persist across reloads in that
// environment.
const safeLocalStorage = createJSONStorage<{ user: User | null }>(() => {
  if (typeof window === "undefined") {
    return {
      getItem: () => null,
      setItem: () => undefined,
      removeItem: () => undefined,
    };
  }
  return window.localStorage;
});

// loginAxios is a bare axios instance pointing at the API base URL.
// We use it only inside logout() so the main client's response
// interceptor (which fires a 401 → refresh → retry cycle) does not
// recurse on a logout we are trying to terminate.
//
// We send the refresh_token in the body so the backend can revoke
// exactly that row. The bare instance is intentionally NOT linked
// to the tokenStore — it relies on the caller (logout()) supplying
// the refresh token directly. The 401 handler on the wrapped `api`
// instance is the only thing that triggers tokenStore writes from a
// 401 path; the bare instance bypasses that loop.
export const loginAxios = axios.create({
  baseURL: import.meta.env.VITE_API_URL || "http://localhost:8080/api",
  withCredentials: false,
});

export const useAuthStore = create<AuthState>()(
  persist(
    (set) => ({
      user: null,
      isAuthenticated: false,

      setAuth: (res) => {
        set({
          user: res.user,
          isAuthenticated: true,
        });
        // Wire Sentry user context on successful login, register,
        // refresh, and /api/me reconciliation — all flows that
        // resolve with a fresh user profile. setSentryUser noops
        // when Sentry wasn't initialised, so the call is safe even
        // when VITE_SENTRY_DSN is empty.
        if (res.user) {
          setSentryUser({
            id: res.user.id,
            username: res.user.username,
          });
        }
      },

      // logout clears local state immediately AND fires off a
      // background POST to /api/logout that revokes the supplied
      // refresh token. The local clear is synchronous so the UI
      // snaps to logged-out even if the network is hung or the
      // server is unreachable — a hung logout must not strand the
      // user on a logged-in UI. The backend call is best-effort
      // (swallowed on failure): if /api/logout fails, the refresh
      // family will time out on its own (default 7d) and the
      // in-memory token pair is discarded immediately.
      //
      // The request uses a bare axios instance so the main client's
      // 401-handling interceptor doesn't loop; the captured access
      // token is stamped explicitly below.
      logout: () => {
        set({
          user: null,
          isAuthenticated: false,
        });
        // Clear Sentry user context so the next captured event
        // carries no user info. setSentryUser(null) noops when
        // Sentry wasn't initialised.
        setSentryUser(null);
        // tokenStore is imported above — there's already a circular
        // module graph (services/api.ts imports useAuthStore too),
        // but ES modules tolerate the cycle because both sides
        // reach the imported bindings lazily inside function bodies.
        const rt = tokenStore.getRefreshToken();
        const at = tokenStore.getAccessToken();
        tokenStore.clear();
        void loginAxios
          .post(
            "/logout",
            rt ? { refresh_token: rt } : {},
            at ? { headers: { Authorization: `Bearer ${at}` } } : {},
          )
          .catch(() => {
            // Intentional swallow: the local clear above is the
            // authoritative UX step. Network/server failures here
            // don't surface to the user.
          });
      },

      logoutAll: async () => {
        let ok = false;
        try {
          await api.post("/logout-all", undefined, { skipErrorToast: true });
          ok = true;
        } catch {
          ok = false;
        } finally {
          set({
            user: null,
            isAuthenticated: false,
          });
          setSentryUser(null);
          tokenStore.clear();
        }
        return ok;
      },
    }),
    {
      name: "nyx-auth-storage",
      storage: safeLocalStorage,
      // Only `user` persists. Tokens used to live here too
      // (token / refreshToken / expiresAt) and before that
      // httpOnly cookies carried them; token persistence now lives
      // in the tokenStore, including the opt-in Remember me
      // localStorage path. partialize is the single source of truth
      // for what gets written to disk — DO NOT add tokens here.
      partialize: (state) => ({
        user: state.user,
      }),
      // Derive `isAuthenticated` from the persisted `user` during
      // hydration. `merge` runs with the persisted slice, so the
      // flag is correct on the first render — without it the
      // partialized shape restores `user` but leaves the flag false
      // until the reconciliation probe completes, flashing the auth
      // gate on every reload. The persisted shape stays minimal (no
      // `isAuthenticated` entry in localStorage).
      merge: (persistedState, currentState) => {
        const persisted = persistedState as Partial<AuthState> | null;
        const user = persisted?.user ?? null;
        return {
          ...currentState,
          ...persisted,
          user,
          isAuthenticated: user !== null,
        };
      },
      // migrate handles upgrades from older persist shapes:
      //   v1 → v2 (pre-cookie): dropped token / refreshToken / expiresAt
      //   v2 → v3 (Bearer era): the same shape carried forward, but we
      //     bump the version so any future drift triggers the same
      //     fail-safe strip path.
      // We strip every token-shaped field on every migration and keep
      // only the non-secret profile data. The user re-logs in if
      // their session isn't already backed by the in-memory +
      // persisted tokenStore. Fail-safe, never fail-open. The
      // `version` is bumped on any persist-shape change so existing
      // localStorage is migrated once, not silently dropped.
      version: 3,
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