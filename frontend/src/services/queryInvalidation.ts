import { useAuthStore } from '~/store/authStore';
import { queryClient } from './queryClient';
import { FEEDS_QUERY_KEY } from './queryKeys';

export const invalidateFeedQueries = (): void => {
  if (!useAuthStore.getState().isAuthenticated) return;
  void queryClient.invalidateQueries({ queryKey: [FEEDS_QUERY_KEY] });
};
