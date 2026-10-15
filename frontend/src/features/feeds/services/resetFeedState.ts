import { queryClient } from '~/services/queryClient';
import { FEEDS_QUERY_KEY } from '../hooks/useFeeds';
import { useFeedUiStore } from '../store/feedUiStore';

// Wipe everything that's scoped to the previous user's identity so a
// new login (or logout) renders a clean slate. Called from the
// App-level useEffect that subscribes to useAuthStore.user?.id — see
// App.tsx for the wiring.
//
// We drop every cached query under FEEDS_QUERY_KEY (across every
// user) because the per-user key changes the moment the user id
// changes, so the previous user's pages are stale by definition.
// The UI store reset clears in-flight modals + a leftover search box
// so user B doesn't see user A's query against an empty list.
export const resetFeedStateForNewUser = () => {
  queryClient.removeQueries({ queryKey: [FEEDS_QUERY_KEY] });
  useFeedUiStore.getState().reset();
};