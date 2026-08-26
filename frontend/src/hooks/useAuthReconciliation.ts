import { useEffect } from 'react';
import axios from 'axios';
import { useAuthStore } from '~/store/authStore';
import type { User } from '~/api/openapi';

/**
 * On mount, if the auth store has a persisted user, fire GET /api/me
 * so the localStorage profile is reconciled against the server-truth.
 *
 * SECURITY.md L7. The persisted user can drift from the server
 * (deleted row, username change, manual localStorage edit). Without
 * the round-trip the SPA keeps showing stale data — or worse,
 * presents a logged-in UI to a user whose cookies have already
 * been revoked. We use raw axios (not the wrapped `api` instance)
 * so the response interceptor's logout + toast side effects don't
 * fire on this background probe: a 401 here is the reconciliation
 * result itself, not a user-visible failure.
 *
 * On 200 we update the store with the server-truth; on 401 we clear
 * the persisted user via the same logout path the rest of the app
 * uses; on any other failure (network error, 5xx) we leave the
 * store untouched so a flaky connection doesn't log the user out
 * of an otherwise valid session.
 */
export function useAuthReconciliation(): void {
  useEffect(() => {
    const initialUser = useAuthStore.getState().user;
    if (!initialUser) return; // nothing to reconcile on a cold start

    let cancelled = false;
    const ctrl = new AbortController();

    void axios
      .get<User>('/api/me', {
        withCredentials: true,
        signal: ctrl.signal,
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
          // Cookies are gone — clear local state so the UI flips
          // to logged-out on the next render. We swallow the
          // toast here on purpose: a 401 on the background probe
          // shouldn't surface as a user-visible error.
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
