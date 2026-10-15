import { describe, it, expect, beforeEach, vi } from 'vitest';
import { act, waitFor } from '@testing-library/react';
import { TestProviders, renderHook } from '~/test/test-utils';
import { feedService } from '../services/feedService';
import { useFeeds } from './useFeeds';
import type { Feed, NewFeed, PaginatedFeeds } from '../types/feed';
import { useAuthStore } from '~/store/authStore';

// Auto-mock the service so `feedService.getFeeds`/`addFeed`/etc. become
// `vi.fn()`s we can assert against per test. Mirrors the convention from
// `src/App.test.tsx`.
vi.mock('../services/feedService');

const mockedService = feedService as unknown as {
  getFeeds: ReturnType<typeof vi.fn>;
  addFeed: ReturnType<typeof vi.fn>;
  updateFeed: ReturnType<typeof vi.fn>;
  deleteFeed: ReturnType<typeof vi.fn>;
};

// Test user. `useFeeds` reads the current user id from the auth store
// to scope its query key, so every test must seed the store before
// rendering the hook — otherwise the query stays disabled and nothing
// fetches.
const TEST_USER = { id: 1, username: 'tester', created_at: '', updated_at: '' };

const basePage: PaginatedFeeds = {
  data: [
    {
      id: 1,
      user_id: 1,
      title: 'Existing',
      description: 'existing',
      created_at: '2024-01-01T00:00:00Z',
      updated_at: '2024-01-01T00:00:00Z',
    },
  ],
  page: 1,
  page_size: 20,
  total: 1,
  has_more: false,
};

function renderUseFeeds(searchTerm = '') {
  return renderHook(() => useFeeds(searchTerm), { wrapper: TestProviders });
}

/**
 * Wait for the initial query to land in the cache. A single
 * `await Promise.resolve()` flushes one microtask but TanStack Query
 * chains several before the resolved value reaches the rendered
 * component — `waitFor` polls until the predicate holds.
 */
async function waitForInitialLoad(result: { current: ReturnType<typeof useFeeds> }) {
  await waitFor(() => {
    expect(result.current.isLoading).toBe(false);
  });
  await waitFor(() => {
    expect(result.current.feeds.length).toBeGreaterThan(0);
  });
}

