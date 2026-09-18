import { useEffect } from 'react';
import axios from 'axios';
import { useAuthStore } from '~/store/authStore';
import { tokenStore, refreshTokensAndReplay } from '~/services/api';
import type { User } from '~/api/openapi';

/**
 * On mount, if the auth store has a persisted user, fire GET /api/me
 * so the localStorage profile is reconciled against the server-truth.
 *
 * SECURITY.md L7. The persisted user can drift from the server
 * (deleted row, username change, manual localStorage edit). Without
 * the round-trip the SPA keeps showing stale data — or worse,
 * presents a logged-in UI to a user whose session has been revoked.
 * We use raw axios (not the wrapped `api` instance) so the response
 * interceptor's logout + toast side effects don't fire on this
 * background probe: a 401 here is the reconciliation result itself,
 * not a user-visible failure.
 *
 * 401 handling: a stale access token is the most common cause. We
 * try `POST /api/refresh` with the sessionStorage refresh token
 * before giving up — only after refresh fails (or the retry /api/me
 * also 401s, meaning the server-side session was actually revoked)
 * do we run the full logout. This keeps an expired-access-token
 * session alive across an F5 refresh, matching the behaviour of
 * every other authenticated request that runs through the wrapped
 * `api` instance's interceptor.
 *
 * On 200 we update the store with the server-truth; on any other
 * failure (network error, 5xx, refresh failure) we either leave the
 * store untouched (network/5xx — a flaky connection must not log
 * the user out of an otherwise valid session) or clear it via the
 * shared logout path (refresh genuinely failed).
 *
 * If an access token is present in the tokenStore we stamp it on the
 * reconciliation probe so a stale localStorage user with a fresh
 * token store can still hit /api/me. Cold-start with no token store
 * means the probe is anonymous; a 200 is impossible in that case and
 * the persisted user is cleared on 401.
 */
export function useAuthReconciliation(): void {
  useEffect(() => {
    const initialUser = useAuthStore.getState().user;
    if (!initialUser) return; // nothing to reconcile on a cold start

    let cancelled = false;
    const ctrl = new AbortController();
    const apiBase = import.meta.env.VITE_API_URL || 'http://localhost:8080/api';

    void axios
      .get<User>(`${apiBase}/me`, {
        withCredentials: false,
        signal: ctrl.signal,
        // Dedicated axios flag — keeps the request out of the
        // wrapped instance's response interceptor chain so a 401
        // here doesn't trigger a refresh attempt and toast.
        // Mirrors the `loginAxios` pattern used by `logout()`.
        headers: tokenStore.getAccessToken()
          ? { Authorization: `Bearer ${tokenStore.getAccessToken()}` }
          : undefined,
      })
      .then((resp) => {
        if (cancelled) return;
        // setAuth accepts the same shape as login/register, so the
        // server-truth user lands in the store unchanged.
        useAuthStore.getState().setAuth({ user: resp.data });
      })
      .catch(async (err: unknown) => {
        if (cancelled) return;
        // Don't treat aborted requests (StrictMode double-invoke,
        // unmount) as a logout signal.
        if (axios.isCancel(err)) return;
        const status = (err as { response?: { status?: number } })?.response?.status;
        if (status !== 401) {
          // Network / 5xx: leave the persisted user in place — a
          // flaky connection must not log the user out of an
          // otherwise valid session.
          return;
        }

        // 401: most likely a stale access token. Try a refresh
        // before declaring the session dead. The refresh updates
        // the tokenStore on success; the retry must read the
        // *new* access token, not the one we just used.
        const refreshed = await refreshTokensAndReplay();
        if (cancelled) return;
        if (!refreshed) {
          // Refresh failed (refresh token gone or rejected). The
          // session is genuinely dead — clear local state so the
          // UI flips to logged-out. We swallow the toast here on
          // purpose: a 401 on the background probe shouldn't
          // surface as a user-visible error.
          useAuthStore.getState().logout();
          return;
        }

        try {
          const retry = await axios.get<User>(`${apiBase}/me`, {
            withCredentials: false,
            signal: ctrl.signal,
            headers: { Authorization: `Bearer ${tokenStore.getAccessToken()}` },
          });
          if (cancelled) return;
          useAuthStore.getState().setAuth({ user: retry.data });
        } catch {
          if (cancelled) return;
          // Refresh succeeded but /api/me still 401s — the
          // server-side session was revoked (deleted user,
          // family-wide revoke) even though the refresh family is
          // technically valid. Log out.
          useAuthStore.getState().logout();
        }
      });

    return () => {
      cancelled = true;
      ctrl.abort();
    };
  }, []);
}