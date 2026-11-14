import { queryClient } from '~/services/queryClient';
import {
  KB_DOCUMENTS_QUERY_KEY,
  KB_SETTINGS_QUERY_KEY,
  KB_SOURCES_QUERY_KEY,
} from '~/services/queryKeys';
import { useUiStore } from '~/store/uiStore';

export const resetKbStateForNewUser = (userId: number | null): void => {
  queryClient.removeQueries({ queryKey: [KB_SOURCES_QUERY_KEY] });
  queryClient.removeQueries({ queryKey: [KB_DOCUMENTS_QUERY_KEY] });
  queryClient.removeQueries({
    queryKey: [KB_SETTINGS_QUERY_KEY],
    predicate: (query) => query.queryKey[1] !== userId,
  });
  useUiStore.getState().setActiveView('feeds');
};
