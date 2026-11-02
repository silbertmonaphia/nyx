import { beforeEach, describe, expect, it } from 'vitest';
import { queryClient } from '~/services/queryClient';
import {
  KB_DOCUMENTS_QUERY_KEY,
  KB_SETTINGS_QUERY_KEY,
  KB_SOURCES_QUERY_KEY,
} from '~/services/queryKeys';
import { useUiStore } from '~/store/uiStore';
import { resetKbStateForNewUser } from './resetKbState';

describe('resetKbStateForNewUser', () => {
  beforeEach(() => {
    queryClient.clear();
    useUiStore.setState({ activeView: 'feeds' });
  });

  it('drops every KB cache entry and returns to the feeds view', () => {
    queryClient.setQueryData([KB_SOURCES_QUERY_KEY, 1], [{ id: 1, name: 'a' }]);
    queryClient.setQueryData([KB_DOCUMENTS_QUERY_KEY, 1, {}], [{ id: 2 }]);
    queryClient.setQueryData([KB_DOCUMENTS_QUERY_KEY, 'detail', 1, 2], { id: 2 });
    queryClient.setQueryData([KB_SETTINGS_QUERY_KEY, 1], { enabled: true });
    useUiStore.setState({ activeView: 'knowledge' });

    resetKbStateForNewUser();

    expect(queryClient.getQueryData([KB_SOURCES_QUERY_KEY, 1])).toBeUndefined();
    expect(queryClient.getQueryData([KB_DOCUMENTS_QUERY_KEY, 1, {}])).toBeUndefined();
    expect(
      queryClient.getQueryData([KB_DOCUMENTS_QUERY_KEY, 'detail', 1, 2]),
    ).toBeUndefined();
    expect(queryClient.getQueryData([KB_SETTINGS_QUERY_KEY, 1])).toBeUndefined();
    expect(useUiStore.getState().activeView).toBe('feeds');
  });
});
