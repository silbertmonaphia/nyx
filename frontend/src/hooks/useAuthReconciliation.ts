import { useEffect } from 'react';
import axios from 'axios';
import { useAuthStore } from '~/store/authStore';
import { tokenStore } from '~/services/api';
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
 * On 200 we update the store with the server-truth; on 401 we clear
 * the persisted user via the same logout path the rest of the app
 * uses (which also clears the in-memory + sessionStorage token pair);
 * on any other failure (network error, 5xx) we leave the store
 * untouched so a flaky connection doesn't log the user out of an
 * otherwise valid session.
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
    const at = tokenStore.getAccessToken();

    void axios
      .get<User>(`${import.meta.env.VITE_API_URL || 'http://localhost:8080/api'}/me`, {
        withCredentials: false,
        signal: ctrl.signal,
        headers: at ? { Authorization: `Bearer ${at}` } : undefined,
        // Dedicated axios flag — keeps the request out of the
        // wrapped instance's response interceptor chain so a 401
        // here doesn't trigger a refresh attempt and toast.
        // Mirrors the `loginAxios` pattern used by `logout()`.
      })
      .then((resp) => {
        if (cancelled) return;
        // setAuth accepts the same shape as login/register, so the
        // server-truth user lands in the store unchanged.
        useAuthStore.getState().setAuth({ user: resp.data });
      })
      .catch((err: unknown) => {
        if (cancelled) return;
        // Don't treat aborted requests (StrictMode double-invoke,
        // unmount) as a logout signal.
        if (axios.isCancel(err)) return;
        const status = (err as { response?: { status?: number } })?.response?.status;
        if (status === 401) {
          // Tokens are gone (or rejected) — clear local state so
          // the UI flips to logged-out on the next render. We
          // swallow the toast here on purpose: a 401 on the
          // background probe shouldn't surface as a user-visible
          // error.
          useAuthStore.getState().logout();
        }
        // Any other failure (network, 5xx) leaves the persisted
        // user in place — a flaky network must not log the user
        // out of an otherwise valid session.
      });

    return () => {
      cancelled = true;
      ctrl.abort();
    };
  }, []);
}