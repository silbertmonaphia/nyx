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

  it('drops the previous user KB cache entries and returns to the feeds view', () => {
    queryClient.setQueryData([KB_SOURCES_QUERY_KEY, 1], [{ id: 1, name: 'a' }]);
    queryClient.setQueryData([KB_DOCUMENTS_QUERY_KEY, 1, {}], [{ id: 2 }]);
    queryClient.setQueryData([KB_DOCUMENTS_QUERY_KEY, 'detail', 1, 2], { id: 2 });
    queryClient.setQueryData([KB_SETTINGS_QUERY_KEY, 1], { enabled: true });
    queryClient.setQueryData([KB_SETTINGS_QUERY_KEY, 2], { enabled: true });
    useUiStore.setState({ activeView: 'knowledge' });

    resetKbStateForNewUser(2);

    expect(queryClient.getQueryData([KB_SOURCES_QUERY_KEY, 1])).toBeUndefined();
    expect(queryClient.getQueryData([KB_DOCUMENTS_QUERY_KEY, 1, {}])).toBeUndefined();
    expect(
      queryClient.getQueryData([KB_DOCUMENTS_QUERY_KEY, 'detail', 1, 2]),
    ).toBeUndefined();
    expect(queryClient.getQueryData([KB_SETTINGS_QUERY_KEY, 1])).toBeUndefined();
    expect(queryClient.getQueryData([KB_SETTINGS_QUERY_KEY, 2])).toEqual({
      enabled: true,
    });
    expect(useUiStore.getState().activeView).toBe('feeds');
  });

  it('keeps the in-flight settings probe for the current user on mount', () => {
    queryClient.setQueryData([KB_SETTINGS_QUERY_KEY, 7], { enabled: true });

    resetKbStateForNewUser(7);

    expect(queryClient.getQueryData([KB_SETTINGS_QUERY_KEY, 7])).toEqual({
      enabled: true,
    });
  });

  it('clears every settings entry on logout', () => {
    queryClient.setQueryData([KB_SETTINGS_QUERY_KEY, 1], { enabled: true });
    queryClient.setQueryData([KB_SETTINGS_QUERY_KEY, 2], { enabled: true });

    resetKbStateForNewUser(null);

    expect(queryClient.getQueryData([KB_SETTINGS_QUERY_KEY, 1])).toBeUndefined();
    expect(queryClient.getQueryData([KB_SETTINGS_QUERY_KEY, 2])).toBeUndefined();
  });
});