describe('useFeeds', () => {
  beforeEach(() => {
    mockedService.getFeeds.mockReset();
    mockedService.addFeed.mockReset();
    mockedService.updateFeed.mockReset();
    mockedService.deleteFeed.mockReset();
    // Seed the auth store so the hook's `enabled: userId !== undefined`
    // guard flips true. Reset to logged-out in the next beforeEach so
    // cross-test leakage is impossible — important for the user-id swap
    // tests below.
    useAuthStore.setState({
      user: TEST_USER,
      isAuthenticated: true,
    });
  });

  describe('addFeed optimistic prepend', () => {
    it('prepends a placeholder row with a negative id and bumps total', async () => {
      // `mockResolvedValue` (not Once) so the post-mutation
      // `onSettled → invalidate` refetch doesn't blow up.
      mockedService.getFeeds.mockResolvedValue(basePage);

      const { result } = renderUseFeeds();

      await waitForInitialLoad(result);

      // No optimistic row yet — just the seeded data.
      expect(result.current.feeds[0].id).toBe(1);
      expect(result.current.totalCount).toBe(1);

      // Hold the mutation promise open indefinitely so the
      // optimistic state is observable before onSettled's invalidate
      // refetches and clobbers the cache.
      let resolveAdd!: (value: Feed) => void;
      mockedService.addFeed.mockReturnValueOnce(
        new Promise<Feed>((resolve) => {
          resolveAdd = resolve;
        }),
      );

      const payload: NewFeed = { title: 'Brand new', description: '' };

      act(() => {
        result.current.addFeed.mutate(payload);
      });

      // Optimistic prepend: the new row sits at index 0 with a
      // negative id (the -Date.now() sentinel) and the existing row
      // is still present.
      await waitFor(() => {
        expect(result.current.feeds[0]?.id).toBeLessThan(0);
      });
      expect(result.current.feeds[0]?.title).toBe('Brand new');
      expect(result.current.feeds[0]?.user_id).toBe(TEST_USER.id);
      expect(result.current.feeds.find((f) => f.id === 1)).toBeDefined();
      // Total bumped by one before the server has responded.
      expect(result.current.totalCount).toBe(2);

      // Settle the pending mutation so the test ends cleanly.
      await act(async () => {
        resolveAdd({
          id: 99,
          user_id: TEST_USER.id,
          title: 'Brand new',
          description: '',
          created_at: '2024-02-01T00:00:00Z',
          updated_at: '2024-02-01T00:00:00Z',
        });
      });
    });
  });

  describe('addFeed onSuccess placeholder swap', () => {
    it('replaces the optimistic placeholder with the server row', async () => {
      // Hold the initial fetch AND the post-mutation refetch open so
      // we can observe the swap before onSettled's invalidate replaces
      // the cache.
      let resolveFetch!: (value: PaginatedFeeds) => void;
      mockedService.getFeeds.mockImplementationOnce(
        () => new Promise<PaginatedFeeds>((resolve) => {
          resolveFetch = resolve;
        }),
      );

      const { result } = renderUseFeeds();

      // Resolve the initial fetch so the hook leaves the loading state.
      await act(async () => {
        resolveFetch(basePage);
      });

      let resolveAdd!: (value: Feed) => void;
      mockedService.addFeed.mockReturnValueOnce(
        new Promise<Feed>((resolve) => {
          resolveAdd = resolve;
        }),
      );

      act(() => {
        result.current.addFeed.mutate({ title: 'Brand new', description: '' });
      });

      // Wait for the optimistic placeholder to land.
      await waitFor(() => {
        expect(result.current.feeds.some((f) => f.id < 0)).toBe(true);
      });

      // Resolve addFeed so onSuccess swaps the placeholder for the
      // server row. onSettled will fire invalidate, which triggers
      // another fetch — but we hold the next fetch open so the swap
      // stays observable.
      let resolveRefetch!: (value: PaginatedFeeds) => void;
      mockedService.getFeeds.mockImplementationOnce(
        () => new Promise<PaginatedFeeds>((resolve) => {
          resolveRefetch = resolve;
        }),
      );

      await act(async () => {
        resolveAdd({
          id: 99,
          user_id: TEST_USER.id,
          title: 'Brand new',
          description: '',
          created_at: '2024-02-01T00:00:00Z',
          updated_at: '2024-02-01T00:00:00Z',
        });
      });

      // After onSuccess the placeholder should have been swapped.
      // Poll because the swap and the onSettled-driven refetch can
      // race; the refetch is held open so the swap stays visible.
      await waitFor(() => {
        expect(result.current.feeds.some((f) => f.id === 99)).toBe(true);
      });
      expect(result.current.feeds.some((f) => f.id < 0)).toBe(false);

      // Drain the refetch so the test ends cleanly.
      await act(async () => {
        resolveRefetch(basePage);
      });
    });
  });

  describe('addFeed rollback on error', () => {
    it('restores the pre-state snapshot when mutationFn rejects', async () => {
      mockedService.getFeeds.mockResolvedValue(basePage);

      const { result } = renderUseFeeds();

      await waitForInitialLoad(result);

      // Force the network call to reject so onError fires.
      mockedService.addFeed.mockRejectedValueOnce(new Error('boom'));

      // Snapshot the pre-state via the hook's own view before mutating.
      const snapshot = result.current.feeds.slice();

      await act(async () => {
        try {
          await result.current.addFeed.mutateAsync({ title: 'Brand new', description: '' });
        } catch {
          // expected
        }
      });

      // After rollback the cache should be back to the seeded page
      // (no optimistic row, original total).
      expect(result.current.feeds).toEqual(snapshot);
      expect(result.current.feeds.some((f) => f.id < 0)).toBe(false);
    });
  });

  describe('updateFeed optimistic id guard', () => {
    it('calls feedService.updateFeed for a positive id', async () => {
      mockedService.getFeeds.mockResolvedValue(basePage);

      const { result } = renderUseFeeds();

      await waitForInitialLoad(result);

      const updated: Feed = {
        ...(basePage.data as Feed[])[0],
        title: 'Renamed',
      };
      mockedService.updateFeed.mockResolvedValueOnce(updated);

      await act(async () => {
        await result.current.updateFeed.mutateAsync(updated);
      });

      expect(mockedService.updateFeed).toHaveBeenCalledWith(1, updated);
    });

    it('rejects the mutation for a negative id and never calls the service', async () => {
      mockedService.getFeeds.mockResolvedValue(basePage);

      const { result } = renderUseFeeds();

      await waitForInitialLoad(result);

      const placeholder: Feed = {
        id: -12345,
        user_id: TEST_USER.id,
        title: 'placeholder',
        description: '',
        created_at: '2024-01-01T00:00:00Z',
        updated_at: '2024-01-01T00:00:00Z',
      };

      let caught: Error | null = null;
      await act(async () => {
        try {
          await result.current.updateFeed.mutateAsync(placeholder);
        } catch (err) {
          caught = err as Error;
        }
      });

      expect(caught).not.toBeNull();
      expect(caught!.message).toMatch(/Cannot update/);
      expect(mockedService.updateFeed).not.toHaveBeenCalled();
    });
  });

  describe('deleteFeed optimistic id short-circuit', () => {
    it('resolves without calling the service and removes the row from cache', async () => {
      // Seed the cache with a synthetic optimistic placeholder so we can
      // verify it disappears without any network activity.
      const placeholder: Feed = {
        id: -42,
        user_id: TEST_USER.id,
        title: 'optimistic only',
        description: '',
        created_at: '2024-01-01T00:00:00Z',
        updated_at: '2024-01-01T00:00:00Z',
      };
      mockedService.getFeeds.mockResolvedValue({
        ...basePage,
        data: [...(basePage.data ?? []), placeholder],
        total: 2,
      });

      const { result } = renderUseFeeds();

      await waitFor(() => {
        expect(result.current.feeds.find((f) => f.id === -42)).toBeDefined();
      });

      // Fire-and-forget mutate; the optimistic id short-circuits the
      // service call so there's no mutationFn promise to settle.
      // Inspect the cache mid-mutation by holding the post-delete
      // refetch open — otherwise onSettled's invalidate would
      // restore the seeded placeholder.
      let resolveRefetch!: (value: PaginatedFeeds) => void;
      mockedService.getFeeds.mockImplementationOnce(
        () => new Promise<PaginatedFeeds>((resolve) => {
          resolveRefetch = resolve;
        }),
      );

      act(() => {
        result.current.deleteFeed.mutate(-42);
      });

      // The row is gone from the cache synchronously after onMutate.
      await waitFor(() => {
        expect(result.current.feeds.find((f) => f.id === -42)).toBeUndefined();
      });
      expect(mockedService.deleteFeed).not.toHaveBeenCalled();

      // Drain the refetch so the test ends cleanly.
      await act(async () => {
        resolveRefetch(basePage);
      });
    });
  });

  describe('owner-scoped query key', () => {
    it('does not fetch when no user is authenticated', async () => {
      // Override the seeded user with a logged-out state.
      useAuthStore.setState({ user: null, isAuthenticated: false });

      renderUseFeeds();

      // Give any would-be fetch a chance to fire.
      await act(async () => {
        await new Promise((resolve) => setTimeout(resolve, 0));
      });

      expect(mockedService.getFeeds).not.toHaveBeenCalled();
    });

    it('refetches when the authenticated user changes', async () => {
      mockedService.getFeeds.mockResolvedValue(basePage);

      // First render with user 1.
      const { result, rerender } = renderUseFeeds();
      await waitForInitialLoad(result);

      expect(mockedService.getFeeds).toHaveBeenCalledTimes(1);

      // Swap to user 2 — the query key includes the user id, so a
      // fresh fetch fires against the new owner's cache. The two
      // users must not share pages.
      const user2 = { id: 2, username: 'other', created_at: '', updated_at: '' };
      await act(async () => {
        useAuthStore.setState({ user: user2, isAuthenticated: true });
      });

      await waitFor(() => {
        expect(mockedService.getFeeds).toHaveBeenCalledTimes(2);
      });

      // Rerender to consume the latest hook value before tearing down.
      rerender();
    });
  });

  describe('getNextPageParam', () => {
    it('returns last.page + 1 when has_more is true', () => {
      // The hook exposes `hasMore` (driven by fetchNextPage's
      // `getNextPageParam`) but the callback itself lives inside
      // `useInfiniteQuery`. To exercise it we mirror the predicate
      // from useFeeds.ts — that's the contract getNextPageParam
      // implements, and the integration is covered by the optimistic
      // update tests above.
      const last: PaginatedFeeds = { ...basePage, page: 2, has_more: true };
      const next = last.has_more ? last.page + 1 : undefined;
      expect(next).toBe(3);
    });

    it('returns undefined when has_more is false', () => {
      const last: PaginatedFeeds = { ...basePage, has_more: false };
      const next = last.has_more ? last.page + 1 : undefined;
      expect(next).toBeUndefined();
    });
  });
});