import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { kbService } from '../services/kbService';
import type { KbDocumentFilters } from '../types/kb';
import { useAuthStore } from '~/store/authStore';
import { KB_DOCUMENTS_QUERY_KEY, KB_SETTINGS_QUERY_KEY, KB_SOURCES_QUERY_KEY } from '~/services/queryKeys';

const useKbUserId = () => useAuthStore((state) => state.user?.id);

const invalidateSourcesAndDocuments = (queryClient: ReturnType<typeof useQueryClient>) => {
  void queryClient.invalidateQueries({ queryKey: [KB_SOURCES_QUERY_KEY] });
  void queryClient.invalidateQueries({ queryKey: [KB_DOCUMENTS_QUERY_KEY] });
};

export const useKbSources = () => {
  const userId = useKbUserId();
  return useQuery({
    queryKey: [KB_SOURCES_QUERY_KEY, userId],
    queryFn: kbService.listSources,
    enabled: userId !== undefined,
    staleTime: 15_000,
  });
};

export const useKbSettings = () => {
  const userId = useKbUserId();
  return useQuery({
    queryKey: [KB_SETTINGS_QUERY_KEY, userId],
    queryFn: kbService.getSettings,
    enabled: userId !== undefined,
    staleTime: 60_000,
  });
};

export const useKbDocuments = (filters: KbDocumentFilters = {}) => {
  const userId = useKbUserId();
  return useQuery({
    queryKey: [KB_DOCUMENTS_QUERY_KEY, userId, filters],
    queryFn: () => kbService.listDocuments(filters),
    enabled: userId !== undefined,
    staleTime: 15_000,
  });
};

export const useKbDocument = (id: number | null) => {
  const userId = useKbUserId();
  return useQuery({
    queryKey: [KB_DOCUMENTS_QUERY_KEY, 'detail', userId, id],
    queryFn: () => kbService.getDocument(id as number),
    enabled: id !== null && userId !== undefined,
  });
};

export const useKbMutations = () => {
  const queryClient = useQueryClient();
  const createSource = useMutation({
    mutationFn: ({ name, rootPath }: { name: string; rootPath: string }) =>
      kbService.createSource(name, rootPath),
    onSuccess: () => invalidateSourcesAndDocuments(queryClient),
  });
  const updateSource = useMutation({
    mutationFn: ({ id, enabled }: { id: number; enabled: boolean }) =>
      kbService.updateSource(id, enabled),
    onSuccess: () => invalidateSourcesAndDocuments(queryClient),
  });
  const deleteSource = useMutation({
    mutationFn: (id: number) => kbService.deleteSource(id),
    onSuccess: () => invalidateSourcesAndDocuments(queryClient),
  });
  const uploadDocument = useMutation({
    mutationFn: (file: File) => kbService.uploadDocument(file),
    onSuccess: () => invalidateSourcesAndDocuments(queryClient),
  });
  const createTextDocument = useMutation({
    mutationFn: ({ name, content }: { name: string; content: string }) =>
      kbService.createTextDocument(name, content),
    onSuccess: () => invalidateSourcesAndDocuments(queryClient),
  });
  const updateDocument = useMutation({
    mutationFn: ({ id, content }: { id: number; content: string }) =>
      kbService.updateDocument(id, content),
    onSuccess: () => invalidateSourcesAndDocuments(queryClient),
  });
  const deleteDocument = useMutation({
    mutationFn: (id: number) => kbService.deleteDocument(id),
    onSuccess: () => invalidateSourcesAndDocuments(queryClient),
  });
  const indexSources = useMutation({
    mutationFn: kbService.indexSources,
    onSuccess: () => invalidateSourcesAndDocuments(queryClient),
  });
  return {
    createSource,
    updateSource,
    deleteSource,
    uploadDocument,
    createTextDocument,
    updateDocument,
    deleteDocument,
    indexSources,
  };
};
