import { describe, it, expect, beforeEach, vi, afterEach } from 'vitest';
import { renderHook, waitFor } from '@testing-library/react';
import axios from 'axios';
import { useAuthReconciliation } from './useAuthReconciliation';
import { useAuthStore } from '~/store/authStore';
import type { User } from '~/api/openapi';

// We intentionally do NOT `vi.mock('axios')` — auto-mocking the
// module would replace `axios.create` with `undefined`, breaking
// the authStore's logout() path when our hook subsequently
// triggers it via a 401 reconciliation. Instead, spy on the
// specific static methods the hook calls (`axios.get`) and
// stub the responses per test.

const baseUser: User = {
  id: 7,
  username: 'alice',
  email: 'alice@example.com',
  created_at: '2024-01-01T00:00:00Z',
  updated_at: '2024-01-01T00:00:00Z',
};

describe('useAuthReconciliation', () => {
  let getSpy: ReturnType<typeof vi.spyOn>;

  beforeEach(() => {
    // Reset the auth store to a known shape — clear persisted
    // fields too so each test starts from "no user".
    useAuthStore.setState({ user: null, isAuthenticated: false });
    getSpy = vi.spyOn(axios, 'get');
  });

  afterEach(() => {
    getSpy.mockRestore();
    vi.restoreAllMocks();
  });

  it('does not fire /api/me when no user is persisted', async () => {
    // Cold start: nothing in localStorage, nothing to reconcile.
    // The hook should be a no-op — no network round-trip, no
    // state mutation.
    renderHook(() => useAuthReconciliation());

    // waitFor a tick to ensure any async setup has settled.
    await Promise.resolve();
    expect(getSpy).not.toHaveBeenCalled();
  });

  it('reconciles the persisted user against the server-truth on 200', async () => {
    // SECURITY.md L7: when /api/me returns the up-to-date
    // profile, the store is updated to match. A future username
    // change on the server surfaces here without a manual
    // logout/login cycle.
    useAuthStore.getState().setAuth({ user: baseUser });

    const serverTruth: User = {
      ...baseUser,
      username: 'alice-renamed',
      updated_at: '2025-06-15T00:00:00Z',
    };
    getSpy.mockResolvedValueOnce({ data: serverTruth } as never);

    renderHook(() => useAuthReconciliation());

    await waitFor(() => {
      expect(useAuthStore.getState().user).toEqual(serverTruth);
    });
    expect(getSpy).toHaveBeenCalledTimes(1);
    expect(getSpy).toHaveBeenCalledWith(
      '/api/me',
      expect.objectContaining({ withCredentials: true }),
    );
  });

  it('clears the persisted user on 401 (cookies revoked)', async () => {
    // The user was logged in server-side (e.g. an admin revoked
    // the family) but the SPA still has a stale localStorage
    // entry. The reconciliation probe must surface this as a
    // logged-out UI on the next render — otherwise the SPA
    // would keep showing a logged-in shell with no working API.
    useAuthStore.getState().setAuth({ user: baseUser });

    const err = Object.assign(new Error('Unauthorized'), {
      response: { status: 401 },
    });
    getSpy.mockRejectedValueOnce(err);

    renderHook(() => useAuthReconciliation());

    await waitFor(() => {
      expect(useAuthStore.getState().user).toBeNull();
      expect(useAuthStore.getState().isAuthenticated).toBe(false);
    });
  });

  it('leaves the persisted user alone on a network failure', async () => {
    // A flaky network must not log the user out of an
    // otherwise valid session. The reconciliation probe is
    // best-effort; the next user action will surface any real
    // auth failure via the normal response interceptor path.
    useAuthStore.getState().setAuth({ user: baseUser });

    getSpy.mockRejectedValueOnce(new Error('Network Error'));

    renderHook(() => useAuthReconciliation());

    // Give the microtask queue a chance to drain; the user
    // must still be set.
    await Promise.resolve();
    await Promise.resolve();
    expect(useAuthStore.getState().user).toEqual(baseUser);
    expect(useAuthStore.getState().isAuthenticated).toBe(true);
  });

  it('leaves the persisted user alone on a 5xx', async () => {
    // Transient server failure: the cookies may still be
    // valid; logging the user out would be over-reaction.
    useAuthStore.getState().setAuth({ user: baseUser });

    const err = Object.assign(new Error('Server Error'), {
      response: { status: 503 },
    });
    getSpy.mockRejectedValueOnce(err);

    renderHook(() => useAuthReconciliation());

    await Promise.resolve();
    await Promise.resolve();
    expect(useAuthStore.getState().user).toEqual(baseUser);
  });
});
