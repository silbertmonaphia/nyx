import { queryClient } from '~/services/queryClient';
import {
  KB_DOCUMENTS_QUERY_KEY,
  KB_SETTINGS_QUERY_KEY,
  KB_SOURCES_QUERY_KEY,
} from '~/services/queryKeys';
import { useUiStore } from '~/store/uiStore';

export const resetKbStateForNewUser = (): void => {
  queryClient.removeQueries({ queryKey: [KB_SOURCES_QUERY_KEY] });
  queryClient.removeQueries({ queryKey: [KB_DOCUMENTS_QUERY_KEY] });
  queryClient.removeQueries({ queryKey: [KB_SETTINGS_QUERY_KEY] });
  useUiStore.getState().setActiveView('feeds');
};
