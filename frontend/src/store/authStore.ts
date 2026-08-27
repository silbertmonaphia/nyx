import { create } from "zustand";
import { persist, createJSONStorage } from "zustand/middleware";
import axios from "axios";
import type { User as ApiUser, AuthResponse } from "~/api/openapi";
import { tokenStore } from "../services/api";

// Wire type — sourced from the generated OpenAPI schema so the
// frontend stays in lockstep with the backend.
export type User = ApiUser;

// Subset of the wire envelope we persist from login/register/refresh.
// Post-Bearer migration, the body carries access_token, refresh_token,
// token_type, expires_at, and user. We persist only `user` for the UI
// hint — the actual access / refresh tokens live in the in-memory +
// sessionStorage tokenStore in services/api.ts. Persisting tokens to
// localStorage would re-introduce XSS-exfiltration risk and defeat
// the whole point of moving off cookies.
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

      setAuth: (res) =>
        set({
          user: res.user,
          isAuthenticated: true,
        }),

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
      // 401-handling interceptor doesn't loop back into a refresh
      // attempt we're trying to terminate.
      logout: () => {
        set({
          user: null,
          isAuthenticated: false,
        });
        // tokenStore is imported above — there's already a circular
        // module graph (services/api.ts imports useAuthStore too),
        // but ES modules tolerate the cycle because both sides
        // reach the imported bindings lazily inside function bodies.
        const rt = tokenStore.getRefreshToken();
        tokenStore.clear();
        void loginAxios
          .post("/logout", rt ? { refresh_token: rt } : {})
          .catch(() => {
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
      // (token / refreshToken / expiresAt) and before that
      // httpOnly cookies carried them; persisting them to
      // localStorage would defeat the migration's whole point (XSS
      // exfiltration). partialize is the single source of truth for
      // what gets written to disk — DO NOT add tokens here.
      partialize: (state) => ({
        user: state.user,
      }),
      // migrate handles upgrades from older persist shapes:
      //   v1 → v2 (pre-cookie): dropped token / refreshToken / expiresAt
      //   v2 → v3 (Bearer era): the same shape carried forward, but we
      //     bump the version so any future drift triggers the same
      //     fail-safe strip path.
      // We strip every token-shaped field on every migration and keep
      // only the non-secret profile data. The user re-logs in if
      // their session isn't already backed by the in-memory +
      // sessionStorage tokenStore. Fail-safe, never fail-open. The
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