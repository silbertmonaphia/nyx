import {
  useInfiniteQuery,
  useMutation,
  useQueryClient,
  type InfiniteData,
} from '@tanstack/react-query';
import { useMemo } from 'react';
import { feedService } from '../services/feedService';
import { Feed, NewFeed, PaginatedFeeds } from '../types/feed';
import type { SortOrder } from '../store/feedUiStore';

const FEEDS_QUERY_KEY = 'feeds' as const;
const PAGE_SIZE = 20;

// Negative ids are placeholders for rows that have not yet been
// confirmed by the server (see addFeed's optimistic update). They
// must never be sent to the API.
const isOptimisticId = (id: number) => id < 0;

// The generated OpenAPI types declare `FeedsPage.data` as `Feed[] | null`
// because huma emits `"type": ["array", "null"]` for the slice. The backend
// always sends a non-null array (`NewFeedsPage` coerces nil to []), so the
// nullable type is a schema artefact rather than something the wire
// actually carries. Coerce at the boundary so the optimistic-update
// callbacks can keep working with plain arrays.
const pageData = (page: PaginatedFeeds): Feed[] => page.data ?? [];

interface FeedsContext {
  previous: Array<[readonly unknown[], unknown]> | undefined;
}

export const useFeeds = (searchTerm: string, sortOrder: SortOrder = 'desc') => {
  const queryClient = useQueryClient();
  // sortOrder is part of the key so flipping the toggle triggers
  // a refetch against the opposite-direction cache and never serves
  // a stale page from the other sort.
  const queryKey = [FEEDS_QUERY_KEY, searchTerm, sortOrder] as const;

  const {
    data,
    fetchNextPage,
    hasNextPage,
    isFetchingNextPage,
    isLoading,
    isError,
  } = useInfiniteQuery<PaginatedFeeds, Error, InfiniteData<PaginatedFeeds>, typeof queryKey, number>({
    queryKey,
    queryFn: ({ pageParam }) => feedService.getFeeds(searchTerm, pageParam, PAGE_SIZE, sortOrder),
    initialPageParam: 1,
    getNextPageParam: (last) => (last ? (last.has_more ? last.page + 1 : undefined) : undefined),
    staleTime: 30_000,
    // Keep the cache around long enough to make back-navigation feel
    // instant; the default 5 min is too eager for this dataset.
    gcTime: 30 * 60 * 1000,
  });

  const feeds = useMemo<Feed[]>(
    // `data` is `Feed[] | null` in the OpenAPI schema; the backend always
    // emits a non-null array (see NewFeedsPage in model.go), but the spec
    // doesn't pin that down, so we coalesce defensively.
    () => data?.pages?.flatMap((p) => p.data ?? []) ?? [],
    [data],
  );

  const totalCount = data?.pages?.[0]?.total ?? 0;

  // ---- Mutations with optimistic updates ----------------------------------

  const addFeed = useMutation<Feed, Error, NewFeed, FeedsContext>({
    mutationFn: (newFeed) => feedService.addFeed(newFeed),
    onMutate: async (newFeed) => {
      await queryClient.cancelQueries({ queryKey });
      const previous = queryClient.getQueriesData({ queryKey });
      queryClient.setQueriesData<InfiniteData<PaginatedFeeds>>(
        { queryKey },
        (old) => {
          if (!old || old.pages.length === 0) return old;
          const optimistic: Feed = {
            ...newFeed,
            rating: 0, // form no longer collects rating; placeholder until server replaces this row
            id: -Date.now(), // negative id marks it as unconfirmed
            created_at: new Date().toISOString(),
            updated_at: new Date().toISOString(),
          };
          const first = old.pages[0];
          return {
            ...old,
            pages: [
              { ...first, data: [optimistic, ...pageData(first)], total: first.total + 1 },
              ...old.pages.slice(1),
            ],
          };
        },
      );
      return { previous };
    },
    onSuccess: (created) => {
      // Swap the optimistic placeholder (negative id) for the real row
      // returned by the server so subsequent edit/delete targets the
      // persisted id. Failures still flow through onError → onSettled.
      queryClient.setQueriesData<InfiniteData<PaginatedFeeds>>(
        { queryKey },
        (old) => {
          if (!old) return old;
          return {
            ...old,
            pages: old.pages.map((page) => ({
              ...page,
              data: pageData(page).map((f) => (isOptimisticId(f.id) ? created : f)),
            })),
          };
        },
      );
    },
    onError: (_err, _vars, ctx) => {
      ctx?.previous?.forEach(([key, snapshot]) => {
        queryClient.setQueryData(key, snapshot);
      });
    },
    onSettled: () => {
      queryClient.invalidateQueries({ queryKey });
    },
  });

  const updateFeed = useMutation<Feed, Error, Feed, FeedsContext>({
    mutationFn: (feed) => {
      if (isOptimisticId(feed.id)) {
        // Refuse to PUT a placeholder id — the server has no row to
        // update. The optimistic row will be replaced by addFeed's
        // onSuccess once the create mutation settles.
        return Promise.reject(new Error('Cannot update a feed before it has been created'));
      }
      return feedService.updateFeed(feed.id, feed);
    },
    onMutate: async (updated) => {
      await queryClient.cancelQueries({ queryKey });
      const previous = queryClient.getQueriesData({ queryKey });
      queryClient.setQueriesData<InfiniteData<PaginatedFeeds>>(
        { queryKey },
        (old) => {
          if (!old) return old;
          return {
            ...old,
            pages: old.pages.map((page) => ({
              ...page,
              data: pageData(page).map((f) => (f.id === updated.id ? { ...f, ...updated } : f)),
            })),
          };
        },
      );
      return { previous };
    },
    onError: (_err, _vars, ctx) => {
      ctx?.previous?.forEach(([key, snapshot]) => {
        queryClient.setQueryData(key, snapshot);
      });
    },
    onSettled: () => {
      queryClient.invalidateQueries({ queryKey });
    },
  });

  const deleteFeed = useMutation<void, Error, number, FeedsContext>({
    mutationFn: (id) => {
      if (isOptimisticId(id)) {
        // The row never reached the server. Removing it locally is
        // safe — we already removed it in onMutate, just skip the API
        // call that would 404 on a negative id.
        return Promise.resolve();
      }
      return feedService.deleteFeed(id);
    },
    onMutate: async (id) => {
      await queryClient.cancelQueries({ queryKey });
      const previous = queryClient.getQueriesData({ queryKey });
      queryClient.setQueriesData<InfiniteData<PaginatedFeeds>>(
        { queryKey },
        (old) => {
          if (!old) return old;
          return {
            ...old,
            pages: old.pages.map((page) => ({
              ...page,
              data: pageData(page).filter((f) => f.id !== id),
              total: Math.max(0, page.total - 1),
            })),
          };
        },
      );
      return { previous };
    },
    onError: (_err, _vars, ctx) => {
      ctx?.previous?.forEach(([key, snapshot]) => {
        queryClient.setQueryData(key, snapshot);
      });
    },
    onSettled: () => {
      queryClient.invalidateQueries({ queryKey });
    },
  });

  return {
    feeds,
    totalCount,
    isLoading,
    isError,
    hasMore: !!hasNextPage,
    isLoadingMore: isFetchingNextPage,
    loadMore: fetchNextPage,
    addFeed,
    updateFeed,
    deleteFeed,
  };
};