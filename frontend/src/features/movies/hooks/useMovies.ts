import {
  useInfiniteQuery,
  useMutation,
  useQueryClient,
  type InfiniteData,
} from '@tanstack/react-query';
import { useMemo } from 'react';
import { movieService } from '../services/movieService';
import { Movie, NewMovie, PaginatedMovies } from '../types/movie';

const MOVIES_QUERY_KEY = 'movies' as const;
const PAGE_SIZE = 20;

interface MoviesContext {
  previous: Array<[readonly unknown[], unknown]> | undefined;
}

export const useMovies = (searchTerm: string) => {
  const queryClient = useQueryClient();
  const queryKey = [MOVIES_QUERY_KEY, searchTerm] as const;

  const {
    data,
    fetchNextPage,
    hasNextPage,
    isFetchingNextPage,
    isLoading,
    isError,
  } = useInfiniteQuery<PaginatedMovies, Error, InfiniteData<PaginatedMovies>, typeof queryKey, number>({
    queryKey,
    queryFn: ({ pageParam }) => movieService.getMovies(searchTerm, pageParam, PAGE_SIZE),
    initialPageParam: 1,
    getNextPageParam: (last) => (last.has_more ? last.page + 1 : undefined),
    staleTime: 30_000,
  });

  const movies = useMemo<Movie[]>(
    () => data?.pages.flatMap((p) => p.data) ?? [],
    [data],
  );

  const totalCount = data?.pages[0]?.total ?? 0;

  // ---- Mutations with optimistic updates ----------------------------------

  const addMovie = useMutation<Movie, Error, NewMovie, MoviesContext>({
    mutationFn: (newMovie) => movieService.addMovie(newMovie),
    onMutate: async (newMovie) => {
      await queryClient.cancelQueries({ queryKey });
      const previous = queryClient.getQueriesData({ queryKey });
      queryClient.setQueriesData<InfiniteData<PaginatedMovies>>(
        { queryKey },
        (old) => {
          if (!old || old.pages.length === 0) return old;
          const optimistic: Movie = {
            ...newMovie,
            id: -Date.now(), // negative id marks it as unconfirmed
            created_at: new Date().toISOString(),
            updated_at: new Date().toISOString(),
          };
          const first = old.pages[0];
          return {
            ...old,
            pages: [
              { ...first, data: [optimistic, ...first.data], total: first.total + 1 },
              ...old.pages.slice(1),
            ],
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

  const updateMovie = useMutation<Movie, Error, Movie, MoviesContext>({
    mutationFn: (movie) => movieService.updateMovie(movie.id, movie),
    onMutate: async (updated) => {
      await queryClient.cancelQueries({ queryKey });
      const previous = queryClient.getQueriesData({ queryKey });
      queryClient.setQueriesData<InfiniteData<PaginatedMovies>>(
        { queryKey },
        (old) => {
          if (!old) return old;
          return {
            ...old,
            pages: old.pages.map((page) => ({
              ...page,
              data: page.data.map((m) => (m.id === updated.id ? { ...m, ...updated } : m)),
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

  const deleteMovie = useMutation<void, Error, number, MoviesContext>({
    mutationFn: (id) => movieService.deleteMovie(id),
    onMutate: async (id) => {
      await queryClient.cancelQueries({ queryKey });
      const previous = queryClient.getQueriesData({ queryKey });
      queryClient.setQueriesData<InfiniteData<PaginatedMovies>>(
        { queryKey },
        (old) => {
          if (!old) return old;
          return {
            ...old,
            pages: old.pages.map((page) => ({
              ...page,
              data: page.data.filter((m) => m.id !== id),
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
    movies,
    totalCount,
    isLoading,
    isError,
    hasMore: !!hasNextPage,
    isLoadingMore: isFetchingNextPage,
    loadMore: fetchNextPage,
    addMovie,
    updateMovie,
    deleteMovie,
  };
};